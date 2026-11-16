from .normalize import nfc

def skills_name(skills) -> list: 
    """Nhận [{"name": ..., "level": ...}] hoặc ["python", "react:JUNIOR"] -> ["python", "react"]."""
    out = []
    for s in skills or []:
        name = s.get("name", "") if isinstance(s, dict) else s.split(":", 1)[0]
        name = nfc(name).strip()
        if name:
            out.append(name)
    return list(dict.fromkeys(out))  # loại trùng, giữ thứ tự

def profile_to_text(profile: dict, desired_title: str = "") -> str:
    parts = []
    if desired_title:
        parts.append(f"Công việc mong muốn: {desired_title}")
    titles = [profile.get("current_title", "")] + list(profile.get("job_titles", []))
    titles = list(dict.fromkeys(t for t in titles if t))  # loại trùng, bỏ rỗng
    if titles:
        parts.append("Chức danh: " + ", ".join(titles))
    years = profile.get("years_experience")
    if years is not None:
        parts.append(f"Số năm kinh nghiệm: {years}")
    skills = skills_name(profile.get("skills"))
    if skills:
        parts.append("Kỹ năng: " + ", ".join(skills))
    if profile.get("education"):
        parts.append("Học vấn: " + profile["education"])
    if profile.get("languages"):
        parts.append("Ngoại ngữ: " + ", ".join(profile["languages"]))
    if profile.get("summary"):
        parts.append("Tóm tắt: " + profile["summary"])
    return ". ".join(parts)

def job_to_text(job: dict, max_desc_chars: int = 2000) -> str:
    """Job -> đoạn văn. Phần có cấu trúc đặt trước mô tả để không bị cắt mất khi văn bản dài."""
    parts = [nfc(job.get("title", ""))]
    if job.get("skills"):
        parts.append("Kỹ năng yêu cầu: " + ", ".join(job["skills"]))
    if job.get("min_years") is not None:
        parts.append(f"Kinh nghiệm tối thiểu: {job['min_years']:g} năm")
    if job.get("location"):
        parts.append("Địa điểm: " + job["location"])
    if job.get("is_remote"):
        parts.append("Làm việc từ xa")
    if job.get("languages"):
        parts.append("Ngoại ngữ: " + ", ".join(job["languages"]))
    if job.get("description"):
        parts.append("Mô tả: " + nfc(job["description"])[:max_desc_chars])
    return ". ".join(parts)