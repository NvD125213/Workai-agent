"""Giao tiếp với LLM: phân tích CV và phân tích job.

development: Qwen qua Ollama trên máy. production: API tương thích OpenAI (DeepSeek, OpenAI), xem config.LLM_BACKEND.
"""
import hashlib
import json
import re
from datetime import date, datetime, timezone

from . import config
from .filters import required_years
from .normalize import (
    CITY_DISPLAY, CITY_KEYWORDS, GENERIC_INTERN_TITLES, LANGUAGE_KEYWORDS, NON_SKILLS, SOFT_SKILLS, detect_city,
    find_known_skills, is_extracurricular, is_remote, is_role_title, nfc,
    normalize_language, normalize_skill, parse_skill_list, term_in_text,
)

LEVELS = ("JUNIOR", "MID", "SENIOR")          # khớp hằng số SkillLevel* bên Go
_LEVEL_RANK = {"": 0, "JUNIOR": 1, "MID": 2, "SENIOR": 3}

# Độ dài cột trong DB (backend/internal/models/models.go)
MAX_SKILL_LEN = 100        # user_skills.skill_name, job_skills.skill_name
MAX_LOCATION_LEN = 100     # user_profiles.location, jobs.location
MAX_TITLE_LEN = 255        # user_profiles.current_title

_client = None
_api_clients = None


# ============================ KẾT NỐI OLLAMA ============================
def get_client():
    global _client
    if _client is None:
        import ollama

        _client = ollama.Client(host=config.OLLAMA_HOST, timeout=config.LLM_TIMEOUT)
    return _client


def check_ollama():
    """Kiểm tra Ollama đang chạy và đã có mô hình. Trả về (ok, thông báo)."""
    try:
        resp = get_client().list()
    except Exception as e:  # noqa: BLE001
        return False, f"Không kết nối được Ollama ({config.OLLAMA_HOST}). Hãy mở ứng dụng Ollama. Chi tiết: {e}"

    models = getattr(resp, "models", None)
    if models is None and isinstance(resp, dict):
        models = resp.get("models", [])
    names = []
    for m in models or []:
        name = getattr(m, "model", None)
        if name is None and isinstance(m, dict):
            name = m.get("model") or m.get("name")
        if name:
            names.append(name)

    target = config.LLM_MODEL
    if target in names or f"{target}:latest" in names:
        return True, f"Ollama sẵn sàng với mô hình {target}"
    return False, f"Ollama đang chạy nhưng chưa có mô hình {target}. Hãy chạy: ollama pull {target}"


# ============================ KẾT NỐI API (DeepSeek, OpenAI) ============================
def get_api_clients() -> list:
    """[(ProviderConfig, OpenAI client)] theo thứ tự: provider chính rồi provider dự phòng."""
    global _api_clients
    if _api_clients is None:
        from openai import OpenAI

        missing = [p.name for p in config.LLM_PROVIDERS if not p.api_key]
        if missing:
            keys = ", ".join(config.PROVIDER_PRESETS[n]["key_env"] for n in missing)
            raise RuntimeError(f"Chưa đặt API key cho LLM provider {', '.join(missing)}: thêm {keys} vào .env")
        _api_clients = [
            (p, OpenAI(base_url=p.base_url, api_key=p.api_key,
                       timeout=config.LLM_TIMEOUT, max_retries=config.LLM_MAX_RETRIES))
            for p in config.LLM_PROVIDERS
        ]
    return _api_clients


def check_api():
    """Kiểm tra API key và gọi thử provider chính. Trả về (ok, thông báo)."""
    try:
        provider, client = get_api_clients()[0]
        client.models.list()
    except Exception as e:  # noqa: BLE001
        return False, f"Không gọi được LLM API ({config.LLM_PROVIDER}). Chi tiết: {e}"
    return True, f"LLM API sẵn sàng: {provider.name} / {provider.model}"


def check_llm():
    """Kiểm tra LLM theo config.LLM_BACKEND. Trả về (ok, thông báo)."""
    return check_api() if config.LLM_BACKEND == "api" else check_ollama()


def _parse_json_object(text: str) -> dict:
    text = re.sub(r"<think>.*?</think>", "", text, flags=re.S).strip()
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        m = re.search(r"\{.*\}", text, re.S)
        if not m:
            raise ValueError(f"LLM không trả về JSON: {text[:200]}")
        data = json.loads(m.group(0))
    if not isinstance(data, dict):
        raise ValueError("LLM trả về JSON nhưng không phải object")
    return data


