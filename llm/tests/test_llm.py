"""Test phần phân tích CV/job bằng LLM.

Chạy: .venv\\Scripts\\python.exe tests\\test_llm.py          (gồm cả test gọi Ollama thật)
      .venv\\Scripts\\python.exe tests\\test_llm.py --offline (chỉ test không cần Ollama)
"""
import json
import sys
import time
from datetime import date
from pathlib import Path

LLM_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(LLM_DIR))
# Khi output bị chuyển hướng (nút Run của VS Code, pipe), Windows dùng cp1252 và print tiếng Việt sẽ lỗi
sys.stdout.reconfigure(encoding="utf-8")

from matching import config, cv_check, cv_parser, llm  # noqa: E402
from matching.normalize import broad_keyword, normalize_skill, skill_in_text  # noqa: E402

TODAY = date(2026, 10, 26)
SAMPLE_CV = LLM_DIR / "data" / "cv" / "CV_Duong_Tuan_Khang.pdf"
SAMPLE_CV_DOCX = LLM_DIR / "data" / "cv" / "CV_Nguyen_Thi_My_Chau.docx"
PROFILES_DIR = LLM_DIR / "data" / "profiles"     # hồ sơ phân tích được, test_matching.py đọc lại
VI_CHARS = "ăâđêôơưạảấầẩậắằẳặẹẻẽếềểệịọỏốồổộớờởợụủứừửựỳỷỹ"


def fake_llm(response: dict):
    """Thay _chat_json bằng hàm trả về response cố định."""
    llm._chat_json = lambda system, user, schema, retries=1: response


# ============================ TEST KHÔNG CẦN OLLAMA ============================
def test_normalize_skill_parentheses():
    assert normalize_skill("JavaScript (ES6+)") == "javascript"
    assert normalize_skill("Role-Based Access Control (RBAC)") == "rbac"
    assert normalize_skill("Kê khai thuế (TNCN)") == "kê khai thuế"
    assert normalize_skill(".NET") == ".net"
    assert normalize_skill("RESTful APIs") == "rest api"
    assert normalize_skill("Tailwind CSS") == "tailwind"


def test_ambiguous_alias_not_matched():
    # "rest" và "node" là từ tiếng Anh thường, không được coi là REST API / Node.js
    assert not skill_in_text("rest api", "i spent the rest of the year on java")
    assert not skill_in_text("node.js", "each node in the tree")
    assert skill_in_text("node.js", "backend: nodejs, express")


def test_experience_years():
    exps = [
        {"start": "05/2026", "end": "07/2026", "is_internship": True},     # 3 tháng thực tập
        {"start": "01/2020", "end": "12/2021", "is_internship": False},    # 24 tháng
        {"start": "06/2021", "end": "03/2022", "is_internship": False},    # chồng 7 tháng -> +3
    ]
    assert llm.experience_years(exps, TODAY) == round((24 + 3 + 1.5) / 12, 1)
    assert llm.experience_years([{"start": "2024", "end": "Hiện tại"}], TODAY) == round(34 / 12, 1)
    assert llm.experience_years([{"start": "Mar 2025", "end": "present"}], TODAY) == round(20 / 12, 1)
    assert llm.experience_years([{"start": "", "end": ""}], TODAY) is None
    assert llm.experience_years([], TODAY) is None


def test_location_must_be_in_text():
    assert llm._grounded_location("Hà Nội", "Kỹ năng: Java. Không ghi địa chỉ") == ""
    assert llm._grounded_location("Sài Gòn", "Địa chỉ: Quận 7, TP.HCM") == "TP. Hồ Chí Minh"
    assert llm._grounded_location("", "Địa chỉ: Cầu Giấy, Hà Nội") == "Hà Nội"


def test_location_from_district():
    cv = "⌂ Huỳnh Văn Nghệ, Tân Bình"
    assert llm._grounded_location("Huỳnh Văn Nghệ, Tân Bình", cv) == "TP. Hồ Chí Minh"
    assert llm._grounded_location("", "Địa chỉ: Quận 10") == "TP. Hồ Chí Minh"
    assert llm._grounded_location("", "Mã số 1, Quận 1") == "TP. Hồ Chí Minh"


