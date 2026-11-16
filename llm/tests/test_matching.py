"""Chạy thử phần 3. Phần A không cần mô hình; phần B tải bge-m3 và reranker (lần đầu ~4.5 GB).

Dữ liệu: job cào từ TopCV (backend/data/jobs-scrape/topcv/jobs-description.json);
hồ sơ ứng viên do tests\\test_llm.py phân tích từ CV_Duong_Tuan_Khang.pdf và lưu vào data\\profiles\\ (chạy file đó trước).
Kết quả xếp hạng ghi ra data\\reports\\matching_CV_Duong_Tuan_Khang.md.

Chạy: .venv\\Scripts\\python.exe tests\\test_matching.py            (phần A + B, báo cáo có rerank)
      .venv\\Scripts\\python.exe tests\\test_matching.py --offline  (chỉ phần A, báo cáo chỉ có điểm luật)
"""
import json
import sys
import time
from pathlib import Path

import numpy as np

LLM_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(LLM_DIR))
# Khi output bị chuyển hướng (nút Run của VS Code, pipe), Windows dùng cp1252 và print tiếng Việt sẽ lỗi
sys.stdout.reconfigure(encoding="utf-8")

from matching import config, report  # noqa: E402  (đặt HF_HOME trước khi import sentence_transformers)
from matching.filters import apply_hard_filters, must_have_groups, required_years, seniority_levels  # noqa: E402
from matching.matcher import Matcher  # noqa: E402
from matching.normalize import detect_languages, find_known_skills, nfc, normalize_skill, parse_skill_list  # noqa: E402
from matching.texts import job_to_text, profile_to_text, skills_name  # noqa: E402

JOBS_FILE = LLM_DIR.parent / "backend" / "data" / "jobs-scrape" / "topcv" / "jobs-description.json"
SAMPLE_CV = LLM_DIR / "data" / "cv" / "CV_Duong_Tuan_Khang.pdf"
PROFILES_DIR = LLM_DIR / "data" / "profiles"     # do tests\test_llm.py ghi ra
REPORTS_DIR = LLM_DIR / "data" / "reports"
DESIRED_TITLE = "Fullstack Developer"

# Job không thuộc ngành phần mềm, ứng viên IT không nên được xếp chúng lên đầu
NON_SOFTWARE_WORDS = ("cnc", "plc", "phay tiện")


# ============================ DỮ LIỆU ============================
def _skills_of(raw: dict, text: str) -> list:
    """Kỹ năng job: dùng danh sách TopCV ghi sẵn; không có thì dò từ điển trong văn bản.

    Ở hệ thống thật, kỹ năng do analyze_job (LLM) trích ra; ở đây không gọi LLM cho 50 job vì quá chậm.
    """
    tagged = raw.get("skills") or {}
    if isinstance(tagged, dict) and tagged.get("all"):
        return parse_skill_list(tagged["all"])[:15]
    return find_known_skills(text)[:15]


def scraped_job_to_matcher(raw: dict) -> dict:
    """Bản ghi cào từ TopCV -> dict job mà Matcher nhận (giống dữ liệu Go gửi sang)."""
    ov = raw.get("overview") or {}
    description = "\n".join(nfc(raw.get(k) or "") for k in ("description", "requirements") if raw.get(k))
    months = ov.get("experience_months")
    work_type = (ov.get("work_type") or "").lower()
    return {
        "id": raw["id"],
        "url": raw.get("url", ""),
        "title": nfc(raw.get("title", "")),
        "description": description,
        "skills": _skills_of(raw, description),
        "min_years": int(round(months / 12)) if months is not None else None,
        "location": ov.get("location", ""),
        "is_remote": "remote" in work_type and "văn phòng" not in work_type,   # chỉ khi làm từ xa hoàn toàn
        "languages": sorted(detect_languages(description.lower())),
    }


def load_jobs() -> list:
    with open(JOBS_FILE, encoding="utf-8") as f:
        return [scraped_job_to_matcher(r) for r in json.load(f)]


