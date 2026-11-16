# Dịch vụ LLM: chế độ development và production

Tài liệu này mô tả cấu trúc của dịch vụ AI (thư mục `llm/`), luồng xử lý của từng API và những gì thay đổi
khi chạy ở chế độ `development` (máy cá nhân) so với `production` (VPS ~6GB RAM).

## 1. Chọn chế độ

Chế độ được chọn bằng một biến trong `llm/.env`:

```
APP_ENV=development   # hoặc production
```

`matching/config.py` đọc `APP_ENV` và suy ra cấu hình cho ba thành phần nặng: LLM, embedding, reranker.
Từng thành phần vẫn ghi đè riêng được bằng biến môi trường (xem mục 5).

Tóm tắt: **production gọi API cho LLM và reranker, còn embedding chạy trên máy giống hệt development.**

| Thành phần             | development                                                | production                                                                           |
| ---------------------- | ---------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| LLM (phân tích CV)     | Ollama trên máy, `qwen2.5:3b`                              | API DeepSeek `deepseek-chat` (hoặc OpenAI), có provider dự phòng                     |
| Embedding              | `BAAI/bge-m3`, PyTorch fp32, chuỗi 1024 / batch 8          | `BAAI/bge-m3`, PyTorch fp32, chuỗi 1024 / batch 8                                    |
| Reranker               | `BAAI/bge-reranker-v2-m3` trên máy                         | API SiliconFlow `Qwen/Qwen3-Reranker-0.6B` nếu có `RERANK_API_KEY`, không có thì tắt |
| Thời gian chờ LLM      | 300 giây                                                   | 60 giây                                                                              |
| Thư viện cài           | `requirements.txt` (= prod + ollama, pandas, streamlit)    | `requirements-prod.txt` (torch bản CPU, không có ollama)                             |
| RAM của service        | ~8GB trở lên (Ollama ~3GB + bge-m3 ~2GB + reranker ~2-3GB) | ~2.2GB (đo: RSS 2.18GB, peak 2.26GB)                                                 |
| `model_version` trả về | `BAAI/bge-m3+BAAI/bge-reranker-v2-m3`                      | `BAAI/bge-m3+Qwen/Qwen3-Reranker-0.6B` (hoặc `BAAI/bge-m3` khi tắt rerank)           |

Những phần **không đổi** giữa hai chế độ: embedding (model, độ chính xác, độ dài chuỗi, nên vector giống hệt),
prompt và JSON schema phân tích CV, toàn bộ bước hậu xử lý sau LLM, bộ lọc cứng, công thức chấm điểm,
trọng số, ngưỡng `MATCH_THRESHOLD`, định dạng request/response của các API.

## 2. Cấu trúc thư mục

```
llm/
├── api.py                  FastAPI: /parse-cv, /analyze-cv, /score, /health (chỉ backend Go gọi)
├── .env / .env.example     APP_ENV, API key, các ghi đè
├── requirements.txt        development (gồm requirements-prod.txt + ollama, pandas, streamlit)
├── requirements-prod.txt   production
├── matching/
│   ├── config.py           đọc .env, chọn backend theo APP_ENV, mọi hằng số (trọng số, ngưỡng...)
│   ├── cv_parser.py        pdf/docx/txt -> văn bản
│   ├── llm.py              phân tích CV bằng LLM: Ollama (_chat_json) hoặc API (_chat_json_api)
│   ├── embedder.py         load_embedder(): Embedder (PyTorch) hoặc OnnxEmbedder (onnxruntime)
│   ├── reranker.py         load_reranker(): Reranker (PyTorch) hoặc ApiReranker (HTTP)
│   ├── crawled.py          job Go vừa cào -> dict job cho Matcher (không gọi LLM), đọc số năm và lương
│   ├── filters.py          bộ lọc cứng: kinh nghiệm, kỹ năng bắt buộc theo tiêu đề, khoảng kinh nghiệm (exp)
│   ├── matcher.py          Matcher.score_jobs: lọc cứng -> chức danh -> rerank -> chấm điểm
│   ├── scoring.py          các điểm thành phần (cả salary, exp_fit), hệ số phạt, combine()
│   ├── texts.py            hồ sơ/job -> đoạn văn đưa vào embedding và rerank
│   ├── normalize.py        chuẩn hoá kỹ năng, thành phố, ngoại ngữ
│   └── report.py           báo cáo markdown data/reports/find_job_<id>.md
└── tests/                  test_llm.py, test_matching.py, test_normalize.py, test_reranker.py
```

