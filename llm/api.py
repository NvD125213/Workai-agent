"""Service HTTP nội bộ của phần AI, chỉ backend Go gọi (không mở ra ngoài).

  POST /parse-cv    file CV (pdf, docx, txt, md) -> văn bản; 422 nếu nội dung không giống CV
  POST /analyze-cv  văn bản CV -> hồ sơ có cấu trúc + embedding + keyword tìm việc
  POST /score       hồ sơ + job vừa cào -> bộ lọc cứng (cả job hết hạn nộp) + điểm phù hợp (+ báo cáo data\\reports\\find_job_<id>.md)
  GET  /health

Chạy (từ thư mục llm):
  .venv\\Scripts\\python.exe -m uvicorn api:app --host 127.0.0.1 --port 8001
Tài liệu API: http://127.0.0.1:8001/docs
Mô hình embedding/reranker được nạp ở lần gọi đầu tiên cần đến (khoảng 30-80 giây).
APP_ENV=production: LLM và reranker qua API, bge-m3 trên máy giống development. Chỉ chạy 1 worker
uvicorn, mỗi worker nạp một bản mô hình riêng. Xem matching\\config.py.
"""
import logging
import threading
from datetime import datetime, timezone

import numpy as np
from fastapi import FastAPI, File, HTTPException, UploadFile
from pydantic import BaseModel, ConfigDict, Field

from matching import config  # đặt HF_HOME, HF_HUB_OFFLINE trước khi import sentence_transformers
from matching import llm, report
from matching.crawled import crawled_job_to_matcher
from matching.cv_check import check_cv, check_profile
from matching.cv_parser import read_cv_bytes
from matching.filters import apply_hard_filters, experience_band, required_years
from matching.matcher import Matcher
from matching.normalize import broad_keyword, nfc
from matching.texts import job_to_text, profile_to_text, skills_name

MAX_CV_BYTES = 10 << 20  # 10 MiB
# 5 nguồn x FIND_JOB_MAX_PAGES (3) trang x tới 50 tin/trang = 750, chừa thêm cho nguồn trả nhiều tin hơn
MAX_JOBS_PER_SCORE = 1500
MODEL_VERSION = config.EMBED_MODEL + (":onnx-int8" if config.EMBED_BACKEND == "onnx" else "") + (
    f"+{config.RERANK_MODEL}" if config.RERANK_ENABLED else "")

log = logging.getLogger("uvicorn.error")
app = FastAPI(title="Notification Job AI service", version="1.0")
_matcher = Matcher()
# Mô hình lớn và ăn CPU: chạy song song chỉ làm mỗi request chậm hơn và tốn RAM gấp đôi
_model_lock = threading.Lock()


class AnalyzeRequest(BaseModel):
    cv_text: str = Field(min_length=50, max_length=200_000)


class CrawledJob(BaseModel):
    """Một services.AnalyzedJob bên Go."""
    model_config = ConfigDict(extra="ignore")

    id: str
    source: str
    url: str = ""
    title: str
    company: str = ""
    description: str = ""
    requirements: str = ""
    skills: list[str] = []
    location: str = ""
    experience: str = ""
    salary: str = ""
    deadline: str = ""            # hạn nộp: RFC 3339, yyyy-mm-dd hoặc dd/mm/yyyy; rỗng = không rõ


class ScoreRequest(BaseModel):
    profile: dict                 # kết quả "profile" của /analyze-cv
    cv_text: str = ""
    desired_title: str = ""
    desired_location: str = ""
    jobs: list[CrawledJob] = Field(max_length=MAX_JOBS_PER_SCORE)
    # Có thì ghi báo cáo data\reports\find_job_<search_id>.md
    search_id: int | None = Field(default=None, ge=1)
    keyword: str = ""
    # Lọc job theo khoảng kinh nghiệm hợp với ứng viên, xếp hạng thêm theo lương rồi kinh nghiệm
    exp_filter: bool = False


@app.get("/health")
def health():
    return {"status": "ok", "app_env": config.APP_ENV, "llm": f"{config.LLM_BACKEND}:{config.LLM_MODEL}",
            "embedding_model": config.EMBED_MODEL, "model_version": MODEL_VERSION}


@app.post("/parse-cv")
def parse_cv(file: UploadFile = File(...)):
    data = file.file.read(MAX_CV_BYTES + 1)
    if len(data) > MAX_CV_BYTES:
        raise HTTPException(413, f"CV lớn hơn {MAX_CV_BYTES >> 20} MB")
    try:
        text = read_cv_bytes(data, file.filename or "")
    except ValueError as e:              # sai định dạng, CV là ảnh scan...
        raise HTTPException(422, str(e)) from e
    if reason := check_cv(text):         # không phải CV: loại luôn, không tốn thời gian của LLM
        raise HTTPException(422, reason)
    return {"text": text, "chars": len(text)}