def _chat_json(system: str, user: str, schema: dict, retries: int = 1) -> dict:
    """Gọi LLM và bắt buộc đầu ra theo đúng JSON schema. JSON hỏng thì gọi lại."""
    if config.LLM_BACKEND == "api":
        return _chat_json_api(system, user, schema, retries)
    options = {
        "temperature": config.LLM_TEMPERATURE or 0,
        "num_ctx": config.LLM_NUM_CTX,
        "num_predict": config.LLM_NUM_PREDICT,  # chặn sinh lặp vô hạn
        "repeat_penalty": 1.1,
    }
    if config.LLM_NUM_THREAD is not None:
        options["num_thread"] = config.LLM_NUM_THREAD
    for attempt in range(retries + 1):
        resp = get_client().chat(
            model=config.LLM_MODEL,
            messages=[{"role": "system", "content": system}, {"role": "user", "content": user}],
            format=schema,                      # structured output: Ollama ép đúng cấu trúc
            options=options,
            keep_alive=config.LLM_KEEP_ALIVE,
        )
        try:
            return _parse_json_object(resp["message"]["content"])
        except ValueError:                      # json.JSONDecodeError cũng là ValueError
            if attempt == retries:
                raise


def _chat_json_api(system: str, user: str, schema: dict, retries: int = 1) -> dict:
    """Gọi API ở chế độ JSON object; schema được đưa vào system prompt vì DeepSeek chưa nhận JSON schema.

    Provider chính lỗi (sau khi SDK đã tự gọi lại) hoặc trả JSON hỏng quá số lần thì chuyển sang provider dự phòng.
    """
    system = (f"{system}\n\nChỉ trả về MỘT JSON object, không kèm giải thích, "
              f"đúng theo JSON schema sau:\n{json.dumps(schema, ensure_ascii=False)}")
    messages = [{"role": "system", "content": system}, {"role": "user", "content": user}]
    error = None
    for provider, client in get_api_clients():
        kwargs = {provider.max_tokens_param: config.LLM_NUM_PREDICT}
        if config.LLM_TEMPERATURE is not None:
            kwargs["temperature"] = config.LLM_TEMPERATURE
        try:
            for attempt in range(retries + 1):
                resp = client.chat.completions.create(
                    model=provider.model, messages=messages, response_format={"type": "json_object"}, **kwargs)
                try:
                    return _parse_json_object(resp.choices[0].message.content or "")
                except ValueError:
                    if attempt == retries:
                        raise
        except Exception as e:  # noqa: BLE001
            error = e
    raise error


# ============================ HÀM TIỆN ÍCH ============================
def _str(v) -> str:
    if v is None or isinstance(v, (dict, list)):
        return ""
    return nfc(v).strip()


def _str_list(v) -> list:
    if isinstance(v, str):
        v = re.split(r"[;,\n]", v)
    if not isinstance(v, list):
        return []
    out = {}
    for x in v:
        s = _str(x)
        if s and s.lower() not in out:        # bỏ trùng không phân biệt hoa thường
            out[s.lower()] = s
    return list(out.values())


def _to_float(v):
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return float(v)
    m = re.search(r"-?\d+(?:[.,]\d+)?", str(v or ""))
    return float(m.group(0).replace(",", ".")) if m else None


def _grounded_location(raw: str, source_text: str) -> str:
    """Chuẩn hoá thành phố ('Sài Gòn' -> 'TP. Hồ Chí Minh'), chỉ nhận khi có thật trong văn bản gốc.

    LLM nhỏ hay tự đoán địa điểm khi CV/tin không ghi, nên giá trị không tìm thấy trong
    văn bản sẽ bị bỏ, sau đó thử tự dò thành phố trong văn bản.
    """
    src = nfc(source_text).lower()
    city = detect_city(raw)
    if city and any(term_in_text(k, src) for k in CITY_KEYWORDS[city]):
        return CITY_DISPLAY[city]
    if not city and raw and term_in_text(nfc(raw).lower(), src):
        return raw[:MAX_LOCATION_LEN]
    return CITY_DISPLAY.get(detect_city(src), "")


def _is_native_language(name: str) -> bool:
    low = nfc(name).lower()
    return any(k in low for k in ("tiếng việt", "vietnamese", "tiếng mẹ đẻ"))