def load_profile():
    """Hồ sơ do tests\\test_llm.py phân tích từ CV mẫu và lưu lại. Trả về (hồ sơ, văn bản CV) hoặc None."""
    f = PROFILES_DIR / f"{SAMPLE_CV.stem}.json"
    if not f.exists():
        print(f"  BỎ QUA: chưa có {f}. Hãy chạy tests\\test_llm.py (cần Ollama) để tạo hồ sơ từ {SAMPLE_CV.name}")
        return None
    data = json.loads(f.read_text(encoding="utf-8"))
    return data["profile"], data["cv_text"]


def _is_non_software(job) -> bool:
    return any(w in job["title"].lower() for w in NON_SOFTWARE_WORDS)


def _check_result_shape(out, jobs):
    by_id = {j["id"]: j for j in jobs}
    results, rejected = out["results"], out["rejected"]
    assert sorted(r["job_id"] for r in results + rejected) == sorted(by_id), "mỗi job phải ở đúng một danh sách"
    assert [r["rank"] for r in results] == list(range(1, len(results) + 1))
    assert all(a["score"] >= b["score"] for a, b in zip(results, results[1:])), "chưa sắp xếp giảm dần"
    for r in results:
        assert 0 <= r["score"] <= 100, r
        assert r["passed"] == (r["score"] >= config.MATCH_THRESHOLD)
        assert config.PENALTY_FLOOR <= r["penalty"] <= 1, r
        assert all(v is None or 0 <= v <= 1 for v in r["components"].values()), r["components"]
    for r in rejected:
        assert r["score"] == 0 and not r["passed"] and r["reject_reasons"], r
    for r in results + rejected:
        assert sorted(r["matched_skills"] + r["missing_skills"]) == sorted(parse_skill_list(by_id[r["job_id"]]["skills"]))


# ============================ BÁO CÁO MARKDOWN ============================
def write_report(mode: str, profile: dict, out: dict, jobs: list, timings: dict = None, cosines=None) -> Path:
    path = report.write_report(
        REPORTS_DIR / f"matching_{SAMPLE_CV.stem}.md", f"Kết quả chấm điểm: {SAMPLE_CV.name}",
        [f"Chế độ: {mode}",
         f"Dữ liệu job: `{JOBS_FILE.relative_to(LLM_DIR.parent).as_posix()}` ({len(jobs)} job)",
         f"Công việc mong muốn: {DESIRED_TITLE}"],
        profile, out, jobs, timings, cosines)
    print(f"  Đã ghi báo cáo: {path}")
    return path


# ============================ PHẦN A: KHÔNG CẦN MÔ HÌNH ============================
def test_load_scraped_jobs():
    jobs = load_jobs()
    assert len(jobs) == 50
    assert len({j["id"] for j in jobs}) == 50, "id bị trùng"
    assert all(j["title"] and j["description"] for j in jobs)
    by_id = {j["id"]: j for j in jobs}
    assert by_id["2291824"]["min_years"] == 5                       # experience_months = 60
    assert by_id["2291824"]["languages"] == ["english"]              # "Good At English"
    assert all(j["min_years"] is None or 0 <= j["min_years"] <= 10 for j in jobs)
    assert all(s == normalize_skill(s) for j in jobs for s in j["skills"]), "kỹ năng chưa chuẩn hoá"
    no_skills = [j["title"] for j in jobs if not j["skills"]]
    print(f"  {len(jobs)} job, {len(no_skills)} job không có kỹ năng nào: {no_skills}")


def test_semantic_score_spread():
    from matching.scoring import semantic_score

    # Cosine đo được với bge-m3 trên dữ liệu TopCV (xem báo cáo): phải trải đều trên thang 0..1
    assert semantic_score(None) is None
    assert semantic_score(0.42) == 0.0                     # job CNC
    assert semantic_score(0.55) == 0.0
    assert 0.4 < semantic_score(0.668) < 0.6               # trung vị
    assert semantic_score(0.805) == 1.0                    # job fullstack khớp nhất
    assert semantic_score(0.95) == 1.0
    assert semantic_score(0.60) < semantic_score(0.70) < semantic_score(0.78)


