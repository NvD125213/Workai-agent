package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"notification-job-mb/internal/middlewares"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/services"
)

const maxUserAgentLen = 512

type authService interface {
	Login(ctx context.Context, email, password string, client services.ClientInfo) (*services.AuthResult, error)
	Refresh(ctx context.Context, rawRefreshToken string, client services.ClientInfo) (*services.AuthResult, error)
	Logout(ctx context.Context, rawRefreshToken string) error
	CurrentUser(ctx context.Context, userID uint) (*models.User, error)
}

type AuthHandler struct {
	auth authService
}

func NewAuthHandler(auth authService) *AuthHandler {
	return &AuthHandler{auth: auth}
}

type LoginRequest struct {
	Email    string `json:"email" binding:"required,max=150" example:"user@example.com"`
	Password string `json:"password" binding:"required,max=128" example:"secret123"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required,max=256"`
}

type UserResponse struct {
	ID            uint   `json:"id" example:"1"`
	Email         string `json:"email" example:"user@example.com"`
	Name          string `json:"name" example:"Nguyen Van A"`
	Role          string `json:"role" example:"USER"`
	EmailVerified bool   `json:"email_verified" example:"false"`
}

type AuthResponse struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	TokenType    string       `json:"token_type" example:"Bearer"`
	ExpiresIn    int64        `json:"expires_in" example:"900"`
	User         UserResponse `json:"user"`
}

// Login godoc
// @Summary      Đăng nhập
// @Description  Đăng nhập bằng email/mật khẩu, trả về access token (JWT) và refresh token
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      LoginRequest  true  "Thông tin đăng nhập"
// @Success      200   {object}  AuthResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Failure      401   {object}  middlewares.ErrorResponse
// @Failure      403   {object}  middlewares.ErrorResponse
// @Router       /api/v1/auth/login [post]
func (h *AuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "email and password are required")
		return
	}

	res, err := h.auth.Login(c.Request.Context(), req.Email, req.Password, clientInfo(c))
	if err != nil {
		h.writeAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, newAuthResponse(res))
}

// Refresh godoc
// @Summary      Làm mới token
// @Description  Đổi refresh token lấy cặp token mới. Refresh token cũ bị thu hồi.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      RefreshRequest  true  "Refresh token"
// @Success      200   {object}  AuthResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Failure      401   {object}  middlewares.ErrorResponse
// @Failure      403   {object}  middlewares.ErrorResponse
// @Router       /api/v1/auth/refresh [post]
func (h *AuthHandler) Refresh(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}

	res, err := h.auth.Refresh(c.Request.Context(), req.RefreshToken, clientInfo(c))
	if err != nil {
		h.writeAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, newAuthResponse(res))
}

// Logout godoc
// @Summary      Đăng xuất
// @Description  Thu hồi refresh token. Gọi lại với token đã thu hồi vẫn trả 204.
// @Tags         auth
// @Accept       json
// @Param        body  body  RefreshRequest  true  "Refresh token"
// @Success      204
// @Failure      400   {object}  middlewares.ErrorResponse
// @Router       /api/v1/auth/logout [post]
func (h *AuthHandler) Logout(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}

	if err := h.auth.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		h.writeAuthError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Me godoc
// @Summary      Thông tin người dùng hiện tại
// @Tags         auth
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  UserResponse
// @Failure      401  {object}  middlewares.ErrorResponse
// @Failure      403  {object}  middlewares.ErrorResponse
// @Router       /api/v1/auth/me [get]
func (h *AuthHandler) Me(c *gin.Context) {
	userID, ok := middlewares.CurrentUserID(c)
	if !ok {
		middlewares.AbortWithError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	user, err := h.auth.CurrentUser(c.Request.Context(), userID)
	if err != nil {
		h.writeAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, newUserResponse(user))
}

func (h *AuthHandler) writeAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, services.ErrInvalidCredentials):
		middlewares.AbortWithError(c, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
	case errors.Is(err, services.ErrInvalidRefreshToken):
		middlewares.AbortWithError(c, http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired")
	case errors.Is(err, services.ErrAccountDisabled):
		middlewares.AbortWithError(c, http.StatusForbidden, "account_disabled", "account is disabled")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		middlewares.AbortWithError(c, http.StatusServiceUnavailable, "unavailable", "request was cancelled or timed out")
	default:
		log.Printf("auth %s %s: %v", c.Request.Method, c.FullPath(), err)
		middlewares.AbortWithError(c, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func clientInfo(c *gin.Context) services.ClientInfo {
	ua := c.Request.UserAgent()
	if len(ua) > maxUserAgentLen {
		ua = ua[:maxUserAgentLen]
	}
	// PostgreSQL text rejects invalid UTF-8 (e.g. a rune cut above) and NUL.
	ua = strings.ReplaceAll(strings.ToValidUTF8(ua, ""), "\x00", "")
	return services.ClientInfo{UserAgent: ua, IPAddress: c.ClientIP()}
}

func newUserResponse(u *models.User) UserResponse {
	return UserResponse{
		ID:            u.ID,
		Email:         u.Email,
		Name:          u.Name,
		Role:          u.Role,
		EmailVerified: u.EmailVerified,
	}
}

func newAuthResponse(res *services.AuthResult) AuthResponse {
	return AuthResponse{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(res.ExpiresIn.Seconds()),
		User:         newUserResponse(res.User),
	}
}