def _normalize_languages(items) -> list:
    """Danh sách ngoại ngữ dạng tên chuẩn ('english', 'japanese'...), bỏ tiếng Việt."""
    return list(dict.fromkeys(
        normalize_language(x) for x in _str_list(items) if x and not _is_native_language(x)
    ))


def _cap_level(level: str, years) -> str:
    """Level kỹ năng không thể cao hơn tổng số năm kinh nghiệm cho phép."""
    if years is None or not level:
        return level
    cap = "JUNIOR" if years < 2 else "MID" if years < 5 else "SENIOR"
    return level if _LEVEL_RANK[level] <= _LEVEL_RANK[cap] else cap


# Dòng ngắn cỡ này được coi là một mục liệt kê nguyên dòng ("Thiết kế, chỉnh sửa ảnh/video");
# dòng dài hơn là câu mô tả công việc.
MAX_LISTED_LINE_WORDS = 10


def _listed_items(text: str) -> set:
    """Các mục được liệt kê trong văn bản: từng phần tách theo dấu phẩy/chấm phẩy/gạch đứng,
    và cả dòng ngắn nguyên vẹn (mục Kỹ năng hay ghi một kỹ năng có dấu phẩy trên một dòng)."""
    items = set()
    for line in nfc(text).splitlines():
        items.update(normalize_skill(x) for x in re.split(r"[,;|•·\t]", line))
        if len(line.split()) <= MAX_LISTED_LINE_WORDS:
            items.add(normalize_skill(line.strip(" •·-*▪●■\t")))
    return items


def _looks_like_task(name: str, listed: set) -> bool:
    """'phối hợp với team digital để tối ưu...' là câu mô tả công việc, không phải tên kỹ năng.

    Cụm dài vẫn được giữ nếu CV liệt kê nguyên cụm đó (ví dụ mục Kỹ năng ghi 'Thiết kế và dựng video').
    """
    return name not in listed and (len(name.split()) >= 5 or "," in name)


def _strip_group_prefixes(names) -> list:
    """Cắt tên nhóm LLM ghép vào đầu tên kỹ năng, trả về danh sách cùng độ dài, đã chuẩn hoá lại.

    'AI-assisted Development: ChatGPT, Claude, Copilot' bị LLM trả thành 'ai-assisted development chatgpt',
    'ai-assisted development claude'... Chỉ cắt khi phần đầu dài từ 2 từ và có từ 3 kỹ năng dùng chung,
    để không cắt nhầm các cặp hợp lệ như 'spring boot'/'spring security', 'google ads'/'google analytics'.
    """
    words = [normalize_skill(n).split() for n in names]
    counts = {}
    for w in words:
        for k in range(2, len(w)):              # phần đầu 2 từ trở lên, còn lại ít nhất 1 từ
            counts[tuple(w[:k])] = counts.get(tuple(w[:k]), 0) + 1
    out = []
    for w in words:
        k = next((k for k in range(len(w) - 1, 1, -1) if counts[tuple(w[:k])] >= 3), 0)
        out.append(normalize_skill(" ".join(w[k:])))
    return out


def _normalize_skills_with_level(items, years=None, listed=None) -> list:
    """Chuẩn hoá tên kỹ năng, bỏ trùng; nếu trùng thì giữ level cao hơn.

    listed: các mục liệt kê trong CV; nếu có thì bỏ những mục trông như câu mô tả công việc.
    """
    pairs = []
    for it in items or []:
        if isinstance(it, str):
            pairs.append((it, ""))
        elif isinstance(it, dict):
            pairs.append((_str(it.get("name")), _str(it.get("level")).upper()))
    names = _strip_group_prefixes([name for name, _ in pairs])

    best = {}
    for n, (_, level) in zip(names, pairs):
        if not n or len(n) > 60 or n in NON_SKILLS or n in SOFT_SKILLS:
            continue
        if listed is not None and _looks_like_task(n, listed):
            continue
        if normalize_language(n) in LANGUAGE_KEYWORDS:
            continue                    # ngoại ngữ đã có trường languages riêng
        if level not in LEVELS:
            level = ""
        level = _cap_level(level, years)
        if n not in best or _LEVEL_RANK[level] > _LEVEL_RANK[best[n]]:
            best[n] = level
    return [{"name": n, "level": lv} for n, lv in best.items()]


