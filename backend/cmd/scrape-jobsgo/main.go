// Command scrape-jobsgo fetches JobsGO jobs (Ha Noi) for a keyword and writes them as UTF-8 JSON.
//
// Search only:
//
//	go run ./cmd/scrape-jobsgo -keyword "frontend developer" -pages 3 -o jobs.json
//
// Search and fetch every job detail (overview, full description, requirements, benefits):
//
//	go run ./cmd/scrape-jobsgo -keyword "golang" -pages 0 -o jobs.json -details jobs-description.json
//
// Fetch details for a jobs file from an earlier run:
//
//	go run ./cmd/scrape-jobsgo -in jobs.json -details jobs-description.json
//
// The details file is saved after every job and jobs already in it are skipped,
// so an interrupted or blocked run can be resumed by running the same command again.
//
// Prefer -o/-details over shell redirection: Windows PowerShell 5.1 re-encodes redirected
// output with the console code page and UTF-16, which garbles Vietnamese text.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"

	"notification-job-mb/internal/adapter/job-sources/jobsgo"
)

func main() {
	keyword := flag.String("keyword", "golang", "job keyword to search")
	pages := flag.Int("pages", 1, "max search pages to fetch (0 = all)")
	output := flag.String("o", "", "jobs list output file (default stdout, unless -details is set)")
	input := flag.String("in", "", "read the jobs list from this file instead of searching")
	detailsOut := flag.String("details", "", "fetch each job detail and write it to this file")
	limit := flag.Int("limit", 0, "max job details to fetch for -details (0 = all)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := jobsgo.NewClient(nil, "")

	var jobs []jobsgo.Job
	if *input != "" {
		if err := readJSON(*input, &jobs); err != nil {
			log.Fatalf("read jobs: %v", err)
		}
		log.Printf("loaded %d jobs from %s", len(jobs), *input)
	} else {
		var err error
		jobs, err = client.FetchAllJobs(ctx, *keyword, *pages)
		if err != nil {
			log.Fatalf("scrape jobsgo: %v", err)
		}
		log.Printf("fetched %d jobs for %q", len(jobs), *keyword)

		switch {
		case *output != "":
			if err := writeJSONFile(*output, jobs); err != nil {
				log.Fatalf("write jobs: %v", err)
			}
		case *detailsOut == "":
			if err := writeJSON(os.Stdout, jobs); err != nil {
				log.Fatalf("encode json: %v", err)
			}
		}
	}

	if *detailsOut != "" {
		if err := fetchDetails(ctx, client, jobs, *detailsOut, *limit); err != nil {
			log.Fatal(err)
		}
	}
}

// fetchDetails fetches the job details not yet present in path and saves path after each one.
func fetchDetails(ctx context.Context, client *jobsgo.Client, jobs []jobsgo.Job, path string, limit int) error {
	var details []jobsgo.JobDetail
	if err := readJSON(path, &details); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	done := make(map[string]struct{}, len(details))
	for _, d := range details {
		done[d.ID] = struct{}{}
	}

	var todo []jobsgo.Job
	for _, j := range jobs {
		if _, ok := done[j.ID]; !ok {
			todo = append(todo, j)
		}
	}
	if limit > 0 && len(todo) > limit {
		todo = todo[:limit]
	}
	log.Printf("details: %d already in %s, %d to fetch", len(details), path, len(todo))
	if len(todo) == 0 {
		return writeJSONFile(path, details)
	}

	n := 0
	_, failures, err := client.FetchJobDetails(ctx, todo, func(d jobsgo.JobDetail) {
		details = append(details, d)
		n++
		log.Printf("details: [%d/%d] %s - %s", n, len(todo), d.ID, d.Title)
		if werr := writeJSONFile(path, details); werr != nil {
			log.Printf("details: save progress: %v", werr)
		}
	})
	if werr := writeJSONFile(path, details); werr != nil {
		return werr
	}
	for _, f := range failures {
		log.Printf("details: failed %s (%s): %s", f.ID, f.URL, f.Error)
	}
	log.Printf("details: fetched %d, failed %d, total %d in %s", n, len(failures), len(details), path)

	switch {
	case errors.Is(err, jobsgo.ErrBlocked):
		return errors.New("details: stopped, JobsGO/Cloudflare is challenging requests; wait a while (30-60 min) and rerun the same command to resume")
	case errors.Is(err, context.Canceled):
		log.Printf("details: interrupted; rerun the same command to resume")
		return nil
	}
	return err
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSONFile writes to a temp file and renames it, so an interrupted write never
// leaves a truncated file behind.
func writeJSONFile(path string, v any) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := writeJSON(tmp, v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
