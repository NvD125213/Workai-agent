# Database: lọc job ứng viên theo kinh nghiệm trước khi tìm theo vector

Tài liệu này mô tả thay đổi ở tầng database cho bước lấy job ứng viên của một hồ sơ. Trước đây có bước chấm điểm AI nhưng chưa có query lấy job ứng viên; nay có `JobRepository.FindCandidateJobs`, trong đó bộ lọc cứng về kinh nghiệm chạy ngay trong SQL, cùng lúc với tìm kiếm HNSW.

## Bối cảnh

Dịch vụ AI (`llm/`) chấm điểm hai tầng:

1. **Bộ lọc cứng** (`llm/matching/filters.py`): loại job sai cấp kinh nghiệm hoặc thiếu kỹ năng bắt buộc.
2. **Chấm điểm mềm**: embedding tiêu đề, rerank bằng cross-encoder và điểm luật. Rerank là bước chậm nhất (khoảng 1.2 giây/job trên CPU).

Nếu Go lấy 50 job gần nhất theo vector rồi mới gửi sang Python, phần lớn trong số đó có thể là job Senior mà ứng viên Junior chắc chắn bị loại. Khi đó Python nhận về ít job dùng được, và băng thông, thời gian bị phí cho những job không bao giờ đạt. Lọc kinh nghiệm ngay trong SQL giải quyết cả hai vấn đề: 50 chỗ trong `LIMIT` chỉ dành cho job ứng viên có thể đáp ứng.

## Thay đổi

### 1. Cột mới `jobs.required_years`

| Cột | Kiểu | Ý nghĩa |
|---|---|---|
| `min_years` (đã có) | `int NULL` | Số năm tin tuyển dụng ghi rõ. `NULL` = tin không ghi. |
| `required_years` (mới) | `int NULL` | Số năm thực sự cần: `max(min_years, sàn theo cấp bậc trong tiêu đề)`. `NULL` = không xác định. |

Mức sàn theo cấp bậc lấy từ `SENIORITY_MIN_YEARS` trong `llm/matching/config.py`: intern/fresher/junior 0, middle 2, senior 3, lead 4. Tiêu đề ghi nhiều cấp ("Junior - Middle") thì lấy cấp thấp nhất. Cột này cần thiết vì nhiều tin "Senior ..." chỉ ghi 1-2 năm hoặc không ghi gì; lọc theo `min_years` thì những tin đó lọt qua.

- **Ai ghi:** dịch vụ AI. `analyze_job` tính `required_years`, `job_to_db` trả về để ghi vào `jobs` (`llm/matching/llm.py`).
- **Model Go:** `models.Job.RequiredYears *int` (`internal/models/models.go`).
- **Không thêm index.** Query sắp xếp theo khoảng cách vector nên planner dùng index HNSW `idx_jobs_embedding`; điều kiện kinh nghiệm được kiểm tra trong lúc duyệt index đó (xem mục 3). Một B-tree trên `required_years` sẽ gần như không được dùng.

### 2. `JobRepository.FindCandidateJobs`

File `internal/repositories/job_repository.go`.

```go
jobs, err := jobRepo.FindCandidateJobs(ctx, repositories.CandidateQuery{
    Embedding:      *profile.Embedding,     // user_profiles.embedding
    EmbeddingModel: profile.EmbeddingModel, // chỉ so với job nhúng cùng mô hình
    ExpYears:       expYears,               // *int; nil = chưa biết, không lọc kinh nghiệm
    Limit:          50,                     // mặc định 50, tối đa 200
})
// jobs[i].Job: các cột cần cho chấm điểm + JobSkills; jobs[i].Cosine: độ tương đồng 0..1
```

SQL sinh ra:

```sql
SELECT j.id, j.source_id, j.unique_identifier, j.title, j.company, j.description, j.url, j.raw_salary, j.posted_at,
       j.location, j.is_remote, j.min_years, j.required_years, j.languages,
       1 - (j.embedding <=> @embedding) AS cosine
FROM jobs j
WHERE j.embedding IS NOT NULL
  AND j.embedding_model = @model
  AND (CAST(@max_years AS int) IS NULL
       OR COALESCE(j.required_years, j.min_years) IS NULL
       OR COALESCE(j.required_years, j.min_years) <= CAST(@max_years AS int))
ORDER BY j.embedding <=> @embedding
LIMIT @limit
```

