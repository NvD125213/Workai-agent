"""Chuẩn hoá kỹ năng, ngoại ngữ và địa điểm để so khớp CV với công việc."""
import re
import unicodedata
from functools import lru_cache

from rapidfuzz import fuzz


def nfc(text) -> str:
    """Đưa tiếng Việt về dạng Unicode dựng sẵn (NFC). PDF hay trả về dạng tổ hợp (NFD)."""
    return unicodedata.normalize("NFC", str(text or ""))


# =========================== KỸ NĂNG ===========================
# Các cách viết khác nhau -> tên chuẩn. Bổ sung thêm theo ngành của bạn.
SKILL_ALIASES = {
    "reactjs": "react", "react.js": "react", "react js": "react",
    "nodejs": "node.js", "node js": "node.js", "node": "node.js",
    "vuejs": "vue", "vue.js": "vue",
    "nextjs": "next.js", "angularjs": "angular",
    "js": "javascript", "ts": "typescript",
    "golang": "go", "postgres": "postgresql", "mongo": "mongodb",
    "k8s": "kubernetes", "ci cd": "ci/cd", "cicd": "ci/cd",
    "ml": "machine learning", "dl": "deep learning",
    "xử lý ngôn ngữ tự nhiên": "nlp",
    "ms excel": "excel", "microsoft excel": "excel",
    "ms word": "word", "microsoft word": "word",
    "powerbi": "power bi", "rest": "rest api", "restful api": "rest api", "restful": "rest api",
    "phần mềm misa": "misa", "misa sme": "misa",
    "seo onpage": "seo", "facebook ads manager": "facebook ads",
    "tiếng anh": "english", "tiếng nhật": "japanese",
    "next js": "next.js", "nest.js": "nestjs", "nest js": "nestjs",
    "es6": "javascript", "es6+": "javascript",
    "html5": "html", "css3": "css", "tailwind css": "tailwind", "tailwindcss": "tailwind",
    "restful apis": "rest api", "rest apis": "rest api",
    "microservices architecture": "microservices", "microservice": "microservices",
    "docker-compose": "docker compose", "agile/scrum": "agile",
    "role-based access control": "rbac",
    "chat gpt": "chatgpt", "github copilot": "copilot",
    "meta ads": "facebook ads", "microsoft office": "ms office", "office": "ms office",
    "adobe photoshop": "photoshop", "adobe illustrator": "illustrator", "adobe premiere": "premiere",
    "premiere pro": "premiere", "làm việc theo nhóm": "làm việc nhóm",
}

# Biến thể là từ tiếng Anh thông dụng: không dùng để dò trong văn bản ("the rest of", "a node")
AMBIGUOUS_VARIANTS = {"rest", "node", "office"}

# Từ điển kỹ năng (tên chuẩn) để dò bù những kỹ năng LLM bỏ sót trong CV.
# Không đưa vào các từ dễ trùng với tiếng Anh thường: go, c, r, swift, express, spark, word...
KNOWN_SKILLS = {
    # Ngôn ngữ lập trình
    "python", "java", "javascript", "typescript", "c++", "c#", ".net", "php", "ruby", "kotlin",
    "dart", "rust", "scala", "sql",
    # Web, mobile
    "react", "next.js", "vue", "angular", "node.js", "nestjs", "django", "flask", "fastapi",
    "spring boot", "spring security", "laravel", "html", "css", "tailwind", "bootstrap", "jquery",
    "ant design", "flutter", "react native", "android", "ios",
    # Dữ liệu, hạ tầng
    "mysql", "postgresql", "mongodb", "nosql", "redis", "elasticsearch", "sql server", "sqlite", "firebase",
    "docker", "docker compose", "kubernetes", "aws", "azure", "gcp", "linux", "nginx", "terraform",
    "git", "github", "gitlab", "jenkins", "ci/cd", "kafka", "rabbitmq",
    "grpc", "graphql", "rest api", "jwt", "rbac", "microservices",
    # AI, dữ liệu
    "machine learning", "deep learning", "nlp", "pandas", "numpy", "tensorflow", "pytorch",
    "power bi", "tableau", "airflow", "hadoop",
    # Công cụ, nghiệp vụ
    "figma", "jira", "agile", "selenium", "excel", "photoshop", "autocad",
    "chatgpt", "gemini", "copilot",
    # Marketing, thiết kế, văn phòng
    "seo", "facebook ads", "google ads", "tiktok ads", "google analytics", "google trends",
    "copywriting", "content marketing", "canva", "capcut", "illustrator", "premiere", "lightroom",
    "wordpress", "ms office", "misa",
}