Chỗ duy nhất quyết định "chạy trên máy hay gọi API" là `config.py`. Các module khác chỉ hỏi config:

| Biến trong config | Ai dùng                                        | Giá trị                  | development | production              |
| ----------------- | ---------------------------------------------- | ------------------------ | ----------- | ----------------------- |
| `LLM_BACKEND`     | `llm._chat_json`, `llm.check_llm`              | `ollama` / `api`         | `ollama`    | `api`                   |
| `EMBED_BACKEND`   | `embedder.load_embedder`                       | `torch` / `onnx`         | `torch`     | `torch`                 |
| `RERANK_BACKEND`  | `reranker.load_reranker`, `Matcher.score_jobs` | `torch` / `api` / `none` | `torch`     | `api` (có key) / `none` |

Mô hình được nạp **lười**: lần đầu cần đến mới nạp (`Matcher.embedder`, `Matcher.reranker`), sau đó giữ
trong RAM. Lần gọi đầu tiên sau khi khởi động vì vậy chậm hơn (nạp bge-m3 mất khoảng 30-80 giây).

## 3. Luồng xử lý

Backend Go là bên duy nhất gọi dịch vụ này (`backend/internal/adapter/aiservice/client.go`).

### 3.1. Tải CV: `POST /parse-cv`

```
file CV ──> cv_parser.read_cv_bytes ──> văn bản
```

Giống hệt nhau ở hai chế độ, không dùng mô hình.

### 3.2. Phân tích CV: `POST /analyze-cv`

```
cv_text
  │
  ├─(1) llm.analyze_profile
  │       _chat_json(PROFILE_PROMPT, CV, PROFILE_SCHEMA)
  │         ├─ development: Ollama, format=schema (Ollama ép đúng cấu trúc JSON)
  │         └─ production : chat.completions, response_format=json_object,
  │                         schema chèn vào cuối system prompt;
  │                         provider chính lỗi -> provider dự phòng (LLM_FALLBACK_PROVIDER)
  │       hậu xử lý (giống nhau): tự tính số năm kinh nghiệm từ mốc thời gian, chuẩn hoá kỹ năng,
  │       chức danh, ngoại ngữ, địa điểm (chỉ nhận khi có trong CV)
  │
  ├─(2) embedding hồ sơ: embedder.encode_docs(profile_to_text(profile))     [bge-m3 trên máy, cả hai chế độ]
  │
  └─(3) trả về profile, user_profile, user_skills, embedding (1024 chiều), search_keyword
```

Go lưu `profile` lại và gửi kèm khi gọi `/score`.

Bên Go, `POST /api/v1/analyze-cv` gọi endpoint này **trong nền** (bảng `cv_analyses`, trạng thái `QUEUED → ANALYZING → DONE / FAILED`) và trả 202 kèm `id` ngay, vì một lần phân tích mất 30 giây tới vài phút. Endpoint `/analyze-cv` của dịch vụ AI không đổi.

### 3.3. Chấm điểm job: `POST /score`

Go cào job theo keyword rồi gửi cả lô (tối đa 500 job) kèm `profile` sang đây. Mỗi job có thêm `salary` (ô "Mức lương"); request có thêm `exp_filter` (lấy từ query `exp` của `POST /api/v1/find-job/{id}`).

```
jobs vừa cào
  │
  ├─(1) crawled_job_to_matcher: lấy kỹ năng từ tag của trang + dò từ điển, số năm từ ô "Kinh nghiệm",
  │       lương tháng (triệu VND) từ ô "Mức lương" (parse_salary_million; "Thoả thuận" -> None)
  │       (không gọi LLM cho từng job vì quá chậm)
  │
  ├─(2) bộ lọc cứng (filters.apply_hard_filters): loại job thiếu kinh nghiệm hoặc thiếu kỹ năng bắt buộc;
  │       exp_filter=true thì loại thêm job có số năm yêu cầu ngoài experience_band(số năm ứng viên)
  │       -> job bị loại có score 0, kèm reject_reasons; không tốn embedding/rerank
  │
  ├─(3) embedding hồ sơ và các job còn lại -> cosine hồ sơ-job           [bge-m3 trên máy]
  │
  ├─(4) Matcher.score_jobs
  │       ├─ độ khớp chức danh: embedding desired_title so với tiêu đề job    [bge-m3 trên máy]
  │       ├─ rerank (hồ sơ, job)                       [development: trên máy | production: API hoặc tắt]
  │       │     API lỗi (sai key, 429, 5xx, hết giờ, đã gọi lại 2 lần)
  │       │       -> chấm tiếp không có rerank, response có "rerank_error", log cảnh báo
  │       ├─ điểm luật: kỹ năng, kinh nghiệm, ngoại ngữ/địa điểm
  │       ├─ exp_filter=true: thêm salary (thứ hạng phần trăm lương trong lô) và exp_fit
  │       │     (job yêu cầu ít năm hơn trong khoảng phù hợp thì cao hơn)
  │       ├─ combine(): tổng có trọng số, thành phần None bị bỏ và trọng số được chia lại
  │       └─ nhân hệ số phạt thiếu techstack; passed = score >= MATCH_THRESHOLD (60)
  │
  └─(5) job đạt ngưỡng có thêm job_data (kỹ năng, số năm, embedding...) để Go lưu
```

