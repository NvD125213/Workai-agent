package integration

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/pgvector/pgvector-go"

	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

const testModel = "BAAI/bge-m3"

// unitVec trả về vector 1024 chiều có cosine = cos(angle) với trục đầu tiên.
func unitVec(angle float64) pgvector.Vector {
	v := make([]float32, 1024)
	v[0], v[1] = float32(math.Cos(angle)), float32(math.Sin(angle))
	return pgvector.NewVector(v)
}

func intPtr(v int) *int { return &v }

func TestFindCandidateJobs(t *testing.T) {
	db := testDB(t)
	if err := db.Exec("TRUNCATE sources, jobs, job_skills RESTART IDENTITY CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	repo := repositories.NewJobRepository(db)
	ctx := context.Background()

	vec := func(a float64) *pgvector.Vector { v := unitVec(a); return &v }
	jobs := []models.Job{
		// Gần hồ sơ nhất nhưng là Senior: phải bị loại với ứng viên 0 năm, không chiếm chỗ trong LIMIT
		{UniqueIdentifier: "senior", Title: "Senior Dev", MinYears: intPtr(1), RequiredYears: intPtr(3), Embedding: vec(0.1)},
		{UniqueIdentifier: "junior", Title: "Junior Dev", MinYears: intPtr(1), RequiredYears: intPtr(1), Embedding: vec(0.2)},
		// Phân tích trước khi có required_years: dùng tạm min_years
		{UniqueIdentifier: "legacy", Title: "Dev", MinYears: intPtr(1), Embedding: vec(0.3)},
		{UniqueIdentifier: "legacy-mid", Title: "Dev", MinYears: intPtr(2), Embedding: vec(0.35)},
		// Không ghi yêu cầu: không lọc
		{UniqueIdentifier: "open", Title: "Dev", Embedding: vec(0.4)},
		// Khác mô hình nhúng hoặc chưa nhúng: không bao giờ trả về
		{UniqueIdentifier: "other-model", Title: "Dev", Embedding: vec(0)},
		{UniqueIdentifier: "no-embedding", Title: "Dev"},
	}
	for i := range jobs {
		jobs[i].URL, jobs[i].Status = "u", models.JobStatusAIProcessed
		jobs[i].EmbeddingModel = testModel
		if jobs[i].UniqueIdentifier == "other-model" {
			jobs[i].EmbeddingModel = "other"
		}
		if err := db.Create(&jobs[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&[]models.JobSkill{
		{JobID: jobs[1].ID, SkillName: "react"}, {JobID: jobs[1].ID, SkillName: "java"},
	}).Error; err != nil {
		t.Fatal(err)
	}

	ids := func(got []repositories.CandidateJob) []string {
		out := make([]string, len(got))
		for i, j := range got {
			out[i] = j.UniqueIdentifier
		}
		return out
	}
	profile := unitVec(0)

	// Ứng viên 0 năm: chỉ nhận job cần <= 1 năm, theo thứ tự cosine giảm dần
	got, err := repo.FindCandidateJobs(ctx, repositories.CandidateQuery{
		Embedding: profile, EmbeddingModel: testModel, ExpYears: intPtr(0), Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"junior", "legacy"}; !slices.Equal(ids(got), want) {
		t.Fatalf("ExpYears=0 Limit=2: got %v, want %v", ids(got), want)
	}
	if math.Abs(got[0].Cosine-math.Cos(0.2)) > 1e-4 {
		t.Errorf("cosine = %v, want %v", got[0].Cosine, math.Cos(0.2))
	}
	if len(got[0].JobSkills) != 2 || got[0].JobSkills[0].SkillName != "react" {
		t.Errorf("skills = %+v, want react, java", got[0].JobSkills)
	}

	got, err = repo.FindCandidateJobs(ctx, repositories.CandidateQuery{
		Embedding: profile, EmbeddingModel: testModel, ExpYears: intPtr(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"junior", "legacy", "open"}; !slices.Equal(ids(got), want) {
		t.Errorf("ExpYears=0: got %v, want %v", ids(got), want)
	}

	// Chưa biết số năm: không lọc kinh nghiệm
	got, err = repo.FindCandidateJobs(ctx, repositories.CandidateQuery{Embedding: profile, EmbeddingModel: testModel})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"senior", "junior", "legacy", "legacy-mid", "open"}; !slices.Equal(ids(got), want) {
		t.Errorf("ExpYears=nil: got %v, want %v", ids(got), want)
	}

	if _, err := repo.FindCandidateJobs(ctx, repositories.CandidateQuery{Embedding: profile}); err == nil {
		t.Error("missing embedding model: want error")
	}
}