# Techstack cốt lõi: ngôn ngữ, framework chính, nền tảng. Thiếu những kỹ năng này nặng hơn nhiều
# so với thiếu một công cụ phụ (git, jira, docker...), nên được tính trọng số cao và bị phạt khi thiếu.
CORE_TECH = {
    "python", "java", "javascript", "typescript", "c++", "c#", ".net", "php", "ruby", "kotlin", "dart",
    "rust", "scala", "go", "swift",
    "react", "next.js", "vue", "angular", "node.js", "nestjs", "django", "flask", "fastapi",
    "spring boot", "laravel", "flutter", "react native", "android", "ios",
}

# Công nghệ/ngành chỉ dò trong TIÊU ĐỀ job (tiêu đề ngắn nên ít nhầm, còn trong mô tả thì dễ nhầm).
# Tiêu đề chứa các từ này nghĩa là job thuộc hẳn mảng đó: "Kỹ Sư Lập Trình CNC", "SAP Fiori Developer".
TITLE_TECH = CORE_TECH | {
    "magento", "sap", "servicenow", "salesforce", "odoo", "unity", "plc", "cnc", "embedded",
}

# Kỹ năng mềm mà LLM hay xếp nhầm vào kỹ năng chuyên môn
SOFT_SKILLS = {
    "làm việc nhóm", "teamwork", "team collaboration", "giao tiếp", "communication",
    "giải quyết vấn đề", "problem solving", "tư duy phản biện", "critical thinking",
    "quản lý thời gian", "time management", "thuyết trình", "presentation", "tự học", "self-learning",
    "lãnh đạo", "leadership", "làm việc độc lập", "chịu được áp lực", "sáng tạo", "creativity",
}

# Tên nhóm/tiêu đề mục mà LLM hay nhầm là kỹ năng
NON_SKILLS = {
    "frontend", "backend", "database", "others", "other", "tools", "skills", "soft skills",
    "programming languages", "frameworks", "system design & architecture", "ai-assisted development",
    "kỹ năng", "kỹ năng mềm", "công cụ", "khác",
}

# Từ chỉ vai trò: chức danh trong experiences phải chứa ít nhất một từ này,
# để loại tên dự án/sản phẩm LLM ghi nhầm vào chức danh ("Authentication System")
ROLE_WORDS = {
    "developer", "engineer", "intern", "trainee", "fresher", "designer", "analyst", "tester",
    "manager", "leader", "lead", "specialist", "executive", "consultant", "accountant",
    "marketing", "content", "creator", "sales", "admin", "assistant", "officer",
    "thực tập", "nhân viên", "chuyên viên", "kỹ sư", "kế toán", "trưởng", "giám đốc", "trợ lý",
    "lập trình viên",
    # Ngành sáng tạo, giáo dục, dịch vụ, kỹ thuật
    "editor", "photographer", "videographer", "producer", "animator", "artist", "writer", "copywriter",
    "biên tập", "biên tập viên", "nhiếp ảnh", "quay phim", "dựng phim",
    "teacher", "tutor", "lecturer", "giáo viên", "giảng viên", "gia sư",
    "coordinator", "representative", "associate", "staff", "operator", "technician", "programmer",
    "architect", "scientist", "planner", "recruiter", "secretary", "receptionist", "cashier",
    "kỹ thuật viên", "thư ký", "lễ tân", "thu ngân", "tuyển dụng",
}

# Vai trò ngoại khoá (câu lạc bộ, đoàn hội, tình nguyện): vẫn là kinh nghiệm nhưng không phải nghề để tìm việc
EXTRACURRICULAR_WORDS = ("clb", "câu lạc bộ", "club", "đoàn thanh niên", "đoàn trường", "hội sinh viên",
                         "tình nguyện", "volunteer", "ban cán sự", "student union")

