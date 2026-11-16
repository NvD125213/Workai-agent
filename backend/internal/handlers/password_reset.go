package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"notification-job-mb/internal/middlewares"
	"notification-job-mb/internal/services"
)

type passwordResetService interface {
	RequestReset(ctx context.Context, email string) error
	VerifyResetCode(ctx context.Context, email, code string) (string, time.Time, error)
	ResetPassword(ctx context.Context, resetToken, newPassword string) error
}

type PasswordResetHandler struct {
	reset passwordResetService
}

func NewPasswordResetHandler(reset passwordResetService) *PasswordResetHandler {
	return &PasswordResetHandler{reset: reset}
}

type ForgotPasswordRequest struct {
	Email string `json:"email" binding:"required,email,max=150" example:"user@example.com"`
}

type VerifyResetCodeRequest struct {
	Email string `json:"email" binding:"required,email,max=150" example:"user@example.com"`
	Code  string `json:"code" binding:"required,len=6,numeric" example:"123456"`
}

type VerifyResetCodeResponse struct {
	ResetToken string    `json:"reset_token"`
	ExpiresIn  int       `json:"expires_in" example:"600"` // seconds
	ExpiresAt  time.Time `json:"expires_at"`
}

type ResetPasswordRequest struct {
	ResetToken  string `json:"reset_token" binding:"required,max=64"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=128"`
}

type MessageResponse struct {
	Message string `json:"message"`
}

// ForgotPassword godoc
// @Summary      Quên mật khẩu — bước 1: gửi OTP
// @Description  Gửi mã OTP 6 số qua email nếu tài khoản tồn tại. Luôn trả 202 với cùng nội dung để không lộ email nào đã đăng ký.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      ForgotPasswordRequest  true  "Email"
// @Success      202   {object}  MessageResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Router       /api/v1/auth/forgot-password [post]
func (h *PasswordResetHandler) ForgotPassword(c *gin.Context) {
	var req ForgotPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "a valid email is required")
		return
	}

	if err := h.reset.RequestReset(c.Request.Context(), req.Email); err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, MessageResponse{
		Message: "if the email is registered, a verification code has been sent",
	})
}

// VerifyResetCode godoc
// @Summary      Quên mật khẩu — bước 2: xác thực OTP
// @Description  Xác thực OTP và trả về reset_token dùng một lần, hết hạn sau 10 phút. OTP bị tiêu thụ khi xác thực thành công; sai quá 5 lần sẽ bị vô hiệu. Yêu cầu OTP mới sẽ huỷ reset_token cũ.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      VerifyResetCodeRequest  true  "Email và OTP"
// @Success      200   {object}  VerifyResetCodeResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Router       /api/v1/auth/verify-reset-code [post]
func (h *PasswordResetHandler) VerifyResetCode(c *gin.Context) {
	var req VerifyResetCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "email and a 6-digit code are required")
		return
	}

	token, expiresAt, err := h.reset.VerifyResetCode(c.Request.Context(), req.Email, req.Code)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, VerifyResetCodeResponse{
		ResetToken: token,
		ExpiresIn:  int(time.Until(expiresAt).Round(time.Second).Seconds()),
		ExpiresAt:  expiresAt.UTC(),
	})
}

// ResetPassword godoc
// @Summary      Quên mật khẩu — bước 3: đặt mật khẩu mới
// @Description  Đổi reset_token (từ bước 2) lấy mật khẩu mới. Token chỉ dùng được một lần. Thành công sẽ thu hồi mọi refresh token của người dùng.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      ResetPasswordRequest  true  "reset_token và mật khẩu mới"
// @Success      200   {object}  MessageResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Router       /api/v1/auth/reset-password [post]
func (h *PasswordResetHandler) ResetPassword(c *gin.Context) {
	var req ResetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request",
			"a reset token and a new password of 8-128 characters are required")
		return
	}

	if err := h.reset.ResetPassword(c.Request.Context(), req.ResetToken, req.NewPassword); err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, MessageResponse{Message: "password has been reset"})
}

func (h *PasswordResetHandler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, services.ErrInvalidResetCode):
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_reset_code", "the code is invalid or has expired")
	case errors.Is(err, services.ErrInvalidResetToken):
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_reset_token", "the reset token is invalid, used or has expired")
	case errors.Is(err, services.ErrWeakPassword):
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "password must be 8-128 characters")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		middlewares.AbortWithError(c, http.StatusServiceUnavailable, "unavailable", "request was cancelled or timed out")
	default:
		log.Printf("password reset %s %s: %v", c.Request.Method, c.FullPath(), err)
		middlewares.AbortWithError(c, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
