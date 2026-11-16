"""Nhận diện văn bản có phải CV không, trước khi tốn 1-2 phút cho LLM phân tích.

Chỉ dùng regex nên chạy tức thì. Một CV có: thông tin liên hệ (email hoặc số điện thoại) và vài
đề mục quen thuộc (kinh nghiệm, học vấn, kỹ năng...) đứng thành dòng riêng. Chỉ đếm đề mục chứ không
đếm từ trong câu, vì tài liệu hay tin tuyển dụng cũng nhắc "kinh nghiệm", "kỹ năng" trong câu văn.
Tin tuyển dụng được nhận diện riêng qua các cụm từ đặc trưng (quyền lợi, phúc lợi, hạn nộp hồ sơ...).
"""
import re

from .normalize import nfc

# Mỗi nhóm là một loại đề mục; đếm số nhóm xuất hiện.
_CV_HEADINGS = {
    "experience": ("kinh nghiệm", "quá trình làm việc", "quá trình công tác", "work experience",
                   "professional experience", "employment history", "work history", "experience"),
    "education": ("học vấn", "trình độ học vấn", "quá trình học tập", "education", "academic background"),
    "skills": ("kỹ năng", "kĩ năng", "skills", "technical skills", "competencies"),
    "objective": ("mục tiêu", "career objective", "objective", "summary", "about me", "giới thiệu bản thân",
                  "professional summary", "profile"),
    "projects": ("dự án", "projects", "personal projects"),
    "certificates": ("chứng chỉ", "certifications", "certificates", "giải thưởng", "awards"),
    "languages": ("ngoại ngữ", "languages"),
    "personal": ("thông tin cá nhân", "personal information", "contact", "liên hệ", "sở thích", "hobbies",
                 "interests", "người tham chiếu", "references", "hoạt động", "activities"),
}

_JD_PHRASES = ("mô tả công việc", "yêu cầu ứng viên", "yêu cầu công việc", "quyền lợi", "phúc lợi", "đãi ngộ",
               "chế độ bảo hiểm", "lương thưởng", "môi trường làm việc", "hạn nộp hồ sơ", "cách thức ứng tuyển",
               "ứng viên", "số lượng tuyển", "job description", "job requirements", "responsibilities",
               "benefits", "what we offer", "how to apply", "we are looking for")

_EMAIL = re.compile(r"[\w.+-]+@[\w-]+(\.[\w-]+)+")
_PHONE = re.compile(r"(?<!\d)(?:\+?84|0)(?:[\s.-]?\d){8,10}(?!\d)")
# Ký tự đầu dòng của đề mục: số thứ tự, gạch đầu dòng, biểu tượng, dấu #
_HEADING_PREFIX = re.compile(r"^[\s#*\-–•·▪►■●◆✓✔→:.)(\dIVX]*(?=\w)")
# Dòng của bảng, JSON, code: không phải đề mục
_NOT_HEADING = re.compile(r"[|\"`{}=<>]")
# Phần chữ còn lại sau từ khoá của một dòng đề mục ("KINH NGHIỆM LÀM VIỆC" -> " làm việc")
MAX_HEADING_TAIL = 25

MIN_CV_HEADINGS = 2


def _headings(lines) -> int:
    found = set()
    for line in lines:
        if _NOT_HEADING.search(line):
            continue
        line = _HEADING_PREFIX.sub("", line.strip()).strip()
        if not line or len(line) > 60:
            continue
        for group, keys in _CV_HEADINGS.items():
            if group not in found and any(
                    line.startswith(k) and len(line) - len(k) <= MAX_HEADING_TAIL
                    and not line[len(k):len(k) + 1].isalnum() for k in keys):
                found.add(group)
    return len(found)


def check_cv(text: str) -> str:
    """Trả "" nếu văn bản giống CV, ngược lại trả lý do (tiếng Việt) để báo cho người dùng."""
    low = nfc(text).lower()
    headings = _headings(low.splitlines())
    jd = sum(p in low for p in _JD_PHRASES)
    contact = bool(_EMAIL.search(low) or _PHONE.search(low))

    if jd >= 3 and jd > headings:
        return "File này giống tin tuyển dụng chứ không phải CV. Hãy gửi CV của bạn."
    if headings >= MIN_CV_HEADINGS and contact:
        return ""
    if headings >= MIN_CV_HEADINGS + 2:   # CV đã ẩn thông tin liên hệ nhưng đủ các đề mục
        return ""
    return ("Nội dung file không giống CV (thiếu các phần như kinh nghiệm, học vấn, kỹ năng, "
            "thông tin liên hệ). Hãy gửi đúng file CV của bạn.")


def check_profile(profile: dict) -> str:
    """Kiểm tra lại sau LLM: không trích được gì về nghề nghiệp thì không phải CV."""
    if (profile.get("current_title") or profile.get("job_titles") or profile.get("experiences")
            or profile.get("skills")):
        return ""
    return "Không tìm thấy chức danh, kinh nghiệm hay kỹ năng nào trong file. Hãy gửi đúng file CV của bạn."
