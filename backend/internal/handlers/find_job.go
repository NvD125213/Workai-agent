package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"notification-job-mb/internal/jobs"
	"notification-job-mb/internal/middlewares"
	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

type findJobService interface {
	Start(ctx context.Context, userID uint, req jobs.StartRequest) (*models.JobSearch, error)
	Get(ctx context.Context, userID, id uint) (*jobs.SearchView, error)
}

type FindJobHandler struct {
	svc findJobService
}

func NewFindJobHandler(svc findJobService) *FindJobHandler {
	return &FindJobHandler{svc: svc}
}

type FindJobRequest struct {
	// Chức danh để chấm điểm độ khớp; mặc định là chức danh hiện tại trong CV.
	DesiredTitle string `json:"desired_title" binding:"max=255" example:"Fullstack Developer"`
	// Keyword cào job; mặc định là keyword nghĩa rộng từ CV (vd "Developer").
	Keyword  string `json:"keyword" binding:"max=100" example:"Developer"`
	Location string `json:"location" binding:"max=100" example:"Hà Nội"`
}

type MatchResponse struct {
	JobID   uint    `json:"job_id" example:"42"`
	Rank    int     `json:"rank" example:"1"`
	Score   float64 `json:"score" example:"78.04"`
	Passed  bool    `json:"passed" example:"true"` // luôn true với lượt tìm mới (chỉ lưu job đạt)
	Title   string  `json:"title" example:"Fresher Fullstack Developer"`
	Company string  `json:"company" example:"Công ty ABC"`
	URL     string  `json:"url" example:"https://www.topcv.vn/viec-lam/..."`
	Source  string  `json:"source" example:"topcv"`
	Salary  string  `json:"salary" example:"10 - 15 triệu"`
	// Details: điểm thành phần, hệ số phạt, kỹ năng có/thiếu.
	Details json.RawMessage `json:"details" swaggertype:"object"`
}