# ============================ TÍNH SỐ NĂM KINH NGHIỆM ============================
_PRESENT_WORDS = ("present", "now", "current", "hiện tại", "hien tai", "nay")
_MONTH_NAMES = {m: i for i, m in enumerate(
    ("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"), start=1)}


def _parse_month(value, is_end: bool, today: date):
    """'05/2026', '2026-05', 'May 2026', '2026', 'Hiện tại' -> số tháng (năm*12 + tháng - 1)."""
    low = _str(value).lower()
    if not low:
        return None
    if any(term_in_text(w, low) for w in _PRESENT_WORDS):
        return today.year * 12 + today.month - 1
    year = month = None
    if m := re.search(r"\b(\d{1,2})\s*[/\-.]\s*(\d{4})\b", low):
        month, year = int(m.group(1)), int(m.group(2))
    elif m := re.search(r"\b(\d{4})\s*[/\-.]\s*(\d{1,2})\b", low):
        year, month = int(m.group(1)), int(m.group(2))
    elif m := re.search(r"\b([a-z]{3})[a-z]*\.?\s+(\d{4})\b", low):
        month, year = _MONTH_NAMES.get(m.group(1)), int(m.group(2))
    elif m := re.search(r"\b(\d{4})\b", low):
        year, month = int(m.group(1)), (12 if is_end else 1)
    if not year or not month or not (1 <= month <= 12) or not (1950 <= year <= today.year + 1):
        return None
    return year * 12 + month - 1


def experience_years(experiences, today: date = None):
    """Tính tổng số năm kinh nghiệm từ các mốc thời gian, không để LLM tự cộng.

    Các giai đoạn chồng nhau chỉ tính một lần; tháng thực tập tính một nửa.
    Trả về None nếu không đọc được mốc thời gian nào.
    """
    today = today or date.today()
    now = today.year * 12 + today.month - 1
    weights = {}
    for e in experiences or []:
        if not isinstance(e, dict):
            continue
        start = _parse_month(e.get("start"), False, today)
        if start is None:
            continue
        end = _parse_month(e.get("end"), True, today)
        end = min(end if end is not None else start, now)
        weight = 0.5 if e.get("is_internship") else 1.0
        for month in range(start, end + 1):
            weights[month] = max(weights.get(month, 0), weight)
    return round(sum(weights.values()) / 12, 1) if weights else None


# ============================ PHÂN TÍCH CV ============================
PROFILE_SCHEMA = {
    "type": "object",
    "properties": {
        "name": {"type": "string"},
        "current_title": {"type": "string"},
        "job_titles": {"type": "array", "items": {"type": "string"}},
        "experiences": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "title": {"type": "string"},
                    "company": {"type": "string"},
                    "start": {"type": "string"},
                    "end": {"type": "string"},
                    "is_internship": {"type": "boolean"},
                },
                "required": ["title", "company", "start", "end", "is_internship"],
            },
        },
        "years_experience": {"type": "number"},
        "skills": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "name": {"type": "string"},
                    "level": {"type": "string", "enum": list(LEVELS)},
                },
                "required": ["name", "level"],
            },
        },
        "soft_skills": {"type": "array", "items": {"type": "string"}},
        "education": {"type": "string"},
        "languages": {"type": "array", "items": {"type": "string"}},
        "location": {"type": "string"},
        "summary": {"type": "string"},
    },
    "required": ["name", "current_title", "job_titles", "experiences", "years_experience", "skills",
                 "soft_skills", "education", "languages", "location", "summary"],
}

