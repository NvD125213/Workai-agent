"""Bộ lọc cứng (hard constraints): loại job ứng viên CHẮC CHẮN không đáp ứng, trước khi chấm điểm mềm.

Điểm mềm (rerank, title, cosine) đo độ "giống nhau" của văn bản nên không phân biệt được job .NET với job
React khi cả hai đều là fullstack, hay job Senior với job Junior cùng chức danh. Vì vậy các điều kiện
không thể bù bằng điểm khác được kiểm tra ở đây, và job trượt bị loại khỏi danh sách xếp hạng:
  1. Kinh nghiệm: số năm yêu cầu (lấy từ min_years và cấp bậc trong tiêu đề) vượt quá khả năng ứng viên.
  2. Kỹ năng bắt buộc: techstack ghi trong tiêu đề job (hoặc job["must_have"]) mà CV không có.
  3. (Khi find-job bật exp) Khoảng kinh nghiệm: job yêu cầu quá ít hoặc quá nhiều năm so với ứng viên,
     ví dụ job thực tập sinh với ứng viên 5 năm (experience_band).
  4. Hạn nộp: job đã hết hạn tuyển dụng (job["deadline"]). Job sắp hết hạn không bị loại mà bị trừ điểm
     (scoring.deadline_factor).

Các hàm ở đây chỉ dùng regex và từ điển, không gọi mô hình, nên chạy rất nhanh. Matcher gọi chúng đầu tiên
để chỉ những job còn lại mới phải qua embedding tiêu đề và rerank (bước tốn thời gian nhất).
"""
import re
from datetime import datetime, timezone

from . import config
from .normalize import IMPLIED_SKILLS, TITLE_TECH, match_skills, nfc, parse_skill_list, skill_in_text, term_in_text

# Từ khoá cấp bậc trong tiêu đề job -> khoá của config.SENIORITY_MIN_YEARS
LEVEL_KEYWORDS = {
    "intern": ["intern", "internship", "trainee", "thực tập", "thực tập sinh"],
    "fresher": ["fresher", "mới ra trường"],
    "junior": ["junior", "jr"],
    "middle": ["middle", "mid", "mid-level"],
    "senior": ["senior", "sr", "cấp cao"],
    "lead": ["lead", "leader", "tech lead", "team lead", "trưởng nhóm", "trưởng phòng", "principal",
             "architect", "manager"],
}


# ============================ 1. LỌC KINH NGHIỆM ============================
def seniority_levels(title) -> list:
    """Các cấp bậc ghi trong tiêu đề: 'Full Stack Developer (Junior - Middle)' -> ['junior', 'middle']."""
    low = nfc(title).lower()
    return [lv for lv, kws in LEVEL_KEYWORDS.items() if any(term_in_text(k, low) for k in kws)]


def required_years(job: dict):
    """Số năm kinh nghiệm tối thiểu thực sự của job, None nếu không xác định được.

    Lấy giá trị lớn hơn giữa min_years (tin ghi rõ) và mức sàn theo cấp bậc trong tiêu đề, vì nhiều tin
    "Senior ..." chỉ ghi 1-2 năm hoặc không ghi gì. Tiêu đề ghi nhiều cấp thì lấy cấp thấp nhất.
    """
    levels = seniority_levels(job.get("title", ""))
    floor = min(config.SENIORITY_MIN_YEARS[lv] for lv in levels) if levels else None
    values = [v for v in (job.get("min_years"), floor) if v is not None]
    return max(values) if values else None


def experience_gate(cv_years, need_years):
    """Trả về lý do loại, hoặc None nếu đạt.

    Loại khi ứng viên có ít hơn (need_years - EXP_TOLERANCE_YEARS) năm: job yêu cầu 3 năm thì cần >= 2 năm.
    Thiếu một trong hai số liệu thì không loại (không đủ căn cứ), phần điểm kinh nghiệm sẽ bỏ qua.
    """
    if cv_years is None or need_years is None:
        return None
    if cv_years < need_years - config.EXP_TOLERANCE_YEARS:
        return f"cần {need_years:g} năm kinh nghiệm, ứng viên có {cv_years:g} năm"
    return None


def experience_band(cv_years):
    """Khoảng số năm job yêu cầu (thấp, cao) hợp với ứng viên, theo config.EXP_BANDS; None nếu không rõ số năm.

    2 năm -> (0, 2), 3 năm -> (2, 3), 4.7 năm -> (2, 5), 8 năm -> (3, 8).
    """
    if cv_years is None:
        return None
    years = int(cv_years + 0.5)
    for max_years, low, high in config.EXP_BANDS:
        if years <= max_years:
            return low, high
    return config.EXP_BAND_SENIOR_MIN, years


def band_gate(need_years, band):
    """Lý do loại khi số năm job yêu cầu nằm ngoài band, hoặc None. Job không rõ số năm thì giữ (không đủ căn cứ)."""
    if band is None or need_years is None:
        return None
    low, high = band
    if not low <= need_years <= high:
        return f"job cần {need_years:g} năm, ngoài khoảng {low:g}-{high:g} năm hợp với ứng viên"
    return None