type SearchResponse struct {
	ID uint `json:"id" example:"7"`
	// AnalysisID: lượt phân tích CV (POST /api/v1/analyze-cv) dùng để tìm.
	AnalysisID *uint `json:"analysis_id" example:"3"`
	// ExpFilter: có lọc theo khoảng kinh nghiệm và xếp hạng theo lương hay không (query exp).
	ExpFilter    bool       `json:"exp_filter" example:"true"`
	Status       string     `json:"status" example:"DONE" enums:"QUEUED,CRAWLING,RANKING,DONE,FAILED"`
	Keyword      string     `json:"keyword" example:"Developer"`
	DesiredTitle string     `json:"desired_title" example:"Full-Stack Developer"`
	Location     string     `json:"location" example:"Hà Nội"`
	JobsFound    int        `json:"jobs_found" example:"96"`
	MatchesFound int        `json:"matches_found" example:"2"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	// Summary: số job đã cào / bị loại / được chấm / đạt, theo từng nguồn, các lý do loại phổ biến.
	Summary json.RawMessage `json:"summary,omitempty" swaggertype:"object"`
	// Matches: job đạt ngưỡng điểm, xếp theo điểm; chỉ có khi status = DONE.
	Matches []MatchResponse `json:"matches,omitempty"`
}

// FindJob godoc
// @Summary      Tìm việc phù hợp với một lượt phân tích CV
// @Description  Tạo một lượt tìm chạy nền cho lượt phân tích CV {id} (POST /api/v1/analyze-cv, phải ở trạng thái DONE): cào tất cả nguồn (topcv, itviec, jobsgo, indeed, vietnamworks) cùng lúc theo keyword từ CV, lọc cứng (kinh nghiệm, kỹ năng bắt buộc) rồi chấm điểm. exp=true: chỉ giữ job có số năm yêu cầu hợp với ứng viên (≤2 năm: job 0-2 năm, 3 năm: 2-3 năm, 4-5 năm: 2-5 năm, trên 5 năm: từ 3 năm) và xếp hạng thêm theo lương (nếu tin có ghi) rồi tới kinh nghiệm. Chỉ job đạt ngưỡng điểm mới được lưu. Trả về ngay; theo dõi bằng GET /api/v1/find-job/{id lượt tìm}. Mỗi người dùng chỉ có một lượt tìm đang chạy.
// @Tags         find-job
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      int             true   "ID lượt phân tích CV"
// @Param        exp   query     bool            false  "Lọc theo khoảng kinh nghiệm, xếp hạng theo lương rồi kinh nghiệm"  default(false)
// @Param        body  body      FindJobRequest  false  "Tuỳ chọn, bỏ trống thì lấy từ CV"
// @Success      202   {object}  SearchResponse
// @Failure      400   {object}  middlewares.ErrorResponse
// @Failure      401   {object}  middlewares.ErrorResponse
// @Failure      404   {object}  middlewares.ErrorResponse  "Không có lượt phân tích CV này"
// @Failure      409   {object}  middlewares.ErrorResponse  "Đang có lượt tìm chưa xong, lượt phân tích chưa xong / thất bại, hoặc đã có CV mới"
// @Failure      503   {object}  middlewares.ErrorResponse
// @Router       /api/v1/find-job/{id} [post]
func (h *FindJobHandler) Start(c *gin.Context) {
	userID, ok := middlewares.CurrentUserID(c)
	if !ok {
		middlewares.AbortWithError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	analysisID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		middlewares.AbortWithError(c, http.StatusNotFound, "analysis_not_found", "CV analysis not found")
		return
	}
	exp := false
	if v := c.Query("exp"); v != "" {
		if exp, err = strconv.ParseBool(v); err != nil {
			middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "exp must be true or false")
			return
		}
	}
	var req FindJobRequest
	if c.Request.ContentLength != 0 { // the body is optional
		if err := c.ShouldBindJSON(&req); err != nil {
			middlewares.AbortWithError(c, http.StatusBadRequest, "invalid_request", "desired_title, keyword or location is invalid")
			return
		}
	}

	s, err := h.svc.Start(c.Request.Context(), userID, jobs.StartRequest{
		AnalysisID: uint(analysisID), ExpFilter: exp,
		DesiredTitle: req.DesiredTitle, Keyword: req.Keyword, Location: req.Location,
	})
	if errors.Is(err, repositories.ErrNotFound) {
		middlewares.AbortWithError(c, http.StatusNotFound, "analysis_not_found", "CV analysis not found")
		return
	}
	if err != nil {
		writeFindJobError(c, err)
		return
	}
	c.Header("Location", "/api/v1/find-job/"+strconv.FormatUint(uint64(s.ID), 10))
	c.JSON(http.StatusAccepted, newSearchResponse(&jobs.SearchView{Search: s}))
}

// GetFindJob godoc
// @Summary      Trạng thái và kết quả một lượt tìm việc
// @Description  status: QUEUED (chờ), CRAWLING (đang cào), RANKING (đang chấm điểm), DONE (xong, có matches), FAILED (xem error). Khi DONE, error có thể liệt kê nguồn bị lỗi trong khi các nguồn khác vẫn có kết quả.
// @Tags         find-job
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      int  true  "ID lượt tìm"
// @Success      200  {object}  SearchResponse
// @Failure      401  {object}  middlewares.ErrorResponse
// @Failure      404  {object}  middlewares.ErrorResponse
// @Router       /api/v1/find-job/{id} [get]
func (h *FindJobHandler) Get(c *gin.Context) {
	userID, ok := middlewares.CurrentUserID(c)
	if !ok {
		middlewares.AbortWithError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		middlewares.AbortWithError(c, http.StatusNotFound, "not_found", "job search not found")
		return
	}
	view, err := h.svc.Get(c.Request.Context(), userID, uint(id))
	if err != nil {
		writeFindJobError(c, err)
		return
	}
	c.JSON(http.StatusOK, newSearchResponse(view))
}

func writeFindJobError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, jobs.ErrProfileNotAnalyzed):
		middlewares.AbortWithError(c, http.StatusConflict, "cv_not_analyzed", "analyze your CV with POST /api/v1/analyze-cv first")
	case errors.Is(err, jobs.ErrAnalysisNotDone):
		middlewares.AbortWithError(c, http.StatusConflict, "analysis_not_done", "the CV analysis is still running or failed; check GET /api/v1/analyze-cv/{id}")
	case errors.Is(err, jobs.ErrAnalysisOutdated):
		middlewares.AbortWithError(c, http.StatusConflict, "analysis_outdated", "a newer CV was uploaded after this analysis; analyze again")
	case errors.Is(err, jobs.ErrNoKeyword):
		middlewares.AbortWithError(c, http.StatusBadRequest, "keyword_required", "no keyword could be derived from the CV; send \"keyword\"")
	case errors.Is(err, jobs.ErrSearchInProgress):
		middlewares.AbortWithError(c, http.StatusConflict, "search_in_progress", "a job search is already running; wait for it to finish")
	case errors.Is(err, repositories.ErrNotFound):
		middlewares.AbortWithError(c, http.StatusNotFound, "not_found", "job search not found")
	case errors.Is(err, jobs.ErrShuttingDown), errors.Is(err, context.Canceled):
		middlewares.AbortWithError(c, http.StatusServiceUnavailable, "unavailable", "service is unavailable, try again later")
	default:
		log.Printf("find job %s %s: %v", c.Request.Method, c.FullPath(), err)
		middlewares.AbortWithError(c, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func newSearchResponse(v *jobs.SearchView) SearchResponse {
	s := v.Search
	out := SearchResponse{
		ID: s.ID, AnalysisID: s.AnalysisID, ExpFilter: s.ExpFilter,
		Status: s.Status, Keyword: s.Keyword, DesiredTitle: s.DesiredTitle, Location: s.Location,
		JobsFound: s.JobsFound, MatchesFound: s.MatchesFound, Error: s.Error,
		CreatedAt: s.CreatedAt, FinishedAt: s.FinishedAt, Summary: json.RawMessage(s.Summary),
	}
	for _, m := range v.Matches {
		r := MatchResponse{
			JobID: m.JobID, Rank: m.Rank, Score: m.Score, Passed: m.Status != models.AIMatchStatusSkipped,
			Details: json.RawMessage(m.Details),
		}
		if m.Job != nil {
			r.Title, r.Company, r.URL, r.Salary = m.Job.Title, m.Job.Company, m.Job.URL, m.Job.RawSalary
			if m.Job.Source != nil {
				r.Source = m.Job.Source.Name
			}
		}
		out.Matches = append(out.Matches, r)
	}
	return out
}