PROFILE_PROMPT = """Bạn là chuyên viên tuyển dụng giàu kinh nghiệm. Đọc CV và trích xuất thông tin theo đúng cấu trúc JSON được yêu cầu.

Quy tắc:
- Chỉ dùng thông tin có trong CV, không bịa, không suy đoán. Trường nào không có thì để chuỗi rỗng hoặc danh sách rỗng.
- experiences: mỗi vị trí đã làm tại công ty/tổ chức (kể cả thực tập, trainee). KHÔNG đưa đồ án môn học, dự án cá nhân trong mục Projects vào đây.
  start, end ghi dạng MM/YYYY (chỉ có năm thì ghi YYYY). Đang làm thì end = "Hiện tại".
  is_internship = true nếu là thực tập sinh, intern, trainee.
  title là VAI TRÒ của ứng viên (ví dụ "Backend Developer", "Thực tập sinh", "Content Creator"), KHÔNG phải tên dự án hay sản phẩm.
  Mỗi công ty/vai trò chỉ một mục; các dự án trong cùng một công việc KHÔNG tách thành mục riêng.
- years_experience: tổng số năm đi làm chính thức. Sinh viên chưa đi làm thì 0.
- skills: liệt kê ĐẦY ĐỦ từng kỹ năng chuyên môn, công cụ, phần mềm, ngôn ngữ lập trình, nghiệp vụ có trong CV. Mỗi kỹ năng một mục, tên ngắn gọn (ví dụ "Python", "Excel", "Kê khai thuế"). Không đưa kỹ năng mềm, ngoại ngữ, tên nhóm như "Frontend", "Backend", "Others", hay tên chức năng/module của dự án (ví dụ "Quản lý kho", "Grammar Quiz").
  skills tối đa 30 mục, ưu tiên kỹ năng được dùng trong kinh nghiệm làm việc và dự án.
- level của mỗi kỹ năng:
  JUNIOR = mới học, dùng dưới 2 năm, ghi "cơ bản", hoặc ứng viên là sinh viên/mới ra trường;
  MID = dùng thường xuyên trong công việc 2-4 năm;
  SENIOR = dùng từ 5 năm trở lên, hoặc có dẫn dắt, thiết kế hệ thống bằng kỹ năng đó.
- languages: ngoại ngữ (không tính tiếng Việt), ví dụ "English", "Japanese".
- location: thành phố ứng viên đang sống hoặc muốn làm việc, CHỈ khi CV ghi rõ. Không đoán từ tên trường hay công ty.
- Nếu CV ghi theo nhóm như "Nhóm: A, B, C" thì chỉ lấy A, B, C. KHÔNG ghép tên nhóm vào tên kỹ năng.
- summary: tóm tắt hồ sơ 2-3 câu, BẮT BUỘC viết bằng tiếng Việt kể cả khi CV viết bằng tiếng Anh.
  summary không nêu con số số năm kinh nghiệm."""


def _display_name(name: str) -> str:
    """'DUONG TUAN KHANG' -> 'Duong Tuan Khang'."""
    return name.title() if name.isupper() else name


def _soft_skills(llm_soft, llm_skills, hard_names: set) -> list:
    """Kỹ năng mềm: lấy từ LLM, thêm các mục LLM xếp nhầm vào skills, bỏ mục đã là kỹ năng chuyên môn."""
    misplaced = [it.get("name") if isinstance(it, dict) else it for it in llm_skills or []]
    misplaced = [s for s in misplaced if normalize_skill(s) in SOFT_SKILLS]
    return [s for s in _str_list(_str_list(llm_soft) + _str_list(misplaced))
            if normalize_skill(s) not in hard_names]


def _by_recency(experiences, today: date = None) -> list:
    """Sắp experiences từ mới nhất đến cũ nhất theo ngày kết thúc ('nay', 'hiện tại'... là mới nhất).

    Mục không đọc được ngày kết thúc thì dùng ngày bắt đầu; không có ngày nào thì xếp cuối.
    Không đọc được ngày của mục nào thì giữ nguyên thứ tự.
    """
    today = today or date.today()

    def key(e):
        m = _parse_month(e.get("end"), True, today)
        return m if m is not None else _parse_month(e.get("start"), False, today)

    keys = [key(e) for e in experiences]
    order = sorted(range(len(experiences)), key=lambda i: (keys[i] is None, -(keys[i] or 0)))
    return [experiences[i] for i in order]          # sorted ổn định: cùng ngày thì giữ thứ tự cũ


# Phần đầu CV chứa dòng tiêu đề (ngay dưới họ tên): số dòng có chữ được xét
HEADLINE_LINES = 6


def _comparable(text: str) -> str:
    """'VIDEO EDITOR - PHOTOGRAPHER' và 'Video Editor – Photographer' -> 'video editor photographer'."""
    return " ".join(re.findall(r"\w+", nfc(text).lower()))


def _is_headline(title: str, cv_text: str) -> bool:
    """title là chức danh ghi ở phần đầu CV, tức công việc ứng viên đang hướng tới."""
    if not title or not is_role_title(title) or not cv_text:
        return False
    head = [line for line in nfc(cv_text).splitlines() if line.strip()][:HEADLINE_LINES]
    return _comparable(title) in _comparable("\n".join(head))


