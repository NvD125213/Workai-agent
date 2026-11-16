from . import config
from .normalize import CORE_TECH, TITLE_TECH, count_skill_mentions, detect_city, detect_languages, nfc, normalize_language

def _skill_weight(skill):
    return config.CORE_SKILL_WEIGHT if skill in CORE_TECH else 1

def skill_score(matched, missing):
    """Tỉ lệ kỹ năng có, tính theo trọng số: thiếu một techstack cốt lõi (java, .net...) mất điểm gấp
    CORE_SKILL_WEIGHT lần so với thiếu một công cụ phụ (jira, git...), thay vì đếm cào bằng."""
    total = sum(map(_skill_weight, matched)) + sum(map(_skill_weight, missing))
    return None if total == 0 else sum(map(_skill_weight, matched)) / total

def key_skills(job) -> set:
    """Kỹ năng trọng tâm của job: kỹ năng cốt lõi được nhắc lại nhiều lần trong JD (>= KEY_SKILL_MIN_MENTIONS)."""
    text = nfc(f"{job.get('title', '')}\n{job.get('description', '')}").lower()
    return {s for s in job.get("skills") or []
            if s in TITLE_TECH and count_skill_mentions(s, text) >= config.KEY_SKILL_MIN_MENTIONS}

def skill_penalty(missing, key) -> float:
    """Hệ số nhân vào điểm tổng (PENALTY_FLOOR..1) theo các techstack quan trọng còn thiếu.

    Kỹ năng bắt buộc (tiêu đề) đã bị bộ lọc cứng xử lý; ở đây phạt phần còn lại:
    thiếu kỹ năng trọng tâm của JD trừ PENALTY_MISSING_KEY, thiếu kỹ năng cốt lõi khác trừ PENALTY_MISSING_CORE.
    Phạt dạng nhân nên rerank/title cao cũng không kéo lại được một job lệch techstack.
    """
    p = sum(config.PENALTY_MISSING_KEY if s in key else config.PENALTY_MISSING_CORE if s in CORE_TECH else 0
            for s in missing)
    return max(config.PENALTY_FLOOR, 1.0 - p)

def deadline_factor(days_left) -> float:
    """Hệ số nhân vào điểm tổng theo số ngày còn lại tới hạn nộp (xem config.DEADLINE_*).

    Không rõ hạn -> 1 (không phạt). Đã hết hạn -> 0 (bộ lọc cứng đã loại, đây chỉ là phòng hờ).
    """
    if days_left is None or days_left >= config.DEADLINE_PENALTY_DAYS:
        return 1.0
    if days_left <= 0:
        return 0.0
    floor = config.DEADLINE_FLOOR
    return floor + (1.0 - floor) * (days_left / config.DEADLINE_PENALTY_DAYS) ** config.DEADLINE_CURVE

def experience_score(cv_years, min_years):
    if cv_years is None or min_years is None:
        return None                       # thiếu một trong hai -> bỏ qua
    if min_years <= 0 or cv_years >= min_years:
        return 1.0
    return max(0.0, cv_years / min_years)

def _stretch(cosine, low, high):
    """Kéo giãn khoảng cosine [low, high] về [0, 1]."""
    if cosine is None:
        return None
    return min(1.0, max(0.0, (cosine - low) / (high - low)))

def semantic_score(cosine, low=config.SEMANTIC_LOW, high=config.SEMANTIC_HIGH):
    """Điểm semantic dựa trên cosine similarity."""
    return _stretch(cosine, low, high)

def title_score(cosine):
    # chức danh ngắn nên cosine cao hơn: không liên quan ~0.4, gần nghĩa ~0.75 trở lên
    return _stretch(cosine, 0.40, 0.85)

def language_score(job_languages, profile_languages, cv_text_lower):
    need = {normalize_language(x) for x in job_languages or [] if x}
    if not need:
        return None
    have = {normalize_language(x) for x in profile_languages or []} | detect_languages(cv_text_lower)
    return len(need & have) / len(need)

def location_score(job, target_city):
    if job.get("is_remote"):
        return 1.0
    job_city = detect_city(job.get("location", ""))
    if not job_city or not target_city:
        return None
    return 1.0 if job_city == target_city else 0.0

def extra_score(job, profile, cv_text_lower, target_city):
    parts = [
        language_score(job.get("languages"), profile.get("languages"), cv_text_lower),
        location_score(job, target_city),
    ]
    parts = [p for p in parts if p is not None]
    return None if not parts else sum(parts) / len(parts)

def salary_scores(salaries: list) -> list:
    """Thứ hạng phần trăm (0..1) của từng mức lương trong danh sách: cao nhất 1, thấp nhất 0, bằng nhau cùng điểm.
    Lương None (không ghi) -> None. Dưới 2 job có lương thì không so được -> toàn None."""
    known = [s for s in salaries if s is not None]
    if len(known) < 2:
        return [None] * len(salaries)
    n = len(known) - 1
    return [None if s is None else (sum(x < s for x in known) + (sum(x == s for x in known) - 1) / 2) / n
            for s in salaries]

def exp_fit_score(need_years, band):
    """Trong khoảng kinh nghiệm phù hợp, job yêu cầu ít năm hơn được điểm cao hơn (cận dưới 1, cận trên 0)."""
    if need_years is None or band is None:
        return None
    low, high = band
    if high <= low:
        return 1.0
    return min(1.0, max(0.0, (high - need_years) / (high - low)))

def combine(components: dict, weights: dict = None) -> float:
    """Tổng hợp thành điểm 0..100. Thành phần None bị bỏ qua, trọng số còn lại được chia lại.
    weights: mặc định config.WEIGHTS."""
    used = {k: w for k, w in (weights or config.WEIGHTS).items() if components.get(k) is not None}
    total = sum(used.values())
    if total == 0:
        return 0.0
    return 100.0 * sum(w * components[k] for k, w in used.items()) / total