def test_skill_version_suffix_stripped():
    cases = {"Java 11+": "java", "JavaScript ES6+": "javascript", "Python 3.x": "python", "python3": "python",
             "Angular 17": "angular", ".NET 8": ".net", "PHP 8.2": "php", "Spring Boot 3": "spring boot",
             "Java (11+)": "java", "Office 365": "ms office", "ES6+": "javascript"}
    for raw, want in cases.items():
        assert normalize_skill(raw) == want, (raw, normalize_skill(raw))
    # Phần còn lại không phải kỹ năng đã biết thì giữ nguyên
    for raw in ("C++", "C#", "Web3", "S3", "Windows 10"):
        assert normalize_skill(raw) == raw.lower(), raw


def test_compound_skill_split():
    assert parse_skill_list(["React/Next.js", "SQL/NoSQL", "AWS/GCP"]) == \
        ["react", "next.js", "sql", "nosql", "aws", "gcp"]
    assert parse_skill_list(["HTML / CSS", "React/Next.js/Vue", "react"]) == ["html", "css", "react", "next.js", "vue"]
    # Tên vốn có dấu '/' hoặc có phần chưa biết thì giữ nguyên
    for raw in ("CI/CD", "UI/UX", "PL/SQL", "TCP/IP", "A/B testing", "AI/LLM", "C/C++"):
        assert parse_skill_list([raw]) == [normalize_skill(raw)], raw
    assert parse_skill_list(["Agile/Scrum"]) == ["agile"]       # alias có sẵn


def test_scraped_tags_now_match():
    """Các tag TopCV trước đây bị tính là thiếu dù ứng viên có kỹ năng đó."""
    from matching.normalize import match_skills

    jobs = {j["id"]: j for j in load_jobs()}
    have = ["java", "react", "next.js", "sql", "javascript", "typescript"]
    cv = "java, react, next.js, sql, javascript, typescript"
    for job_id, want in (("2331815", "java"), ("2330748", "react"), ("2330748", "next.js"),
                         ("2330748", "sql"), ("2293340", "javascript")):
        matched, missing = match_skills(jobs[job_id]["skills"], have, cv)
        assert want in matched, (job_id, want, matched, missing)
    assert "java 11+" not in jobs["2331815"]["skills"]
    assert "react/next.js" not in jobs["2330748"]["skills"]


def test_implied_skills():
    from matching.normalize import match_skills

    # Ứng viên ghi MongoDB, job yêu cầu NoSQL -> có
    assert match_skills(["nosql", "sql"], ["mongodb", "postgresql"], "") == (["nosql", "sql"], [])
    # Chiều ngược lại không suy ra: biết NoSQL chung chung không có nghĩa là biết MongoDB
    assert match_skills(["mongodb"], ["nosql"], "") == ([], ["mongodb"])
    assert match_skills(["nosql"], ["java"], "") == ([], ["nosql"])


JUNIOR = {"years_experience": 0.5, "skills": [{"name": s} for s in ("react", "next.js", "typescript", "java", "spring boot")]}


def _filter_one(job, profile=JUNIOR, cv_text=""):
    """Lý do loại một job (rỗng = đạt)."""
    _, rejected = apply_hard_filters([job], profile["years_experience"], skills_name(profile["skills"]), cv_text)
    return rejected[0][1] if rejected else []