Dùng `CAST(@x AS int)` chứ không viết `@x::int`: GORM không nhận diện tham số đặt tên khi ngay sau nó là `::`, nên tên tham số bị gửi nguyên văn tới Postgres (lỗi `column "max_years" does not exist`).

- `@max_years = ExpYears + ExpToleranceYears`, với `ExpToleranceYears = 1`: job cần X năm thì ứng viên phải có ít nhất X − 1 năm. Hằng số này phải bằng `EXP_TOLERANCE_YEARS` trong `llm/matching/config.py`; nếu khác nhau, SQL và Python sẽ loại hai tập job khác nhau.
- `COALESCE(required_years, min_years)`: job phân tích trước khi có cột mới vẫn được lọc theo `min_years`, không cần chạy lại AI cho dữ liệu cũ.
- Job không có yêu cầu kinh nghiệm (cả hai cột `NULL`) không bị loại. Thiếu căn cứ thì không lọc, giống Python.
- `embedding_model = @model`: vector của hai mô hình khác nhau không so sánh được với nhau. Job nhúng bằng mô hình cũ sẽ không xuất hiện cho đến khi được nhúng lại.
- **Không lấy cột `embedding`** (1024 số thực/job). Python đã nhận `cosine`, không cần vector.
- **Kỹ năng được nạp bằng một truy vấn** `job_id IN (...)` cho cả tập kết quả, không phải một truy vấn cho mỗi job.

### 3. HNSW và bộ lọc: `hnsw.iterative_scan`

Mặc định, index HNSW chỉ lấy `hnsw.ef_search` (40) ứng viên gần nhất **rồi mới** áp `WHERE`. Với một ứng viên Junior, 40 job gần nhất có thể phần lớn là Senior; sau khi lọc chỉ còn vài dòng, dù DB vẫn còn nhiều job phù hợp ở xa hơn một chút. Lọc như vậy làm *mất* kết quả.

Vì vậy, trong cùng transaction với query, `FindCandidateJobs` đặt hai tham số:

```sql
SELECT set_config('hnsw.iterative_scan', 'strict_order', true),
       set_config('hnsw.ef_search', '<max(limit, 40)>', true);
```

- `iterative_scan = strict_order`: nếu sau khi lọc vẫn chưa đủ `LIMIT` dòng, HNSW duyệt tiếp, và kết quả vẫn đúng thứ tự khoảng cách.
- `ef_search >= limit`: tránh trường hợp `LIMIT 50` chỉ trả về tối đa 40 dòng.
- Tham số thứ ba `true` của `set_config` tương đương `SET LOCAL`: chỉ có hiệu lực trong transaction đó, không rò sang các kết nối khác trong pool. Vì thế query và lệnh `set_config` phải chạy trong cùng một `Transaction`.

**Đã đo trên pgvector 0.8.7** với 5000 job giả, 10% (500 job) đạt điều kiện kinh nghiệm, ép planner dùng index HNSW:

| Cấu hình | Số dòng trả về với `LIMIT 50` |
|---|---|
| Mặc định (`iterative_scan = off`) | **5** |
| Như `FindCandidateJobs` (`strict_order`, `ef_search = 50`) | **50**, `Index Scan using idx_jobs_embedding`, khoảng 30 ms |

Bảng nhỏ (vài nghìn dòng) thường được planner quét tuần tự rồi sắp xếp, cách này luôn chính xác. Lỗi mất kết quả chỉ xuất hiện khi bảng đủ lớn để planner chuyển sang index, nên không thể dựa vào việc hệ thống chạy đúng lúc dữ liệu còn ít.

**Yêu cầu: pgvector ≥ 0.8.0.** Image `pgvector/pgvector:pg16` trong `docker-compose.yml` hiện kéo bản 0.8.7. Kiểm tra trên DB đang chạy:

