package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"notification-job-mb/internal/middlewares"
	"notification-job-mb/internal/services"
)

type botService interface {
	SendMessage(ctx context.Context, text string) error
}

type BotHandler struct {
	bot botService
}

func NewBotHandler(bot botService) *BotHandler {
	return &BotHandler{bot: bot}
}

type SendMessageRequest struct {
	Text string `json:"text" binding:"required" example:"Xin chào từ notification-job-mb"`
}

type SendMessageResponse struct {
	Status string `json:"status" example:"sent"`
}

// SendMessage godoc
// @Summary      Gửi tin nhắn Telegram (thử)
// @Description  Gửi tin nhắn text thuần (1-4096 ký tự) tới TELEGRAM_CHAT_ID qua bot đã cấu hình.
// @Tags         bot
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      SendMessageRequest  true  "Nội dung tin nhắn"
// @Success      200   {object}  SendMessageResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Failure      401   {object}  middlewares.ErrorResponse
// @Failure      502   {object}  middlewares.ErrorResponse  "Telegram lỗi hoặc từ chối (token, chat id)"
// @Router       /api/v1/bot/send-message [post]
func (h *BotHandler) SendMessage(c *gin.Context) {
	var req SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "field \"text\" is required")
		return
	}

	err := h.bot.SendMessage(c.Request.Context(), req.Text)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, SendMessageResponse{Status: "sent"})
	case errors.Is(err, services.ErrBotMessageInvalid):
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "text must be 1-4096 characters")
	case errors.Is(err, services.ErrBotUnavailable):
		log.Printf("bot send message: %v", err)
		middlewares.AbortWithError(c, http.StatusBadGateway, "bot_unavailable", "telegram did not accept the message")
	default:
		log.Printf("bot send message: %v", err)
		middlewares.AbortWithError(c, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
