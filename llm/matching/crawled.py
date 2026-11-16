"""Job vừa cào (AnalyzedJob bên Go: backend/internal/services/job_service.go) -> dict job mà Matcher nhận.

Luồng find-job chấm điểm ngay sau khi cào nên không gọi LLM analyze_job cho từng job (mỗi job vài chục giây).
Kỹ năng lấy từ tag của trang tuyển dụng, thiếu thì dò từ điển trong mô tả; số năm lấy từ ô "Kinh nghiệm",
lương từ ô "Mức lương" (dùng khi find-job bật bộ lọc kinh nghiệm), hạn nộp từ ô "Hạn nộp hồ sơ".
"""
import re
from datetime import datetime, timedelta, timezone

from .config import DEADLINE_TZ_HOURS, USD_TO_VND_MILLION
from .normalize import detect_languages, find_known_skills, is_remote, nfc, parse_skill_list

MAX_JOB_SKILLS = 15

_NO_EXPERIENCE = ("không yêu cầu", "chưa có kinh nghiệm", "không cần kinh nghiệm", "no experience", "not required")
_BELOW = ("dưới", "under", "less than")


def parse_experience_years(text):
    """Ô kinh nghiệm của trang tuyển dụng -> số năm tối thiểu (int), None nếu không rõ.

    '2 năm' -> 2, '1 - 3 năm' -> 1, 'Trên 5 năm' -> 5, '6 tháng' -> 0 (làm tròn),
    'Dưới 1 năm' / 'Không yêu cầu kinh nghiệm' -> 0.
    """
    low = nfc(text).lower().strip()
    if not low:
        return None
    if any(k in low for k in _NO_EXPERIENCE) or any(k in low for k in _BELOW):
        return 0
    m = re.search(r"\d+(?:[.,]\d+)?", low)
    if not m:
        return None
    value = float(m.group().replace(",", "."))
    if "tháng" in low or "month" in low:
        value /= 12
    return int(round(value)) if 0 <= value <= 30 else None


_NUMBER = re.compile(r"\d{1,3}(?:[.,]\d{3})+(?!\d)|\d+(?:[.,]\d+)?")


def _to_number(token: str) -> float:
    """'15.000.000' / '1,000' (phân cách hàng nghìn) -> 15000000 / 1000; '12,5' -> 12.5."""
    if re.fullmatch(r"\d{1,3}(?:[.,]\d{3})+", token):
        return float(re.sub(r"[.,]", "", token))
    return float(token.replace(",", "."))


def parse_salary_million(text):
    """Ô lương của trang tuyển dụng -> lương tháng (triệu VND), None nếu không ghi ('Thoả thuận', 'Cạnh tranh').

    '10 - 15 triệu' -> 12.5 (giữa khoảng), 'Tới 40 triệu' -> 40, 'Từ 20 triệu' -> 20,
    '1,000 - 2,000 USD' -> 37.5, '15.000.000 ₫ - 20.000.000 ₫ mỗi tháng' -> 17.5, '240 triệu/năm' -> 20.
    """
    low = nfc(text or "").lower()
    values = [_to_number(m) for m in _NUMBER.findall(low)]
    if not values:
        return None
    value = sum(values[:2]) / len(values[:2])
    if "usd" in low or "$" in low:
        value *= USD_TO_VND_MILLION
    elif value >= 100_000:                    # ghi đủ số đồng
        value /= 1_000_000
    if "năm" in low or "year" in low:
        value /= 12
    elif any(k in low for k in ("giờ", "hour", "ngày", "day", "tuần", "week")):
        return None                           # lương theo giờ/ngày không so được với lương tháng
    return round(value, 2) if 0 < value < 1000 else None


_VN_TZ = timezone(timedelta(hours=DEADLINE_TZ_HOURS))
_ISO_DATETIME = re.compile(r"\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?")
_YMD = re.compile(r"(\d{4})-(\d{1,2})-(\d{1,2})")
_DMY = re.compile(r"(\d{1,2})/(\d{1,2})/(\d{4})")


def parse_deadline(text):
    """Hạn nộp hồ sơ -> datetime có múi giờ, None nếu không rõ.

    '2026-11-19T23:59:59+07:00' -> đúng thời điểm đó; '30/11/2026', 'Hạn nộp hồ sơ: 30/11/2026',
    '2026-10-28' -> 23:59:59 ngày đó giờ Việt Nam (hạn chỉ ghi ngày thì vẫn nộp được trong ngày).
    """
    low = nfc(text or "").strip()
    if not low:
        return None
    if m := _ISO_DATETIME.search(low):
        try:
            dt = datetime.fromisoformat(m.group())
            return dt if dt.tzinfo else dt.replace(tzinfo=_VN_TZ)
        except ValueError:
            pass
    if m := _YMD.search(low):
        y, mo, d = m.groups()
    elif m := _DMY.search(low):
        d, mo, y = m.groups()
    else:
        return None
    try:
        return datetime(int(y), int(mo), int(d), 23, 59, 59, tzinfo=_VN_TZ)
    except ValueError:
        return None


def unique_id(job: dict) -> str:
    """'<source>:<id>', giống jobs.unique_identifier bên Go, để Go ghép kết quả về đúng job trong DB."""
    return f"{str(job.get('source', '')).strip().lower()}:{str(job.get('id', '')).strip()}"


def crawled_job_to_matcher(job: dict) -> dict:
    description = "\n".join(nfc(job.get(k) or "") for k in ("description", "requirements") if job.get(k))
    title = nfc(job.get("title", ""))
    location = nfc(job.get("location", ""))
    skills = parse_skill_list(job.get("skills"))[:MAX_JOB_SKILLS] \
        or find_known_skills(f"{title}\n{description}")[:MAX_JOB_SKILLS]
    return {
        "id": unique_id(job),
        "url": job.get("url", ""),
        "title": title,
        "description": description,
        "skills": skills,
        "min_years": parse_experience_years(job.get("experience", "")),
        "location": location,
        "is_remote": is_remote(location),
        "languages": sorted(detect_languages(description.lower())),
        "salary": nfc(job.get("salary") or ""),
        "salary_million": parse_salary_million(job.get("salary")),
        "deadline": parse_deadline(job.get("deadline")),
    }