```sql
SELECT extversion FROM pg_extension WHERE extname = 'vector';  -- cần >= 0.8.0
SHOW hnsw.iterative_scan;                                        -- lỗi "unrecognized parameter" = bản cũ
```

Với bản cũ hơn, kết quả tuỳ phiên kết nối. Nếu thư viện pgvector đã nạp, PostgreSQL 15+ coi `hnsw.` là tiền tố đã đăng ký và báo lỗi tham số không tồn tại, nên `FindCandidateJobs` trả lỗi `configure hnsw scan`. Nếu chưa nạp, tham số được nhận nhưng không có tác dụng và query quay lại kiểu lọc sau như mô tả ở trên. Cả hai trường hợp đều cần nâng cấp. Volume tạo từ bản cũ thì chạy thêm `ALTER EXTENSION vector UPDATE;` sau khi kéo image mới.

### 4. Quan hệ với bộ lọc bên Python

SQL là **tầng lọc thô**, Python là **tầng lọc cuối**:

| Điều kiện | SQL (`FindCandidateJobs`) | Python (`filters.py`) |
|---|---|---|
| Kinh nghiệm | Có | Có |
| Kỹ năng bắt buộc theo tiêu đề (".NET", "Java/Go", CNC...) | Không | Có |
| Cùng mô hình nhúng | Có | Không cần |

