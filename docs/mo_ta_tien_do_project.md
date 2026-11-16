# Tiến độ dự án Notification Job

Cập nhật: 2026-10-30

Hệ thống gợi ý việc làm theo CV: người dùng tải CV lên, AI phân tích hồ sơ, hệ thống cào tin tuyển dụng từ 5 trang việc làm, lọc và chấm điểm độ phù hợp, rồi trả về danh sách việc nên ứng tuyển.

## 1. Tóm tắt

| Thành phần | Trạng thái | Ghi chú |
|---|---|---|
| Backend API (Go): đăng nhập, refresh token, quên mật khẩu | ✅ Hoàn thành | Có test API và integration |
| Cào job từ 5 trang (TopCV, ITviec, JobsGO, Indeed, VietnamWorks) | ✅ Hoàn thành | 5 trang cào song song; trong mỗi trang request cách nhau 2-4 giây chống chặn |
| Dịch vụ AI (Python): đọc CV, phân tích hồ sơ, chấm điểm | ✅ Hoàn thành | FastAPI nội bộ, chỉ backend gọi |
| Luồng tìm việc theo CV (tải CV → phân tích → tìm việc → kết quả) | ✅ Hoàn thành, đã chạy thử với dữ liệu thật | Phân tích CV và tìm việc đều chạy nền; tìm theo id lượt phân tích; chỉ lưu job đạt |
| Bộ lọc cứng (kinh nghiệm, kỹ năng bắt buộc) + phạt điểm | ✅ Hoàn thành | Trên cả Python và SQL |
| Bộ lọc khoảng kinh nghiệm + xếp hạng theo lương (`exp=true`) | ✅ Hoàn thành, chưa chạy thử với lượt cào thật | Mới có test offline |
| Báo cáo markdown cho mỗi lượt tìm | ✅ Hoàn thành | `llm/data/reports/find_job_<id>.md` |
| Worker cào theo lịch + nhập job | ⚠️ Dở dang | Thiếu bước AI phân tích file `data/raw/` → `data/inbox/` |
| Bot Telegram: đăng ký + gửi CV → top 10 job | ✅ Hoàn thành, chưa chạy thử trọn luồng với Telegram thật | Đăng ký hỏi tên, email ngay trong chat; CV đi đúng luồng parse → phân tích → tìm việc. Xem [backend/docs/telegram-register-flow.md](../backend/docs/telegram-register-flow.md) |
| Loại file không phải CV | ✅ Hoàn thành | `/parse-cv` (regex, tức thì) và `/analyze-cv` (sau LLM) trả 422 |
| Đăng ký tài khoản qua API | ❌ Chưa làm | Mới có đăng ký qua Telegram (không mật khẩu) |
| Gửi thông báo khi có job mới (push / Telegram) | ❌ Chưa làm | Có bảng `notifications`; bot đã gửi được tin cho người dùng |
| Ứng dụng di động | ❌ Chưa bắt đầu | Thư mục `mobile-app/` trống |

## 2. Kiến trúc

```
                  ┌──────────────── Backend Go (cmd/api, cổng 8080) ────────────────┐
 Client  ──JWT──► │ /api/v1/auth/*      /parse-cv   /analyze-cv   /find-job/:id      │
 (mobile)         │                                     │             │ chạy nền     │
                  │                                     │             ▼              │
                  │                                     │   CrawlAllSources (5 trang)│──► TopCV, ITviec, JobsGO,
                  │                                     │             │              │    Indeed, VietnamWorks
                  └─────────────────────────────────────┼─────────────┼──────────────┘
                                 HTTP nội bộ            ▼             ▼
                  ┌──────── Dịch vụ AI Python (llm/api.py, 127.0.0.1:8001) ────────┐
                  │ /parse-cv: PDF/DOCX → văn bản                                   │
                  │ /analyze-cv: Ollama qwen2.5:3b → hồ sơ; bge-m3 → embedding      │
                  │ /score: bộ lọc cứng → bge-reranker-v2-m3 + điểm luật → báo cáo  │
                  └─────────────────────────────────────────────────────────────────┘
                                                        │
                                    PostgreSQL 16 + pgvector 0.8 (Docker)
```

