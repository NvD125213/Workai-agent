# Telegram bot: đăng ký và tìm việc theo CV

Người dùng làm mọi thứ ngay trong khung chat với bot, không cần mật khẩu, không mở trang web nào:

```
👤 /start
🤖 Chào mừng bạn! 👋  Tên bạn là gì?            [ Văn Đức ]  ← nút gợi ý từ tên Telegram
👤 Nguyễn Văn Đức
🤖 Chào Nguyễn Văn Đức! Bây giờ hãy nhập email để kích hoạt tài khoản sử dụng hệ thống nhé!
👤 duc@gmail.com
🤖 ✅ Kích hoạt tài khoản thành công! ...
   Tiếp theo, hãy gửi CV để hệ thống tìm kiếm job phù hợp cho bạn nhé.
👤 📎 CV_Nguyen_Van_Duc.pdf
🤖 📄 Đã nhận CV! Mình đang phân tích CV của bạn, mất khoảng 1-2 phút...
🤖 ✅ Đã phân tích xong CV của bạn:
   • Chức danh: Backend Developer  • Kinh nghiệm: 2 năm  • Kỹ năng: go, docker, postgresql
🤖 🌐 Đang cào job từ 5 trang tuyển dụng (TopCV, ITviec, JobsGO, Indeed, VietnamWorks)
   rồi chấm điểm độ phù hợp với CV của bạn.
   ⏳ Quá trình sẽ mất 10-15 phút, xin hãy chờ.
🤖 🧮 Đã cào xong, đang chấm điểm độ phù hợp của từng job với CV của bạn...   ← khi lượt tìm sang RANKING
🤖 🎯 Top 10 việc làm phù hợp nhất với bạn
   Từ 550 tin tuyển dụng, có 30 tin đạt yêu cầu.
   1. Golang Developer (link)          🏢 Công ty A · topcv
      💰 15 - 20 triệu · ⭐ 86.7/100      ✅ Khớp: go, docker
   ...
```

Gửi file không phải CV (tin tuyển dụng, tài liệu, ảnh...) thì bot trả lời lý do và dừng ngay, không phân tích.

## Kiến trúc

Bot chạy **bên trong `cmd/api`** (goroutine long polling `getUpdates`), không còn process `cmd/bot` riêng. Lý do: bot dùng chung `CVService` và `FindJobs` với API HTTP, nên:

- chung một giới hạn tốc độ cào cho mỗi trang tuyển dụng (giới hạn này chỉ có hiệu lực trong một process);
- API khởi động lại thì đánh dấu `FAILED` các lượt chạy dở, đúng với lượt của chính nó, không đè lên lượt của process khác;
- tắt server thì bot dừng trước, rồi mới tới phân tích CV và tìm việc.

```
 Telegram ──getUpdates──▶ cmd/api: TelegramBot.Run (1 goroutine, xử lý update tuần tự)
                           ├─ /start, tên, email ──▶ TelegramRegisterService ──▶ users
                           └─ file CV ──▶ goroutine riêng cho mỗi người (processCV)
                                 1. DownloadFile (getFile + tải, ≤ 10 MB)
                                 2. CVService.ParseCV ──▶ AI /parse-cv: đọc chữ + kiểm tra có phải CV
                                 3. CVService.StartAnalysis ──▶ AI /analyze-cv (LLM) + kiểm tra lại
                                    hỏi trạng thái mỗi 5 giây, tối đa 15 phút
                                 4. FindJobs.Start ──▶ cào 5 trang ──▶ AI /score
                                    hỏi trạng thái mỗi 20 giây, tối đa 90 phút; báo khi sang RANKING
                                 5. sendMessage (HTML): top 10 job đạt, xếp theo điểm
```

Các bước 2-4 **giống hệt** luồng HTTP (`/parse-cv` → `/analyze-cv` → `/find-job/{id}`): cùng service, cùng bảng `user_profiles`, `cv_analyses`, `job_searches`, `ai_matches`. Kết quả tìm việc qua Telegram cũng xem được bằng `GET /api/v1/find-job/{id}` nếu user đăng nhập trên app.

