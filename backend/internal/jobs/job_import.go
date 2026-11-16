package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"notification-job-mb/internal/services"
)

// Sub-folders of the data directory.
const (
	rawDir       = "raw"       // AutoCrawl writes scraped *.json files here for the AI service
	inboxDir     = "inbox"     // the AI service drops analyzed *.json files here
	processedDir = "processed" // imported files
	failedDir    = "failed"    // malformed files, or files that kept failing
)

type jobImporter interface {
	ImportFile(ctx context.Context, path string) (services.ImportResult, error)
}

type JobImportConfig struct {
	DataDir      string
	PollInterval time.Duration
	// MaxAttempts is how many times a file failing on storage errors is tried
	// before it is moved to failed/. Malformed files are moved at once.
	MaxAttempts int
}

func (c *JobImportConfig) applyDefaults() {
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
}

// JobImport watches <DataDir>/inbox and imports every *.json file through the
// job service, then moves it to processed/ or failed/.
//
// Writers must create the file under another name (e.g. "x.json.tmp") and
// rename it to *.json when complete, so a half-written file is never read.
type JobImport struct {
	svc      jobImporter
	cfg      JobImportConfig
	attempts map[string]int // storage failures per file name, kept in memory only
}

// NewJobImport creates the data sub-folders if they are missing.
func NewJobImport(svc jobImporter, cfg JobImportConfig) (*JobImport, error) {
	cfg.applyDefaults()
	for _, dir := range []string{inboxDir, processedDir, failedDir} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, dir), 0o755); err != nil {
			return nil, fmt.Errorf("job import: create data folder: %w", err)
		}
	}
	return &JobImport{svc: svc, cfg: cfg, attempts: make(map[string]int)}, nil
}

// Run scans the inbox immediately and then every PollInterval until ctx ends.
func (j *JobImport) Run(ctx context.Context) {
	log.Printf("job import: watching %s every %s", filepath.Join(j.cfg.DataDir, inboxDir), j.cfg.PollInterval)
	ticker := time.NewTicker(j.cfg.PollInterval)
	defer ticker.Stop()
	for {
		j.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce imports the files currently in the inbox, oldest name first.
func (j *JobImport) RunOnce(ctx context.Context) {
	names, err := j.pending()
	if err != nil {
		log.Printf("job import: %v", err)
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		j.importFile(ctx, name)
	}
}

func (j *JobImport) pending() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(j.cfg.DataDir, inboxDir))
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (j *JobImport) importFile(ctx context.Context, name string) {
	path := filepath.Join(j.cfg.DataDir, inboxDir, name)
	res, err := j.svc.ImportFile(ctx, path)

	switch {
	case err == nil:
		delete(j.attempts, name)
		for _, p := range res.Problems {
			log.Printf("job import: %s: skipped %s", name, p)
		}
		log.Printf("job import: %s: imported %d, skipped %d", name, res.Imported, res.Skipped)
		j.move(name, processedDir)

	case ctx.Err() != nil:
		// Shutting down: the file stays in the inbox and is re-imported next start.

	case errors.Is(err, services.ErrInvalidJobFile):
		delete(j.attempts, name)
		log.Printf("job import: %v", err)
		j.move(name, failedDir)

	default:
		j.attempts[name]++
		n := j.attempts[name]
		log.Printf("job import: %s: attempt %d/%d failed (imported %d before the error): %v",
			name, n, j.cfg.MaxAttempts, res.Imported, err)
		if n >= j.cfg.MaxAttempts {
			delete(j.attempts, name)
			j.move(name, failedDir)
		}
	}
}

// move renames an inbox file into dir, prefixing a timestamp so a file name
// re-sent by the AI service never overwrites an earlier one.
func (j *JobImport) move(name, dir string) {
	src := filepath.Join(j.cfg.DataDir, inboxDir, name)
	dst := filepath.Join(j.cfg.DataDir, dir, time.Now().UTC().Format("20060102T150405Z")+"_"+name)
	if err := os.Rename(src, dst); err != nil {
		log.Printf("job import: move %s to %s: %v", name, dir, err)
	}
}