@app.post("/analyze-cv")
def analyze_cv(req: AnalyzeRequest):
    try:
        profile = llm.analyze_profile(req.cv_text)
    except Exception as e:               # Ollama tắt / API lỗi, hết thời gian chờ, trả JSON hỏng
        raise HTTPException(502, f"Phân tích CV bằng LLM thất bại: {e}") from e
    if reason := check_profile(profile):
        raise HTTPException(422, reason)

    with _model_lock:
        embedding = _matcher.embedder.encode_docs([profile_to_text(profile)])[0]
    db = llm.profile_to_db(profile, req.cv_text)
    title = profile.get("current_title") or next(iter(profile.get("job_titles") or []), "")
    return {
        "profile": profile,                      # đầy đủ, Go lưu lại để gửi kèm khi gọi /score
        "user_profile": db["user_profile"],      # giá trị cột của user_profiles
        "user_skills": db["user_skills"],
        "embedding": embedding.tolist(),
        "embedding_model": config.EMBED_MODEL,
        "desired_title": title,
        "search_keyword": broad_keyword(title),  # "Frontend Developer" -> "Developer"
    }


@app.post("/score")
def score(req: ScoreRequest):
    """Kết quả giống Matcher.score_jobs; job_id là '<source>:<id>' (jobs.unique_identifier bên Go).

    Job đạt ngưỡng có thêm "job_data" (số năm yêu cầu, địa điểm, kỹ năng, embedding...) để Go lưu job
    đã phân tích đầy đủ; job không đạt Go không lưu nên không cần. "summary" đếm số job theo từng bước.
    """
    jobs = [crawled_job_to_matcher(j.model_dump()) for j in req.jobs]
    profile = req.profile
    now = datetime.now(timezone.utc)     # cùng một mốc cho cả hai lần lọc để kết quả lọc không lệch nhau
    with _model_lock:
        # Cosine hồ sơ - job chỉ cần cho job qua bộ lọc cứng (score_jobs lọc lại, nhưng chỉ tốn regex)
        cv_lower = nfc(req.cv_text or profile_to_text(profile)).lower()
        years = profile.get("years_experience")
        kept, _ = apply_hard_filters(jobs, years, skills_name(profile.get("skills")), cv_lower,
                                     experience_band(years) if req.exp_filter else None, now)
        cosine, job_emb = {}, {}
        if kept:
            profile_emb = _matcher.embedder.encode_docs([profile_to_text(profile, req.desired_title)])[0]
            embs = _matcher.embedder.encode_docs([job_to_text(j) for j in kept])
            job_emb = {j["id"]: e for j, e in zip(kept, embs)}
            cosine = {j["id"]: float(c) for j, c in zip(kept, embs @ profile_emb)}
        for j in jobs:
            j["cosine"] = cosine.get(j["id"])
        out = _matcher.score_jobs(profile, jobs, desired_title=req.desired_title,
                                  desired_location=req.desired_location, cv_text=req.cv_text,
                                  exp_filter=req.exp_filter, now=now)

    if out.get("rerank_error"):
        log.warning("rerank API lỗi, chấm điểm không có rerank: %s", out["rerank_error"])
    by_id = {j["id"]: j for j in jobs}
    for r in out["results"]:
        if r["passed"]:
            r["job_data"] = _job_data(by_id[r["job_id"]], job_emb.get(r["job_id"]))
    out["summary"] = {
        "crawled": len(jobs),
        "rejected": len(out["rejected"]),
        "scored": len(out["results"]),
        "passed": sum(r["passed"] for r in out["results"]),
        "reject_reasons": [{"reason": k, "count": n} for k, n in report.reject_summary(out["rejected"])],
    }
    out["model_version"] = MODEL_VERSION
    out["threshold"] = config.MATCH_THRESHOLD
    if req.search_id:
        out["report"] = _write_find_job_report(req, profile, out, jobs)
    return _jsonable(out)


def _job_data(job: dict, embedding) -> dict:
    """Giá trị cột của jobs mà bước chấm điểm đã tính, cho job sẽ được lưu."""
    return {
        "min_years": job.get("min_years"),
        "required_years": required_years(job),
        "location": job.get("location", ""),
        "is_remote": bool(job.get("is_remote")),
        "languages": job.get("languages", []),
        "skills": job.get("skills", []),
        "embedding": embedding.tolist() if embedding is not None else None,
        "embedding_model": config.EMBED_MODEL if embedding is not None else "",
    }


def _write_find_job_report(req: ScoreRequest, profile: dict, out: dict, jobs: list) -> str:
    """Báo cáo của một lượt find-job; lỗi ghi file không làm hỏng kết quả chấm điểm."""
    path = config.DATA_DIR / "reports" / f"find_job_{req.search_id}.md"
    try:
        report.write_report(
            path, f"Kết quả tìm việc #{req.search_id}",
            [f"Keyword cào: {req.keyword or '-'}", f"Công việc mong muốn: {req.desired_title or '-'}",
             f"Địa điểm mong muốn: {req.desired_location or '-'}", f"Số job đã cào: {len(jobs)}",
             f"Lọc kinh nghiệm + lương (exp): {'bật' if req.exp_filter else 'tắt'}"],
            profile, out, jobs, out.get("timings"), [j["cosine"] for j in jobs if j.get("cosine") is not None])
    except OSError as e:
        log.warning("write report %s: %s", path, e)
        return ""
    return str(path)


def _jsonable(v):
    """numpy float -> float để FastAPI trả JSON được."""
    if isinstance(v, dict):
        return {k: _jsonable(x) for k, x in v.items()}
    if isinstance(v, list):
        return [_jsonable(x) for x in v]
    if isinstance(v, np.generic):
        return v.item()
    return v