Trọng số (`config.WEIGHTS`): skills 0.35, rerank 0.20, title 0.10, experience 0.20, semantic 0.05, extra 0.10.

**Bộ lọc kinh nghiệm + lương (`exp_filter=true`):**

| Số năm ứng viên (làm tròn) | Giữ job yêu cầu |
| -------------------------- | --------------- |
| ≤ 2                        | 0-2 năm         |
| 3                          | 2-3 năm         |
| 4-5                        | 2-5 năm         |
| trên 5                     | từ 3 năm (`EXP_BAND_SENIOR_MIN`) tới số năm ứng viên |

Job không rõ số năm yêu cầu thì giữ. Cấu hình ở `config.EXP_BANDS`. Thêm trọng số `config.EXP_FILTER_WEIGHTS`:
salary 0.20, exp_fit 0.10. Lương nặng hơn kinh nghiệm, nên job 2 năm 20 triệu đứng trên job 3 năm 18 triệu, còn job
3 năm 21 triệu đứng trên job 2 năm 17 triệu. Lương USD quy đổi theo `USD_TO_VND_MILLION` (0.025, tức 1 USD ≈ 25.000 đ).
Response có thêm `exp_band` ([thấp, cao]); báo cáo `find_job_<id>.md` có thêm cột Salary, ExpFit, Năm YC, Lương.

Các request dùng mô hình chạy **tuần tự** (`_model_lock` trong `api.py`), vì chạy song song chỉ làm mỗi
request chậm hơn và tốn RAM gấp đôi.

## 4. Chi tiết từng thành phần

### 4.1. LLM

|                       | development (`ollama`)                       | production (`api`)                                                 |
| --------------------- | -------------------------------------------- | ------------------------------------------------------------------ |
| Client                | `ollama.Client`                              | `openai.OpenAI` (DeepSeek và OpenAI cùng định dạng)                |
| Ép JSON               | `format=schema`                              | `response_format={"type": "json_object"}`, schema nằm trong prompt |
| Giới hạn token đầu ra | `num_predict`                                | `max_tokens` (DeepSeek) / `max_completion_tokens` (OpenAI)         |
| Gọi lại khi lỗi mạng  | không                                        | SDK tự gọi lại `LLM_MAX_RETRIES` lần (mặc định 3)                  |
| JSON hỏng             | gọi lại 1 lần                                | gọi lại 1 lần, rồi chuyển sang provider dự phòng                   |
| Kiểm tra sẵn sàng     | `check_ollama()`: Ollama chạy và đã có model | `check_api()`: có key và gọi được `/models`                        |

`llm.check_llm()` tự chọn hàm kiểm tra theo chế độ.

### 4.2. Embedding

Cả hai chế độ dùng `BAAI/bge-m3` trên PyTorch fp32 (`Embedder`), độ dài chuỗi 1024, batch 8. Vector tạo ở
máy dev và trên VPS giống hệt nhau, nên chuyển hay dùng chung dữ liệu embedding giữa hai môi trường không
có vấn đề gì.

Nếu sau này cần chạy trên VPS RAM thấp (~2GB), đặt `EMBED_BACKEND=onnx` (thêm `EMBED_MAX_SEQ=512`,
`EMBED_BATCH=4`) để dùng bge-m3 lượng tử hoá int8 (`OnnxEmbedder`, repo `Xenova/bge-m3`).
Số đo trên máy dev, 2 luồng CPU, 20 đoạn văn:

|              | PyTorch fp32 (mặc định) | PyTorch bf16 | ONNX int8 (`EMBED_BACKEND=onnx`) |
| ------------ | ----------------------- | ------------ | -------------------------------- |
| RAM khi chạy | ~1.95GB                 | ~1.87GB      | ~0.9GB                           |
| Peak lúc nạp | ~2.0GB                  | ~3.7GB       | ~1.2GB                           |
| Thời gian    | 48 giây                 | 223 giây     | 23 giây                          |