def test_seniority_from_title():
    assert seniority_levels("Senior Fullstack Java Developer") == ["senior"]
    assert seniority_levels("Full Stack Developver ( Junior - Middle )") == ["junior", "middle"]
    assert seniority_levels("Trưởng Nhóm Web Fullstack Developer") == ["lead"]
    assert seniority_levels("Kỹ Sư Phần Mềm Cấp Cao") == ["senior"]
    assert seniority_levels("Fullstack Developer") == []
    # Mức sàn theo cấp bậc thắng min_years ghi thấp; nhiều cấp thì lấy cấp thấp nhất
    assert required_years({"title": "Senior Backend Developer", "min_years": 2}) == 3
    assert required_years({"title": "Developer (Junior - Middle)", "min_years": 1}) == 1
    assert required_years({"title": "Fullstack Developer", "min_years": None}) is None


def test_must_have_from_title():
    assert must_have_groups({"title": "Full Stack Developer (.Net)"}) == [[".net"]]
    assert must_have_groups({"title": "Full Stack Developer (Java/Go)"}) == [["go", "java"]]          # Java HOẶC Go
    assert must_have_groups({"title": "Fullstack Developer (C#, .NET, Vuejs)"}) == [["c#"], [".net"], ["vue"]]
    assert must_have_groups({"title": "Senior Fullstack Developer (React Js + .NET)"}) == [["react"], [".net"]]
    assert must_have_groups({"title": "Kỹ Sư Lập Trình C++ (Senior)"}) == [["c++"]]
    assert must_have_groups({"title": "Nhân Viên Lập Trình Phay Tiện CNC"}) == [["cnc"]]
    assert must_have_groups({"title": "Fullstack Developer"}) == []
    assert must_have_groups({"title": "Developer", "must_have": ["Python 3.x"]}) == [["python"]]


def test_hard_filters():
    # Thiếu kỹ năng bắt buộc trong tiêu đề -> loại, dù kinh nghiệm đạt
    assert _filter_one({"title": "Full Stack Developer (.Net)", "min_years": 1})
    # Biết C# thì đạt yêu cầu .NET
    assert not _filter_one({"title": "Full Stack Developer (.Net)", "min_years": 1},
                           {"years_experience": 2, "skills": [{"name": "c#"}]})
    # Junior 0.5 năm: loại job Senior (sàn 3 năm) và job 2 năm, giữ job 1 năm và job Junior - Middle
    assert _filter_one({"title": "Senior Fullstack Developer", "min_years": None})
    assert _filter_one({"title": "Fullstack Developer", "min_years": 2})
    assert not _filter_one({"title": "Fullstack Developer", "min_years": 1})
    assert not _filter_one({"title": "Full Stack Developer (Junior - Middle)", "min_years": 1})
    # Java/Go: biết một trong hai là đủ
    assert not _filter_one({"title": "Full Stack Developer (Java/Go)", "min_years": 1})
    # Không có số năm của ứng viên thì không loại theo kinh nghiệm
    assert not _filter_one({"title": "Senior Developer"}, {"years_experience": None, "skills": []})


def test_parse_salary():
    from matching.crawled import parse_salary_million

    assert parse_salary_million("10 - 15 triệu") == 12.5
    assert parse_salary_million("Tới 40 triệu") == 40
    assert parse_salary_million("Từ 20 triệu") == 20
    assert parse_salary_million("1,000 - 2,000 USD") == 37.5
    assert parse_salary_million("15.000.000 ₫ - 20.000.000 ₫ mỗi tháng") == 17.5
    assert parse_salary_million("240 triệu/năm") == 20
    assert parse_salary_million("Thoả thuận") is None
    assert parse_salary_million("") is None


def test_experience_band():
    from matching.filters import experience_band

    assert experience_band(2) == (0, 2)
    assert experience_band(3) == (2, 3)
    assert experience_band(4) == (2, 5)
    assert experience_band(4.7) == (2, 5)
    assert experience_band(8) == (3, 8)
    assert experience_band(None) is None