def _titles_from_experiences(current_title: str, job_titles: list, experiences: list, today: date = None,
                             cv_text: str = ""):
    """Chọn current_title, chức danh dùng để tìm việc; các chức danh khác được gộp vào job_titles.

    1. Chức danh ở dòng tiêu đề đầu CV ("VIDEO EDITOR - PHOTOGRAPHER" ngay dưới họ tên) là công việc
       ứng viên muốn tìm, nên được giữ, kể cả khi khác chức danh của công việc gần nhất.
    2. Không có thì lấy chức danh của công việc gần nhất. Chỉ dùng chức danh có từ chỉ vai trò (bỏ qua
       tên dự án LLM ghi nhầm) và bỏ vai trò ngoại khoá (câu lạc bộ, đoàn hội, tình nguyện).
    3. Không có chức danh hợp lệ nào thì giữ nguyên current_title của LLM.
    """
    work = [e for e in experiences if is_role_title(e["title"]) and not is_extracurricular(e["title"], e["company"])]
    titles = [e["title"] for e in work if nfc(e["title"]).lower().strip() not in GENERIC_INTERN_TITLES]
    if _is_headline(current_title, cv_text):
        return current_title, _str_list(job_titles + titles)
    roles = [e["title"] for e in _by_recency(work, today)]
    if not roles:
        return current_title, job_titles
    return _display_name(roles[0]), _str_list(job_titles + [current_title] + titles)


def analyze_profile(cv_text: str, today: date = None) -> dict:
    """CV dạng văn bản -> hồ sơ có cấu trúc, khớp với user_profiles và user_skills."""
    cv_text = nfc(cv_text)[: config.LLM_MAX_CV_CHARS]
    # Nhắc lại ở cuối vì mô hình nhỏ hay quên quy tắc nằm xa trong system prompt
    user = (f'CV:\n"""\n{cv_text}\n"""\n\nNhớ: summary viết bằng tiếng Việt, không nêu số năm kinh nghiệm; '
            "experiences không gồm mục Projects.")
    data = _chat_json(PROFILE_PROMPT, user, PROFILE_SCHEMA)

    experiences = [
        {
            "title": _str(e.get("title")),
            "company": _str(e.get("company")),
            "start": _str(e.get("start")),
            "end": _str(e.get("end")),
            "is_internship": bool(e.get("is_internship")),
        }
        for e in data.get("experiences") or [] if isinstance(e, dict)
    ]
    # Ưu tiên số năm tự tính từ mốc thời gian; LLM nhỏ cộng trừ hay sai
    years = experience_years(experiences, today)
    if years is None:
        years = _to_float(data.get("years_experience"))
        if years is not None and not (0 <= years <= 50):
            years = None

    skills = _normalize_skills_with_level(data.get("skills"), years, _listed_items(cv_text))
    # Dò bù kỹ năng LLM bỏ sót (chưa rõ level)
    have = {s["name"] for s in skills}
    skills += [{"name": s, "level": ""} for s in find_known_skills(cv_text) if s not in have]
    current_title, job_titles = _titles_from_experiences(
        _display_name(_str(data.get("current_title"))), _str_list(data.get("job_titles")), experiences, today,
        cv_text)

    return {
        "name": _display_name(_str(data.get("name"))),
        "current_title": current_title,
        "job_titles": job_titles,
        "experiences": experiences,
        "years_experience": years,
        "skills": skills,
        "soft_skills": _soft_skills(data.get("soft_skills"), data.get("skills"), {s["name"] for s in skills}),
        "education": _str(data.get("education")),
        "languages": _normalize_languages(data.get("languages")),
        "location": _grounded_location(_str(data.get("location")), cv_text),
        "summary": _str(data.get("summary")),
    }