- **Backend Go** (Gin, GORM): API public, xác thực JWT, lưu dữ liệu, cào job, điều phối lượt tìm.
- **Dịch vụ AI Python** (FastAPI): mọi phần dùng mô hình. Chỉ lắng nghe trên `127.0.0.1`, client không gọi trực tiếp.
- **Mô hình chạy cục bộ, không gọi API trả phí:** Ollama `qwen2.5:3b` (phân tích CV), `BAAI/bge-m3` (embedding 1024 chiều), `BAAI/bge-reranker-v2-m3` (rerank). Mô hình Hugging Face đọc từ cache `D:\llm_setup\huggingface`, không tải lại.

## 3. Luồng tìm việc theo CV (đã tích hợp)

Tất cả API cần header `Authorization: Bearer <access_token>`.

| Bước | API | Việc thực hiện | Thời gian |
|---|---|---|---|
| 1 | `POST /api/v1/parse-cv` | Nhận file CV (pdf, docx, txt, md; ≤ 10 MB), đọc chữ, **loại file không phải CV** (422), lưu làm CV hiện tại. Kết quả phân tích của CV cũ bị xoá. | vài giây |
| 2 | `POST /api/v1/analyze-cv` | Trả **202** kèm `id` lượt phân tích ngay. Ở nền: LLM trích chức danh, số năm kinh nghiệm, kỹ năng (kèm cấp độ), ngoại ngữ, địa điểm; tạo embedding hồ sơ và keyword nghĩa rộng để cào ("Full-Stack Developer" → "Developer", "Marketing Executive" → "Marketing"). | ~2 phút |
| 3 | `GET /api/v1/analyze-cv/{id}` | Trạng thái `QUEUED → ANALYZING → DONE / FAILED`; khi `DONE` có `result` (hồ sơ đã phân tích). `outdated: true` nếu đã tải CV mới sau lượt này. | tức thì |
| 4 | `POST /api/v1/find-job/{id lượt phân tích}?exp=true` | Lượt phân tích phải `DONE` và vẫn là CV hiện tại. Trả **202** kèm `id` lượt tìm ngay. Ở nền: cào 5 trang song song (1 trang kết quả mỗi trang) → chấm điểm trong bộ nhớ → **chỉ lưu job đạt ngưỡng** vào `jobs` và `ai_matches`. | 10-12 phút |
| 5 | `GET /api/v1/find-job/{id lượt tìm}` | Trạng thái `QUEUED → CRAWLING → RANKING → DONE / FAILED`; khi `DONE` có danh sách job xếp theo điểm và bản tóm tắt. | tức thì |

Các quy tắc của luồng:
- **Phân tích CV chạy nền,** client không phải giữ request 1-2 phút. Mỗi lượt phân tích có `id` riêng (bảng `cv_analyses`); lượt tìm việc ghi lại `analysis_id` để biết đã tìm cho CV nào.
- **Mỗi người dùng chỉ có một lượt phân tích và một lượt tìm đang chạy.** DB đảm bảo bằng index unique, nên hai request đồng thời cũng không lọt. Lượt thứ hai nhận 409.
- **Tối đa 3 lượt tìm chạy cùng lúc** (chỉnh bằng `FIND_JOB_MAX_CONCURRENT`), các lượt khác chờ ở `QUEUED`. Các lượt dùng chung giới hạn request của từng trang việc làm, nên chạy nhiều lượt không làm tăng tải lên trang; lợi ích là lượt này cào trong lúc lượt khác đang chấm điểm.
- **API khởi động lại thì lượt phân tích và lượt tìm đang chạy dở được đánh dấu `FAILED`,** để người dùng chạy lại được.
- **Một nguồn lỗi hay bị chặn không làm hỏng lượt tìm.** Lượt vẫn `DONE` với kết quả từ các nguồn còn lại, lỗi ghi trong `error` và `summary.sources`.
- **Job bị loại không lưu vào DB.** Dấu vết của chúng nằm trong `job_searches.summary` (số job theo từng bước và từng nguồn, các lý do loại phổ biến) và trong báo cáo markdown.

