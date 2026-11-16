"""Cấu hình chung cho toàn bộ dự án. Muốn đổi mô hình, trọng số... thì sửa ở đây.

APP_ENV (trong .env) chọn bộ cấu hình:
  development  chạy toàn bộ trên máy: LLM qua Ollama, bge-m3 (PyTorch fp32), reranker trên máy.
  production   VPS (~6GB RAM): LLM qua API (DeepSeek/OpenAI), bge-m3 PyTorch fp32 giống development
               (~2GB RAM), reranker qua API (SiliconFlow) nếu có RERANK_API_KEY.
Từng mục vẫn ghi đè được bằng biến môi trường riêng (LLM_BACKEND, EMBED_BACKEND, RERANK_BACKEND...);
VPS RAM thấp (~2GB) thì đặt EMBED_BACKEND=onnx.
"""
import os
from dataclasses import dataclass
from pathlib import Path

from dotenv import load_dotenv

load_dotenv(Path(__file__).resolve().parent.parent / ".env")


def _env_int(name: str, default):
    v = os.getenv(name, "").strip()
    return int(v) if v else default


APP_ENV = os.getenv("APP_ENV", "development").strip().lower()
if APP_ENV not in ("development", "production"):
    raise ValueError(f"APP_ENV phải là development hoặc production, đang là {APP_ENV!r}")
IS_PRODUCTION = APP_ENV == "production"

# Nơi lưu mô hình Hugging Face. Nếu biến môi trường HF_HOME đã đặt thì dùng giá trị đó;
# trên Linux (VPS) không đặt thì dùng mặc định của Hugging Face (~/.cache/huggingface).
# Dòng này PHẢI chạy trước khi import sentence_transformers / huggingface_hub.
if os.name == "nt":
    os.environ.setdefault("HF_HOME", r"D:\llm_setup\huggingface")
os.environ.setdefault("HF_HUB_DISABLE_SYMLINKS_WARNING", "1")  # tắt cảnh báo symlink trên Windows
# Chỉ dùng mô hình đã có trong cache, không hỏi Hugging Face bản mới. Đổi sang mô hình chưa tải thì đặt HF_HUB_OFFLINE=0.
os.environ.setdefault("HF_HUB_OFFLINE", "1")

# ---- Đường dẫn ----
BASE_DIR = Path(__file__).resolve().parent.parent   # thư mục llm\
DATA_DIR = BASE_DIR / "data"
JOBS_FILE = DATA_DIR / "jobs" / "jobs.csv"
INDEX_DIR = DATA_DIR / "index"
JOBS_JSON = INDEX_DIR / "jobs_processed.json"
EMB_FILE = INDEX_DIR / "job_embeddings.npy"
META_FILE = INDEX_DIR / "meta.json"

# ---- LLM ----
# ollama = mô hình chạy trên máy (development); api = API tương thích OpenAI (production)
LLM_BACKEND = os.getenv("LLM_BACKEND", "").strip().lower() or ("api" if IS_PRODUCTION else "ollama")
if LLM_BACKEND not in ("ollama", "api"):
    raise ValueError(f"LLM_BACKEND phải là ollama hoặc api, đang là {LLM_BACKEND!r}")

LLM_NUM_PREDICT = _env_int("LLM_MAX_TOKENS", 2048)  # số token đầu ra tối đa, chặn mô hình sinh lặp vô hạn
LLM_MAX_CV_CHARS = 12000        # cắt CV quá dài trước khi đưa vào LLM
LLM_TIMEOUT = float(os.getenv("LLM_TIMEOUT") or (60 if LLM_BACKEND == "api" else 300))  # giây
# Để trống (LLM_TEMPERATURE=) thì không gửi temperature, cho các mô hình chỉ nhận giá trị mặc định
_temperature = os.getenv("LLM_TEMPERATURE", "0").strip()
LLM_TEMPERATURE = float(_temperature) if _temperature else None

# -- Ollama (development) --
OLLAMA_HOST = os.environ.get("OLLAMA_HOST_URL", "http://localhost:11434")
LLM_NUM_CTX = 6144              # độ dài ngữ cảnh (token); tăng thì tốn RAM hơn
LLM_KEEP_ALIVE = "30m"          # giữ mô hình trong RAM 30 phút để lần sau nhanh hơn
LLM_NUM_THREAD = None           # số luồng CPU cho Ollama; None = để Ollama tự chọn