def test_exp_filter_ranking():
    """exp=true: ứng viên 4.7 năm không được xếp job thực tập sinh; lương xếp trước kinh nghiệm."""
    from matching.crawled import crawled_job_to_matcher

    def job(i, title, experience, salary):
        return crawled_job_to_matcher({"id": str(i), "source": "test", "title": title, "experience": experience,
                                       "salary": salary, "description": "Viết content, quay dựng video, SEO."})

    profile = {"years_experience": 4.7, "skills": [{"name": "seo"}], "languages": []}
    jobs = [
        job(1, "Thực Tập Sinh Content Creator", "Không yêu cầu kinh nghiệm", "3 - 5 triệu"),
        job(2, "Content Creator", "2 năm", "20 triệu"),
        job(3, "Content Creator", "3 năm", "18 triệu"),
        job(4, "Content Creator", "2 năm", "17 triệu"),
        job(5, "Content Creator", "3 năm", "21 triệu"),
        job(6, "Content Creator", "7 năm", "50 triệu"),
    ]
    matcher = Matcher(embedder=object(), reranker=object())

    plain = matcher.score_jobs(profile, jobs, use_rerank=False)
    assert "test:1" in {r["job_id"] for r in plain["results"]}, "không bật exp thì giữ job thực tập như cũ"

    out = matcher.score_jobs(profile, jobs, use_rerank=False, exp_filter=True)
    assert out["exp_band"] == [2, 5]
    assert {r["job_id"] for r in out["rejected"]} == {"test:1", "test:6"}
    rank = {r["job_id"]: r["rank"] for r in out["results"]}
    assert rank["test:2"] < rank["test:3"], "2 năm 20tr phải trên 3 năm 18tr"
    assert rank["test:5"] < rank["test:4"], "lương quan trọng hơn kinh nghiệm: 3 năm 21tr trên 2 năm 17tr"
    assert all("salary" in r["components"] and "exp_fit" in r["components"] for r in out["results"])


def test_parse_deadline():
    from datetime import datetime, timedelta, timezone

    from matching.crawled import parse_deadline

    vn = timezone(timedelta(hours=config.DEADLINE_TZ_HOURS))
    end_of_day = datetime(2026, 11, 30, 23, 59, 59, tzinfo=vn)
    assert parse_deadline("30/11/2026") == end_of_day
    assert parse_deadline("Hạn nộp hồ sơ: 30/11/2026") == end_of_day
    assert parse_deadline("2026-11-30") == end_of_day
    assert parse_deadline("2026-11-19T23:59:59+07:00") == datetime(2026, 11, 19, 23, 59, 59, tzinfo=vn)
    assert parse_deadline("2026-11-19T10:00:00Z") == datetime(2026, 11, 19, 10, tzinfo=timezone.utc)
    assert parse_deadline("") is None and parse_deadline(None) is None
    assert parse_deadline("Còn 5 ngày") is None
    assert parse_deadline("31/02/2026") is None


def test_deadline_factor():
    from matching.scoring import deadline_factor

    T, floor = config.DEADLINE_PENALTY_DAYS, config.DEADLINE_FLOOR
    assert deadline_factor(None) == 1.0
    assert deadline_factor(20) == deadline_factor(15) == deadline_factor(T) == 1.0
    assert deadline_factor(0) == deadline_factor(-1) == 0.0
    assert floor < deadline_factor(0.01) < deadline_factor(0.5) < deadline_factor(2) < deadline_factor(4) < 1.0
    assert abs(deadline_factor(T / 4) - (floor + (1 - floor) * 0.25 ** config.DEADLINE_CURVE)) < 1e-9