# Vai trò nghề nghiệp đủ rộng để làm keyword tìm việc: "Frontend Developer" -> tìm "Developer"
# để bao quát cả fullstack/backend; bộ lọc cứng và chấm điểm sẽ lọc lại sau. Tên chuẩn -> cách hiển thị.
BROAD_ROLES = {
    "lập trình viên": "Lập trình viên", "developer": "Developer", "engineer": "Engineer", "kỹ sư": "Kỹ sư",
    "designer": "Designer", "tester": "Tester", "analyst": "Analyst", "architect": "Architect",
    "kế toán": "Kế toán", "accountant": "Accountant",
}
# Từ chỉ cấp bậc hoặc vị trí chung chung: bỏ đi để còn lại lĩnh vực ("Marketing Executive" -> "Marketing")
_KEYWORD_NOISE = [
    "thực tập sinh", "thực tập", "intern", "internship", "trainee", "fresher", "junior", "jr", "middle", "mid",
    "senior", "sr", "lead", "leader", "principal", "head", "chief", "cấp cao",
    "executive", "specialist", "officer", "assistant", "manager", "staff", "coordinator", "consultant",
    "representative", "associate", "nhân viên", "chuyên viên", "trưởng phòng", "trưởng nhóm", "trợ lý", "giám đốc",
]

# Chức danh chỉ nói là thực tập, không nêu vai trò cụ thể: không đưa vào job_titles
GENERIC_INTERN_TITLES = {"intern", "internship", "trainee", "fresher", "thực tập", "thực tập sinh",
                         "sinh viên thực tập"}

# Bảng ngược: tên chuẩn -> các biến thể, để tìm trong văn bản CV
_REVERSE_ALIASES: dict = {}
for _variant, _canon in SKILL_ALIASES.items():
    _REVERSE_ALIASES.setdefault(_canon, []).append(_variant)


def _is_known(s: str) -> bool:
    return s in SKILL_ALIASES or s in KNOWN_SKILLS


def normalize_skill(skill) -> str:
    s = nfc(skill).lower().strip()
    # Không bỏ dấu chấm ở đầu để giữ ".net"
    s = re.sub(r"\s+", " ", s).lstrip(" ,;:-•*").rstrip(" .,;:-•*")
    # "JavaScript (ES6+)" -> "javascript", "Role-Based Access Control (RBAC)" -> "rbac"
    m = re.fullmatch(r"(.+?)\s*\(([^()]*)\)?", s)
    if m:
        head, inner = m.group(1).strip(), m.group(2).strip()
        s = inner if (not _is_known(head) and _is_known(inner)) else head
    if not _is_known(s):
        # "Java 11+" -> "java", "JavaScript ES6+" -> "javascript"; chỉ cắt khi phần còn lại là kỹ năng đã biết
        m = re.fullmatch(r"(.+?)\s*(?:v?\d+(?:\.\d+)*(?:\.x)?|es\d+|es20\d\d)\+?", s)
        if m and _is_known(m.group(1)):
            s = m.group(1)
    return SKILL_ALIASES.get(s, s)


def split_compound_skill(skill) -> list:
    """'React/Next.js' -> ['react', 'next.js']. Chỉ tách khi mọi phần đều là kỹ năng đã biết,
    để giữ nguyên các tên có sẵn dấu '/' như 'ci/cd', 'ui/ux', 'pl/sql', 'tcp/ip'."""
    s = normalize_skill(skill)
    if "/" not in s or _is_known(s):
        return [s]
    parts = [normalize_skill(p) for p in s.split("/")]
    return parts if all(p in KNOWN_SKILLS for p in parts) else [s]


def parse_skill_list(value) -> list:
    """'Python; SQL, Docker' hoặc list -> list kỹ năng đã chuẩn hoá, không trùng."""
    if value is None:
        return []
    if isinstance(value, float) and value != value:  # NaN từ pandas
        return []
    items = value if isinstance(value, (list, tuple, set)) else re.split(r"[;,|\n]", str(value))
    out, seen = [], set()
    for item in items:
        for s in split_compound_skill(item):
            if s and s not in seen and len(s) <= 60:
                seen.add(s)
                out.append(s)
    return out


