"""Chấm điểm một hồ sơ với danh sách job ứng viên (Go đã lọc sẵn bằng pgvector).

Quy trình hai tầng:
  1. Bộ lọc cứng (filters.py): loại job sai cấp kinh nghiệm, thiếu kỹ năng bắt buộc hoặc đã hết hạn nộp.
     Chỉ regex, rất nhanh.
  2. Chấm điểm mềm cho job còn lại: embedding tiêu đề + rerank (chậm) + điểm luật, rồi nhân hệ số phạt
     khi thiếu techstack quan trọng và hệ số giảm khi sắp hết hạn nộp.
"""
import time
from datetime import datetime, timezone

from . import config
from .filters import apply_hard_filters, days_left, experience_band, required_years
from .normalize import detect_city, match_skills, nfc, parse_skill_list
from .scoring import (
    combine, deadline_factor, exp_fit_score, experience_score, extra_score, key_skills, salary_scores, semantic_score,
    skill_penalty, skill_score, title_score,
)
from .texts import job_to_text, profile_to_text, skills_name


class Matcher:
    def __init__(self, embedder=None, reranker=None):
        self._embedder = embedder
        self._reranker = reranker

    @property
    def embedder(self):
        if self._embedder is None:
            from .embedder import load_embedder
            self._embedder = load_embedder()
        return self._embedder

    @property
    def reranker(self):
        if self._reranker is None:
            from .reranker import load_reranker
            self._reranker = load_reranker()
        return self._reranker

    def score_jobs(self, profile: dict, jobs: list, desired_title: str = "",
                   desired_location: str = "", cv_text: str = "", use_rerank: bool = None,
                   exp_filter: bool = False, now: datetime = None) -> dict:
        """
        profile: kết quả analyze_profile (phần 2).
        jobs: mỗi job là dict gồm id, title, description, skills, min_years, location,
              is_remote, languages, tuỳ chọn cosine (Go tính sẵn bằng pgvector), must_have, salary_million
              và deadline (datetime có múi giờ, crawled.parse_deadline).
        use_rerank: None = theo config.RERANK_ENABLED (production tắt để tiết kiệm RAM).
        exp_filter: loại job ngoài khoảng kinh nghiệm hợp với ứng viên (filters.experience_band) và xếp hạng
              thêm theo lương rồi tới kinh nghiệm (config.EXP_FILTER_WEIGHTS).
        now: thời điểm so với hạn nộp (mặc định lúc gọi). Job hết hạn bị loại; job còn ít hơn
              config.DEADLINE_PENALTY_DAYS ngày bị nhân scoring.deadline_factor vào điểm.
        Trả về {
          "results": job qua bộ lọc cứng, đã sắp xếp giảm dần theo điểm,
          "rejected": job bị bộ lọc cứng loại (score 0, passed False, kèm reject_reasons),
          "timings": {...},
          "rerank_error": chỉ có khi API rerank lỗi; khi đó các job được chấm không có rerank,
          "exp_band": [thấp, cao] số năm job được giữ, chỉ có khi exp_filter và biết số năm của ứng viên,
        }.
        """
        timings = {}
        now = now or datetime.now(timezone.utc)
        if use_rerank is None:
            use_rerank = config.RERANK_ENABLED
        if not jobs:
            return {"results": [], "rejected": [], "timings": timings}

        cv_names = skills_name(profile.get("skills"))
        cv_lower = nfc(cv_text or profile_to_text(profile)).lower()
        cv_years = profile.get("years_experience")
        band = experience_band(cv_years) if exp_filter else None
        weights = {**config.WEIGHTS, **config.EXP_FILTER_WEIGHTS} if exp_filter else config.WEIGHTS
        target_city = detect_city(desired_location) or detect_city(profile.get("location", ""))
        query = profile_to_text(profile, desired_title)

        def skills_split(job):
            return match_skills(parse_skill_list(job.get("skills")), cv_names, cv_lower)

        # 1) Bộ lọc cứng: chạy trước để job bị loại không tốn thời gian embedding và rerank
        t = time.perf_counter()
        kept, dropped = apply_hard_filters(jobs, cv_years, cv_names, cv_lower, band, now)
        rejected = []
        for job, reasons in dropped:
            matched, missing = skills_split(job)
            rejected.append({
                "job_id": job.get("id"), "score": 0.0, "passed": False,
                "reject_reasons": reasons, "matched_skills": matched, "missing_skills": missing,
            })
        timings["hard_filter"] = time.perf_counter() - t
        if not kept:
            out = {"results": [], "rejected": rejected, "timings": timings}
            if band:
                out["exp_band"] = list(band)
            return out

        # 2) Độ khớp chức danh: so công việc mong muốn với tiêu đề từng job
        title_cos = [None] * len(kept)
        if desired_title:
            t = time.perf_counter()
            q = self.embedder.encode_query(desired_title)
            title_embs = self.embedder.encode_docs([nfc(j.get("title", "")) for j in kept])
            title_cos = [float(x) for x in title_embs @ q]
            timings["title"] = time.perf_counter() - t

        # 3) Các thành phần rẻ cho từng job, cùng hệ số phạt thiếu techstack và hệ số sắp hết hạn
        salary = salary_scores([j.get("salary_million") for j in kept]) if exp_filter else [None] * len(kept)
        rows = []
        for k, job in enumerate(kept):
            matched, missing = skills_split(job)
            need = required_years(job)                                  # tính cả cấp bậc trong tiêu đề
            comps = {
                "skills": skill_score(matched, missing),
                "rerank": None,
                "title": title_score(title_cos[k]),
                "experience": experience_score(cv_years, need),
                "semantic": semantic_score(job.get("cosine")),
                "extra": extra_score(job, profile, cv_lower, target_city),
            }
            if exp_filter:
                comps["salary"] = salary[k]
                comps["exp_fit"] = exp_fit_score(need, band)
            left = days_left(job, now)
            factor = skill_penalty(missing, key_skills(job)) * deadline_factor(left)
            rows.append((job, comps, matched, missing, left, factor))

        # 4) Rerank: cross-encoder đọc cặp (hồ sơ, job), chỉ cho RERANK_TOP_N job có điểm sơ bộ cao nhất
        rerank_error = ""
        if use_rerank:
            order = sorted(range(len(rows)), key=lambda k: combine(rows[k][1], weights) * rows[k][5], reverse=True)
            top = order[:config.RERANK_TOP_N] if config.RERANK_TOP_N > 0 else order
            t = time.perf_counter()
            try:
                scores = self.reranker.score(query, [job_to_text(rows[k][0]) for k in top])
                for k, s in zip(top, scores):
                    rows[k][1]["rerank"] = float(s)
            except Exception as e:  # noqa: BLE001
                # API rerank lỗi thì chấm tiếp không có rerank, không làm hỏng cả lượt tìm việc
                if config.RERANK_BACKEND != "api":
                    raise
                rerank_error = f"{type(e).__name__}: {e}"
            timings["rerank"] = time.perf_counter() - t
            reranked = len(top)
        else:
            reranked = 0

        # 5) Điểm cuối
        results = []
        for job, comps, matched, missing, left, factor in rows:
            penalty = skill_penalty(missing, key_skills(job))
            deadline_f = deadline_factor(left)
            score = round(combine(comps, weights) * factor, 2)
            results.append({
                "job_id": job.get("id"),
                "score": score,
                "passed": score >= config.MATCH_THRESHOLD,
                "penalty": round(penalty, 3),
                "deadline_factor": round(deadline_f, 3),
                "days_left": round(left, 2) if left is not None else None,
                "components": {key: (round(v, 3) if v is not None else None) for key, v in comps.items()},
                "matched_skills": matched,
                "missing_skills": missing,
            })

        results.sort(key=lambda r: r["score"], reverse=True)
        for rank, r in enumerate(results, 1):
            r["rank"] = rank
        out = {"results": results, "rejected": rejected, "timings": timings, "reranked": reranked}
        if band:
            out["exp_band"] = list(band)
        if rerank_error:
            out["rerank_error"] = rerank_error
        return out