# -- API (production) --
@dataclass(frozen=True)
class ProviderConfig:
    name: str
    base_url: str
    api_key: str
    model: str
    max_tokens_param: str       # tên tham số giới hạn token đầu ra của nhà cung cấp


PROVIDER_PRESETS = {
    "deepseek": {
        "base_url": "https://api.deepseek.com",
        "key_env": "DEEPSEEK_API_KEY",
        "default_model": "deepseek-chat",
        "max_tokens_param": "max_tokens",
    },
    "openai": {
        "base_url": "https://api.openai.com/v1",
        "key_env": "OPENAI_API_KEY",
        "default_model": "gpt-4.1-mini",
        "max_tokens_param": "max_completion_tokens",
    },
}

LLM_PROVIDER = os.getenv("LLM_PROVIDER", "deepseek").strip().lower()
LLM_FALLBACK_PROVIDER = os.getenv("LLM_FALLBACK_PROVIDER", "").strip().lower()   # rỗng = không dùng dự phòng
LLM_MAX_RETRIES = _env_int("LLM_MAX_RETRIES", 3)  # số lần SDK tự gọi lại khi lỗi mạng, 429, 5xx


def provider_config(name: str, model: str = None) -> ProviderConfig:
    """Cấu hình một nhà cung cấp API. Thiếu API key chỉ báo lỗi khi thực sự gọi."""
    if name not in PROVIDER_PRESETS:
        raise ValueError(f"LLM provider {name!r} không hỗ trợ, chọn một trong: {', '.join(PROVIDER_PRESETS)}")
    p = PROVIDER_PRESETS[name]
    return ProviderConfig(
        name=name,
        base_url=os.getenv(f"{name.upper()}_BASE_URL") or p["base_url"],
        api_key=os.getenv(p["key_env"], "").strip(),
        model=model or p["default_model"],
        max_tokens_param=p["max_tokens_param"],
    )


# Provider chính rồi tới provider dự phòng (nếu có); LLM_MODEL/LLM_FALLBACK_MODEL ghi đè model mặc định
LLM_PROVIDERS = [provider_config(LLM_PROVIDER, os.getenv("LLM_MODEL") or None)]
if LLM_FALLBACK_PROVIDER:
    LLM_PROVIDERS.append(provider_config(LLM_FALLBACK_PROVIDER, os.getenv("LLM_FALLBACK_MODEL") or None))

# Tên mô hình đang dùng (hiện ở /health, check_llm)
if LLM_BACKEND == "api":
    LLM_MODEL = LLM_PROVIDERS[0].model
else:
    LLM_MODEL = os.getenv("LLM_MODEL") or "qwen2.5:3b"   # tốt hơn nhưng chậm hơn: "qwen2.5:7b"

# ---- Embedding và reranker ----
CPU_THREADS = _env_int("CPU_THREADS", None)  # số luồng CPU cho PyTorch/ONNX; None = để thư viện tự chọn
# torch = sentence-transformers fp32 (~2GB RAM), mặc định cả hai chế độ;
# onnx = bge-m3 lượng tử hoá int8 (~0.9GB RAM, không cần PyTorch), cho VPS RAM thấp.
# Hai bản cho cosine lệch nhau ~0.01 nên vẫn dùng chung tên EMBED_MODEL, Go so được vector của nhau.
EMBED_BACKEND = os.getenv("EMBED_BACKEND", "").strip().lower() or "torch"
if EMBED_BACKEND not in ("torch", "onnx"):
    raise ValueError(f"EMBED_BACKEND phải là torch hoặc onnx, đang là {EMBED_BACKEND!r}")