# ============================ 2. LỌC KỸ NĂNG BẮT BUỘC ============================
# Ngăn cách các cụm trong tiêu đề. Dấu '/' KHÔNG ngăn cách: 'Java/Go' là một cụm "Java hoặc Go".
# '+' chỉ tách khi có khoảng trắng hai bên để không cắt 'C++'.
_TITLE_SPLIT = re.compile(r"[,;()\[\]{}|:–—]|\s[+&-]\s")
_TITLE_OR = re.compile(r"\s+(?:or|hoặc|hay)\s+")


def must_have_groups(job: dict) -> list:
    """Các nhóm kỹ năng bắt buộc của job; ứng viên phải có ÍT NHẤT MỘT kỹ năng trong MỖI nhóm.

    Nguồn:
      - job["must_have"] nếu có (ví dụ LLM/Go đánh dấu sẵn): mỗi kỹ năng là một nhóm.
      - Tiêu đề job: công nghệ trong TITLE_TECH xuất hiện trong tiêu đề. Các công nghệ cùng một cụm
        có dấu '/' hoặc 'or' là lựa chọn thay thế, gộp thành một nhóm:
          'Full Stack Developer (.Net)'          -> [['.net']]
          'Fullstack Developer (C#, .NET, Vuejs)' -> [['c#'], ['.net'], ['vue']]
          'Full Stack Developer (Java/Go)'       -> [['go', 'java']]
    Biết C# thì coi như biết .NET và ngược lại (IMPLIED_SKILLS), nên ['c#'] và ['.net'] đều đạt với ứng viên .NET.
    """
    groups = [[s] for s in parse_skill_list(job.get("must_have"))]
    title = _TITLE_OR.sub("/", nfc(job.get("title", "")).lower())
    for segment in _TITLE_SPLIT.split(title):
        found = [t for t in sorted(TITLE_TECH) if skill_in_text(t, segment)]
        if not found:
            continue
        if "/" in segment:
            groups.append(found)
        else:
            # 'React Js' dò ra cả react lẫn javascript ('js'): bỏ kỹ năng đã được kỹ năng khác cùng cụm suy ra
            # (.net và c# suy ra lẫn nhau nên giữ cả hai)
            groups.extend([t] for t in found if not any(
                f in IMPLIED_SKILLS.get(t, ()) and t not in IMPLIED_SKILLS.get(f, ()) for f in found))
    unique = []
    for g in groups:
        if g not in unique:
            unique.append(g)
    return unique


def missing_must_have(groups, cv_skills, cv_text_lower) -> list:
    """Các nhóm bắt buộc mà CV không có kỹ năng nào. Dùng cùng match_skills với phần chấm điểm,
    nên kết quả lọc luôn khớp với cột "kỹ năng có/thiếu"."""
    return [g for g in groups if not match_skills(g, cv_skills, cv_text_lower)[0]]


# ============================ 3. LỌC HẠN NỘP ============================
def days_left(job: dict, now: datetime = None):
    """Số ngày (có phần lẻ) còn lại tới hạn nộp, âm nếu đã quá hạn; None nếu job không ghi hạn."""
    deadline = job.get("deadline")
    if deadline is None:
        return None
    return (deadline - (now or datetime.now(timezone.utc))).total_seconds() / 86400


def deadline_gate(job: dict, now: datetime = None):
    """Lý do loại khi job đã hết hạn nộp, hoặc None. Job không ghi hạn thì giữ (không đủ căn cứ)."""
    left = days_left(job, now)
    if left is not None and left <= 0:
        return f"đã hết hạn nộp hồ sơ ({job['deadline']:%d/%m/%Y})"
    return None


# ============================ GỘP CÁC BỘ LỌC ============================
def check_job(job: dict, cv_years, cv_skills, cv_text_lower, exp_band=None, now: datetime = None) -> list:
    """Chạy các bộ lọc cho một job, trả về danh sách lý do loại (rỗng = đạt).
    exp_band (experience_band): có thì loại thêm job yêu cầu số năm ngoài khoảng đó.
    now: thời điểm so với hạn nộp, mặc định là lúc gọi."""
    reasons = []
    if deadline_reason := deadline_gate(job, now):
        reasons.append(deadline_reason)
    need = required_years(job)
    exp_reason = experience_gate(cv_years, need) or band_gate(need, exp_band)
    if exp_reason:
        reasons.append(exp_reason)
    for g in missing_must_have(must_have_groups(job), cv_skills, cv_text_lower):
        reasons.append("thiếu kỹ năng bắt buộc: " + " hoặc ".join(g))
    return reasons


def apply_hard_filters(jobs: list, cv_years, cv_skills, cv_text_lower, exp_band=None, now: datetime = None):
    """Tách jobs thành (đạt, bị loại). Phần tử bị loại là (job, [lý do])."""
    now = now or datetime.now(timezone.utc)
    kept, rejected = [], []
    for job in jobs:
        reasons = check_job(job, cv_years, cv_skills, cv_text_lower, exp_band, now)
        if reasons:
            rejected.append((job, reasons))
        else:
            kept.append(job)
    return kept, rejected
