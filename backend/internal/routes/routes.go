package routes

import (
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"notification-job-mb/internal/handlers"
	"notification-job-mb/internal/middlewares"
)

// authBodyLimit bounds auth request bodies; they only carry short credentials.
const authBodyLimit = 4 << 10 // 4 KiB

// cvUploadLimit: the CV file plus multipart overhead.
const cvUploadLimit = handlers.MaxCVBytes + 64<<10

const findJobBodyLimit = 4 << 10

// botBodyLimit: 4096 characters of up to 4 bytes each, plus the JSON.
const botBodyLimit = 20 << 10

type Dependencies struct {
	Health        *handlers.HealthHandler
	Auth          *handlers.AuthHandler
	PasswordReset *handlers.PasswordResetHandler // optional
	CV            *handlers.CVHandler
	FindJob       *handlers.FindJobHandler
	Bot           *handlers.BotHandler // optional
	TokenVerifier middlewares.AccessTokenVerifier
}

func Register(engine *gin.Engine, deps Dependencies) {
	engine.GET("/health", deps.Health.Check)
	engine.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	v1 := engine.Group("/api/v1")

	auth := v1.Group("/auth")
	auth.POST("/login", middlewares.BodyLimit(authBodyLimit), deps.Auth.Login)
	auth.POST("/refresh", middlewares.BodyLimit(authBodyLimit), deps.Auth.Refresh)
	auth.POST("/logout", middlewares.BodyLimit(authBodyLimit), deps.Auth.Logout)
	auth.GET("/me", middlewares.RequireAuth(deps.TokenVerifier), deps.Auth.Me)

	if deps.PasswordReset != nil {
		auth.POST("/forgot-password", middlewares.BodyLimit(authBodyLimit), deps.PasswordReset.ForgotPassword)
		auth.POST("/verify-reset-code", middlewares.BodyLimit(authBodyLimit), deps.PasswordReset.VerifyResetCode)
		auth.POST("/reset-password", middlewares.BodyLimit(authBodyLimit), deps.PasswordReset.ResetPassword)
	}

	// Luồng tìm việc: tải CV -> phân tích (chạy nền, trả id) -> tìm việc theo id phân tích (chạy nền) -> xem kết quả
	user := v1.Group("", middlewares.RequireAuth(deps.TokenVerifier))
	user.POST("/parse-cv", middlewares.BodyLimit(cvUploadLimit), deps.CV.ParseCV)
	user.POST("/analyze-cv", middlewares.BodyLimit(findJobBodyLimit), deps.CV.AnalyzeCV)
	user.GET("/analyze-cv/:id", deps.CV.GetAnalysis)
	user.POST("/find-job/:id", middlewares.BodyLimit(findJobBodyLimit), deps.FindJob.Start) // :id = id lượt phân tích CV
	user.GET("/find-job/:id", deps.FindJob.Get)                                             // :id = id lượt tìm

	if deps.Bot != nil {
		user.POST("/bot/send-message", middlewares.BodyLimit(botBodyLimit), deps.Bot.SendMessage)
	}
}