def test_soft_skills_moved_out():
    fake_llm({"name": "", "current_title": "SENIOR CONTENT MARKETING", "job_titles": [], "experiences": [],
              "years_experience": 0, "summary": "", "education": "", "languages": [], "location": "",
              "skills": [{"name": "Content SEO", "level": "MID"}, {"name": "Làm việc nhóm", "level": "MID"}],
              "soft_skills": ["Content SEO", "Làm việc nhóm"]})
    p = llm.analyze_profile("CÔNG CỤ\nPhotoshop, Canva, CapCut\nMS Office, Meta Ads\nGoogle Trends, Chat GPT")
    names = {s["name"] for s in p["skills"]}
    assert "làm việc nhóm" not in names
    assert p["soft_skills"] == ["Làm việc nhóm"]
    assert {"canva", "capcut", "ms office", "facebook ads", "google trends", "chatgpt"} <= names
    assert p["current_title"] == "Senior Content Marketing"


def test_task_phrases_filtered():
    cv = "KỸ NĂNG\nThiết kế và dựng video\nPhân tích số liệu\nKINH NGHIỆM\n" \
         "Phối hợp với team Media, Digital & Sales sản xuất nội dung"
    items = ["Thiết kế và dựng video", "Phân tích số liệu", "Phối hợp với team Media, Digital & Sales",
             "Phối hợp với team digital", "Kê khai thuế"]
    names = [s["name"] for s in llm._normalize_skills_with_level(items, listed=llm._listed_items(cv))]
    assert names == ["thiết kế và dựng video", "phân tích số liệu", "kê khai thuế"]


def _docx_with_textbox() -> bytes:
    """docx có 1 đoạn thường và 1 text box (kèm bản Fallback VML như Word vẫn ghi)."""
    import io
    import zipfile
    from docx import Document

    buf = io.BytesIO()
    Document().save(buf)
    zin = zipfile.ZipFile(io.BytesIO(buf.getvalue()))
    xml = zin.read("word/document.xml").decode("utf-8")
    box = "<w:txbxContent><w:p><w:r><w:t>Trong text box</w:t></w:r></w:p></w:txbxContent>"
    para = (
        '<w:p><w:r><w:t>Đoạn thường</w:t></w:r></w:p>'
        '<w:p><w:r><mc:AlternateContent '
        'xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006">'
        f"<mc:Choice Requires=\"wps\"><w:drawing>{box}</w:drawing></mc:Choice>"
        f"<mc:Fallback><w:pict>{box}</w:pict></mc:Fallback>"
        "</mc:AlternateContent></w:r></w:p>"
    )
    xml = xml.replace("<w:body>", "<w:body>" + para, 1)
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as zout:
        for item in zin.infolist():
            zout.writestr(item, xml if item.filename == "word/document.xml" else zin.read(item.filename))
    return out.getvalue()


def test_read_docx_textbox():
    text = cv_parser._read_docx(_docx_with_textbox())
    assert "Đoạn thường" in text
    assert text.count("Trong text box") == 1     # đọc được text box, không trùng bản Fallback