def test_deadline_filter_and_score():
    """Job hết hạn bị loại, job sát hạn bị trừ điểm, job không ghi hạn giữ nguyên."""
    from datetime import datetime, timezone

    from matching.crawled import crawled_job_to_matcher

    now = datetime(2026, 11, 12, 3, 0, tzinfo=timezone.utc)      # 10:00 giờ Việt Nam

    def job(i, deadline):
        return crawled_job_to_matcher({"id": str(i), "source": "test", "title": "Content Creator",
                                       "experience": "1 năm", "deadline": deadline,
                                       "description": "Viết content, quay dựng video, SEO."})

    jobs = [job(1, "30/11/2026"), job(2, "13/11/2026"), job(3, "11/11/2026"), job(4, ""),
            job(5, "2026-11-12T09:00:00+07:00"), job(6, "12/11/2026")]
    profile = {"years_experience": 2, "skills": [{"name": "seo"}], "languages": []}
    out = Matcher(embedder=object(), reranker=object()).score_jobs(profile, jobs, use_rerank=False, now=now)

    rejected = {r["job_id"]: r["reject_reasons"] for r in out["rejected"]}
    assert set(rejected) == {"test:3", "test:5"}, rejected
    assert rejected["test:3"] == ["đã hết hạn nộp hồ sơ (11/11/2026)"]
    by_id = {r["job_id"]: r for r in out["results"]}
    assert by_id["test:1"]["deadline_factor"] == 1.0 and by_id["test:4"]["deadline_factor"] == 1.0
    assert by_id["test:4"]["days_left"] is None
    assert 1 < by_id["test:2"]["days_left"] < 2
    assert 0 < by_id["test:6"]["days_left"] < 1, "hạn chỉ ghi ngày: vẫn nộp được tới hết ngày hôm đó"
    assert by_id["test:1"]["score"] > by_id["test:2"]["score"] > by_id["test:6"]["score"]
    assert by_id["test:1"]["rank"] == 1


def test_rerank_only_top_n():
    """Chỉ RERANK_TOP_N job có điểm sơ bộ cao nhất được rerank; job còn lại chấm không có rerank."""
    from matching.crawled import crawled_job_to_matcher

    class FakeReranker:
        def __init__(self):
            self.docs = []

        def score(self, query, docs):
            self.docs += docs
            return [0.9] * len(docs)

    def job(i, skills):
        return crawled_job_to_matcher({"id": str(i), "source": "test", "title": "Digital Marketing",
                                       "experience": "1 năm", "skills": skills,
                                       "description": "Chạy quảng cáo, đo lường hiệu quả."})

    jobs = [job(1, "seo, photoshop, figma"), job(2, "seo, google analytics"), job(3, "photoshop, premiere, figma"),
            job(4, "seo, google analytics, facebook ads")]
    profile = {"years_experience": 2, "languages": [],
               "skills": [{"name": "seo"}, {"name": "google analytics"}, {"name": "facebook ads"}]}
    reranker = FakeReranker()
    old = config.RERANK_TOP_N
    config.RERANK_TOP_N = 2
    try:
        out = Matcher(embedder=object(), reranker=reranker).score_jobs(profile, jobs, use_rerank=True)
    finally:
        config.RERANK_TOP_N = old
    assert len(reranker.docs) == 2 and out["reranked"] == 2, (len(reranker.docs), out.get("reranked"))
    by_id = {r["job_id"]: r["components"]["rerank"] for r in out["results"]}
    assert by_id["test:4"] == 0.9 and by_id["test:2"] == 0.9, by_id
    assert by_id["test:3"] is None and by_id["test:1"] is None, by_id


def test_skill_weight_and_penalty():
    from matching.scoring import skill_penalty, skill_score

    # Thiếu .net (cốt lõi) nặng hơn thiếu jira (công cụ): cùng 1 có / 1 thiếu nhưng điểm khác nhau
    assert skill_score(["jira"], [".net"]) == 1 / 4
    assert skill_score([".net"], ["jira"]) == 3 / 4
    assert skill_penalty(["jira", "git"], set()) == 1.0                   # thiếu công cụ phụ: không phạt
    assert skill_penalty(["vue"], set()) == 1 - config.PENALTY_MISSING_CORE
    assert skill_penalty(["vue"], {"vue"}) == 1 - config.PENALTY_MISSING_KEY
    assert skill_penalty(["vue", "angular", "python", "c#"], {"vue", "angular", "python", "c#"}) == config.PENALTY_FLOOR


