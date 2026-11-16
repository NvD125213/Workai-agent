// Command scrape-vieclam24h fetches Việc Làm 24h (vieclam24h.vn) jobs for a keyword and
// province (default Ha Noi) from the site's JSON API and writes them as UTF-8 JSON.
//
// Every job comes complete from the search API (requirements, salary, experience,
// deadline...), so there is no separate details step:
//
//	go run ./cmd/scrape-vieclam24h -keyword "video editor" -pages 2 -o jobs.json
//	go run ./cmd/scrape-vieclam24h -keyword "kế toán" -province "TP.HCM" -o jobs.json
//
// Prefer -o over shell redirection: Windows PowerShell 5.1 re-encodes redirected
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

	"notification-job-mb/internal/adapter/job-sources/vieclam24h"
)

func main() {
	keyword := flag.String("keyword", "golang", "job keyword to search")
	pages := flag.Int("pages", 1, "max search pages to fetch, 50 jobs each (0 = all)")
	province := flag.String("province", "Hà Nội", `province name or slug, e.g. "TP.HCM", "da-nang" ("" = every province)`)
	token := flag.String("token", "", "bearer token, if the site's guest token stops working")
	output := flag.String("o", "", "output file (default stdout)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := vieclam24h.NewClient(nil, "")
	if err := client.SetProvince(*province); err != nil {
		log.Fatal(err)
	}
	if *token != "" {
		client.SetToken(*token)
	}

	jobs, err := client.FetchAllJobs(ctx, *keyword, *pages)
	where := client.Province().Name
	if where == "" {
		where = "every province"
	}
	log.Printf("fetched %d jobs for %q in %s", len(jobs), *keyword, where)
	if err != nil {
		if errors.Is(err, vieclam24h.ErrBlocked) {
			log.Printf("the API refused the token; pass a fresh one with -token (DevTools > Network > get-job-list > authorization)")
		}
		if len(jobs) == 0 {
			log.Fatalf("scrape vieclam24h: %v", err)
		}
		log.Printf("scrape vieclam24h: stopped early: %v", err)
	}

	if *output == "" {
		if err := writeJSON(os.Stdout, jobs); err != nil {
			log.Fatalf("encode json: %v", err)
		}
		return
	}
	if err := writeJSONFile(*output, jobs); err != nil {
		log.Fatalf("write jobs: %v", err)
	}
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