## 4. Chấm điểm độ phù hợp

### 4.1. Bộ lọc cứng (chạy trước, chỉ dùng regex nên rất nhanh)

Job trượt bộ lọc bị loại, không vào bước rerank. Bước rerank tốn thời gian nhất (khoảng 1 giây mỗi job trên CPU).

- **Kinh nghiệm:** số năm yêu cầu là giá trị lớn hơn giữa số năm tin ghi và mức sàn theo cấp bậc trong tiêu đề (Middle 2 năm, Senior 3, Lead 4). Ứng viên có ít hơn (yêu cầu − 1) năm thì bị loại.
- **Kỹ năng bắt buộc:** công nghệ ghi trong tiêu đề. Ví dụ: job "Full Stack Developer (.Net)" bắt buộc .NET hoặc C#; "Java/Go" chỉ cần một trong hai. Bộ lọc nhận cả công nghệ ngoài ngành phần mềm như CNC, PLC, SAP, Magento.
- **Khoảng kinh nghiệm (chỉ khi `exp=true`):** loại cả job yêu cầu **quá ít** năm so với ứng viên, ví dụ job thực tập sinh với ứng viên 5 năm. Số năm ứng viên được làm tròn:

  | Ứng viên | Giữ job yêu cầu |
  |---|---|
  | ≤ 2 năm | 0-2 năm |
  | 3 năm | 2-3 năm |
  | 4-5 năm | 2-5 năm |
  | trên 5 năm | từ 3 năm tới số năm của ứng viên |

  Job không rõ số năm yêu cầu thì giữ lại. Cấu hình ở `EXP_BANDS`, `EXP_BAND_SENIOR_MIN` trong `llm/matching/config.py`.

### 4.2. Điểm mềm (0-100, đạt khi ≥ 60)

| Thành phần | Trọng số |
|---|---|
| Kỹ năng (techstack cốt lõi nặng gấp 3 công cụ phụ) | 0.35 |
| Kinh nghiệm | 0.20 |
| Rerank (cross-encoder đọc cặp hồ sơ - job) | 0.20 |
| Chức danh mong muốn so với tiêu đề job | 0.10 |
| Ngoại ngữ, địa điểm | 0.10 |
| Cosine embedding hồ sơ - job | 0.05 |

Khi `exp=true` có thêm hai thành phần (cộng vào các trọng số trên, `combine` chia lại theo tổng):

| Thành phần | Trọng số |
|---|---|
| Lương: thứ hạng phần trăm của lương job so với các job khác cùng lượt; tin không ghi lương ("Thoả thuận") thì bỏ qua | 0.20 |
| `exp_fit`: trong khoảng kinh nghiệm phù hợp, job yêu cầu ít năm hơn được điểm cao hơn | 0.10 |

Lương nặng hơn kinh nghiệm: job 2 năm 20 triệu xếp trên job 3 năm 18 triệu, còn job 3 năm 21 triệu xếp trên job 2 năm 17 triệu.
Lương đọc từ ô "Mức lương" của tin: "10 - 15 triệu" → 12.5, "Tới 40 triệu" → 40, "1,000 - 2,000 USD" → 37.5 (1 USD ≈ 25.000 đ), "240 triệu/năm" → 20.
Hai thành phần này nằm trong điểm tổng, nên job lương thấp có thể mất tới khoảng 15 điểm và rơi dưới ngưỡng 60.
Trọng số chỉnh ở `EXP_FILTER_WEIGHTS` trong `llm/matching/config.py`.

**Hệ số phạt nhân vào tổng điểm:** thiếu kỹ năng trọng tâm của JD (được nhắc ≥ 3 lần) trừ 25%; thiếu kỹ năng cốt lõi khác trừ 10%; hệ số không thấp hơn 0.3.