bf16 không được dùng vì chậm và tốn RAM lúc nạp. So với fp32 trên dữ liệu thật (2 CV, 50 job TopCV), ONNX int8
cho cosine lệch trung bình ~0.01, tương quan thứ hạng 0.99. Vì vậy hai bản dùng chung tên
`embedding_model = BAAI/bge-m3` và chung các ngưỡng `SEMANTIC_LOW/HIGH`. Go chỉ so vector có cùng
`embedding_model`, nên vector của hai bản vẫn so được với nhau.

### 4.3. Reranker

| `RERANK_BACKEND`              | RAM thêm          | Tốc độ                           | Model                                       |
| ----------------------------- | ----------------- | -------------------------------- | ------------------------------------------- |
| `torch` (development)         | ~3GB (model fp32) | ~31 giây cho 8 job (2 luồng CPU) | `BAAI/bge-reranker-v2-m3`                   |
| `api` (production có key)     | gần như 0         | vài giây mỗi lượt (ước tính)     | `Qwen/Qwen3-Reranker-0.6B` trên SiliconFlow |
| `none` (production không key) | 0                 | 0                                | –                                           |

Rerank chỉ chạy trên các job đã qua bộ lọc cứng (thường vài đến vài chục job). Mỗi request API gửi tối đa
32 job. Chi phí ước tính dưới 0.001 USD mỗi lượt với Qwen3-Reranker-0.6B ($0.01 / 1 triệu token).
Đoạn hồ sơ gửi đi chỉ có chức danh, kỹ năng, số năm kinh nghiệm, học vấn, ngoại ngữ và tóm tắt,
không có tên hay thông tin liên hệ.

**Ảnh hưởng đến kết quả khi tắt rerank** (so trên 2 CV, 50 job TopCV):

- Thứ hạng gần như giữ nguyên: tương quan 0.95-0.99, top 5 trùng 5/5.
- Điểm tăng trung bình ~6-7 điểm, vì trọng số 0.20 của rerank được chia cho các thành phần còn lại.
  Job "na ná nhưng không hợp" bị đẩy lên, ví dụ một job NodeJS từ 52.9 lên 63.5, từ trượt thành đạt.
- Nếu chạy production không có rerank, nên nâng `MATCH_THRESHOLD` lên khoảng 65.

**Khi dùng rerank qua API:** SiliconFlow bản quốc tế không có `bge-reranker-v2-m3` nên mặc định dùng
Qwen3-Reranker. Điểm của model này phân bố khác bge, nên cần chạy so sánh trên dữ liệu thật rồi chỉnh
ngưỡng (chưa làm, cần API key). Đây là khác biệt duy nhất về chất lượng còn lại giữa hai chế độ
(ngoài model LLM).

## 5. Biến môi trường

Chỉ `APP_ENV` và các API key là phải đặt; các biến khác có mặc định theo chế độ.

