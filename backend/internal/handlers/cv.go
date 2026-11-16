package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"notification-job-mb/internal/middlewares"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

// MaxCVBytes is the largest CV file accepted; the AI service has the same limit.
const MaxCVBytes = 10 << 20 // 10 MiB

type cvService interface {
	ParseCV(ctx context.Context, userID uint, filename string, file io.Reader) (*models.UserProfile, error)
	StartAnalysis(ctx context.Context, userID uint) (*models.CVAnalysis, error)
	GetAnalysis(ctx context.Context, userID, id uint) (*services.AnalysisView, error)
}

type CVHandler struct {
	cv cvService
}

func NewCVHandler(cv cvService) *CVHandler {
	return &CVHandler{cv: cv}
}

type ParseCVResponse struct {
	ProfileID uint   `json:"profile_id" example:"1"`
	Chars     int    `json:"chars" example:"3805"`
	Text      string `json:"text" example:"DƯƠNG TUẤN KHANG\nFull-Stack Developer..."`
}

type SkillResponse struct {
	Name  string `json:"name" example:"react"`
	Level string `json:"level" example:"JUNIOR"`
}

type AnalyzeCVResponse struct {
	CurrentTitle  string          `json:"current_title" example:"Full-Stack Developer"`
	ExpYears      int             `json:"exp_years" example:"1"`
	Location      string          `json:"location" example:"Hà Nội"`
	Summary       string          `json:"summary"`
	SearchKeyword string          `json:"search_keyword" example:"Developer"`
	Skills        []SkillResponse `json:"skills"`
	ParsedAt      *time.Time      `json:"parsed_at"`
	// Profile is the full analysis: years as a decimal, all job titles, experiences, languages...
	Profile json.RawMessage `json:"profile" swaggertype:"object"`
}

