package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"notification-job-mb/internal/models"
)

type fakeJobStore struct {
	sources   map[string]string // name -> url
	jobs      []models.Job
	skills    map[string][]string // unique identifier -> skills
	upsertErr error
}

func newFakeJobStore() *fakeJobStore {
	return &fakeJobStore{sources: map[string]string{}, skills: map[string][]string{}}
}

func (f *fakeJobStore) EnsureSource(_ context.Context, name, url string) (uint, error) {
	f.sources[name] = url
	return uint(len(f.sources)), nil
}

func (f *fakeJobStore) UpsertWithSkills(_ context.Context, job *models.Job, skills []string) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.jobs = append(f.jobs, *job)
	f.skills[job.UniqueIdentifier] = skills
	return nil
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportFileObject(t *testing.T) {
	store := newFakeJobStore()
	path := writeFile(t, `{
		"source": "topcv",
		"jobs": [
			{"id": "1", "url": "https://www.topcv.vn/viec-lam/a/1.html", "title": " Golang Dev ",
			 "company": "ACME", "description": "Build APIs", "requirements": "3y Go",
			 "salary": "Tới 25 triệu", "posted_at": "2026-10-20",
			 "skills": ["Go", " go ", "Docker  Compose", ""]},
			{"id": "2", "url": "https://www.topcv.vn/viec-lam/b/2.html"},
			{"id": "3", "source": "ITviec", "url": "https://itviec.com/it-jobs/c-3", "title": "Java"}
		]
	}`)

	res, err := NewJobService(store).ImportFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if res.Imported != 2 || res.Skipped != 1 || len(res.Problems) != 1 {
		t.Fatalf("result = %+v, want 2 imported, 1 skipped", res)
	}

	got := store.jobs[0]
	if got.UniqueIdentifier != "topcv:1" || got.Title != "Golang Dev" || got.Status != models.JobStatusAIProcessed {
		t.Errorf("job = %+v", got)
	}
	if got.PostedAt == nil || got.PostedAt.Format("2006-01-02") != "2026-10-20" {
		t.Errorf("PostedAt = %v", got.PostedAt)
	}
	if !strings.Contains(got.Description, "Build APIs") || !strings.Contains(got.Description, "3y Go") {
		t.Errorf("Description = %q", got.Description)
	}
	if want := []string{"Go", "Docker Compose"}; !reflect.DeepEqual(store.skills["topcv:1"], want) {
		t.Errorf("skills = %q, want %q", store.skills["topcv:1"], want)
	}
	if store.jobs[1].UniqueIdentifier != "itviec:3" {
		t.Errorf("second job = %q, want itviec:3", store.jobs[1].UniqueIdentifier)
	}
	if store.sources["itviec"] != "https://itviec.com" {
		t.Errorf("itviec source url = %q", store.sources["itviec"])
	}
}

func TestImportFileArray(t *testing.T) {
	store := newFakeJobStore()
	path := writeFile(t, "\xef\xbb\xbf"+`[{"id": "9", "source": "jobsgo", "url": "https://jobsgo.vn/x.html", "title": "QA"}]`)

	res, err := NewJobService(store).ImportFile(context.Background(), path)
	if err != nil || res.Imported != 1 {
		t.Fatalf("ImportFile = %+v, %v", res, err)
	}
}

func TestImportFileErrors(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		_, err := NewJobService(newFakeJobStore()).ImportFile(context.Background(), writeFile(t, `{"jobs": [`))
		if !errors.Is(err, ErrInvalidJobFile) {
			t.Fatalf("err = %v, want ErrInvalidJobFile", err)
		}
	})
	t.Run("storage", func(t *testing.T) {
		store := newFakeJobStore()
		store.upsertErr = errors.New("db down")
		path := writeFile(t, `[{"id": "1", "source": "topcv", "url": "https://www.topcv.vn/1.html", "title": "Go"}]`)
		_, err := NewJobService(store).ImportFile(context.Background(), path)
		if err == nil || errors.Is(err, ErrInvalidJobFile) {
			t.Fatalf("err = %v, want a retryable error", err)
		}
	})
}