| Biến                                          | Mặc định development                 | Mặc định production                     | Ý nghĩa                                             |
| --------------------------------------------- | ------------------------------------ | --------------------------------------- | --------------------------------------------------- |
| `APP_ENV`                                     | `development`                        |                                         | Chọn chế độ                                         |
| `LLM_BACKEND`                                 | `ollama`                             | `api`                                   |                                                     |
| `LLM_MODEL`                                   | `qwen2.5:3b`                         | model mặc định của provider             | Ghi đè model LLM                                    |
| `LLM_PROVIDER`                                |                                      | `deepseek`                              | `deepseek` hoặc `openai`                            |
| `DEEPSEEK_API_KEY` / `OPENAI_API_KEY`         |                                      | bắt buộc với provider đang dùng         |                                                     |
| `LLM_FALLBACK_PROVIDER`, `LLM_FALLBACK_MODEL` |                                      | rỗng (không dự phòng)                   |                                                     |
| `LLM_TIMEOUT`                                 | 300                                  | 60                                      | Giây                                                |
| `LLM_MAX_TOKENS`                              | 2048                                 | 2048                                    |                                                     |
| `LLM_TEMPERATURE`                             | 0                                    | 0                                       | Để trống thì không gửi                              |
| `LLM_MAX_RETRIES`                             |                                      | 3                                       | Số lần SDK gọi lại                                  |
| `OLLAMA_HOST_URL`                             | `http://localhost:11434`             |                                         |                                                     |
| `EMBED_BACKEND`                               | `torch`                              | `torch`                                 | `onnx` cho VPS RAM thấp                             |
| `EMBED_MAX_SEQ` / `EMBED_BATCH`               | 1024 / 8                             | 1024 / 8                                | Giảm thì đỡ RAM                                     |
| `CPU_THREADS`                                 | để thư viện tự chọn                  | để thư viện tự chọn                     | Trên VPS nên đặt số vCPU - 1 (chừa cho Go/Postgres) |
| `RERANK_BACKEND`                              | `torch`                              | `api` nếu có key, không thì `none`      |                                                     |
| `RERANK_API_KEY`                              |                                      |                                         | Key SiliconFlow                                     |
| `RERANK_API_URL`                              |                                      | `https://api.siliconflow.com/v1/rerank` | Tài khoản Trung Quốc: `api.siliconflow.cn`          |
| `RERANK_MODEL`                                | `BAAI/bge-reranker-v2-m3`            | `Qwen/Qwen3-Reranker-0.6B`              |                                                     |
| `RERANK_API_TIMEOUT` / `RERANK_API_RETRIES`   |                                      | 30 / 2                                  |                                                     |
| `HF_HOME`                                     | `D:\llm_setup\huggingface` (Windows) | `~/.cache/huggingface` (Linux)          | Nơi lưu mô hình                                     |
| `HF_HUB_OFFLINE`                              | 1                                    | 1                                       | Đặt 0 khi cần tải mô hình lần đầu                   |

## 6. Chạy và triển khai

### Development (máy cá nhân)

```
cd llm
.venv\Scripts\pip install -r requirements.txt
ollama pull qwen2.5:3b
.venv\Scripts\python.exe -m uvicorn api:app --host 127.0.0.1 --port 8001
```

### Production (VPS)

```
cd llm
python -m venv .venv
# Cài torch bản CPU trước; nếu không, pip trên Linux tải bản CUDA vài GB
.venv/bin/pip install torch --index-url https://download.pytorch.org/whl/cpu
.venv/bin/pip install -r requirements-prod.txt
# .env: APP_ENV=production, DEEPSEEK_API_KEY=..., RERANK_API_KEY=..., CPU_THREADS=<số vCPU - 1>
# Lần đầu: tải BAAI/bge-m3 (~2.3GB)
HF_HUB_OFFLINE=0 .venv/bin/python -c "from matching.embedder import load_embedder; load_embedder()"
.venv/bin/uvicorn api:app --host 127.0.0.1 --port 8001 --workers 1
```

- Chạy **1 worker**: mỗi worker nạp một bản bge-m3 riêng (~2GB mỗi bản).
- Kiểm tra `GET /health`: trả về `app_env`, `llm` (backend:model), `model_version`.
- Response của `/score` có `timings` (thời gian từng bước) và `rerank_error` nếu API rerank lỗi.

### Dung lượng VPS (6GB RAM)

| Thành phần                                                  | RAM                               |
| ----------------------------------------------------------- | --------------------------------- |
| Hệ điều hành + Docker                                       | ~0.5-0.8GB                        |
| PostgreSQL + pgvector (cấu hình mặc định)                   | ~0.3-1GB                          |
| Go api + worker (cào bằng HTTP, không chạy Chrome headless) | ~0.1-0.3GB                        |
| Service LLM (bge-m3 PyTorch; LLM và rerank qua API)         | ~2.2GB (đo được)                  |
| **Tổng**                                                    | **~3.1-4.3GB, còn dư ~1.7-2.9GB** |

Không nên bật reranker trên máy (`RERANK_BACKEND=torch`) với VPS 6GB: service sẽ lên khoảng 5GB, gần hết RAM.
Nếu sau này chuyển sang VPS ~2GB, đặt `EMBED_BACKEND=onnx` như mục 4.2.

## 7. Test

```
.venv\Scripts\python.exe tests\test_llm.py --offline   # không cần Ollama/API, có test nhánh API giả
.venv\Scripts\python.exe tests\test_reranker.py        # ApiReranker với API giả
.venv\Scripts\python.exe tests\test_normalize.py
.venv\Scripts\python.exe tests\test_matching.py        # phần B nạp bge-m3 + reranker trên máy
.venv\Scripts\python.exe tests\test_matching.py --offline  # chỉ phần A: lọc cứng, đọc lương, xếp hạng exp
```

Các test chạy theo `APP_ENV` trong `.env`. Nên chạy với `APP_ENV=development`.
