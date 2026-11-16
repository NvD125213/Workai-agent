# notification-job-mb

Hệ thống cào tin tuyển dụng, phân tích CV và gợi ý việc làm phù hợp.

| Thư mục | Thành phần |
|---|---|
| [backend/](backend/) | Go: REST API (`api`), worker cào job + import (`worker`), CLI migrate DB |
| [llm/](llm/) | Python FastAPI: parse/analyze CV, chấm điểm job (embedding bge-m3, LLM, reranker) |
| [docs/](docs/) | Tài liệu dự án |

## Triển khai bằng Docker (toàn bộ hệ thống)

Yêu cầu: Docker + Docker Compose v2, RAM tối thiểu ~4GB (khuyến nghị 6GB+).

```bash
git clone <repo-url> notification-job-mb
cd notification-job-mb
cp .env.example .env
# Sửa .env: điền JWT_ACCESS_SECRET (openssl rand -base64 48) và DEEPSEEK_API_KEY
docker compose up -d --build
```

Các service khởi động theo thứ tự:

```
db (Postgres + pgvector) ──healthy──> migrate (tạo bảng, chạy xong thoát)
                                        ├──> api     :8080  (gọi ai qua http://ai:8001)
                                        └──> worker         (cào job theo lịch)
ai (FastAPI :8001, chỉ trong mạng nội bộ)
```

- API: http://localhost:8080. Kiểm tra sống: http://localhost:8080/health
- Lần đầu chạy, `ai` tải mô hình bge-m3 (~2.3GB) vào volume `hf_models`. Request AI đầu tiên mất thêm 30-80 giây để nạp mô hình.

### Lệnh thường dùng

| Lệnh | Mục đích |
|---|---|
| `docker compose ps` | Xem trạng thái các service |
| `docker compose logs -f api ai worker` | Xem log |
| `git pull && docker compose up -d --build` | Cập nhật code mới. `migrate` tự chạy lại nên schema cũng được cập nhật |
| `docker compose down` | Dừng hệ thống, giữ nguyên dữ liệu |
| `docker compose down -v` | Dừng hệ thống và **xoá toàn bộ dữ liệu** (DB, mô hình đã tải) |

### Tuỳ chọn

- **Production**: đặt `APP_ENV=production` và điền đủ `SMTP_*` (bắt buộc để gửi OTP). Đổi `DB_PASSWORD`.
- **Chạy LLM trên máy (Ollama)** thay vì gọi API: đặt `AI_APP_ENV=development` trong `.env`, rồi:
  ```bash
  docker compose --profile ollama up -d --build
  docker compose exec ollama ollama pull qwen2.5:3b
  ```
- **VPS RAM thấp**: đặt `EMBED_BACKEND=onnx` (~0.9GB RAM) và `RERANK_BACKEND=api` (kèm `RERANK_API_KEY`) hoặc `RERANK_BACKEND=none`.
- **LinkedIn qua VPN**: đặt file WireGuard vào `./secrets/wg.conf`, rồi trong `.env` đặt `LINKEDIN_ENABLED=true` và `LINKEDIN_WG_CONFIG=./secrets/wg.conf`.

## Phát triển local (không Docker)

Xem [backend/README.md](backend/README.md) và [llm/docs/](llm/docs/).