type AnalysisResponse struct {
	// ID lượt phân tích; dùng cho GET /api/v1/analyze-cv/{id} và POST /api/v1/find-job/{id}.
	ID         uint       `json:"id" example:"3"`
	ProfileID  uint       `json:"profile_id" example:"1"`
	Status     string     `json:"status" example:"DONE" enums:"QUEUED,ANALYZING,DONE,FAILED"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Outdated: đã tải lên CV mới sau lượt phân tích này, kết quả không còn; phân tích lại.
	Outdated bool `json:"outdated,omitempty"`
	// Result: hồ sơ đã phân tích; chỉ có khi status = DONE và không outdated.
	Result *AnalyzeCVResponse `json:"result,omitempty"`
}

// ParseCV godoc
// @Summary      Tải lên CV
// @Description  Đọc chữ trong CV (pdf, docx, txt, md; tối đa 10 MB) và lưu làm CV hiện tại của người dùng. Kết quả phân tích của CV cũ bị xoá; gọi tiếp POST /api/v1/analyze-cv.
// @Tags         cv
// @Accept       multipart/form-data
// @Produce      json
// @Security     BearerAuth
// @Param        file  formData  file  true  "File CV"
// @Success      200   {object}  ParseCVResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Failure      401   {object}  middlewares.ErrorResponse
// @Failure      413   {object}  middlewares.ErrorResponse
// @Failure      422   {object}  middlewares.ErrorResponse  "CV không đọc được (ảnh scan, sai định dạng) hoặc nội dung không phải CV"
// @Failure      500   {object}  middlewares.ErrorResponse
// @Failure      502   {object}  middlewares.ErrorResponse  "AI service lỗi hoặc không chạy"
// @Router       /api/v1/parse-cv [post]
func (h *CVHandler) ParseCV(c *gin.Context) {
	userID, ok := middlewares.CurrentUserID(c)
	if !ok {
		middlewares.AbortWithError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			middlewares.AbortWithError(c, http.StatusRequestEntityTooLarge, "file_too_large", "CV must be at most 10 MB")
			return
		}
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "multipart field \"file\" is required")
		return
	}
	if fh.Size > MaxCVBytes {
		middlewares.AbortWithError(c, http.StatusRequestEntityTooLarge, "file_too_large", "CV must be at most 10 MB")
		return
	}
	file, err := fh.Open()
	if err != nil {
		middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "cannot read the uploaded file")
		return
	}
	defer file.Close()

	p, err := h.cv.ParseCV(c.Request.Context(), userID, fh.Filename, file)
	if err != nil {
		writeCVError(c, err)
		return
	}
	c.JSON(http.StatusOK, ParseCVResponse{ProfileID: p.ID, Chars: len([]rune(p.RawResume)), Text: p.RawResume})
}

// AnalyzeCV godoc
// @Summary      Phân tích CV (chạy nền)
// @Description  Tạo một lượt phân tích CV hiện tại bằng AI chạy nền: chức danh, số năm kinh nghiệm, kỹ năng, ngoại ngữ...; vector hoá hồ sơ và tạo keyword tìm việc nghĩa rộng (vd "Frontend Developer" -> "Developer"). Trả về ngay với status QUEUED; theo dõi bằng GET /api/v1/analyze-cv/{id} (mất khoảng 30 giây đến vài phút). Khi DONE, dùng id này cho POST /api/v1/find-job/{id}. Mỗi người dùng chỉ có một lượt phân tích đang chạy.
// @Tags         cv
// @Produce      json
// @Security     BearerAuth
// @Success      202  {object}  AnalysisResponse
// @Failure      401  {object}  middlewares.ErrorResponse
// @Failure      404  {object}  middlewares.ErrorResponse  "Chưa tải lên CV"
// @Failure      409  {object}  middlewares.ErrorResponse  "Đang có lượt phân tích chưa xong"
// @Failure      500  {object}  middlewares.ErrorResponse
// @Failure      503  {object}  middlewares.ErrorResponse
// @Router       /api/v1/analyze-cv [post]
func (h *CVHandler) AnalyzeCV(c *gin.Context) {
	userID, ok := middlewares.CurrentUserID(c)
	if !ok {
		middlewares.AbortWithError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	a, err := h.cv.StartAnalysis(c.Request.Context(), userID)
	if err != nil {
		writeCVError(c, err)
		return
	}
	c.Header("Location", "/api/v1/analyze-cv/"+strconv.FormatUint(uint64(a.ID), 10))
	c.JSON(http.StatusAccepted, newAnalysisResponse(&services.AnalysisView{Analysis: a}))
}

// GetAnalysis godoc
// @Summary      Trạng thái và kết quả một lượt phân tích CV
// @Description  status: QUEUED (chờ), ANALYZING (đang phân tích), DONE (xong, có result), FAILED (xem error). outdated = true khi đã tải lên CV mới sau lượt này.
// @Tags         cv
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      int  true  "ID lượt phân tích"
// @Success      200  {object}  AnalysisResponse
// @Failure      401  {object}  middlewares.ErrorResponse
// @Failure      404  {object}  middlewares.ErrorResponse
// @Router       /api/v1/analyze-cv/{id} [get]
func (h *CVHandler) GetAnalysis(c *gin.Context) {
	userID, ok := middlewares.CurrentUserID(c)
	if !ok {
		middlewares.AbortWithError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		middlewares.AbortWithError(c, http.StatusNotFound, "not_found", "CV analysis not found")
		return
	}
	view, err := h.cv.GetAnalysis(c.Request.Context(), userID, uint(id))
	if err != nil {
		writeCVError(c, err)
		return
	}
	c.JSON(http.StatusOK, newAnalysisResponse(view))
}

func newAnalysisResponse(v *services.AnalysisView) AnalysisResponse {
	a := v.Analysis
	out := AnalysisResponse{
		ID: a.ID, ProfileID: a.ProfileID, Status: a.Status, Error: a.Error,
		CreatedAt: a.CreatedAt, FinishedAt: a.FinishedAt, Outdated: v.Outdated,
	}
	if p := v.Profile; p != nil {
		skills := make([]SkillResponse, len(v.Skills))
		for i, s := range v.Skills {
			skills[i] = SkillResponse{Name: s.SkillName, Level: s.Level}
		}
		out.Result = &AnalyzeCVResponse{
			CurrentTitle: p.CurrentTitle, ExpYears: p.ExpYears, Location: p.Location, Summary: p.Summary,
			SearchKeyword: p.SearchKeyword, Skills: skills, ParsedAt: p.ParsedAt, Profile: json.RawMessage(v.Raw),
		}
	}
	return out
}

func writeCVError(c *gin.Context, err error) {
	var rejected *services.CVRejectedError
	switch {
	case errors.As(err, &rejected):
		middlewares.AbortWithError(c, http.StatusUnprocessableEntity, "cv_rejected", rejected.Message)
	case errors.Is(err, services.ErrCVNotUploaded):
		middlewares.AbortWithError(c, http.StatusNotFound, "cv_not_uploaded", "upload a CV with POST /api/v1/parse-cv first")
	case errors.Is(err, services.ErrAnalysisInProgress):
		middlewares.AbortWithError(c, http.StatusConflict, "analysis_in_progress", "a CV analysis is already running; wait for it to finish")
	case errors.Is(err, repositories.ErrNotFound):
		middlewares.AbortWithError(c, http.StatusNotFound, "not_found", "CV analysis not found")
	case errors.Is(err, services.ErrShuttingDown):
		middlewares.AbortWithError(c, http.StatusServiceUnavailable, "unavailable", "service is unavailable, try again later")
	case errors.Is(err, services.ErrAIUnavailable):
		log.Printf("cv %s %s: %v", c.Request.Method, c.FullPath(), err)
		middlewares.AbortWithError(c, http.StatusBadGateway, "ai_service_error", "the AI service failed; try again later")
	case errors.Is(err, context.Canceled):
		middlewares.AbortWithError(c, http.StatusServiceUnavailable, "unavailable", "request was cancelled")
	default:
		log.Printf("cv %s %s: %v", c.Request.Method, c.FullPath(), err)
		middlewares.AbortWithError(c, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