def test_analyze_profile_postprocess():
    fake_llm({
        "name": "NGUYỄN VĂN AN",
        "current_title": "Backend Developer",
        "job_titles": ["Backend Developer", "backend developer"],
        "experiences": [{"title": "Intern", "company": "ABC", "start": "01/2026",
                         "end": "06/2026", "is_internship": True}],
        "years_experience": 3,                       # LLM cộng sai, phải bị thay
        "skills": [{"name": "Java", "level": "SENIOR"}, {"name": "Backend", "level": "MID"},
                   {"name": "ReactJS", "level": "JUNIOR"}, {"name": "react", "level": "MID"},
                   {"name": "English", "level": "MID"}],
        "soft_skills": ["Teamwork"],
        "education": "HUTECH",
        "languages": ["English (TOEIC 750)", "Tiếng Việt (bản ngữ)"],
        "location": "Hà Nội",                        # CV không ghi -> phải bỏ
        "summary": "Lập trình viên backend.",
    })
    cv = "NGUYỄN VĂN AN\nBackend Developer\nSkills: Java, React, Docker, PostgreSQL. TOEIC 750"
    p = llm.analyze_profile(cv, today=TODAY)

    assert p["name"] == "Nguyễn Văn An"
    assert p["years_experience"] == 0.2              # 6 tháng thực tập x 0.5
    assert p["job_titles"] == ["Backend Developer"]
    assert p["location"] == ""
    assert p["languages"] == ["english"]
    skills = {s["name"]: s["level"] for s in p["skills"]}
    assert skills["java"] == "JUNIOR"                # bị giới hạn theo số năm kinh nghiệm
    assert skills["react"] == "JUNIOR"               # gộp ReactJS + react
    assert "backend" not in skills                   # tên nhóm, không phải kỹ năng
    assert "english" not in skills                   # ngoại ngữ nằm ở languages
    assert skills["docker"] == "" and skills["postgresql"] == ""   # dò bù từ văn bản


def test_profile_to_db_fits_columns():
    p = {"years_experience": 2.6, "current_title": "x" * 300, "location": "Hà Nội",
         "languages": ["english"], "summary": "s", "education": "e",
         "skills": [{"name": "y" * 120, "level": "MID"}, {"name": "java", "level": ""}]}
    row = llm.profile_to_db(p, "CV text")
    prof = row["user_profile"]
    assert prof["exp_years"] == 3 and isinstance(prof["exp_years"], int)
    assert len(prof["current_title"]) == 255
    assert len(prof["resume_hash"]) == 64
    assert len(row["user_skills"][0]["skill_name"]) == 100
    assert llm.profile_to_db({**p, "years_experience": None}, "x")["user_profile"]["exp_years"] == 0
    json.dumps(row, ensure_ascii=False)              # phải ghi được ra JSON


def test_analyze_job_postprocess():
    fake_llm({"skills": ["ReactJS", "Frontend", "TypeScript"], "min_years": "-1",
              "location": "Đà Nẵng", "is_remote": False, "languages": ["Tiếng Anh"]})
    j = llm.analyze_job("Frontend Dev", "Yêu cầu React, TypeScript. Tiếng Anh giao tiếp.",
                        location="Hồ Chí Minh")
    assert j["skills"] == ["react", "typescript"]
    assert j["min_years"] is None                    # "-1" = không ghi yêu cầu
    assert j["location"] == "TP. Hồ Chí Minh"        # Đà Nẵng không có trong tin -> dò lại
    assert j["languages"] == ["english"]
    assert llm.job_to_db(j)["job"]["min_years"] is None


def test_group_prefix_stripped():
    tools = ["ChatGPT", "Claude", "Codex", "Copilot", "Cursor", "Gemini", "Antigravity"]
    keep = ["Spring Boot", "Spring Security", "Google Ads", "Google Analytics"]
    items = [{"name": f"AI-assisted Development {t}", "level": "JUNIOR"} for t in tools] \
        + [{"name": k, "level": "JUNIOR"} for k in keep]
    names = [s["name"] for s in llm._normalize_skills_with_level(items)]
    expected = [normalize_skill(x) for x in tools + keep]
    assert names == expected, names
    assert len(names) == 11

    fake_llm({"skills": [f"AI-assisted Development: {t}" for t in tools] + keep, "min_years": -1,
              "location": "", "is_remote": False, "languages": []})
    assert llm.analyze_job("Dev", "AI tools")["skills"] == expected


def test_group_prefix_dedup_keeps_higher_level():
    items = [{"name": "ai-assisted development chatgpt", "level": "JUNIOR"},
             {"name": "ai-assisted development claude", "level": ""},
             {"name": "ai-assisted development cursor", "level": ""},
             {"name": "chatgpt", "level": ""}]
    skills = llm._normalize_skills_with_level(items)
    assert [s for s in skills if s["name"] == "chatgpt"] == [{"name": "chatgpt", "level": "JUNIOR"}]
    assert len(skills) == 3


