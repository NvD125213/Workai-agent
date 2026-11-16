package middlewares

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"notification-job-mb/internal/services"
)

const (
	ctxUserID = "auth.user_id"
	ctxRole   = "auth.role"
)

type AccessTokenVerifier interface {
	ParseAccessToken(raw string) (*services.AccessClaims, error)
}

// ErrorResponse is the JSON body for every error returned by the API.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code" example:"invalid_credentials"`
	Message string `json:"message" example:"invalid email or password"`
}

func AbortWithError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: ErrorBody{Code: code, Message: message}})
}

// RequireAuth accepts only requests carrying a valid "Authorization: Bearer
// <access token>" header and stores the verified identity in the context.
func RequireAuth(verifier AccessTokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		scheme, token, ok := strings.Cut(c.GetHeader("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			c.Header("WWW-Authenticate", `Bearer`)
			AbortWithError(c, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
			return
		}

		claims, err := verifier.ParseAccessToken(strings.TrimSpace(token))
		if err != nil {
			c.Header("WWW-Authenticate", `Bearer error="invalid_token"`)
			AbortWithError(c, http.StatusUnauthorized, "invalid_token", "access token is invalid or expired")
			return
		}

		c.Set(ctxUserID, claims.UserID)
		c.Set(ctxRole, claims.Role)
		c.Next()
	}
}

// CurrentUserID returns the identity set by RequireAuth.
func CurrentUserID(c *gin.Context) (uint, bool) {
	id, ok := c.Get(ctxUserID)
	if !ok {
		return 0, false
	}
	uid, ok := id.(uint)
	return uid, ok
}
