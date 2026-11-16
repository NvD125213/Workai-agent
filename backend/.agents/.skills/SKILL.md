Tạo cấu trúc project Go backend cho project này với:

- Go
- Gin
- GORM
- PostgreSQL
- godotenv
- REST API
- Architecture: Handler → Service → Repository
- Có background worker/job riêng

Cấu trúc:

backend/
├── cmd/
│ ├── api/
│ │ └── main.go
│ └── worker/
│ └── main.go
├── internal/
│ ├── config/
│ ├── database/
│ ├── models/
│ ├── repositories/
│ ├── services/
│ ├── handlers/
│ ├── routes/
│ ├── middlewares/
│ └── jobs/
├── migrations/
├── .env
├── .env.example
├── .gitignore
├── go.mod
└── README.md

Yêu cầu:

1. Khởi tạo go.mod.
2. Cài các dependency cần thiết.
3. Tạo file placeholder cần thiết.
4. Thiết lập PostgreSQL connection bằng GORM.
5. Thiết lập Gin server.
6. Tạo health check GET /health.
7. Worker có entry point riêng nhưng chưa cần implement job.
8. Không tạo business logic giả.
9. Code phải idiomatic Go, dễ mở rộng.
10. Chạy gofmt cho toàn bộ Go source.
11. Sau khi tạo xong, kiểm tra bằng go build ./... và sửa các lỗi nếu có.
12. Giải thích ngắn gọn vai trò của từng folder.