Kỹ năng bắt buộc không lọc trong SQL vì cần chuẩn hoá tên (alias, `React/Next.js`, C# ⇔ .NET, framework suy ra ngôn ngữ). Logic này đã có trong Python; viết lại bằng SQL sẽ thành hai bản dễ lệch nhau.

**SQL không bao giờ chặt hơn Python.** `user_profiles.exp_years` là `int`, do Python làm tròn từ số thực (0.5 năm → 0, 1.5 → 2). Phép làm tròn không làm giảm thứ tự và giữ nguyên số nguyên, nên nếu số năm thật ≥ X − 1 thì số đã làm tròn cũng ≥ X − 1. Do đó mọi job Python giữ lại đều qua được SQL. Ngược lại, SQL có thể lỏng hơn một chút (0.6 năm làm tròn thành 1), và Python sẽ loại nốt những job đó.

## Triển khai

1. Chạy migrate để thêm cột: `go run ./cmd/migrate` (GORM AutoMigrate thêm `required_years int NULL`, không khoá bảng lâu, không đụng dữ liệu cũ).
2. Triển khai bản mới của dịch vụ AI để `job_to_db` bắt đầu ghi `required_years`.
3. Job cũ không cần xử lý lại: query dùng `min_years` khi `required_years` là `NULL`. Muốn job cũ cũng được lọc theo cấp bậc trong tiêu đề thì cho AI phân tích lại các job đó.

Thứ tự 1 → 2 là bắt buộc: dịch vụ AI ghi vào cột chưa tồn tại sẽ lỗi. Code Go cũ vẫn chạy bình thường với schema mới vì cột mới cho phép `NULL`.

## Kiểm thử

- `tests/integration/candidate_jobs_test.go` kiểm tra trên PostgreSQL thật:
  - ứng viên 0 năm không nhận job cần 3 năm, kể cả khi job đó gần nhất theo vector, và job đó không chiếm chỗ trong `LIMIT`;
  - `required_years` `NULL` thì dùng `min_years`;
  - job không có yêu cầu kinh nghiệm không bị loại;
  - `ExpYears = nil` thì không lọc kinh nghiệm;
  - job khác mô hình nhúng hoặc chưa nhúng không bao giờ được trả về;
  - kết quả đúng thứ tự cosine và có `JobSkills`.

  Cách chạy, cần DB scratch vì test sẽ `TRUNCATE` bảng:

  ```bash
  set -a; . ./.env; set +a; TEST_DB_NAME=nj_test go test ./tests/integration/ -run TestFindCandidateJobs -v
  ```

  Với vài dòng dữ liệu, planner chọn quét tuần tự thay vì HNSW. Test vì thế kiểm tra *kết quả* của bộ lọc. Hành vi `iterative_scan` trên index được kiểm tra riêng bằng benchmark ở mục 3; muốn đo lại trên dữ liệu thật, chạy `EXPLAIN ANALYZE` cho query và xem plan có `Index Scan using idx_jobs_embedding`.

- Kết quả lần chạy ngày 2026-10-28 (Postgres 16, pgvector 0.8.7): 11/11 integration test đạt, gồm `TestFindCandidateJobs` và các test có sẵn.

## Hạn chế còn lại

- **Chưa phân biệt được "0 năm" với "không rõ".** `user_profiles.exp_years` là `int NOT NULL DEFAULT 0`, nên CV không xác định được số năm cũng lưu là 0 và sẽ bị lọc như ứng viên mới ra trường. Phía gọi nên truyền `ExpYears = nil` khi `parsed_at IS NULL`. Muốn xử lý triệt để thì đổi `exp_years` thành `NULL`-able (hoặc thêm cột `exp_years_known`). Thay đổi đó đụng đến dữ liệu hiện có nên chưa làm ở đây.
- **Chưa có nơi gọi `FindCandidateJobs`.** Worker xếp hạng (trạng thái `RANKING` của `job_searches`) chưa được viết; khi viết thì gọi hàm này rồi gửi kết quả kèm `cosine` sang dịch vụ AI.

---

# Lượt phân tích CV chạy nền và find-job theo id (2026-10-30)

`POST /api/v1/analyze-cv` không còn giữ request trong lúc AI phân tích (30 giây tới vài phút): nó tạo một lượt phân tích, trả 202 kèm `id`, và chạy nền. `POST /api/v1/find-job/{id}` nhận `id` lượt phân tích đó.

## Bảng mới `cv_analyses`

| Cột | Kiểu | Ý nghĩa |
|---|---|---|
| `id` | `bigserial` | ID lượt phân tích, dùng cho `GET /analyze-cv/{id}` và `POST /find-job/{id}` |
| `user_id` | `bigint NOT NULL` | Chủ lượt phân tích (FK `users`, xoá theo user) |
| `profile_id` | `bigint NOT NULL` | `user_profiles.id` được phân tích (FK, xoá theo profile) |
| `resume_hash` | `varchar(64) NOT NULL` | SHA-256 của CV lúc phân tích. Khác `user_profiles.resume_hash` nghĩa là đã tải CV mới, lượt này `outdated` |
| `status` | `varchar(50)` | `QUEUED` → `ANALYZING` → `DONE` / `FAILED` |
| `error` | `text` | Lý do thất bại, hiển thị được cho người dùng |
| `created_at`, `finished_at` | `timestamptz` | |

- **Kết quả không lưu trong bảng này.** Phân tích xong vẫn ghi vào `user_profiles` và `user_skills` như trước; `GET /analyze-cv/{id}` đọc từ đó khi `resume_hash` còn khớp.
- **Index `idx_cv_analyses_active_user`:** unique trên `user_id` với `status IN ('QUEUED', 'ANALYZING')`. Mỗi người chỉ có một lượt phân tích đang chạy; request thứ hai đồng thời vi phạm index và nhận 409, giống `idx_job_searches_active_user`.
- **Khởi động lại API:** `AnalysisRepository.FailUnfinished` chuyển lượt `QUEUED`/`ANALYZING` còn sót sang `FAILED`.

## Cột mới của `job_searches`

| Cột | Kiểu | Ý nghĩa |
|---|---|---|
| `analysis_id` | `bigint NULL` | Lượt phân tích CV dùng cho lượt tìm (FK `cv_analyses`, `ON DELETE SET NULL`). `NULL` với các lượt tìm tạo trước thay đổi này |
| `exp_filter` | `boolean NOT NULL DEFAULT false` | Lượt tìm có bật query `exp=true` không: lọc theo khoảng kinh nghiệm và xếp hạng theo lương. Khi bật, `ai_matches.details.components` có thêm `salary` và `exp_fit` |

## Triển khai

Cả hai thay đổi do `go run ./cmd/migrate` (AutoMigrate) tạo, không cần SQL tay. Các cột mới đều cho phép `NULL` hoặc có mặc định nên không ảnh hưởng dữ liệu cũ.
