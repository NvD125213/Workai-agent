package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"notification-job-mb/internal/services"
)

type fakeImporter struct {
	errs  map[string]error // by file name
	calls map[string]int
}

func (f *fakeImporter) ImportFile(_ context.Context, path string) (services.ImportResult, error) {
	name := filepath.Base(path)
	f.calls[name]++
	if err := f.errs[name]; err != nil {
		return services.ImportResult{}, err
	}
	return services.ImportResult{Imported: 1}, nil
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestJobImportRunOnce(t *testing.T) {
	dataDir := t.TempDir()
	imp := &fakeImporter{
		errs: map[string]error{
			"bad.json":   fmt.Errorf("%w: broken", services.ErrInvalidJobFile),
			"retry.json": errors.New("db down"),
		},
		calls: map[string]int{},
	}
	j, err := NewJobImport(imp, JobImportConfig{DataDir: dataDir, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}

	inbox := filepath.Join(dataDir, inboxDir)
	for _, name := range []string{"ok.json", "bad.json", "retry.json", "partial.json.tmp"} {
		if err := os.WriteFile(filepath.Join(inbox, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	j.RunOnce(context.Background())
	if got := countFiles(t, filepath.Join(dataDir, processedDir)); got != 1 {
		t.Errorf("processed = %d, want 1", got)
	}
	if got := countFiles(t, filepath.Join(dataDir, failedDir)); got != 1 {
		t.Errorf("failed after first run = %d, want 1 (bad.json)", got)
	}
	if got := countFiles(t, inbox); got != 2 {
		t.Errorf("inbox = %d, want 2 (retry.json, partial.json.tmp)", got)
	}
	if imp.calls["partial.json.tmp"] != 0 {
		t.Error("temp file was imported")
	}

	j.RunOnce(context.Background())
	if got := countFiles(t, filepath.Join(dataDir, failedDir)); got != 2 {
		t.Errorf("failed after second run = %d, want 2 (retry.json gave up)", got)
	}
	if imp.calls["retry.json"] != 2 {
		t.Errorf("retry.json calls = %d, want 2", imp.calls["retry.json"])
	}
}
