# notification-job-mb (Backend)

Backend Go cho hệ thống notification job: REST API (Gin) và background worker, theo kiến trúc phân lớp **Handler → Service → Repository**. Cơ sở dữ liệu PostgreSQL qua GORM.

## Yêu cầu

**Chạy bằng Docker (khuyến nghị):**

- [Docker](https://docs.docker.com/get-docker/) và Docker Compose

**Chạy local (không Docker):**

- [Go](https://go.dev/dl/) (phiên bản tương thích với `go.mod`)
- PostgreSQL đang chạy và có database (mặc định: `notification_job`)

## Cấu trúc thư mục

```
backend/
├── cmd/                    # Điểm khởi chạy các process
│   ├── api/                # HTTP API server
│   ├── worker/             # Background worker
│   ├── migrate/            # CLI chạy GORM AutoMigrate
│   ├── scrape-topcv/       # CLI cào job TopCV
│   ├── scrape-vietnamworks/ # CLI cào job VietnamWorks
│   ├── scrape-itviec/      # CLI cào job ITviec
│   ├── scrape-jobsgo/      # CLI cào job JobsGO
│   ├── scrape-careerviet/  # CLI cào job CareerViet
│   ├── scrape-joboko/      # CLI cào job Joboko
│   ├── scrape-vieclam24h/  # CLI lấy job Việc Làm 24h (API)
│   ├── scrape-123job/      # CLI cào job 123job
│   ├── scrape-careerlink/  # CLI cào job CareerLink
│   └── scrape-linkedin/    # CLI cào job LinkedIn (qua VPN WireGuard)
├── docs/                   # Swagger/OpenAPI docs (sinh bởi swag)
├── internal/               # Mã nguồn nội bộ (không import từ bên ngoài module)
│   ├── config/             # Đọc biến môi trường, cấu hình typed
│   ├── database/           # Kết nối PostgreSQL (GORM)
│   ├── models/             # Entity / model domain (GORM)
│   ├── repositories/       # Tầng truy cập dữ liệu
│   ├── services/           # Logic nghiệp vụ / use-case
│   ├── handlers/           # Adapter HTTP (request/response)
│   ├── routes/             # Đăng ký route Gin
│   ├── middlewares/        # Middleware Gin
│   └── jobs/               # Job chạy trên worker
├── migrations/             # SQL migration / schema (khi có)
├── Dockerfile              # Build image cho api / worker
├── docker-compose.yml      # PostgreSQL (mặc định) + api/worker (profile `full`)
├── .air.toml               # Hot-reload API bằng air
├── .air.worker.toml        # Hot-reload worker bằng air
├── .env.example            # Mẫu biến môi trường
├── go.mod / go.sum         # Dependency Go
└── README.md
```

### Chi tiết từng thư mục

| Đường dẫn | Chức năng |
| --- | --- |
| `cmd/api` | Entry point process HTTP. Load config, kết nối DB, đăng ký route, lắng nghe cổng (mặc định `:8080`). |
| `cmd/worker` | Entry point background worker. Load config, kết nối DB, chạy các job (hiện đang skeleton chờ triển khai). |
| `cmd/migrate` | CLI migration: `go run ./cmd/migrate` — cập nhật schema theo models, không cần rebuild Docker. |
| `cmd/scrape-topcv`, `cmd/scrape-vietnamworks`, `cmd/scrape-itviec`, `cmd/scrape-jobsgo` | CLI cào job từ TopCV / VietnamWorks / ITviec / JobsGO ra file JSON (xem [Cào job](#cào-job-topcv--vietnamworks--itviec--jobsgo)). |
| `internal/adapter/wgtunnel` | Tunnel WireGuard chạy trong process (wireguard-go + netstack), cho nguồn bị nhà mạng chặn (LinkedIn). |
| `internal/adapter/job-sources` | Adapter cào job theo từng nguồn (`topcv`, `vietnamworks`, `itviec`, `jobsgo`, `indeed`, `careerviet`, `joboko`, `vieclam24h`, `123job` (package `onetwothreejob`), `careerlink`, `linkedin`), `htmltext` (chuyển HTML mô tả job sang text, tạo slug từ khoá) và `browser` (HTTP client giả lập Chrome để qua Cloudflare). |
| `docs` | File Swagger sinh tự động (`docs.go`, `swagger.json`, `swagger.yaml`). |
| `internal/config` | Load `.env` / biến môi trường, trả về config có kiểu (`HTTP`, `DB`, `APP_ENV`). |
| `internal/database` | Mở / đóng kết nối PostgreSQL qua GORM. |
| `internal/models` | Định nghĩa entity domain map với bảng DB. |
| `internal/repositories` | Chỉ thao tác dữ liệu (CRUD / query), không chứa logic nghiệp vụ. |
| `internal/services` | Logic nghiệp vụ, gọi repository. |
| `internal/handlers` | Nhận HTTP request, gọi service, trả response JSON. |
| `internal/routes` | Gắn handler vào đường dẫn Gin (ví dụ `GET /health`). |
| `internal/middlewares` | Middleware chung (auth, logging bổ sung, v.v.). |
| `internal/jobs` | Cài đặt các job chạy nền cho worker. |
| `migrations` | File SQL / schema migration cho PostgreSQL. |

Luồng xử lý API điển hình:

```
Request → Middleware → Handler → Service → Repository → PostgreSQL
```

## Cấu hình môi trường

Từ thư mục `backend/`:

```bash
cp .env.example .env
```

Chỉnh `.env` cho đúng môi trường của bạn:

| Biến | Mô tả | Mặc định |
| --- | --- | --- |
| `APP_ENV` | Môi trường (`development` / `production`) | `development` |
| `HTTP_ADDR` | Địa chỉ lắng nghe API (local) | `:8080` |
| `HTTP_PORT` | Port map ra máy host (Docker) | `8080` |
| `DB_HOST` | Host PostgreSQL (`localhost` khi local; trong Compose sẽ ghi đè thành `db`) | `localhost` |
| `DB_PORT` | Port PostgreSQL trên máy host (Docker map ra ngoài; trong container vẫn là `5432`) | `5433` |
| `DB_USER` | User DB | `postgres` |
| `DB_PASSWORD` | Password DB | `postgres` |
| `DB_NAME` | Tên database | `notification_job` |
| `DB_SSLMODE` | SSL mode | `disable` |
| `SMTP_HOST` | SMTP server gửi email (OTP). Để trống khi dev = chỉ log email ra console; bắt buộc ở production | — |
| `SMTP_PORT` | `465` = TLS ngầm định, các cổng khác bắt buộc STARTTLS | `587` |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | Tài khoản SMTP. Giá trị có dấu cách phải đặt trong dấu nháy kép | — |
| `SMTP_FROM` | Địa chỉ gửi, ví dụ `Notify <no-reply@example.com>` | — |
| `AI_SERVICE_URL` | Dịch vụ AI nội bộ (`llm/api.py`) cho parse-cv, analyze-cv, find-job | `http://127.0.0.1:8001` |
| `FIND_JOB_MAX_PAGES` | Số trang kết quả tìm kiếm cào ở mỗi nguồn trong một lượt find-job | `3` |
| `FIND_JOB_MAX_CONCURRENT` | Số lượt find-job chạy cùng lúc (các lượt khác chờ ở `QUEUED`). Các lượt dùng chung giới hạn request của từng trang việc làm, nên tăng số này không làm tăng tải lên trang | `3` |
| `FIND_JOB_SCORE_TIMEOUT_MINUTES` | Thời gian tối đa (phút) chờ AI service chấm điểm một lượt find-job. Quá hạn thì lượt đó `FAILED` với lỗi bắt đầu bằng `score timeout`, bot Telegram báo riêng. Chấm điểm tăng theo số job cào được (bước chậm nhất là rerank, xem `RERANK_TOP_N` bên AI service) | `30` |

## Khởi động môi trường dev (air + Docker DB) — khuyến nghị

Chỉ PostgreSQL chạy trong Docker. API và worker chạy trực tiếp trên máy bằng [air](https://github.com/air-verse/air): mỗi khi lưu file `.go`, air tự build lại và khởi động lại process, không cần rebuild image Docker.

### Bước 1 — Cài đặt (chỉ làm một lần)

```bash
go install github.com/air-verse/air@latest
```

| Lệnh | Chức năng |
| --- | --- |
| `go install github.com/air-verse/air@latest` | Tải và cài công cụ `air` vào `$(go env GOPATH)/bin` (Windows: `%USERPROFILE%\go\bin`). Thư mục này phải có trong `PATH` để gọi được lệnh `air`. Kiểm tra bằng `air -v`. |

Ngoài ra cần cài sẵn [Go](https://go.dev/dl/) và Docker Desktop (đang chạy).

### Bước 2 — Chuẩn bị project (lần đầu sau khi clone)

```bash
cd backend
cp .env.example .env
go mod download
```

| Lệnh | Chức năng |
| --- | --- |
| `cd backend` | Vào thư mục backend — mọi lệnh bên dưới đều chạy từ đây. |
| `cp .env.example .env` | Tạo file cấu hình `.env` từ mẫu. Sau đó điền `JWT_ACCESS_SECRET` (bắt buộc, ≥ 32 byte) và giữ `DB_HOST=localhost`. `DB_PORT` là cổng PostgreSQL map ra máy host. |
| `go mod download` | Tải các thư viện Go khai báo trong `go.mod` về máy (không bắt buộc, `air` cũng tự tải khi build lần đầu). |

### Bước 3 — Khởi động hằng ngày

```bash
docker compose up -d          # 1. bật PostgreSQL
go run ./cmd/migrate          # 2. cập nhật schema DB
air                           # 3. chạy API (terminal 1)
air -c .air.worker.toml       # 4. chạy worker (terminal 2)
```

| Lệnh | Chức năng |
| --- | --- |
| `docker compose up -d` | Bật container PostgreSQL (`notification-job-db`) ở chế độ nền (`-d`). Chỉ có `db` được bật vì `api`/`worker` nằm trong profile `full`. Dữ liệu nằm trong volume nên không mất khi tắt container. |
| `go run ./cmd/migrate` | Tạo / cập nhật bảng theo `internal/models` (GORM AutoMigrate). Bắt buộc ở lần chạy đầu và mỗi khi sửa models; các lần khác có thể bỏ qua. |
| `air` | Đọc `.air.toml`: build `./cmd/api` ra `tmp/`, chạy API tại `http://localhost:8080`, rồi theo dõi file `.go` — sửa code là tự build và khởi động lại. |
| `air -c .air.worker.toml` | Tương tự cho worker (`./cmd/worker`), dùng file cấu hình riêng. Chạy ở terminal khác vì `air` chiếm terminal. |

Kiểm tra API đã lên:

| Lệnh | Chức năng |
| --- | --- |
| `curl http://localhost:8080/health` | Gọi endpoint health — trả về OK nếu API chạy và kết nối được DB. |
| Mở `http://localhost:8080/swagger/index.html` | Xem và thử API qua Swagger UI. |

### Bước 4 — Dừng

| Lệnh | Chức năng |
| --- | --- |
| `Ctrl+C` (trong terminal `air`) | Dừng API / worker và xoá binary tạm trong `tmp/`. |
| `docker compose stop` | Tạm dừng container DB, giữ nguyên container và dữ liệu (bật lại bằng `docker compose up -d`). |
| `docker compose down` | Dừng và xoá container DB, **giữ** dữ liệu trong volume. |
| `docker compose down -v` | Dừng, xoá container **và xoá vĩnh viễn dữ liệu** DB (dùng khi muốn reset DB về trống, sau đó phải migrate lại). |

### Lưu ý

- Sửa `.env` → `Ctrl+C` rồi chạy lại `air`, vì air chỉ theo dõi file `.go`, không đọc lại `.env`.
- Sửa `internal/models` → chạy `go run ./cmd/migrate` (air vẫn đang chạy cũng được), rồi lưu một file `.go` bất kỳ hoặc chờ air tự reload.
- Sau khi `swag init` sinh lại `docs/`, air tự build lại vì `docs/docs.go` thay đổi.
- Binary build ra `tmp/` (đã gitignore). Nếu build lỗi, air in lỗi ra terminal, ghi vào `build-errors-*.log` và dừng process cũ (`stop_on_error = true`) — sửa lỗi rồi lưu file là tự chạy lại.
- Lỗi `bind: address already in use` ở cổng 8080 → còn process API khác đang chạy (ví dụ container `api` từ profile `full`); dừng nó bằng `docker compose --profile full stop api`.
- Lỗi `connection refused` tới DB → Docker Desktop chưa bật hoặc container `db` chưa chạy; kiểm tra bằng `docker compose ps`.

## Cách chạy bằng Docker

Làm việc trong thư mục `backend/`:

```bash
cd backend
cp .env.example .env
```

`api` và `worker` nằm trong profile `full`, nên `docker compose up` không kèm `--profile full` chỉ bật PostgreSQL.

Build và chạy toàn bộ stack (PostgreSQL + API + Worker):

```bash
docker compose --profile full up --build
```

Chạy nền:

```bash
docker compose --profile full up --build -d
```

> Các lệnh `docker compose ... api worker` bên dưới gọi đích danh service nên tự kích hoạt profile; riêng `up`/`down`/`ps`/`logs` không chỉ định service thì cần thêm `--profile full`.

Các service:

| Service | Mô tả | Port (host) |
| --- | --- | --- |
| `db` | PostgreSQL 16 | `5433` (hoặc `DB_PORT`) |
| `api` | HTTP API | `8080` (hoặc `HTTP_PORT`) |
| `worker` | Background worker | — |

Kiểm tra health:

```bash
curl http://localhost:8080/health
```

Xem log:

```bash
docker compose logs -f api
docker compose logs -f worker
docker compose logs -f db
```

Dừng và xoá container (giữ volume DB):

```bash
docker compose down
```

Dừng và xoá cả dữ liệu PostgreSQL:

```bash
docker compose down -v
```

> Trong Docker, `DB_HOST` luôn được set thành `db` (tên service trong Compose), kể cả khi `.env` để `localhost`.

## Cập nhật khi có thay đổi (Docker)

Image `api` / `worker` chứa **binary đã build sẵn**, và container chỉ nhận các biến môi trường được khai báo trong `docker-compose.yml` tại thời điểm tạo container. Vì vậy:

> ⚠️ `docker compose restart` **không** nạp code mới và **không** nạp lại `.env`. Nó chỉ khởi động lại đúng container cũ. Hãy dùng các lệnh bên dưới.

### Chọn lệnh theo loại thay đổi

| Bạn vừa thay đổi | Lệnh cần chạy |
| --- | --- |
| Code Go (`cmd/`, `internal/`, `docs/`) | `docker compose up -d --build api worker` |
| Chỉ code của một service | `docker compose up -d --build --no-deps api` |
| `go.mod` / `go.sum` (thêm, nâng dependency) | `docker compose up -d --build api worker` (layer `go mod download` tự build lại) |
| `internal/models` (thêm bảng, cột) | Chạy migrate trước, rồi build lại (xem [bên dưới](#khi-thay-đổi-models)) |
| Giá trị trong `.env` (SMTP, JWT, …) | `docker compose --profile full up -d` (Compose tự tạo lại container có cấu hình thay đổi) |
| `docker-compose.yml` | `docker compose --profile full up -d` |
| `Dockerfile` | `docker compose build --no-cache api worker && docker compose --profile full up -d` |
| Muốn reset DB về trống | `docker compose --profile full down -v && docker compose up -d db`, rồi migrate lại |

> Biến mới trong `.env` chỉ vào được container nếu đã được khai báo trong mục `environment:` của service đó trong `docker-compose.yml`. Ví dụ `SMTP_*` đã được khai báo cho service `api`.

### Khi thay đổi models

Code mới có thể truy vấn bảng hoặc cột mới, nên cần migrate **trước** khi chạy code mới:

```bash
docker compose up -d db          # đảm bảo DB đang chạy
go run ./cmd/migrate             # cập nhật schema (đọc DB_PORT từ .env)
docker compose up -d --build api worker
```

### Cập nhật đầy đủ (sau khi `git pull`)

```bash
docker compose --profile full down       # dừng, giữ dữ liệu DB
docker compose up -d db
go run ./cmd/migrate
docker compose --profile full up -d --build
docker compose --profile full ps         # kiểm tra trạng thái
docker compose logs -f api               # theo dõi log khởi động
```

### Lệnh hữu ích khác

```bash
docker compose ps                        # trạng thái các service
docker compose logs --tail=100 -f api    # 100 dòng log cuối rồi theo dõi tiếp
docker compose stop api                  # dừng tạm (giữ container)
docker compose start api                 # chạy lại container đã dừng
docker compose exec db psql -U postgres -d notification_job   # mở psql trong DB
docker compose build --pull api worker   # build lại với base image mới nhất (vá bảo mật)
docker image prune -f                    # xoá image cũ không còn dùng sau nhiều lần build
```

| Lệnh | Container | Dữ liệu DB (volume) |
| --- | --- | --- |
| `docker compose stop` | Giữ | Giữ |
| `docker compose down` | Xoá | Giữ |
| `docker compose down -v` | Xoá | **Xoá vĩnh viễn** |

## Migration (CLI)

Khi sửa `internal/models`, chạy CLI — **không cần rebuild Docker**:

```bash
cd backend

# DB đang chạy (Docker chỉ cần service db)
docker compose up db -d

# Migrate theo models hiện tại
go run ./cmd/migrate
```

Hoặc build binary:

```bash
go build -o bin/migrate ./cmd/migrate
./bin/migrate
```

CLI đọc `.env` (`DB_HOST=localhost`, `DB_PORT=5433`, …) rồi chạy GORM AutoMigrate.

## Cách chạy local (không Docker)

Cần PostgreSQL sẵn (có thể chỉ bật DB bằng Docker: `docker compose up db -d`).

```bash
cd backend
cp .env.example .env
go mod download
go run ./cmd/migrate   # tạo / cập nhật bảng
```

Chạy API:

```bash
go run ./cmd/api
```

Chạy Worker (terminal khác):

```bash
go run ./cmd/worker
```

API lắng nghe tại `http://localhost:8080`. Kiểm tra: `curl http://localhost:8080/health`.

### Build binary (tuỳ chọn)

```bash
go build -o bin/api ./cmd/api
go build -o bin/worker ./cmd/worker
go build -o bin/migrate ./cmd/migrate

./bin/api
./bin/worker
```

## Cào job (TopCV / VietnamWorks / ITviec / JobsGO)

Các CLI dùng chung luồng: tìm theo từ khoá qua từng trang → loại trùng theo ID → cào chi tiết từng job (mô tả, yêu cầu, quyền lợi, lương, kỹ năng…) và ghi ra file JSON UTF-8. Chỉ lấy job ở **Hà Nội**. Không cần DB hay `.env`.

### VietnamWorks

Chạy từ thư mục `backend/`:

```bash
# Chỉ lấy danh sách job (3 trang, mỗi trang 50 job)
go run ./cmd/scrape-vietnamworks -keyword "frontend developer" -pages 3 -o jobs.json

# Lấy danh sách và cào chi tiết từng job
go run ./cmd/scrape-vietnamworks -keyword "golang" -pages 0 -o jobs.json -details jobs-description.json

# Cào chi tiết cho file jobs.json có sẵn từ lần chạy trước
go run ./cmd/scrape-vietnamworks -in jobs.json -details jobs-description.json
```

| Flag | Ý nghĩa | Mặc định |
| --- | --- | --- |
| `-keyword` | Từ khoá tìm, gửi nguyên văn (không cần slug) | `golang` |
| `-pages` | Số trang search tối đa (50 job/trang), `0` = tất cả | `1` |
| `-o` | File lưu danh sách job | stdout (nếu không có `-details`) |
| `-in` | Đọc danh sách job từ file thay vì search | — |
| `-details` | File lưu chi tiết job | — |
| `-limit` | Số chi tiết tối đa cào trong một lần chạy, `0` = không giới hạn | `0` |

Nguồn dữ liệu: API JSON `ms.vietnamworks.com` (`job-search/v1.0/search` cho danh sách, `api-gateway/v1.0/tracking/job-info` cho chi tiết). Chi tiết job là dữ liệu của API job-info, được ghép thêm skills, benefits, chuỗi lương và địa chỉ từ kết quả search.

### ITviec

Cùng các flag như trên, từ khoá được chuyển thành slug URL (`"frontend developer"` → `/it-jobs/frontend-developer/ha-noi`), 20 job/trang:

```bash
go run ./cmd/scrape-itviec -keyword "golang" -pages 0 -o jobs.json -details jobs-description.json
```

Nguồn dữ liệu: trang HTML `itviec.com/it-jobs/{từ khoá}/ha-noi` cho danh sách và `itviec.com/it-jobs/{slug}/content` cho chi tiết (gọi với `locale=vi` để nhãn ra tiếng Việt). ID của job là slug. ITviec ẩn lương khi chưa đăng nhập nên trường `salary` thường để trống.

### JobsGO

Cùng các flag như trên, từ khoá được chuyển thành slug URL (`"lập trình viên java"` → `/viec-lam-lap-trinh-vien-java-tai-ha-noi.html`), 50 job/trang:

```bash
go run ./cmd/scrape-jobsgo -keyword "java" -pages 0 -o jobs.json -details jobs-description.json
```

Nguồn dữ liệu: trang HTML `jobsgo.vn/viec-lam-{từ khoá}-tai-ha-noi.html` cho danh sách và `jobsgo.vn/viec-lam/{slug}.html` cho chi tiết (JSON-LD `JobPosting` + HTML). JobsGO nằm sau Cloudflare nên dùng chung HTTP client giả lập Chrome với TopCV.

### TopCV

Cùng các flag như trên, khác ở chỗ cào trang HTML (qua lớp vượt Cloudflare) và từ khoá được chuyển thành slug URL:

```bash
go run ./cmd/scrape-topcv -keyword "golang" -pages 0 -o jobs.json -details jobs-description.json
```

### CareerViet

Cùng các flag như trên, từ khoá được chuyển thành slug URL (`"lập trình viên"` → `/viec-lam/lap-trinh-vien-tai-ha-noi-kl4-vi.html`, trang N: `...-kl4-trang-N-vi.html`), 50 job/trang:

```bash
go run ./cmd/scrape-careerviet -keyword "developer" -pages 0 -o jobs.json -details jobs-description.json
```

Nguồn dữ liệu: trang HTML tìm kiếm cho danh sách (kèm hạn nộp, ngày cập nhật, ngành nghề) và trang chi tiết theo link của tiêu đề job (`/vi/tim-viec-lam/{slug}.{ID}.html`) cho mô tả, yêu cầu, phúc lợi, kinh nghiệm, cấp bậc. ID của job là mã hex cuối link (`35C8B46A`). Ô "Hết hạn nộp" trên trang được ưu tiên vì `validThrough` trong JSON-LD có khi lệch với ngày hiển thị; JSON-LD dùng để bù khi nhà tuyển dụng dùng giao diện riêng.

### Joboko

Cùng các flag như trên, thêm `-location` (mặc định `Hà Nội`), 10 job/trang:

```bash
go run ./cmd/scrape-joboko -keyword "golang" -pages 3 -o jobs.json -details jobs-description.json
go run ./cmd/scrape-joboko -keyword "kế toán" -location "TP.HCM" -pages 2 -o jobs.json
```

- Nguồn dữ liệu: `vn.joboko.com/jobs?q={từ khoá}&l={id địa điểm}&p={trang}` cho danh sách và link tiêu đề job (`/viec-lam-{slug}-xvi{ID}`) cho chi tiết (HTML + JSON-LD `JobPosting`). ID của job là số sau `xvi`.
- Địa điểm: tham số `l` chỉ nhận **id số** (slug bị bỏ qua). Bảng quy đổi nằm ở `internal/adapter/job-sources/joboko/location.json` (66 mục `{id, slug, name}` lấy từ ô "Địa điểm" của trang); `joboko.LookupLocation` tra theo tên hoặc slug, không phân biệt hoa thường/dấu và nhận cả cách viết tắt (`"Hà Nội"`, `"ha-noi"`, `"HN"` → 2; `"TP.HCM"`, `"Sài Gòn"` → 3).
- Joboko không hiện tổng số kết quả: còn trang sau khi có nút "Xem thêm việc làm"; quá trang cuối trang web trả lại trang 1, adapter coi là hết. Kết quả tìm khá lỏng (tìm "developer" có cả job kế toán), phần chấm điểm AI sẽ lọc.
- Tên công ty lấy từ JSON-LD: `.name-company` trên trang thuộc popup đồng ý dữ liệu cá nhân, ghi cùng một công ty ở mọi job.

### Việc Làm 24h

Dùng API JSON của trang, không cào HTML. Mỗi lần gọi trả tối đa 50 job kèm đủ dữ liệu, nên không có bước `-details`:

```bash
go run ./cmd/scrape-vieclam24h -keyword "video editor" -pages 2 -o jobs.json
go run ./cmd/scrape-vieclam24h -keyword "kế toán" -province "TP.HCM" -o jobs.json
```

- API: `apiv2.vieclam24h.vn/employer/fe/job/get-job-list?q=…&page=…&per_page=50&provinces=…`. API bắt buộc header `Authorization: Bearer <token>`; adapter dùng token khách mà JavaScript của trang tự gửi khi chưa đăng nhập (payload `{"channel_code":"vl24h","user":null}`). Token sai/hết hạn thì API trả HTTP 200 với `code: 1009` ("Mã bảo mật không còn hợp lệ") hoặc 401/403: adapter tự tải trang chủ vieclam24h.vn → file `_app-*.js` → lấy token khách hiện tại (`JWT_GUEST||"eyJ…"`) rồi gọi lại **một lần**; nhiều lượt tìm chạy song song chỉ tra một lần, và tra lại trang tối đa 5 phút/lần. Không lấy được token mới thì báo `ErrBlocked`; khi đó có thể truyền token bằng `-token`.
- Dữ liệu: tiêu đề, công ty, lương (`salary_min/max` VND → `"10 - 17 triệu"`), kinh nghiệm, cấp bậc, bằng cấp, hình thức, địa chỉ, hạn nộp (`resume_apply_expired`, 23:59:59 giờ VN) và **phần yêu cầu** (`other_requirement`). API danh sách **không có** "Mô tả công việc" và "Quyền lợi" (chỉ trang chi tiết có).
- Bảng mã: `internal/adapter/job-sources/vieclam24h/lookups.json` (lấy từ dữ liệu trang): tỉnh cả hai phiên bản (v2 `210000160` = Hà Nội sau sáp nhập 2025, v1 `110000073`/mã cũ `73`; API nhận cả ba), ngành (để dựng link job), kinh nghiệm, cấp bậc, bằng cấp, hình thức, giới tính.

### 123job

Cùng các flag như TopCV/ITviec, thêm `-city` (mặc định `Hà Nội`), 30 job/trang:

```bash
go run ./cmd/scrape-123job -keyword "developer" -pages 2 -o jobs.json -details jobs-description.json
go run ./cmd/scrape-123job -keyword "kế toán" -city "TP.HCM" -o jobs.json
```

- Nguồn dữ liệu: `123job.vn/tuyen-dung?q={từ khoá}&cityId={id}&page={trang}` cho danh sách, link tiêu đề (`/viec-lam/{slug}-{mã}`) cho chi tiết (HTML + JSON-LD `JobPosting`). ID của job là `data-job-id` số trên card. Adapter không lọc theo ngành (`cat=30` = IT) như các nguồn khác.
- Địa điểm: `cityId` lấy từ `internal/adapter/job-sources/123job/cities.json` (63 tỉnh, bảng `provinces` trong `locationData-*.js` của trang; Hà Nội = 1, Hồ Chí Minh = 63); `onetwothreejob.LookupCity` tra theo tên/slug, không phân biệt dấu, nhận cả viết tắt.
- Trang chỉ dùng class Tailwind, nên parser bám vào chỗ có nghĩa: nút lưu `data-job-id`, link `/viec-lam/`, `rel="next"`, các mục `<h2>` của trang chi tiết.
- 123job hiển thị kinh nghiệm dạng thô `"OccupationalExperienceRequirements, 24"` (số tháng); adapter đổi thành `"2 năm"` / `"6 tháng"`. Lương ẩn (`__`, "Đang cập nhật") để trống.

### CareerLink

Cùng các flag như TopCV/ITviec, thêm `-city` (mặc định `Hà Nội`; nhận tên, slug hoặc mã tỉnh), 50 job/trang:

```bash
go run ./cmd/scrape-careerlink -keyword "developer" -pages 2 -o jobs.json -details jobs-description.json
go run ./cmd/scrape-careerlink -keyword "kế toán" -city "TP.HCM" -o jobs.json
```

- Nguồn dữ liệu: `careerlink.vn/tim-viec-lam-tai/k/{từ-khoá}/{slug tỉnh}/{mã tỉnh}?page={trang}` cho danh sách (không chọn tỉnh thì `/vieclam/list?keyword=…`), link tiêu đề (`/tim-viec-lam/{slug}/{mã}`) cho chi tiết (HTML + JSON-LD `JobPosting`). ID của job là `data-job-id` trên card.
- Địa điểm: `internal/adapter/job-sources/careerlink/cities.json` (63 tỉnh: Hà Nội = `ha-noi/HN`, Hồ Chí Minh = `ho-chi-minh/HCM`, Đồng Nai = `DNI`, Huế = `hue/TTH`…). Trang không công bố bảng này; mỗi mã được xác nhận bằng cách gọi `/tim-viec-lam-tai/k/x/zz/{mã}`, CareerLink redirect về đúng slug của tỉnh.
- Trang là Next.js với class CSS module có hash (`JobIndex-module__HZLWJW__salary`) đổi theo mỗi lần build, nên parser bám vào `data-testid`, id các section (`section-job-description`, `section-job-benefits`, `section-job-skills`, `section-job-summary`) và phần đuôi cố định của class (`[class*='__salary']`).
- Hạn nộp lấy phần ngày của `validThrough` (`2026-12-12T23:59:59.000Z` là ngày cuối nhận hồ sơ, giờ ghi nhãn UTC nhưng là giờ VN).
- **Captcha:** gửi request dồn dập (thử ~100 request cách nhau 0,3 giây) thì CareerLink trả trang hCaptcha "Xác nhận bạn không phải robot" với **HTTP 200** (header `X-Middleware-Rewrite: /public/captcha…`) cho mọi trang trong một thời gian. Adapter coi đó là `ErrBlocked` và dừng; client mặc định giãn 4–7 giây/request và nghỉ 20 giây sau mỗi 15 trang chi tiết.

### LinkedIn

Cùng các flag như trên, thêm `-wg` (file `.conf` WireGuard; mặc định lấy `LINKEDIN_WG_CONFIG`). Nhà mạng Việt Nam chặn LinkedIn nên cần VPN:

```bash
go run ./cmd/scrape-linkedin -keyword "golang" -pages 3 -o jobs.json -details jobs-description.json -wg vpn.conf
```

- Nguồn dữ liệu: trang công khai cho khách chưa đăng nhập, `linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search` (10 job/lần, lọc Hà Nội `geoId=105790653`, đăng trong 30 ngày) và `linkedin.com/jobs-guest/jobs/api/jobPosting/{id}` cho chi tiết. ID là số của job. Không cần tài khoản.
- LinkedIn gộp cả tin vào một khối nên toàn bộ nằm trong `description`; không có kỹ năng, số năm kinh nghiệm (chỉ có cấp bậc `level`) hay hạn nộp.
- VPN: `-wg` / `LINKEDIN_WG_CONFIG` nhận file cấu hình định dạng wg-quick (`[Interface]` + `[Peer]`) của nhà cung cấp VPN hoặc server riêng. Tunnel chạy ngay trong process bằng wireguard-go + netstack: không cần cài WireGuard, không cần quyền admin, không đổi route của máy; **chỉ** request tới LinkedIn (cả DNS) đi qua VPN.
- Worker / API: LinkedIn mặc định **tắt**, bật bằng `LINKEDIN_ENABLED=true` (+ `LINKEDIN_WG_CONFIG`). Khi bật, LinkedIn được cào cùng các nguồn khác trong lịch cào và find-job.
- Bị chặn (status 999 / 403 hoặc chuyển tới trang đăng nhập) → lệnh dừng; chờ 30–60 phút hoặc đổi server VPN rồi chạy lại.

### Lưu ý

- File `-details` được lưu sau **mỗi** job, job đã có trong file sẽ bị bỏ qua → bị ngắt (`Ctrl+C`) hoặc bị chặn thì chạy lại **đúng lệnh cũ** để làm tiếp.
- Bị chặn (VietnamWorks trả 403, TopCV / ITviec / JobsGO hiện Cloudflare challenge) → lệnh dừng; chờ 30–60 phút rồi chạy lại.
- Mỗi request (trang tìm kiếm, mô tả job) cách nhau ngẫu nhiên 5–10 giây, và nghỉ thêm 15–20 giây sau mỗi 15–25 job để tránh bị chặn, nên cào nhiều job sẽ mất vài phút.
- Dùng `-o` / `-details` thay vì `>`: Windows PowerShell 5.1 đổi encoding khi chuyển hướng output, làm hỏng tiếng Việt.

## API hiện có

| Method | Path | Mô tả |
| --- | --- | --- |
| `GET` | `/health` | Kiểm tra sống / kết nối DB |
| `GET` | `/swagger/index.html` | Swagger UI (xem & thử API) |
| `POST` | `/api/v1/auth/forgot-password` | Quên mật khẩu — bước 1: gửi OTP 6 số qua email (luôn trả 202 để không lộ email đã đăng ký) |
| `POST` | `/api/v1/auth/verify-reset-code` | Bước 2: gửi `email` + `code`, nhận `reset_token` (dùng 1 lần, hết hạn sau 10 phút). OTP sai 5 lần sẽ bị huỷ |
| `POST` | `/api/v1/auth/reset-password` | Bước 3: gửi `reset_token` + `new_password`. Thành công sẽ đăng xuất mọi phiên (thu hồi refresh token) |

Giới hạn của luồng quên mật khẩu: mỗi lần gửi OTP cách nhau ≥ 60 giây, tối đa 5 OTP/giờ/tài khoản; OTP hết hạn sau 10 phút. Yêu cầu OTP mới sẽ huỷ cả OTP và `reset_token` cũ.

### Luồng tìm việc theo CV

Cần header `Authorization: Bearer <access_token>`. Phân tích CV và tìm việc đều **chạy nền**: API trả 202 kèm `id` ngay, client gọi `GET` để xem tiến độ.

| Method | Path | Mô tả |
| --- | --- | --- |
| `POST` | `/api/v1/parse-cv` | Tải CV (pdf, docx, txt, md; ≤ 10 MB), lưu làm CV hiện tại. Kết quả phân tích của CV cũ bị xoá |
| `POST` | `/api/v1/analyze-cv` | Tạo lượt phân tích CV hiện tại, trả **202** + `id` lượt phân tích. Mỗi người chỉ một lượt đang chạy (409 `analysis_in_progress`) |
| `GET` | `/api/v1/analyze-cv/{id}` | Trạng thái `QUEUED → ANALYZING → DONE / FAILED`; khi `DONE` có `result` (chức danh, số năm, kỹ năng, keyword...). `outdated: true` nếu đã tải CV mới sau lượt này |
| `POST` | `/api/v1/find-job/{id}?exp=true` | `{id}` là **id lượt phân tích** (phải `DONE`, vẫn là CV hiện tại). Cào 5 nguồn song song, lọc cứng, chấm điểm; trả **202** + `id` lượt tìm. Body tuỳ chọn: `desired_title`, `keyword`, `location` |
| `GET` | `/api/v1/find-job/{id}` | `{id}` là **id lượt tìm**. Trạng thái `QUEUED → CRAWLING → RANKING → DONE / FAILED`; khi `DONE` có `matches` xếp theo điểm |

Query `exp` của `POST /find-job/{id}` (mặc định `false`): `true` thì chỉ giữ job có số năm yêu cầu hợp với ứng viên (≤ 2 năm → job 0-2 năm, 3 năm → 2-3 năm, 4-5 năm → 2-5 năm, trên 5 năm → từ 3 năm) và xếp hạng thêm theo lương (nếu tin có ghi) rồi tới kinh nghiệm, lương quan trọng hơn: job 2 năm 20 triệu đứng trên job 3 năm 18 triệu.

Lỗi của `POST /find-job/{id}`: 404 `analysis_not_found` (id không có hoặc của người khác), 409 `analysis_not_done` (lượt phân tích chưa xong hoặc `FAILED`), 409 `analysis_outdated` (đã tải CV mới), 409 `search_in_progress` (đang có lượt tìm chưa xong).

Khi API khởi động lại, lượt phân tích và lượt tìm đang chạy dở được đánh dấu `FAILED` để người dùng chạy lại.

## Swagger docs

UI: [http://localhost:8080/swagger/index.html](http://localhost:8080/swagger/index.html)

Thêm annotation Swagger trên handler, rồi regenerate:

```bash
go install github.com/swaggo/swag/cmd/swag@latest
swag init -g cmd/api/main.go -o docs --parseInternal
```

> Không dùng `--parseDependency`: cờ này đổi tên toàn bộ schema (`handlers.X` → `internal_handlers.X`).

Sau khi sinh lại docs, cần build lại API thì Swagger UI mới cập nhật (docs được biên dịch vào binary):

```bash
docker compose up -d --build --no-deps api
```