@lru_cache(maxsize=4096)
def _pattern(term: str):
    # Ranh giới từ: tránh khớp "java" trong "javascript", "c" trong "c++"
    return re.compile(r"(?<![\w+#])" + re.escape(term) + r"(?![\w+#])", re.IGNORECASE)


def term_in_text(term: str, text_lower: str) -> bool:
    return bool(term) and _pattern(term).search(text_lower) is not None


def is_role_title(title) -> bool:
    """'Full-Stack Developer Trainee' là chức danh; 'Warehouse Management System (WMS)' là tên dự án."""
    low = nfc(title).lower()
    return any(term_in_text(w, low) for w in ROLE_WORDS)


def is_extracurricular(*texts) -> bool:
    """'Trưởng ban Hình Ảnh - CLB Thương Mại Số' là vai trò ngoại khoá, không phải công việc."""
    low = nfc(" ".join(t or "" for t in texts)).lower()
    return any(term_in_text(w, low) for w in EXTRACURRICULAR_WORDS)


def _skill_terms(skill: str) -> list:
    return [skill, *(v for v in _REVERSE_ALIASES.get(skill, []) if v not in AMBIGUOUS_VARIANTS)]


def broad_keyword(title) -> str:
    """Chức danh -> keyword nghĩa rộng để cào job trên các trang tuyển dụng.

    Có vai trò trong BROAD_ROLES thì lấy vai trò: 'Full-Stack Developer Trainee' -> 'Developer'.
    Không có thì bỏ từ chỉ cấp bậc/vị trí chung, giữ lĩnh vực: 'Marketing Executive' -> 'Marketing',
    'Nhân viên kinh doanh' -> 'kinh doanh'. Không còn gì thì trả lại chức danh đã làm sạch.
    """
    # Bỏ phần phụ: "(Hà Nội)", "- Lương 20 triệu", "| Remote"
    clean = re.split(r"\s[-–|]\s|\|", re.sub(r"\([^)]*\)|\[[^\]]*\]", " ", nfc(title)))[0]
    clean = re.sub(r"\s+", " ", clean).strip(" ,.;:-")
    low = clean.lower()
    for role in sorted(BROAD_ROLES, key=len, reverse=True):
        if term_in_text(role, low):
            return BROAD_ROLES[role]
    rest = clean
    for word in sorted(_KEYWORD_NOISE, key=len, reverse=True):
        rest = _pattern(word).sub(" ", rest)
    rest = re.sub(r"\s+", " ", rest).strip(" ,.;:-/&")
    return rest or clean


def skill_in_text(skill: str, text_lower: str) -> bool:
    return any(term_in_text(t, text_lower) for t in _skill_terms(skill))


def count_skill_mentions(skill: str, text_lower: str) -> int:
    """Số lần kỹ năng (kể cả cách viết khác) xuất hiện trong văn bản."""
    return sum(len(_pattern(t).findall(text_lower)) for t in _skill_terms(skill) if t)


# Kỹ năng chung được suy ra từ kỹ năng cụ thể: biết MongoDB thì coi như biết NoSQL.
# Framework suy ra ngôn ngữ của nó (biết Spring Boot thì biết Java), không suy ngược lại.
IMPLIED_SKILLS = {
    "nosql": {"mongodb", "redis", "firebase", "elasticsearch", "cassandra", "dynamodb"},
    "sql": {"mysql", "postgresql", "sql server", "sqlite", "oracle"},
    "javascript": {"typescript", "react", "vue", "angular", "node.js", "next.js"},
    "java": {"spring boot"},
    "python": {"django", "flask", "fastapi"},
    "php": {"laravel"},
    "react": {"next.js"},
    "node.js": {"nestjs"},
    ".net": {"c#"},      # .NET và C# gần như luôn đi cùng nhau
    "c#": {".net"},
}


def match_skills(job_skills, cv_skills, cv_text_lower, fuzzy_threshold=90):
    """Chia kỹ năng của job thành (đã có trong CV, còn thiếu)."""
    cv_set = set(cv_skills)
    cv_set |= {general for general, specific in IMPLIED_SKILLS.items() if cv_set & specific}
    matched, missing = [], []
    for js in job_skills:
        ok = (
            js in cv_set                                                     # khớp chính xác
            or any(fuzz.ratio(js, cs) >= fuzzy_threshold for cs in cv_set)   # khớp gần đúng
            or skill_in_text(js, cv_text_lower)                              # có trong văn bản CV
        )
        (matched if ok else missing).append(js)
    return matched, missing


