package integration

import (
	"context"
	"testing"

	"notification-job-mb/internal/models"
	"notification-job-mb/internal/repositories"
)

func TestJobRepositoryUpsertWithSkills(t *testing.T) {
	db := testDB(t)
	if err := db.Exec("TRUNCATE sources, jobs, job_skills RESTART IDENTITY CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	repo := repositories.NewJobRepository(db)
	ctx := context.Background()

	srcID, err := repo.EnsureSource(ctx, "topcv", "https://www.topcv.vn")
	if err != nil {
		t.Fatal(err)
	}
	again, err := repo.EnsureSource(ctx, "topcv", "https://www.topcv.vn")
	if err != nil || again != srcID {
		t.Fatalf("EnsureSource again = %d, %v; want %d", again, err, srcID)
	}

	first := &models.Job{SourceID: &srcID, UniqueIdentifier: "topcv:1", Title: "Go", URL: "u", Status: models.JobStatusAIProcessed}
	if err := repo.UpsertWithSkills(ctx, first, []string{"Go", "Docker"}); err != nil {
		t.Fatal(err)
	}
	second := &models.Job{SourceID: &srcID, UniqueIdentifier: "topcv:1", Title: "Senior Go", URL: "u", Status: models.JobStatusAIProcessed}
	if err := repo.UpsertWithSkills(ctx, second, []string{"Go", "Kafka", "Redis"}); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("upsert ID = %d, want existing %d", second.ID, first.ID)
	}

	var job models.Job
	if err := db.Preload("JobSkills").Take(&job, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Title != "Senior Go" {
		t.Errorf("title = %q, want updated", job.Title)
	}
	if len(job.JobSkills) != 3 {
		t.Errorf("skills = %d, want 3 (replaced)", len(job.JobSkills))
	}

	existing, err := repo.ExistingUniqueIDs(ctx, []string{"topcv:1", "topcv:2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := existing["topcv:1"]; !ok || len(existing) != 1 {
		t.Errorf("ExistingUniqueIDs = %v, want only topcv:1", existing)
	}
}