def _profile_response(current_title, experiences):
    return {"name": "", "current_title": current_title, "job_titles": ["SENIOR CONTENT MARKETING"],
            "experiences": experiences, "years_experience": 0, "skills": [], "soft_skills": [],
            "education": "", "languages": [], "location": "", "summary": ""}


EXPS_UNORDERED = [
    {"title": "Content Marketing", "company": "HTP", "start": "05/2023", "end": "05/2025", "is_internship": False},
    {"title": "Content Creator", "company": "Mira", "start": "09/2025", "end": "nay", "is_internship": False},
    {"title": "Content Marketing", "company": "KAT", "start": "04/2021", "end": "03/2023", "is_internship": False},
]


def test_current_title_from_latest_experience():
    fake_llm(_profile_response("", EXPS_UNORDERED))
    p = llm.analyze_profile("CV", today=TODAY)
    assert p["current_title"] == "Content Creator"
    assert p["job_titles"] == ["SENIOR CONTENT MARKETING", "Content Marketing", "Content Creator"]

    # Không đọc được ngày nào thì giữ thứ tự hiện có
    undated = [{**e, "start": "", "end": ""} for e in EXPS_UNORDERED]
    fake_llm(_profile_response("", undated))
    assert llm.analyze_profile("CV", today=TODAY)["current_title"] == "Content Marketing"


def test_current_title_prefers_latest_experience():
    # current_title của LLM là dòng tiêu đề đầu CV: nhường chỗ cho chức danh gần nhất, chuyển sang job_titles
    fake_llm({**_profile_response("Senior Content Marketing", EXPS_UNORDERED), "job_titles": []})
    p = llm.analyze_profile("CV", today=TODAY)
    assert p["current_title"] == "Content Creator"
    assert "Senior Content Marketing" in p["job_titles"]


def test_current_title_prefers_cv_headline():
    # Dòng tiêu đề ngay dưới họ tên là công việc ứng viên muốn tìm, thắng chức danh của việc gần nhất
    cv = "TRẦN THỊ MAI ANH\nVIDEO EDITOR - PHOTOGRAPHER\nmai@gmail.com 0393681181\nKINH NGHIỆM\n..."
    exps = [
        {"title": "Video Editor", "company": "Công ty TNHH Hải Vân", "start": "06/2024", "end": "04/2025",
         "is_internship": False},
        {"title": "Trưởng ban Hình Ảnh - CLB Thương Mại Số", "company": "CLB Thương Mại Số", "start": "02/2024",
         "end": "2025", "is_internship": False},
    ]
    fake_llm({**_profile_response("Video Editor – Photographer", exps), "job_titles": []})
    p = llm.analyze_profile(cv, today=TODAY)
    assert p["current_title"] == "Video Editor – Photographer"
    assert p["job_titles"] == ["Video Editor"]       # vai trò ở CLB không phải chức danh nghề nghiệp
    assert broad_keyword(p["current_title"]) == "Video Editor"


def test_extracurricular_not_used_as_title():
    # Không có dòng tiêu đề: lấy việc gần nhất, nhưng bỏ qua vai trò CLB dù kết thúc muộn hơn
    exps = [
        {"title": "Video Editor", "company": "Hải Vân", "start": "06/2024", "end": "04/2025", "is_internship": False},
        {"title": "Trưởng ban truyền thông", "company": "Câu lạc bộ Tiếng Anh", "start": "01/2025", "end": "nay",
         "is_internship": False},
    ]
    fake_llm({**_profile_response("", exps), "job_titles": []})
    assert llm.analyze_profile("CV", today=TODAY)["current_title"] == "Video Editor"