def profile_to_db(profile: dict, raw_resume: str) -> dict:
    """Hồ sơ từ analyze_profile -> giá trị cột của user_profiles và danh sách user_skills.

    Chưa gồm embedding và các khoá (user_id, profile_id), do nơi lưu tự điền.
    """
    raw_resume = nfc(raw_resume)
    years = profile.get("years_experience")
    skills = [
        {"skill_name": s["name"][:MAX_SKILL_LEN], "level": s.get("level", "")}
        for s in profile.get("skills", []) if s.get("name")
    ]
    return {
        "user_profile": {
            "raw_resume": raw_resume,
            "resume_hash": hashlib.sha256(raw_resume.encode("utf-8")).hexdigest(),
            "exp_years": int(round(years)) if years is not None else 0,  # cột int NOT NULL
            "current_title": profile.get("current_title", "")[:MAX_TITLE_LEN],
            "summary": profile.get("summary", ""),
            "education": profile.get("education", ""),
            "location": profile.get("location", "")[:MAX_LOCATION_LEN],
            "languages": profile.get("languages", []),               # jsonb
            "parsed_at": datetime.now(timezone.utc).isoformat(),
        },
        "user_skills": skills,
    }


# ============================ PHÂN TÍCH JOB ============================
JOB_SCHEMA = {
    "type": "object",
    "properties": {
        "skills": {"type": "array", "items": {"type": "string"}},
        "min_years": {"type": "number"},
        "location": {"type": "string"},
        "is_remote": {"type": "boolean"},
        "languages": {"type": "array", "items": {"type": "string"}},
    },
    "required": ["skills", "min_years", "location", "is_remote", "languages"],
}

JOB_PROMPT = """Bạn là chuyên viên tuyển dụng. Đọc tin tuyển dụng và trích xuất thông tin theo đúng cấu trúc JSON được yêu cầu.

Quy tắc:
- Chỉ dùng thông tin có trong tin, không bịa, không suy đoán.
- skills: kỹ năng chuyên môn, công cụ, nghiệp vụ được YÊU CẦU hoặc ƯU TIÊN. Tối đa 15 mục, tên ngắn gọn. Không đưa kỹ năng mềm, không đưa quyền lợi.
- min_years: số năm kinh nghiệm tối thiểu được ghi rõ. "1-3 năm" thì lấy 1. Nếu tin không ghi thì -1.
- location: thành phố làm việc. Không ghi thì chuỗi rỗng.
- is_remote: true nếu cho phép làm việc từ xa hoàn toàn.
- languages: ngoại ngữ được yêu cầu (không tính tiếng Việt). Không yêu cầu thì danh sách rỗng."""


def analyze_job(title: str, description: str, location: str = "") -> dict:
    """Tin tuyển dụng -> thông tin có cấu trúc, khớp với jobs và job_skills.

    location: địa điểm do trang tuyển dụng ghi riêng (nếu có), giúp xác định thành phố.
    """
    header = f"Tiêu đề: {nfc(title)}"
    if location:
        header += f"\nĐịa điểm: {nfc(location)}"
    text = f"{header}\n\n{nfc(description)}"[:6000]
    data = _chat_json(JOB_PROMPT, text, JOB_SCHEMA)

    min_years = _to_float(data.get("min_years"))
    if min_years is None or min_years < 0 or min_years > 30:
        min_years = None                 # None = tin không ghi yêu cầu (khác với 0)
    else:
        min_years = int(round(min_years))  # jobs.min_years là *int bên Go
    location_raw = _str(data.get("location"))
    skills = parse_skill_list(_strip_group_prefixes(parse_skill_list(_str_list(data.get("skills")))))
    skills = [s for s in skills if s not in NON_SKILLS]
    return {
        "skills": skills[:15],
        "min_years": min_years,
        # min_years nâng lên theo cấp bậc trong tiêu đề ("Senior" = 3 năm); Go lọc job ứng viên theo cột này
        "required_years": required_years({"title": nfc(title), "min_years": min_years}),
        "location": _grounded_location(location_raw, text),
        "is_remote": bool(data.get("is_remote")) or is_remote(location_raw),
        "languages": _normalize_languages(data.get("languages")),
    }


def job_to_db(job: dict) -> dict:
    """Kết quả analyze_job -> giá trị cột của jobs (phần AI điền) và danh sách job_skills."""
    return {
        "job": {
            "location": job.get("location", "")[:MAX_LOCATION_LEN],
            "is_remote": bool(job.get("is_remote")),
            "min_years": job.get("min_years"),                       # None -> NULL
            "required_years": job.get("required_years"),             # None -> NULL
            "languages": job.get("languages", []),                   # jsonb
            "analyzed_at": datetime.now(timezone.utc).isoformat(),
        },
        "job_skills": [{"skill_name": s[:MAX_SKILL_LEN]} for s in job.get("skills", [])],
    }