EMBED_MODEL = "BAAI/bge-m3"                 # nhẹ hơn: "intfloat/multilingual-e5-base" (768 chiều, chỉ backend torch)
EMBED_ONNX_REPO = "Xenova/bge-m3"           # bge-m3 xuất sẵn sang ONNX
EMBED_ONNX_FILE = "onnx/model_quantized.onnx"
EMBED_DIM = 1024                            # PHẢI khớp cột vector(1024) trong PostgreSQL
# Chuỗi dài hơn và batch lớn hơn tốn RAM hơn (attention tăng theo bình phương độ dài)
EMBED_MAX_SEQ = _env_int("EMBED_MAX_SEQ", 1024)          # VPS RAM thấp: 512
EMBED_BATCH = _env_int("EMBED_BATCH", 8)                 # VPS RAM thấp: 4
# Reranker: torch = mô hình trên máy (thêm ~3GB RAM, chậm trên CPU yếu); api = gọi API /rerank (SiliconFlow,
# hoặc dịch vụ cùng định dạng như Jina, Cohere); none = tắt, trọng số "rerank" được chia lại cho các thành phần khác.
# Mặc định: development = torch; production = api nếu đã đặt RERANK_API_KEY, không thì none.
RERANK_API_KEY = os.getenv("RERANK_API_KEY", "").strip()
RERANK_BACKEND = os.getenv("RERANK_BACKEND", "").strip().lower() or (
    ("api" if RERANK_API_KEY else "none") if IS_PRODUCTION else "torch")
if RERANK_BACKEND not in ("torch", "api", "none"):
    raise ValueError(f"RERANK_BACKEND phải là torch, api hoặc none, đang là {RERANK_BACKEND!r}")
RERANK_ENABLED = RERANK_BACKEND != "none"
RERANK_MAX_LEN = 512
RERANK_BATCH = 8
# -- API --
RERANK_API_URL = os.getenv("RERANK_API_URL") or "https://api.siliconflow.com/v1/rerank"
RERANK_API_TIMEOUT = float(os.getenv("RERANK_API_TIMEOUT") or 30)
RERANK_API_RETRIES = _env_int("RERANK_API_RETRIES", 2)   # gọi lại khi 429, 5xx, hết thời gian chờ
RERANK_API_BATCH = 32                                    # số job mỗi request
# torch: mặc định BAAI/bge-reranker-v2-m3 (nhẹ hơn: "BAAI/bge-reranker-base").
# api: mặc định Qwen/Qwen3-Reranker-0.6B (SiliconFlow bản quốc tế không có bge-reranker-v2-m3; tốt hơn: Qwen/Qwen3-Reranker-8B)
RERANK_MODEL = os.getenv("RERANK_MODEL") or (
    "Qwen/Qwen3-Reranker-0.6B" if RERANK_BACKEND == "api" else "BAAI/bge-reranker-v2-m3")

# ---- Truy hồi và chấm điểm ----
TOP_K_RETRIEVE = 50      # số job lấy ra ở bước truy hồi, sau đó đem rerank
# Rerank (cross-encoder) là bước chậm nhất: ~1.2s/job trên CPU. Chỉ rerank N job có điểm sơ bộ
# (các thành phần rẻ, chưa có rerank) cao nhất; job còn lại chấm không có rerank (combine chia lại
# trọng số). 0 = rerank tất cả. Lượt find-job 646 job từng mất 11 phút chỉ riêng rerank.
RERANK_TOP_N = _env_int("RERANK_TOP_N", 150)
TOP_K_RESULTS = 10       # số job trả về cho người dùng
TOP_K_EXPLAIN = 5        # số job được LLM viết nhận xét
HYBRID_ALPHA = 0.6       # trọng số vector so với BM25
# ---- Bộ lọc cứng (chạy TRƯỚC khi chấm điểm, job bị loại không vào embedding/rerank) ----
EXP_TOLERANCE_YEARS = 1  # loại job yêu cầu nhiều hơn kinh nghiệm ứng viên quá 1 năm (yêu cầu X năm -> cần >= X-1)
# Số năm tối thiểu suy ra từ cấp bậc ghi trong tiêu đề job, dùng khi tin không ghi số năm hoặc ghi thấp hơn.
# Tiêu đề ghi nhiều cấp ("Junior - Middle") thì lấy cấp thấp nhất.
SENIORITY_MIN_YEARS = {"intern": 0, "fresher": 0, "junior": 0, "middle": 2, "senior": 3, "lead": 4}

