package jobs

import (
	"context"
	"fmt"
	"math"
	"sync"

	"notification-job-mb/internal/services"
)

// SourceResult is what CrawlAllSources got from one job source.
type SourceResult struct {
	Source string
	Jobs   []services.AnalyzedJob
	// Err is set when the source failed part way; Jobs then holds what was
	// fetched before the failure.
	Err error
}

// CrawlAllSources searches every job-source adapter (topcv, itviec, jobsgo,
// indeed, vietnamworks, careerviet, joboko, vieclam24h, 123job, careerlink, and linkedin when enabled)
// concurrently for keyword, walking up to maxPages search
// pages per source (0 = all pages), and fetches the detail of each job found.
//
// Unlike AutoCrawl it neither skips jobs already in the DB nor writes files: it
// returns everything found, one SourceResult per source in a fixed order. A
// source that fails or is blocked does not stop the others.
func CrawlAllSources(ctx context.Context, keyword string, maxPages int) []SourceResult {
	if maxPages <= 0 {
		maxPages = math.MaxInt // crawl still stops at the last page the site reports
	}
	noneKnown := func(context.Context, []string) (map[string]struct{}, error) {
		return map[string]struct{}{}, nil
	}

	sites := defaultSites()
	results := make([]SourceResult, len(sites))
	var wg sync.WaitGroup
	for i, s := range sites {
		results[i].Source = s.sourceName()
		wg.Go(func() {
			defer func() {
				// A parser bug on one site must not take down the others.
				if r := recover(); r != nil {
					results[i].Err = fmt.Errorf("%s: panic: %v", s.sourceName(), r)
				}
			}()
			results[i].Jobs, results[i].Err = s.crawl(ctx, keyword, maxPages, make(map[string]struct{}), noneKnown)
		})
	}
	wg.Wait()
	return results
}