def find_skills_in_text(text_lower: str, vocab) -> list:
    """Tìm các kỹ năng (lấy từ danh sách job) xuất hiện trong văn bản CV."""
    return [s for s in vocab if skill_in_text(s, text_lower)]


def find_known_skills(text) -> list:
    """Dò các kỹ năng trong từ điển KNOWN_SKILLS xuất hiện trong văn bản."""
    return find_skills_in_text(nfc(text).lower(), sorted(KNOWN_SKILLS))


# =========================== NGOẠI NGỮ ===========================
LANGUAGE_KEYWORDS = {
    "english": ["english", "tiếng anh", "ielts", "toeic", "toefl"],
    "japanese": ["japanese", "tiếng nhật", "jlpt", "nhật ngữ"],
    "korean": ["korean", "tiếng hàn", "topik"],
    "chinese": ["chinese", "tiếng trung", "tiếng hoa", "hsk", "mandarin"],
    "french": ["french", "tiếng pháp", "delf"],
    "german": ["german", "tiếng đức"],
}
LANGUAGE_DISPLAY = {
    "english": "English", "japanese": "Japanese", "korean": "Korean",
    "chinese": "Chinese", "french": "French", "german": "German",
}


def normalize_language(name) -> str:
    low = nfc(name).lower().strip()
    for canon, keywords in LANGUAGE_KEYWORDS.items():
        if any(term_in_text(k, low) for k in keywords):
            return canon
    return low


def detect_languages(text_lower: str) -> set:
    return {c for c, kws in LANGUAGE_KEYWORDS.items() if any(term_in_text(k, text_lower) for k in kws)}


# =========================== ĐỊA ĐIỂM ===========================
CITY_KEYWORDS = {
    "hcm": ["hồ chí minh", "ho chi minh", "tp.hcm", "tphcm", "hcm", "hcmc", "sài gòn", "sai gon", "saigon",
            # Quận/huyện: CV hay chỉ ghi "Tân Bình", "Quận 7" mà không ghi thành phố
            *[f"quận {i}" for i in range(1, 13)], "tân bình", "bình thạnh", "gò vấp", "phú nhuận",
            "thủ đức", "tân phú", "bình tân", "nhà bè", "hóc môn", "bình chánh", "củ chi"],
    "hanoi": ["hà nội", "ha noi", "hanoi",
              "cầu giấy", "đống đa", "hoàn kiếm", "ba đình", "hai bà trưng", "thanh xuân", "hoàng mai",
              "long biên", "tây hồ", "nam từ liêm", "bắc từ liêm", "hà đông"],
    "danang": ["đà nẵng", "da nang", "danang"],
    "haiphong": ["hải phòng", "hai phong"],
    "cantho": ["cần thơ", "can tho"],
    "binhduong": ["bình dương", "binh duong"],
    "dongnai": ["đồng nai", "dong nai", "biên hòa", "biên hoà"],
}
CITY_DISPLAY = {
    "hcm": "TP. Hồ Chí Minh", "hanoi": "Hà Nội", "danang": "Đà Nẵng", "haiphong": "Hải Phòng",
    "cantho": "Cần Thơ", "binhduong": "Bình Dương", "dongnai": "Đồng Nai",
}
REMOTE_KEYWORDS = ["remote", "từ xa", "work from home", "wfh"]


def detect_city(text) -> str:
    """Trả về mã thành phố xuất hiện SỚM NHẤT (địa chỉ thường nằm đầu CV)."""
    low = nfc(text).lower()
    best, best_pos = "", len(low) + 1
    for city, keywords in CITY_KEYWORDS.items():
        for k in keywords:
            m = _pattern(k).search(low)
            if m and m.start() < best_pos:
                best, best_pos = city, m.start()
    return best


def is_remote(text) -> bool:
    low = nfc(text).lower()
    return any(term_in_text(k, low) for k in REMOTE_KEYWORDS)