# ---- Bộ lọc kinh nghiệm + lương (find-job gửi exp=true) ----
# Chỉ giữ job có số năm yêu cầu nằm trong khoảng hợp với ứng viên, để người 5 năm không được xếp vào job
# thực tập sinh. (số năm ứng viên làm tròn tối đa, cận dưới, cận trên) của số năm job yêu cầu:
#   <= 2 năm -> job 0-2 năm, 3 năm -> 2-3 năm, 4-5 năm -> 2-5 năm.
EXP_BANDS = [(2, 0, 2), (3, 2, 3), (5, 2, 5)]
EXP_BAND_SENIOR_MIN = 3    # trên 5 năm: job yêu cầu từ 3 năm tới số năm của ứng viên
# Thêm vào WEIGHTS khi lọc kinh nghiệm (combine chia lại tổng). Lương nặng hơn kinh nghiệm: job 2 năm 20 triệu
# xếp trên job 3 năm 18 triệu, còn job 3 năm 20 triệu xếp trên job 2 năm 18 triệu.
EXP_FILTER_WEIGHTS = {
    "salary": 0.20,      # lương của job so với các job còn lại cùng lượt (thứ hạng phần trăm), None nếu không ghi
    "exp_fit": 0.10,     # job yêu cầu ít năm hơn trong khoảng phù hợp thì cao hơn
}
USD_TO_VND_MILLION = 0.025   # quy đổi lương USD sang triệu VND (1 USD ~ 25.000 VND)

# ---- Hạn nộp hồ sơ ----
# Job đã hết hạn (còn <= 0 ngày) bị bộ lọc cứng loại. Job còn r < DEADLINE_PENALTY_DAYS ngày bị nhân vào điểm tổng
#   f = DEADLINE_FLOOR + (1 - DEADLINE_FLOOR) * (r / DEADLINE_PENALTY_DAYS) ** DEADLINE_CURVE
# Còn 20 hay 15 ngày như nhau (f = 1); chỉ vài ngày cuối mới bị phạt vì ứng viên cần thời gian chuẩn bị CV.
# DEADLINE_CURVE < 1: điểm giảm chậm lúc đầu rồi tụt nhanh khi sát hạn (còn 4 ngày 0.96, 2 ngày 0.85, 0.5 ngày 0.73);
# > 1 thì ngược lại, tụt nhanh ngay khi qua ngưỡng rồi gần như đứng yên ở mức sàn (2: 4 ngày 0.86, 2 ngày 0.66).
DEADLINE_PENALTY_DAYS = 5
DEADLINE_FLOOR = 0.6
DEADLINE_CURVE = 0.5
DEADLINE_TZ_HOURS = 7      # hạn chỉ ghi ngày (dd/mm/yyyy) được hiểu là hết 23:59:59 ngày đó, giờ Việt Nam

# ---- Phạt khi thiếu techstack (nhân vào điểm tổng) ----
CORE_SKILL_WEIGHT = 3          # trong điểm skills, kỹ năng thuộc CORE_TECH nặng gấp 3 công cụ phụ
KEY_SKILL_MIN_MENTIONS = 3     # kỹ năng cốt lõi được nhắc >= 3 lần trong JD là kỹ năng trọng tâm của job
PENALTY_MISSING_KEY = 0.25     # mỗi kỹ năng trọng tâm bị thiếu: trừ 25% điểm tổng
PENALTY_MISSING_CORE = 0.10    # mỗi kỹ năng cốt lõi khác bị thiếu: trừ 10%
PENALTY_FLOOR = 0.30           # hệ số phạt không thấp hơn mức này

# bge-m3 cho cosine hồ sơ - job khá cao kể cả khi không liên quan (đo trên 50 job TopCV với CV IT:
# job CNC/PLC ~0.41-0.55, trung vị 0.67, job fullstack phù hợp nhất ~0.80) nên phải kéo giãn đoạn này
SEMANTIC_LOW = 0.55      # cosine dưới mức này coi như không liên quan (điểm 0)
SEMANTIC_HIGH = 0.80     # từ mức này trở lên coi như rất khớp (điểm 1)

MATCH_THRESHOLD = 60    # điểm tối thiểu để Go lưu vào ai_matches
WEIGHTS = {             # tổng = 1; nhóm tương đồng ngữ nghĩa (rerank, title, semantic) chỉ còn 35%
    "skills": 0.35,      # tỉ lệ kỹ năng job yêu cầu có trong hồ sơ (có trọng số theo CORE_TECH)
    "rerank": 0.20,      # cross-encoder đánh giá tổng thể
    "title": 0.10,       # công việc mong muốn so với tiêu đề job
    "experience": 0.20,  # số năm kinh nghiệm so với yêu cầu
    "semantic": 0.05,    # cosine hồ sơ - job (Go tính bằng pgvector)
    "extra": 0.10,       # ngoại ngữ, địa điểm
}