def test_listed_line_with_commas_kept():
    cv = "KỸ NĂNG\n• Thiết kế, chỉnh sửa ảnh/video\nKINH NGHIỆM\n" \
         "Lên ý tưởng và thực hiện cắt ghép, chỉnh sửa 4 video/ngày phục vụ cho việc quảng cáo sản phẩm"
    items = ["Thiết kế, chỉnh sửa ảnh/video", "Cắt ghép, chỉnh sửa 4 video/ngày phục vụ quảng cáo"]
    names = [s["name"] for s in llm._normalize_skills_with_level(items, listed=llm._listed_items(cv))]
    assert names == ["thiết kế, chỉnh sửa ảnh/video"]


EXPS_WITH_PROJECTS = [
    {"title": "Authentication System", "company": "CyberLogitec Vietnam", "start": "05/2026", "end": "07/2026",
     "is_internship": True},
    {"title": "Warehouse Management System (WMS)", "company": "CyberLogitec Vietnam", "start": "05/2026",
     "end": "07/2026", "is_internship": True},
    {"title": "Full-Stack Developer Trainee", "company": "CyberLogitec Vietnam", "start": "05/2026",
     "end": "07/2026", "is_internship": True},
]


def test_project_names_not_used_as_titles():
    fake_llm({**_profile_response("", EXPS_WITH_PROJECTS), "job_titles": []})
    p = llm.analyze_profile("CV", today=TODAY)
    assert p["current_title"] == "Full-Stack Developer Trainee"
    assert p["job_titles"] == ["Full-Stack Developer Trainee"]
    assert len(p["experiences"]) == 3                # chỉ bỏ qua khi lấy chức danh, không xoá


def test_all_titles_are_projects_keeps_llm_title():
    fake_llm(_profile_response("Full-Stack Developer", EXPS_WITH_PROJECTS[:2]))
    p = llm.analyze_profile("CV", today=TODAY)
    assert p["current_title"] == "Full-Stack Developer"
    assert p["job_titles"] == ["SENIOR CONTENT MARKETING"]


def test_same_company_overlap_not_double_counted():
    one = llm.experience_years(EXPS_WITH_PROJECTS[2:], TODAY)
    assert one == round(3 * 0.5 / 12, 1)             # 3 tháng thực tập x 0.5
    assert llm.experience_years(EXPS_WITH_PROJECTS, TODAY) == one
    full_time = [{**e, "is_internship": False} for e in EXPS_WITH_PROJECTS[:2]]
    assert llm.experience_years(full_time, TODAY) == round(3 / 12, 1)


def test_num_thread_option():
    class FakeClient:
        def chat(self, **kwargs):
            self.options = kwargs["options"]
            return {"message": {"content": "{}"}}

    real_client, real_threads = llm._client, config.LLM_NUM_THREAD
    try:
        llm._client = FakeClient()
        config.LLM_NUM_THREAD = None
        llm._chat_json("s", "u", {})
        assert "num_thread" not in llm._client.options
        config.LLM_NUM_THREAD = 4
        llm._chat_json("s", "u", {})
        assert llm._client.options["num_thread"] == 4
    finally:
        llm._client, config.LLM_NUM_THREAD = real_client, real_threads


def test_api_backend_fallback():
    """production: schema nằm trong system prompt, provider chính lỗi thì chuyển sang provider dự phòng."""
    class Msg:
        def __init__(self, content):
            self.choices = [type("C", (), {"message": type("M", (), {"content": content})()})()]

    class FakeAPI:
        def __init__(self, content=None):
            self.content, self.calls = content, []
            self.chat = type("Chat", (), {"completions": self})()

        def create(self, **kwargs):
            self.calls.append(kwargs)
            if self.content is None:
                raise RuntimeError("503")
            return Msg(self.content)

    real = config.LLM_BACKEND, llm._api_clients
    primary, fallback = FakeAPI(), FakeAPI('```json\n{"skills": ["Go"]}\n```')
    try:
        config.LLM_BACKEND = "api"
        llm._api_clients = [
            (config.ProviderConfig("deepseek", "", "k", "deepseek-chat", "max_tokens"), primary),
            (config.ProviderConfig("openai", "", "k", "gpt-4.1-mini", "max_completion_tokens"), fallback),
        ]
        assert llm._chat_json("hệ thống", "u", {"type": "object"}) == {"skills": ["Go"]}
        assert len(primary.calls) == 1 and len(fallback.calls) == 1
        call = fallback.calls[0]
        assert call["model"] == "gpt-4.1-mini" and call["response_format"] == {"type": "json_object"}
        assert "max_completion_tokens" in call and "max_tokens" in primary.calls[0]
        assert '{"type": "object"}' in call["messages"][0]["content"]

        fallback.content = None
        try:
            llm._chat_json("s", "u", {})
            raise AssertionError("cả hai provider lỗi thì phải báo lỗi")
        except RuntimeError:
            pass
    finally:
        config.LLM_BACKEND, llm._api_clients = real