## Bước kiểm tra "có phải CV không"

Nằm ở dịch vụ AI, nên áp dụng cho **cả** Telegram và API HTTP. Có hai lớp:

| Lớp | Ở đâu | Cách làm | Khi loại |
|---|---|---|---|
| 1. Trước LLM | `POST /parse-cv`, [llm/matching/cv_check.py](../../llm/matching/cv_check.py) `check_cv` | Regex, tức thì. CV phải có **≥ 2 đề mục** quen thuộc (kinh nghiệm, học vấn, kỹ năng, mục tiêu, dự án, chứng chỉ, ngoại ngữ, thông tin cá nhân...) đứng thành dòng riêng **và** email hoặc số điện thoại; hoặc ≥ 4 đề mục nếu CV ẩn liên hệ. Có ≥ 3 cụm từ của tin tuyển dụng (mô tả công việc, quyền lợi, phúc lợi, hạn nộp hồ sơ...) nhiều hơn số đề mục CV thì coi là tin tuyển dụng. | 422, lý do tiếng Việt |
| 2. Sau LLM | `POST /analyze-cv`, `check_profile` | LLM không trích được chức danh, kinh nghiệm hay kỹ năng nào | 422, lượt phân tích `FAILED` với lý do đó |

Chỉ đếm **đề mục** (dòng ngắn bắt đầu bằng từ khoá) chứ không đếm từ trong câu, vì tài liệu hay tin tuyển dụng cũng nhắc "kinh nghiệm", "kỹ năng" trong câu văn. Dòng của bảng markdown, JSON, code (có `| " ` { } = < >`) không được tính là đề mục.

Đã thử: 2 CV mẫu (PDF tiếng Anh, DOCX tiếng Việt) được nhận với 6 đề mục; 40/40 tin tuyển dụng TopCV đã cào, tài liệu dự án, README và đoạn văn thường đều bị loại.

## Hiển thị kết quả

- Lấy tối đa **10 job đầu** của lượt tìm (đã xếp theo điểm; chỉ job đạt ngưỡng 60 mới được lưu).
- Mỗi job: tiêu đề có link tới tin gốc, công ty · nguồn, lương (nếu có) · điểm, tối đa 5 kỹ năng khớp.
- Gửi bằng `parse_mode=HTML`, tắt preview link. Mọi giá trị đều được `html.EscapeString`. Danh sách dài được tách thành nhiều tin, mỗi tin ≤ 3800 byte (giới hạn của Telegram là 4096 ký tự); không cắt giữa một job nên không làm hỏng thẻ HTML.
- Không có job nào đạt: bot báo đã quét bao nhiêu tin và gợi ý gửi lại CV sau.

## Quy tắc

- Chỉ người **đã đăng ký** mới gửi CV được; người chưa đăng ký được nhắc gõ `/start`.
- Nhận file `.pdf`, `.docx`, `.txt`, `.md` (`.doc` được dịch vụ AI giải thích cách chuyển), tối đa 10 MB. Ảnh chụp CV bị từ chối kèm hướng dẫn gửi file.
- Mỗi người chỉ xử lý **một CV tại một thời điểm** (khoá trong bộ nhớ của bot, và index unique của `cv_analyses`, `job_searches` ở DB). Gửi CV khác khi đang xử lý thì bot báo đang bận.
- Gửi CV mới thay CV hiện tại của user (giống `POST /parse-cv`).
- Lỗi dịch vụ AI, timeout, lỗi DB: bot báo người dùng gửi lại sau; chi tiết ghi log `telegram bot: cv of user <id>: <bước>: <lỗi>`.
- Server tắt giữa chừng: bot không nhắn gì thêm; lượt đang chạy bị đánh dấu `FAILED`, người dùng gửi lại CV.

## Đăng ký

| Bước | Hợp lệ | Khi sai |
|---|---|---|
| Tên | 1–100 ký tự sau khi trim | Hỏi lại |
| Email | Địa chỉ trần, có dấu chấm sau `@`, ≤ 150 byte; trim + chữ thường | Hỏi lại |
| Email đã có người dùng | | Yêu cầu email khác |
| Telegram đã đăng ký | | Báo đã đăng ký, kết thúc |

- `/cancel` huỷ đăng ký đang làm; bỏ dở quá 30 phút thì phiên bị huỷ. Trạng thái hội thoại nằm trong bộ nhớ, mất khi khởi động lại.
- `telegram_id` lấy từ người gửi trong update của Telegram, không do người dùng nhập.
- User đăng ký qua Telegram có `password_hash = ''` (không đăng nhập bằng mật khẩu được), `email_verified = false`, `users.telegram_id` unique.

## Các file liên quan

| File | Vai trò |
|---|---|
| [cmd/api/main.go](../cmd/api/main.go) | Tạo client Telegram (timeout 60 giây > thời gian chờ long polling), chạy `TelegramBot.Run` khi có `TELEGRAM_BOT_TOKEN` |
| [internal/jobs/telegram_bot.go](../internal/jobs/telegram_bot.go) | Vòng `getUpdates`, hội thoại đăng ký |
| [internal/jobs/telegram_cv.go](../internal/jobs/telegram_cv.go) | Nhận CV, chạy phân tích + tìm việc, chờ kết quả, hiển thị top 10 |
| [internal/adapter/telegram/bot.go](../internal/adapter/telegram/bot.go) | `GetUpdates`, `SendMessageTo`, `SendHTMLTo`, `DownloadFile`, reply keyboard |
| [internal/services/telegram_register_service.go](../internal/services/telegram_register_service.go) | Kiểm tra tên/email, tạo user |
| [llm/matching/cv_check.py](../../llm/matching/cv_check.py) | `check_cv`, `check_profile` |
| [llm/api.py](../../llm/api.py) | `/parse-cv`, `/analyze-cv` gọi hai hàm trên |

## Cấu hình và chạy

`.env` chỉ cần:

```env
TELEGRAM_BOT_TOKEN=<token từ @BotFather>
TELEGRAM_CHAT_ID=<chat nhận thông báo hệ thống>
```

Chạy đủ ba thành phần như luồng tìm việc thường (xem [docs/mo_ta_tien_do_project.md](../../docs/mo_ta_tien_do_project.md) mục 8):

```powershell
cd backend; docker compose up -d db; go run ./cmd/migrate
cd llm; .venv\Scripts\python.exe -m uvicorn api:app --host 127.0.0.1 --port 8001
cd backend; go run ./cmd/api        # log: "telegram bot: polling for messages"
```

Không cần tunnel hay HTTPS. **Chỉ chạy một bản API** với một token: hai process cùng gọi `getUpdates` thì Telegram trả 409 Conflict (cũng xảy ra khi bot đang có webhook; xoá bằng `https://api.telegram.org/bot<token>/deleteWebhook`).

`TelegramCVConfig` (trong `cmd/api/main.go`) đang để mặc định: `ExpFilter` tắt (giống mặc định của `POST /find-job`), hỏi trạng thái 5 giây / 20 giây, chờ tối đa 15 phút / 90 phút. Mỗi lượt cào `FIND_JOB_MAX_PAGES` = 3 trang mỗi nguồn.

## Test

```bash
cd backend; go test ./internal/jobs ./internal/services ./internal/adapter/telegram
cd llm; .venv\Scripts\python.exe tests\test_llm.py --offline
```

- `telegram_bot_test.go`: đăng ký trọn luồng; luồng CV trọn vẹn (thông báo từng bước, đúng top 10, escape HTML); file bị loại ở `/parse-cv` và sau LLM; người chưa đăng ký, sai định dạng, quá 10 MB, gửi ảnh, đang bận; tách tin nhắn dài.
- `test_llm.py`: `check_cv` nhận 2 CV mẫu, loại tin tuyển dụng, đoạn văn, bảng/JSON; nhận đề mục có số thứ tự, ký hiệu; `check_profile`.