def test_rule_scores_without_models():
    """Chỉ dùng điểm luật (kỹ năng, kinh nghiệm, địa điểm, ngoại ngữ), không rerank, không so chức danh."""
    loaded = load_profile()
    if loaded is None:
        return
    profile, cv_text = loaded
    jobs = load_jobs()
    by_id = {j["id"]: j for j in jobs}
    matcher = Matcher(embedder=object(), reranker=object())       # gọi tới mô hình là lỗi ngay
    out = matcher.score_jobs(profile, jobs, cv_text=cv_text, use_rerank=False)
    results = out["results"]
    _check_result_shape(out, jobs)
    assert all(r["components"]["rerank"] is None and r["components"]["title"] is None for r in results)
    best_skill = max(results, key=lambda r: r["components"]["skills"] or 0)
    assert not _is_non_software(by_id[best_skill["job_id"]]), "job khớp kỹ năng nhất phải là job phần mềm"
    if "--offline" in sys.argv:      # chạy đủ thì phần B ghi báo cáo đầy đủ hơn
        write_report("chỉ điểm luật (không rerank, không so chức danh, không cosine)", profile, out, jobs)


# ============================ PHẦN B: MÔ HÌNH THẬT ============================
def test_ranking_with_models():
    loaded = load_profile()
    if loaded is None:
        return
    profile, cv_text = loaded
    from matching.embedder import Embedder
    from matching.reranker import Reranker

    jobs = load_jobs()
    by_id = {j["id"]: j for j in jobs}

    t = time.perf_counter()
    embedder, reranker = Embedder(), Reranker()
    timings = {"load_models": time.perf_counter() - t}

    # Go tính sẵn cosine hồ sơ - job bằng pgvector, chỉ cho job đã qua lọc trong SQL; ở đây tự tính bằng cùng
    # mô hình và cũng chỉ cho job qua bộ lọc cứng (job bị loại không cần cosine)
    kept, _ = apply_hard_filters(jobs, profile.get("years_experience"), skills_name(profile.get("skills")),
                                 nfc(cv_text).lower())
    t = time.perf_counter()
    job_embs = embedder.encode_docs([job_to_text(j) for j in kept])
    cos = job_embs @ embedder.encode_docs([profile_to_text(profile)])[0]
    timings["embed_jobs"] = time.perf_counter() - t
    cos_by_id = {j["id"]: float(c) for j, c in zip(kept, cos)}
    with_cos = [{**j, "cosine": cos_by_id.get(j["id"])} for j in jobs]

    out = Matcher(embedder, reranker).score_jobs(profile, with_cos, desired_title=DESIRED_TITLE, cv_text=cv_text)
    results = out["results"]
    timings.update(out["timings"])
    _check_result_shape(out, jobs)
    write_report("đầy đủ (bge-m3 + bge-reranker-v2-m3)", profile, out, jobs, timings, cos.tolist())

    ranked = [by_id[r["job_id"]] for r in results]
    assert not any(_is_non_software(j) for j in ranked), "job CNC/PLC phải bị bộ lọc cứng loại"
    assert not any("senior" in seniority_levels(j["title"]) for j in ranked), "ứng viên 0.5 năm không được vào job Senior"


if __name__ == "__main__":
    offline = "--offline" in sys.argv
    tests = [(n, f) for n, f in list(globals().items()) if n.startswith("test_") and callable(f)]
    if offline:
        tests = [(n, f) for n, f in tests if not n.endswith("_with_models")]
    failed = 0
    for name, fn in tests:
        try:
            fn()
            print(f"PASS  {name}")
        except AssertionError as e:
            failed += 1
            print(f"FAIL  {name}: {e}")
    print(f"\n{len(tests) - failed}/{len(tests)} test đạt" if not failed else f"\n{failed} test lỗi")
    sys.exit(1 if failed else 0)