# ============================ TEST VỚI OLLAMA THẬT ============================
def _analyze_with_ollama(path):
    """Đọc CV và gọi Qwen thật. Trả về (văn bản, hồ sơ) hoặc None nếu Ollama chưa sẵn sàng."""
    ok, msg = llm.check_ollama()
    if not ok:
        print(f"  BỎ QUA: {msg}")
        return None
    text = cv_parser.read_cv_file(path)
    start = time.time()
    p = llm.analyze_profile(text, today=TODAY)
    print(f"  {Path(path).name} - thời gian LLM: {time.time() - start:.1f}s")
    compact = {**p, "skills": [f"{s['name']}:{s['level'] or '?'}" for s in p["skills"]],
               "experiences": [f"{e['company']} | {e['title']} | {e['start']} - {e['end']}"
                               + (" (thực tập)" if e["is_internship"] else "") for e in p["experiences"]]}
    print("  " + json.dumps(compact, ensure_ascii=False, indent=1).replace("\n", "\n  "))
    if not any(ch in p["summary"] for ch in VI_CHARS):
        print("  CẢNH BÁO: summary không phải tiếng Việt")
    PROFILES_DIR.mkdir(parents=True, exist_ok=True)
    out = PROFILES_DIR / f"{Path(path).stem}.json"
    out.write_text(json.dumps({"cv_file": Path(path).name, "cv_text": text, "profile": p},
                              ensure_ascii=False, indent=1), encoding="utf-8")
    print(f"  Đã lưu hồ sơ: {out}")
    return text, p


def test_sample_cv_it_pdf_with_ollama():
    result = _analyze_with_ollama(SAMPLE_CV)
    if result is None:
        return
    text, p = result

    skills = {s["name"] for s in p["skills"]}
    expected = {"java", "spring boot", "react", "next.js", "typescript", "postgresql", "docker",
                "nestjs", "grpc", "jwt", "mongodb", "git", "html", "css"}
    missing = expected - skills
    assert not missing, f"thiếu kỹ năng: {missing}"
    assert p["location"] == "", "CV không ghi địa chỉ, không được đoán"
    assert p["years_experience"] is not None and p["years_experience"] < 1, "chỉ có 3 tháng trainee"
    assert all(s["level"] in ("", "JUNIOR") for s in p["skills"]), "dưới 1 năm thì chỉ JUNIOR"
    assert "english" in p["languages"]
    assert p["name"] == "Duong Tuan Khang"
    assert not any(n in skills for n in ("frontend", "backend", "javascript (es6+", "english"))

    row = llm.profile_to_db(p, text)
    assert row["user_profile"]["exp_years"] == 0
    json.dumps(row, ensure_ascii=False)