**Vì sao cần hai tầng:** trước khi có bộ lọc cứng, ứng viên Junior không biết .NET vẫn đứng thứ 2 cho job ".NET Fullstack", vì điểm tương đồng văn bản (rerank, title) rất cao. Sau khi sửa, với CV mẫu, bộ lọc loại 42/50 job TopCV, trong đó có job .NET và mọi job Senior, Middle, Lead.

### 4.3. Lọc trên database

`JobRepository.FindCandidateJobs` lấy job gần hồ sơ nhất theo pgvector, có điều kiện kinh nghiệm ngay trong SQL. Tham số `hnsw.iterative_scan` giúp kết quả không bị hụt. Đo trên 5000 job giả: không bật thì chỉ trả 5/50 job, bật thì trả đủ 50/50, khoảng 30 ms. Chi tiết trong [backend/docs/database.md](../backend/docs/database.md).

Hàm này chưa có nơi gọi; nó dành cho bước gợi ý từ kho job đã phân tích sẵn (mục 7).

## 5. Cào job

- **Nguồn:** TopCV, ITviec, JobsGO, Indeed, VietnamWorks. Mỗi nguồn có lệnh riêng `go run ./cmd/scrape-<nguồn>` để cào thủ công, và hàm `CrawlAllSources(keyword, maxPages)` để cào cả 5 cùng lúc.
- **Chống bị chặn:**
  - Mỗi trang chỉ có **1 request chạy tại một thời điểm**. Khoảng nghỉ ngẫu nhiên tính từ lúc request trước kết thúc.
  - Tất cả lượt cào trong cùng một process dùng chung một client cho mỗi trang, nên xếp hàng chung một giới hạn.
  - 5 trang được cào **song song**; khoảng nghỉ dưới đây là giữa hai request trong **cùng một trang**.
  - Mọi trang: mỗi request (trang tìm kiếm, mô tả job) cách nhau ngẫu nhiên **2-4 giây**, nghỉ thêm **8-11 giây** sau mỗi lô job (TopCV, Indeed 15 job; ITviec, JobsGO 20 job; VietnamWorks 25 job).
  - Gặp 429 hay 5xx thì thử lại với thời gian chờ tăng dần. Gặp trang thử thách Cloudflare thì dừng nguồn đó ngay.

## 6. Kết quả chạy thử thực tế

Dữ liệu: CV mẫu `CV_Duong_Tuan_Khang.pdf` (Full-Stack Developer, 0.5 năm kinh nghiệm), keyword cào "Developer".

