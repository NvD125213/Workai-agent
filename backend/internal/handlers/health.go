package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type HealthHandler struct {
	db *gorm.DB
}

func NewHealthHandler(db *gorm.DB) *HealthHandler {
	return &HealthHandler{db: db}
}

type HealthResponse struct {
	Status string `json:"status" example:"ok"`
}

// Check godoc
// @Summary      Health check
// @Description  Kiểm tra API còn sống và kết nối PostgreSQL
// @Tags         system
// @Produce      json
// @Success      200  {object}  HealthResponse
// @Failure      503  {object}  HealthResponse
// @Router       /health [get]
func (h *HealthHandler) Check(c *gin.Context) {
	sqlDB, err := h.db.DB()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, HealthResponse{Status: "unhealthy"})
		return
	}

	if err := sqlDB.Ping(); err != nil {
		c.JSON(http.StatusServiceUnavailable, HealthResponse{Status: "unhealthy"})
		return
	}

	c.JSON(http.StatusOK, HealthResponse{Status: "ok"})
}