def test_sample_cv_marketing_docx_with_ollama():
    result = _analyze_with_ollama(SAMPLE_CV_DOCX)
    if result is None:
        return
    text, p = result

    assert p["name"] == "Nguyễn Thị Mỹ Châu"
    assert p["location"] == "TP. Hồ Chí Minh", "Tân Bình là quận của TP.HCM"
    assert 4.5 <= p["years_experience"] <= 4.8, "09/2025-03/2026 + 05/2023-05/2025 + 04/2021-03/2023"
    companies = " ".join(e["company"].lower() for e in p["experiences"])
    assert all(c in companies for c in ("mira", "htp pharma", "kat")), companies
    skills = {s["name"]: s["level"] for s in p["skills"]}
    expected = {"photoshop", "canva", "capcut", "ms office", "facebook ads", "google trends",
                "chatgpt", "seo"}
    assert not (expected - skills.keys()), f"thiếu kỹ năng: {expected - skills.keys()}"
    assert "làm việc nhóm" not in skills, "kỹ năng mềm phải nằm ở soft_skills"
    assert all(lv in ("", "JUNIOR", "MID") for lv in skills.values()), "dưới 5 năm thì tối đa MID"
    tasks = [n for n in skills if len(n.split()) >= 5 and n != "thiết kế và dựng video"]
    assert not tasks, f"câu mô tả công việc lọt vào skills: {tasks}"
    assert "thiết kế và dựng video" in skills, "CV liệt kê nguyên cụm này trong mục Kỹ năng"

    row = llm.profile_to_db(p, text)
    assert row["user_profile"]["exp_years"] == 5
    assert row["user_profile"]["location"] == "TP. Hồ Chí Minh"


def test_check_cv_accepts_sample_cvs():
    for path in (SAMPLE_CV, SAMPLE_CV_DOCX):
        assert cv_check.check_cv(cv_parser.read_cv_file(path)) == "", path.name


def test_check_cv_rejects_non_cv():
    jd = """Senior Backend Developer
    Mô tả công việc
    - Phát triển hệ thống thanh toán, làm việc với kỹ năng Go, PostgreSQL
    Yêu cầu ứng viên
    - Có ít nhất 3 năm kinh nghiệm
    Quyền lợi
    - Lương thưởng hấp dẫn, chế độ bảo hiểm đầy đủ. Hạn nộp hồ sơ: 30/11/2026. Liên hệ hr@congty.vn"""
    assert "tin tuyển dụng" in cv_check.check_cv(jd)

    essay = ("Hôm nay trời đẹp, cả nhà đi chơi công viên. Kinh nghiệm cho thấy đi sớm thì đỡ đông, "
             "kỹ năng chụp ảnh của tôi cũng tiến bộ. Gọi cho tôi qua 0912345678 nhé.")
    assert cv_check.check_cv(essay) != ""

    # Bảng markdown, JSON có chữ "kinh nghiệm", "summary" không được tính là đề mục
    doc = "| Kinh nghiệm | 0.20 |\n| Ngoại ngữ | 0.10 |\n  \"summary\": {}\nemail: a@b.co"
    assert cv_check.check_cv(doc) != ""


def test_check_cv_headings():
    cv = """NGUYỄN VĂN A
    Email: a@gmail.com | 0912 345 678
    1. MỤC TIÊU NGHỀ NGHIỆP
    Trở thành backend developer.
    ■ KINH NGHIỆM LÀM VIỆC
    Công ty X (05/2023 - nay)
    • Kỹ năng:
    Go, Docker"""
    assert cv_check.check_cv(cv) == ""


def test_check_profile():
    assert cv_check.check_profile({"current_title": "", "job_titles": [], "experiences": [], "skills": []}) != ""
    assert cv_check.check_profile({"skills": [{"name": "go"}]}) == ""


if __name__ == "__main__":
    offline = "--offline" in sys.argv
    tests = [(n, f) for n, f in list(globals().items()) if n.startswith("test_") and callable(f)]
    if offline:
        tests = [(n, f) for n, f in tests if "ollama" not in n]
    failed = 0
    for name, fn in tests:
        real_chat = llm._chat_json
        try:
            fn()
            print(f"PASS  {name}")
        except AssertionError as e:
            failed += 1
            print(f"FAIL  {name}: {e}")
        finally:
            llm._chat_json = real_chat
    print(f"\n{len(tests) - failed}/{len(tests)} test đạt" if not failed else f"\n{failed} test lỗi")
    sys.exit(1 if failed else 0)