| Chỉ số | Lượt tìm #3 |
|---|---|
| Job đã cào | 184 (topcv 49, jobsgo 50, vietnamworks 50, itviec 20, indeed 15) |
| Bị bộ lọc cứng loại | 138 (112 vì không đủ kinh nghiệm, còn lại thiếu .NET, PLC, C#, CNC...) |
| Được chấm điểm | 46 |
| Đạt và được lưu | **12 job**, cả 12 đều có embedding và dữ liệu phân tích |
| Top 3 | Remote-Fullstack Developer (TypeScript, Node.js, NestJS) 86.7 · Thực Tập Sinh AI/Lập Trình Viên Full-Stack 81.5 · Fullstack Java Developer 80.5 |
| Thời gian | Khoảng 6 phút, đo trước khi tăng khoảng nghỉ chống chặn. Ước tính hiện nay 10-12 phút. |

TopCV đã chặn (429) giữa chừng ở lần chạy này. Khoảng nghỉ ở mục 5 đã chỉnh lại hai lần (nay 2-4 giây mỗi request, nghỉ lô 8-11 giây; ban đầu nghỉ lô 40-90 giây); mức mới **chưa được kiểm chứng bằng một lượt cào thật**.

Lượt tìm #4 (CV Content Creator 4.7 năm, keyword "Content", 165 job) xếp job "Thực Tập Sinh Content Creator" 99.7 điểm. Đây là lý do thêm bộ lọc `exp=true` ở mục 4.

**Kiểm thử tự động:**

| Bộ test | Kết quả |
|---|---|
| Python `test_matching.py --offline` | 14/14 (thêm test đọc lương, khoảng kinh nghiệm, xếp hạng `exp`) |
| Python `test_llm.py --offline` | 19/19 |
| Go unit test (adapter, jobs, services, mailer) và test API | Đạt (thêm test kiểm tra lượt phân tích khi tìm việc) |
| Go integration (PostgreSQL thật, gồm `FindCandidateJobs`) | 11/11 |

## 7. Việc còn lại

Theo thứ tự đề xuất:

1. **Nối lại luồng worker.** `AutoCrawl` ghi job thô vào `data/raw/`, `JobImport` đọc `data/inbox/`, nhưng chưa có bước AI phân tích ở giữa. Nên dùng lại `/score` hoặc thêm `/analyze-jobs` để worker gọi trực tiếp, thay vì truyền file.
2. **Gợi ý từ kho job có sẵn.** Khi kho job đã được phân tích (mục 1), dùng `FindCandidateJobs` để gợi ý ngay trong vài giây thay vì cào mới 10 phút; chỉ cào khi kho thiếu job phù hợp.
3. **Đăng ký tài khoản** (`POST /api/v1/auth/register`, xác minh email).
4. **Gửi thông báo** khi có job mới phù hợp. Đã có bảng `notifications` và cột `users.fcm_token`; bot Telegram (chạy trong `cmd/api`) đã nhắn được cho người dùng đăng ký qua bot.
5. **Ứng dụng di động** (`mobile-app/` chưa có mã).
6. **Viết test tự động cho luồng find-job** (handler, phân tích chạy nền). Hiện mới có unit test cho `FindJobs.Start`.
7. **Chạy thử lượt tìm thật với `exp=true`** để chỉnh trọng số lương / `exp_fit` và khoảng kinh nghiệm theo kết quả thực tế.

### Hạn chế đã biết

- **Giới hạn tốc độ cào chỉ áp dụng trong từng process.** Worker và API chạy cùng lúc thì một trang vẫn có thể nhận request từ cả hai. Cách khắc phục: chỉ cho một process cào, hoặc dùng khoá chung qua PostgreSQL.
- **`user_profiles.exp_years` kiểu int không phân biệt "0 năm" với "không rõ".** Luồng find-job không bị ảnh hưởng, vì nó dùng số năm dạng thập phân trong `user_profiles.analysis`.
- **Kỹ năng của job vừa cào lấy từ tag của trang tuyển dụng** (thiếu thì dò từ điển trong mô tả), không qua LLM, để chấm điểm được nhanh.
- **Bản đầu tiên chưa nạp sẵn mô hình:** sau khi bật dịch vụ AI, lần chấm điểm đầu mất thêm 30-80 giây.

## 8. Cách chạy

```powershell
# 1. Database
cd backend; docker compose up -d db; go run ./cmd/migrate

# 2. Dịch vụ AI (cần Ollama đang chạy với qwen2.5:3b)
cd llm; .venv\Scripts\python.exe -m uvicorn api:app --host 127.0.0.1 --port 8001

# 3. Backend API
cd backend; go run ./cmd/api        # Swagger: http://localhost:8080/swagger/index.html

# Kiểm thử
cd llm; .venv\Scripts\python.exe tests\test_matching.py
cd backend; set -a; . ./.env; set +a; TEST_DB_NAME=nj_test go test ./tests/integration/
```

Cấu hình liên quan (trong `backend/.env`): `AI_SERVICE_URL` (mặc định `http://127.0.0.1:8001`), `FIND_JOB_MAX_PAGES` (mặc định 3), `FIND_JOB_MAX_CONCURRENT` (mặc định 3).

## 9. Kiểm thử tay toàn bộ luồng bằng Postman

Thứ tự gọi: **đăng nhập → tải CV lên → phân tích CV (chạy nền) → chờ phân tích `DONE` → tìm việc theo id phân tích (cào + chấm điểm, chạy nền) → xem kết quả**.
Phải phân tích CV trước khi tìm việc, vì bước tìm việc lấy keyword cào và hồ sơ để chấm điểm từ kết quả phân tích.

### 9.1. Chuẩn bị

1. Bật đủ ba dịch vụ như mục 8: database, dịch vụ AI (cổng 8001), backend API (cổng 8080).
   Kiểm tra nhanh:
   - `GET http://localhost:8080/health` trả 200.
   - `GET http://127.0.0.1:8001/health` trả `status: ok`. Trường `app_env` và `llm` cho biết dịch vụ AI đang
     chạy ở development (Ollama) hay production (API), xem [llm/docs/development_vs_production.md](../llm/docs/development_vs_production.md).
2. **Tạo tài khoản test** (chưa có API đăng ký). Lệnh hỏi mật khẩu; để trống thì tự sinh và in ra:
   ```powershell
   cd backend; go run ./cmd/keygen generate-user -email test@example.com -name "Test User"
   ```
3. **Tạo Environment trong Postman** (ví dụ tên `notification-job-local`) với các biến:

   | Biến | Giá trị ban đầu |
   |---|---|
   | `base_url` | `http://localhost:8080/api/v1` |
   | `email` | `test@example.com` |
   | `password` | mật khẩu ở bước 2 |
   | `access_token`, `refresh_token`, `search_id` | để trống, các script bên dưới tự điền |

4. Ở cấp **Collection**, tab **Authorization**: chọn `Bearer Token`, Token = `{{access_token}}`.
   Các request bên dưới để Authorization là `Inherit auth from parent`, trừ request đăng nhập và làm mới token.
5. CV mẫu có sẵn: `llm/data/cv/CV_Duong_Tuan_Khang.pdf` (IT) và `llm/data/cv/CV_Nguyen_Thi_My_Chau.docx` (marketing).

### 9.2. Các bước

**Bước 1. Đăng nhập** (không cần token)

```
POST {{base_url}}/auth/login
Body > raw > JSON:
{ "email": "{{email}}", "password": "{{password}}" }
```

Tab **Scripts > Post-response** (bản Postman cũ: tab **Tests**), để lưu token:

```js
const r = pm.response.json();
pm.environment.set("access_token", r.access_token);
pm.environment.set("refresh_token", r.refresh_token);
```

Kết quả mong đợi: 200, có `access_token` (hết hạn sau 15 phút, `expires_in: 900`) và `refresh_token` (7 ngày).

**Bước 2. Tải CV lên**

```
POST {{base_url}}/parse-cv
Body > form-data: key "file", đổi kiểu từ Text sang File, chọn file CV (pdf, docx, txt, md; tối đa 10 MB)
```

Kết quả mong đợi: 200, mất vài giây.

```json
{ "profile_id": 1, "chars": 3805, "text": "DƯƠNG TUẤN KHANG\nFull-Stack Developer..." }
```

Kiểm tra `text` có đúng nội dung CV không. CV là ảnh scan sẽ nhận 422 `cv_rejected`.
Tải CV mới sẽ xoá kết quả phân tích của CV cũ.

**Bước 3. Phân tích CV (chạy nền)**

```
POST {{base_url}}/analyze-cv
Không có body
```

Kết quả mong đợi: **202** trả về ngay, `status: "QUEUED"`, kèm `id` lượt phân tích. Script **Post-response**:

```js
pm.environment.set("analysis_id", pm.response.json().id);
```

Theo dõi bằng `GET {{base_url}}/analyze-cv/{{analysis_id}}` (khoảng 1-2 phút với Ollama, 10-30 giây với API).
`status` đi qua `QUEUED → ANALYZING → DONE / FAILED`. Khi `DONE`, `result` có dạng:

```json
{
  "id": 3, "profile_id": 1, "status": "DONE",
  "result": {
    "current_title": "Full-Stack Developer",
    "exp_years": 1,
    "location": "Hà Nội",
    "summary": "...",
    "search_keyword": "Developer",
    "skills": [{ "name": "react", "level": "JUNIOR" }, ...],
    "parsed_at": "...",
    "profile": { ... }
  }
}
```

Cần kiểm tra:
- `current_title`, `skills`, số năm kinh nghiệm có khớp CV không. Số năm dạng thập phân nằm trong `result.profile.years_experience`.
- `search_keyword` là keyword nghĩa rộng sẽ dùng để cào ("Full-Stack Developer" → "Developer").
- `summary` viết bằng tiếng Việt.
- `outdated: true` nghĩa là đã tải CV mới sau lượt này, cần phân tích lại.

**Bước 4. Tìm việc**

```
POST {{base_url}}/find-job/{{analysis_id}}?exp=true
Body > raw > JSON (tuỳ chọn, gửi {} hoặc bỏ trống thì lấy từ CV):
{ "desired_title": "Fullstack Developer", "keyword": "Developer", "location": "Hà Nội" }
```

- `desired_title`: chức danh để chấm độ khớp tiêu đề; mặc định là `current_title` của CV.
- `keyword`: keyword cào; mặc định là `search_keyword` của CV. Muốn test nhanh thì dùng keyword hẹp (ví dụ "ReactJS").
- `location`: địa điểm mong muốn, dùng để chấm điểm.
- `{{analysis_id}}`: id lượt phân tích ở bước 3, phải ở trạng thái `DONE`.
- `exp` (query, mặc định `false`): `true` thì chỉ giữ job có số năm yêu cầu hợp với ứng viên (≤ 2 năm → job 0-2 năm,
  3 năm → 2-3 năm, 4-5 năm → 2-5 năm, trên 5 năm → từ 3 năm) và xếp hạng thêm theo lương (nếu tin có ghi) rồi kinh nghiệm:
  job 2 năm 20 triệu đứng trên job 3 năm 18 triệu.

Script **Post-response** để lưu id lượt tìm:

```js
pm.environment.set("search_id", pm.response.json().id);
```

Kết quả mong đợi: **202** trả về ngay, `status: "QUEUED"`. Lượt tìm chạy nền khoảng 10-12 phút.

**Bước 5. Xem tiến độ và kết quả**

```
GET {{base_url}}/find-job/{{search_id}}
```

Gọi lại vài phút một lần. `status` đi qua các bước:

| status | Ý nghĩa |
|---|---|
| `QUEUED` | Đang chờ (đã đủ `FIND_JOB_MAX_CONCURRENT` lượt tìm khác đang chạy) |
| `CRAWLING` | Đang cào 5 trang việc làm (phần lâu nhất) |
| `RANKING` | Dịch vụ AI đang lọc cứng và chấm điểm |
| `DONE` | Xong, có `matches` và `summary` |
| `FAILED` | Lỗi, xem `error` |

Khi `DONE`, kết quả có dạng:

```json
{
  "id": 7, "analysis_id": 3, "exp_filter": true,
  "status": "DONE", "keyword": "Developer", "desired_title": "Full-Stack Developer",
  "jobs_found": 184, "matches_found": 12,
  "summary": {
    "crawled": 184, "rejected": 138, "scored": 46, "passed": 12,
    "sources": [{ "source": "topcv", "crawled": 49, "passed": 4 }, ...],
    "reject_reasons": [{ "reason": "không đủ kinh nghiệm", "count": 112 }, ...],
    "report": "...\\llm\\data\\reports\\find_job_7.md"
  },
  "matches": [
    {
      "job_id": 42, "rank": 1, "score": 86.7, "passed": true,
      "title": "Remote-Fullstack Developer", "company": "...", "url": "https://...", "source": "topcv",
      "salary": "...",
      "details": {
        "penalty": 1,
        "components": { "skills": 0.89, "rerank": 0.91, "title": 0.8, "experience": 1, "semantic": 0.6, "extra": 1,
                        "salary": 0.75, "exp_fit": 1 },
        "matched_skills": ["react", "node.js"], "missing_skills": ["docker"]
      }
    }
  ]
}
```

Cần kiểm tra:
- `summary.crawled = rejected + scored`, và `passed = matches_found` = số phần tử trong `matches`.
- `matches` xếp theo `score` giảm dần, mọi `score` ≥ 60.
- `details.components` cho biết điểm từng phần; `rerank` là `null` khi dịch vụ AI tắt reranker hoặc API rerank lỗi. `salary`, `exp_fit` chỉ có khi `exp=true`; `salary` là `null` khi tin không ghi lương.
- Khi `exp=true`: không còn job yêu cầu số năm ngoài khoảng phù hợp (ví dụ job thực tập với CV 5 năm); `summary.reject_reasons` có mục "ngoài khoảng kinh nghiệm phù hợp".
- `summary.sources[].error` có giá trị nếu một trang bị chặn; lượt tìm vẫn `DONE` với các trang còn lại.
- Xem chi tiết từng job (cả job bị loại và lý do) trong file báo cáo ở `summary.report`.

**Access token hết hạn sau 15 phút**, trong khi lượt tìm chạy 10-12 phút. Nếu `GET` trả 401, gọi:

```
POST {{base_url}}/auth/refresh        (Authorization: No Auth)
Body > raw > JSON: { "refresh_token": "{{refresh_token}}" }
```

với cùng script **Post-response** như bước 1 để lưu cặp token mới, rồi gọi lại bước 5.

### 9.3. Các lỗi thường gặp

Lỗi có dạng `{ "error": { "code": "...", "message": "..." } }`.

| HTTP | `code` | Nguyên nhân | Cách xử lý |
|---|---|---|---|
| 401 | `unauthorized` | Thiếu token hoặc token hết hạn | Đăng nhập lại hoặc gọi `/auth/refresh` |
| 404 | `cv_not_uploaded` | Gọi `/analyze-cv` khi chưa tải CV | Làm bước 2 |
| 409 | `analysis_in_progress` | Đã có lượt phân tích chưa xong | Chờ lượt cũ `DONE`/`FAILED` |
| 404 | `analysis_not_found` | `/find-job/{id}` với id phân tích không tồn tại | Dùng `id` trả về ở bước 3 |
| 409 | `analysis_not_done` | Lượt phân tích chưa xong hoặc `FAILED` | Chờ `DONE`, hoặc phân tích lại |
| 409 | `analysis_outdated` | Đã tải CV mới sau lượt phân tích | Gọi lại `/analyze-cv` |
| 409 | `search_in_progress` | Đã có lượt tìm chưa xong | Chờ lượt cũ `DONE`/`FAILED` |
| 400 | `keyword_required` | Không suy ra được keyword từ CV | Gửi `keyword` trong body bước 4 |
| 413 | | File CV lớn hơn 10 MB | Dùng file nhỏ hơn |
| 422 | `cv_rejected` | CV là ảnh scan, sai định dạng, hoặc nội dung không phải CV (tin tuyển dụng, tài liệu...) | Dùng đúng file CV có chữ chọn được |
| 502 | `ai_service_error` | Dịch vụ AI không chạy, Ollama tắt, hoặc API LLM lỗi/thiếu key | Xem log uvicorn, kiểm tra `GET http://127.0.0.1:8001/health` |

Lượt tìm `FAILED` vì backend khởi động lại giữa chừng là bình thường; tạo lượt tìm mới.

## Tài liệu liên quan

- [backend/README.md](../backend/README.md): cài đặt, chạy, migrate, lệnh cào job.
- [backend/docs/database.md](../backend/docs/database.md): lọc job ứng viên trên pgvector.
- [llm/docs/development_vs_production.md](../llm/docs/development_vs_production.md): cấu trúc, luồng và khác biệt giữa hai chế độ của dịch vụ AI.
- [llm/data/reports/](../llm/data/reports/): báo cáo chấm điểm (`matching_<cv>.md` từ test, `find_job_<id>.md` từ mỗi lượt tìm).
