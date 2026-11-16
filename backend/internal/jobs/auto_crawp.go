package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	onetwothreejob "notification-job-mb/internal/adapter/job-sources/123job"
	"notification-job-mb/internal/adapter/job-sources/careerlink"
	"notification-job-mb/internal/adapter/job-sources/careerviet"
	"notification-job-mb/internal/adapter/job-sources/indeed"
	"notification-job-mb/internal/adapter/job-sources/itviec"
	"notification-job-mb/internal/adapter/job-sources/joboko"
	"notification-job-mb/internal/adapter/job-sources/jobsgo"
	"notification-job-mb/internal/adapter/job-sources/linkedin"
	"notification-job-mb/internal/adapter/job-sources/topcv"
	"notification-job-mb/internal/adapter/job-sources/vieclam24h"
	"notification-job-mb/internal/adapter/job-sources/vietnamworks"
	"notification-job-mb/internal/adapter/wgtunnel"
	"notification-job-mb/internal/services"
)

// knownJobStore reports which "<source>:<id>" identifiers are already saved.
type knownJobStore interface {
	ExistingUniqueIDs(ctx context.Context, ids []string) (map[string]struct{}, error)
}

type AutoCrawlConfig struct {
	DataDir  string
	Keywords []string
	// Sources limits the crawl to these adapters (topcv, itviec, jobsgo, indeed,
	// vietnamworks, careerviet, joboko, vieclam24h, 123job, careerlink, and linkedin when
	// enabled with ConfigureSites).
	// Empty means all of them.
	Sources []string
	// Every is the schedule period. Runs are aligned to local midnight, so 3h
	// runs at 00:00, 03:00, 06:00, ... like the cron "0 */3 * * *".
	Every      time.Duration
	MaxPages   int  // search pages per keyword and source
	RunOnStart bool // also run once right after start
}

func (c *AutoCrawlConfig) applyDefaults() {
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if len(c.Keywords) == 0 {
		c.Keywords = []string{"golang"}
	}
	if c.Every <= 0 {
		c.Every = 3 * time.Hour
	}
	if c.MaxPages <= 0 {
		c.MaxPages = 5
	}
}

// AutoCrawl runs the job-source adapters on a schedule and writes the new jobs
// to <DataDir>/raw/<source>_<time>.json (a services.JobFile) for the AI service.
//
// A job is skipped, before its detail page is fetched, when it is already in
// the Job table or still waiting in raw/ or inbox/ to be analyzed and imported.
type AutoCrawl struct {
	store knownJobStore
	cfg   AutoCrawlConfig
	sites []crawler
	now   func() time.Time
}

func NewAutoCrawl(store knownJobStore, cfg AutoCrawlConfig) (*AutoCrawl, error) {
	cfg.applyDefaults()
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, rawDir), 0o755); err != nil {
		return nil, fmt.Errorf("auto crawl: create data folder: %w", err)
	}

	all := defaultSites()
	var sites []crawler
	if len(cfg.Sources) == 0 {
		sites = all
	} else {
		byName := make(map[string]crawler, len(all))
		for _, s := range all {
			byName[s.sourceName()] = s
		}
		for _, name := range cfg.Sources {
			name = strings.ToLower(strings.TrimSpace(name))
			s, ok := byName[name]
			if !ok && name == linkedin.Source {
				return nil, errors.New("auto crawl: source linkedin is disabled (set LINKEDIN_ENABLED=true)")
			}
			if !ok {
				return nil, fmt.Errorf("auto crawl: unknown source %q", name)
			}
			sites = append(sites, s)
		}
	}
	return &AutoCrawl{store: store, cfg: cfg, sites: sites, now: time.Now}, nil
}

// Run crawls at every scheduled time until ctx ends. A run that overruns the
// next scheduled time delays it rather than overlapping.
func (a *AutoCrawl) Run(ctx context.Context) {
	if a.cfg.RunOnStart {
		a.RunOnce(ctx)
	}
	for {
		next := nextRun(a.now(), a.cfg.Every)
		log.Printf("auto crawl: next run at %s", next.Format(time.RFC3339))
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		a.RunOnce(ctx)
	}
}

// nextRun returns the first time after now that is a whole number of every
// periods past local midnight.
func nextRun(now time.Time, every time.Duration) time.Time {
	y, m, d := now.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	n := now.Sub(midnight)/every + 1
	return midnight.Add(n * every)
}

// RunOnce crawls every source concurrently (they are different sites, each
// with its own rate limiter) and returns when all are done.
func (a *AutoCrawl) RunOnce(ctx context.Context) {
	started := a.now()
	pending := a.pendingIDs()
	log.Printf("auto crawl: start, %d source(s), keywords %q, %d job(s) pending analysis",
		len(a.sites), a.cfg.Keywords, len(pending))

	known := func(ctx context.Context, ids []string) (map[string]struct{}, error) {
		found, err := a.store.ExistingUniqueIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, ok := pending[id]; ok {
				found[id] = struct{}{}
			}
		}
		return found, nil
	}

	var wg sync.WaitGroup
	for _, site := range a.sites {
		wg.Go(func() { a.crawlSite(ctx, site, known, started) })
	}
	wg.Wait()
	log.Printf("auto crawl: done in %s", a.now().Sub(started).Round(time.Second))
}

func (a *AutoCrawl) crawlSite(ctx context.Context, site crawler, known knownFunc, started time.Time) {
	name := site.sourceName()
	defer func() {
		// A parser bug on one site must not stop the worker.
		if r := recover(); r != nil {
			log.Printf("auto crawl: %s: panic: %v", name, r)
		}
	}()

	seen := make(map[string]struct{}) // IDs met in this run, across keywords
	var out []services.AnalyzedJob
	for _, kw := range a.cfg.Keywords {
		jobs, err := site.crawl(ctx, kw, a.cfg.MaxPages, seen, known)
		out = append(out, jobs...)
		if err != nil {
			log.Printf("auto crawl: %s %q: %v", name, kw, err)
			if ctx.Err() != nil || site.isBlocked(err) {
				break // the next keyword would be blocked too
			}
		}
	}

	if len(out) == 0 {
		log.Printf("auto crawl: %s: no new jobs", name)
		return
	}
	path := filepath.Join(a.cfg.DataDir, rawDir,
		fmt.Sprintf("%s_%s.json", name, started.UTC().Format("20060102T150405Z")))
	file := services.JobFile{Source: name, Jobs: out}
	if err := writeJSONFile(path, file); err != nil {
		log.Printf("auto crawl: %s: save %d job(s): %v", name, len(out), err)
		return
	}
	log.Printf("auto crawl: %s: saved %d new job(s) to %s", name, len(out), path)
}

// pendingIDs collects the "<source>:<id>" of jobs in raw/ and inbox/, i.e.
// crawled but not imported yet. Unreadable files are ignored: they are either
// being moved by the importer or will end up in failed/.
func (a *AutoCrawl) pendingIDs() map[string]struct{} {
	ids := make(map[string]struct{})
	for _, dir := range []string{rawDir, inboxDir} {
		paths, _ := filepath.Glob(filepath.Join(a.cfg.DataDir, dir, "*.json"))
		for _, p := range paths {
			file, err := services.ReadJobFile(p)
			if err != nil {
				continue
			}
			for _, j := range file.Jobs {
				src := j.Source
				if src == "" {
					src = file.Source
				}
				if src != "" && j.ID != "" {
					ids[strings.ToLower(src)+":"+j.ID] = struct{}{}
				}
			}
		}
	}
	return ids
}

// ── Generic per-site crawl ──────────────────────────────────────────────────

type knownFunc func(ctx context.Context, uniqueIDs []string) (map[string]struct{}, error)

type crawler interface {
	sourceName() string
	crawl(ctx context.Context, keyword string, maxPages int, seen map[string]struct{}, known knownFunc) ([]services.AnalyzedJob, error)
	isBlocked(err error) bool
}

// site adapts one job-source adapter: J is its search-result job type, D its
// detail type.
type site[J, D any] struct {
	name    string
	search  func(ctx context.Context, keyword string, page int) (jobs []J, totalPages int, err error)
	jobID   func(J) string
	details func(ctx context.Context, jobs []J) ([]D, error)
	convert func(D) services.AnalyzedJob
	blocked error
	// endOfResults reports a search error that only means "no more pages" (optional).
	endOfResults func(error) bool
}

func (s *site[J, D]) sourceName() string { return s.name }

func (s *site[J, D]) isBlocked(err error) bool { return errors.Is(err, s.blocked) }

// crawl walks up to maxPages search pages for keyword, keeps the jobs that are
// neither in seen nor known, then fetches their details.
//
// Paging stops at an empty page, at the last page the site reports, or at a
// page whose jobs were all seen already (sites repeat the last page for
// out-of-range numbers). A page whose jobs are all known is skipped but does
// not stop paging, since results are not always sorted by date.
func (s *site[J, D]) crawl(ctx context.Context, keyword string, maxPages int, seen map[string]struct{}, known knownFunc) ([]services.AnalyzedJob, error) {
	var (
		todo      []J
		searchErr error
		skipped   int
		paged     = make(map[string]struct{}) // IDs on this keyword's pages
	)
	for page := 1; page <= maxPages; page++ {
		jobs, totalPages, err := s.search(ctx, keyword, page)
		if err != nil {
			if page == 1 || s.endOfResults == nil || !s.endOfResults(err) {
				searchErr = fmt.Errorf("search page %d: %w", page, err)
			}
			break
		}
		if len(jobs) == 0 {
			break
		}

		var fresh []J
		var ids []string
		repeated := true
		for _, j := range jobs {
			id := s.jobID(j)
			if id == "" {
				continue
			}
			if _, ok := paged[id]; !ok {
				paged[id] = struct{}{}
				repeated = false
			}
			if _, ok := seen[id]; ok {
				continue // already handled in this run, maybe for another keyword
			}
			seen[id] = struct{}{}
			fresh = append(fresh, j)
			ids = append(ids, s.name+":"+id)
		}
		if repeated {
			break
		}

		existing, err := known(ctx, ids)
		if err != nil {
			return nil, err // without the DB check every job would look new
		}
		for i, j := range fresh {
			if _, ok := existing[ids[i]]; ok {
				skipped++
				continue
			}
			todo = append(todo, j)
		}

		if totalPages == 0 || page >= totalPages {
			break
		}
	}
	log.Printf("auto crawl: %s %q: %d new, %d already saved", s.name, keyword, len(todo), skipped)

	if len(todo) == 0 || ctx.Err() != nil || s.isBlocked(searchErr) {
		return nil, searchErr
	}
	details, err := s.details(ctx, todo)
	out := make([]services.AnalyzedJob, 0, len(details))
	for _, d := range details {
		if j := s.convert(d); j.ID != "" {
			j.Source = s.name
			out = append(out, j)
		}
	}
	return out, errors.Join(searchErr, err)
}

// ── Adapters ────────────────────────────────────────────────────────────────

// siteClients holds one client per job site for the whole process. Each client
// sends one request at a time with a random pause in between, so sharing it
// makes every crawl in the process (AutoCrawl runs, concurrent find-job
// searches) queue behind the same limit instead of hitting a site in parallel.
type siteClients struct {
	topcv        *topcv.Client
	itviec       *itviec.Client
	jobsgo       *jobsgo.Client
	indeed       *indeed.Client
	vietnamworks *vietnamworks.Client
	careerviet   *careerviet.Client
	joboko       *joboko.Client
	vieclam24h   *vieclam24h.Client
	job123       *onetwothreejob.Client
	careerlink   *careerlink.Client
	linkedin     *linkedin.Client // nil unless enabled with ConfigureSites
}

// SiteOptions sets up the optional job sources.
type SiteOptions struct {
	LinkedIn bool
	// LinkedInWireGuardConfig is a wg-quick .conf file; LinkedIn requests go
	// through that VPN (started on the first request). Empty = direct connection.
	LinkedInWireGuardConfig string
}

var (
	siteOptionsMu  sync.Mutex
	siteOptions    SiteOptions
	clientsCreated bool
)

// ConfigureSites must be called before the first crawl (NewAutoCrawl,
// CrawlAllSources, find-job); the site clients are created once per process.
func ConfigureSites(o SiteOptions) error {
	if o.LinkedIn && o.LinkedInWireGuardConfig != "" {
		if err := wgtunnel.CheckConfig(o.LinkedInWireGuardConfig); err != nil {
			return fmt.Errorf("linkedin: %w", err)
		}
	}
	siteOptionsMu.Lock()
	defer siteOptionsMu.Unlock()
	if clientsCreated {
		return errors.New("jobs: ConfigureSites called after the site clients were created")
	}
	siteOptions = o
	return nil
}

var sharedClients = sync.OnceValue(func() siteClients {
	siteOptionsMu.Lock()
	o := siteOptions
	clientsCreated = true
	siteOptionsMu.Unlock()

	c := siteClients{
		topcv:        topcv.NewClient(topcv.NewHTTPClient(30*time.Second), "", ""),
		itviec:       itviec.NewClient(nil, ""),
		jobsgo:       jobsgo.NewClient(nil, ""),
		indeed:       indeed.NewClient(nil, ""),
		vietnamworks: vietnamworks.NewClient(nil, ""),
		careerviet:   careerviet.NewClient(nil, ""),
		joboko:       joboko.NewClient(nil, ""),
		vieclam24h:   vieclam24h.NewClient(nil, ""),
		job123:       onetwothreejob.NewClient(nil, ""),
		careerlink:   careerlink.NewClient(nil, ""),
	}
	if o.LinkedIn {
		var dial linkedin.DialFunc
		if o.LinkedInWireGuardConfig != "" {
			dial = wgtunnel.NewLazyDialer(o.LinkedInWireGuardConfig).DialContext
		}
		c.linkedin = linkedin.NewClient(linkedin.NewHTTPClient(30*time.Second, dial), "")
	}
	return c
})

func defaultSites() []crawler {
	clients := sharedClients()
	tc, ic, jc, dc, vc := clients.topcv, clients.itviec, clients.jobsgo, clients.indeed, clients.vietnamworks

	sites := []crawler{
		&site[topcv.Job, topcv.JobDetail]{
			name: topcv.Source,
			search: func(ctx context.Context, kw string, page int) ([]topcv.Job, int, error) {
				r, err := tc.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j topcv.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []topcv.Job) ([]topcv.JobDetail, error) {
				d, _, err := tc.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d topcv.JobDetail) services.AnalyzedJob {
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: d.Overview.DatePosted,
					Location: d.Overview.Location, Experience: d.Overview.Experience,
					Skills:   concat(d.Skills.Required, d.Skills.Preferred, d.Skills.All),
					Deadline: firstNonEmpty(d.Overview.ValidThrough, d.Overview.Deadline),
				}
			},
			blocked: topcv.ErrBlocked,
		},
		&site[itviec.Job, itviec.JobDetail]{
			name: itviec.Source,
			search: func(ctx context.Context, kw string, page int) ([]itviec.Job, int, error) {
				r, err := ic.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j itviec.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []itviec.Job) ([]itviec.JobDetail, error) {
				d, _, err := ic.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d itviec.JobDetail) services.AnalyzedJob {
				// ITviec only shows a relative posting date ("6 ngày trước") and no deadline.
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, Location: d.Overview.Location, Skills: d.Skills.All,
				}
			},
			blocked: itviec.ErrBlocked,
		},
		&site[jobsgo.Job, jobsgo.JobDetail]{
			name: jobsgo.Source,
			search: func(ctx context.Context, kw string, page int) ([]jobsgo.Job, int, error) {
				r, err := jc.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j jobsgo.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []jobsgo.Job) ([]jobsgo.JobDetail, error) {
				d, _, err := jc.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d jobsgo.JobDetail) services.AnalyzedJob {
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: d.Overview.DatePosted,
					Location: d.Overview.Location, Experience: d.Overview.Experience,
					Deadline: firstNonEmpty(d.Overview.ValidThrough, d.Overview.Deadline),
				}
			},
			blocked: jobsgo.ErrBlocked,
		},
		&site[indeed.Job, indeed.JobDetail]{
			name: indeed.Source,
			search: func(ctx context.Context, kw string, page int) ([]indeed.Job, int, error) {
				r, err := dc.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j indeed.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []indeed.Job) ([]indeed.JobDetail, error) {
				d, _, err := dc.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d indeed.JobDetail) services.AnalyzedJob {
				var posted string
				if d.Overview.PostedAt != nil {
					posted = d.Overview.PostedAt.UTC().Format(time.RFC3339)
				}
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: posted, Location: d.Overview.Location,
					Deadline: d.OtherInfo["Hạn nộp"],
				}
			},
			blocked: indeed.ErrBlocked,
			// Indeed asks to sign in after the first page; that just ends the results.
			endOfResults: func(err error) bool { return errors.Is(err, indeed.ErrLoginRequired) },
		},
		&site[vietnamworks.Job, vietnamworks.JobDetail]{
			name: vietnamworks.Source,
			search: func(ctx context.Context, kw string, page int) ([]vietnamworks.Job, int, error) {
				r, err := vc.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j vietnamworks.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []vietnamworks.Job) ([]vietnamworks.JobDetail, error) {
				d, _, err := vc.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d vietnamworks.JobDetail) services.AnalyzedJob {
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: d.Overview.DatePosted,
					Location: d.Overview.Location, Experience: d.Overview.Experience,
					Skills: d.Skills.All, Deadline: firstNonEmpty(d.Overview.ValidThrough, d.Overview.Deadline),
				}
			},
			blocked: vietnamworks.ErrBlocked,
		},
		&site[careerviet.Job, careerviet.JobDetail]{
			name: careerviet.Source,
			search: func(ctx context.Context, kw string, page int) ([]careerviet.Job, int, error) {
				r, err := clients.careerviet.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j careerviet.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []careerviet.Job) ([]careerviet.JobDetail, error) {
				d, _, err := clients.careerviet.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d careerviet.JobDetail) services.AnalyzedJob {
				// Skills.Keywords are the employer's search tags (mostly title synonyms), not
				// skills: leave Skills empty so the AI service finds them in the text.
				// "Hết hạn nộp" as displayed first; JSON-LD validThrough sometimes differs.
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: firstNonEmpty(d.Overview.DatePosted, d.Overview.UpdatedAt),
					Location: d.Overview.Location, Experience: d.Overview.Experience,
					Deadline: firstNonEmpty(d.Overview.Deadline, d.Overview.ValidThrough),
				}
			},
			blocked: careerviet.ErrBlocked,
		},
		&site[joboko.Job, joboko.JobDetail]{
			name: joboko.Source,
			search: func(ctx context.Context, kw string, page int) ([]joboko.Job, int, error) {
				r, err := clients.joboko.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j joboko.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []joboko.Job) ([]joboko.JobDetail, error) {
				d, _, err := clients.joboko.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d joboko.JobDetail) services.AnalyzedJob {
				// The date shown on the page first: validThrough is often 00:00 of that
				// day, which would expire the job a day early.
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: d.Overview.DatePosted,
					Location: d.Overview.Location, Experience: d.Overview.Experience,
					Deadline: firstNonEmpty(d.Overview.Deadline, d.Overview.ValidThrough),
				}
			},
			blocked: joboko.ErrBlocked,
		},
		// The search API returns each job complete, so there is no detail page to fetch.
		&site[vieclam24h.Job, vieclam24h.Job]{
			name: vieclam24h.Source,
			search: func(ctx context.Context, kw string, page int) ([]vieclam24h.Job, int, error) {
				r, err := clients.vieclam24h.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j vieclam24h.Job) string { return j.ID },
			details: func(_ context.Context, jobs []vieclam24h.Job) ([]vieclam24h.Job, error) {
				return jobs, nil
			},
			convert: func(j vieclam24h.Job) services.AnalyzedJob {
				// The list API has the requirements but no job description or benefits.
				return services.AnalyzedJob{
					ID: j.ID, URL: j.URL, Title: j.Title, Company: j.Company,
					Requirements: j.Requirements, Salary: j.Salary, PostedAt: j.PostedAt,
					Location: j.Location, Experience: j.Experience, Deadline: j.Deadline,
				}
			},
			blocked: vieclam24h.ErrBlocked,
		},
		&site[onetwothreejob.Job, onetwothreejob.JobDetail]{
			name: onetwothreejob.Source,
			search: func(ctx context.Context, kw string, page int) ([]onetwothreejob.Job, int, error) {
				r, err := clients.job123.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j onetwothreejob.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []onetwothreejob.Job) ([]onetwothreejob.JobDetail, error) {
				d, _, err := clients.job123.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d onetwothreejob.JobDetail) services.AnalyzedJob {
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: d.Overview.DatePosted,
					Location: d.Overview.Location, Experience: d.Overview.Experience,
					Deadline: d.Overview.Deadline,
				}
			},
			blocked: onetwothreejob.ErrBlocked,
		},
		&site[careerlink.Job, careerlink.JobDetail]{
			name: careerlink.Source,
			search: func(ctx context.Context, kw string, page int) ([]careerlink.Job, int, error) {
				r, err := clients.careerlink.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j careerlink.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []careerlink.Job) ([]careerlink.JobDetail, error) {
				d, _, err := clients.careerlink.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d careerlink.JobDetail) services.AnalyzedJob {
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: d.Overview.DatePosted,
					Location: d.Overview.Location, Experience: d.Overview.Experience,
					Deadline: d.Overview.Deadline,
				}
			},
			blocked: careerlink.ErrBlocked,
		},
	}
	if lc := clients.linkedin; lc != nil {
		sites = append(sites, &site[linkedin.Job, linkedin.JobDetail]{
			name: linkedin.Source,
			search: func(ctx context.Context, kw string, page int) ([]linkedin.Job, int, error) {
				r, err := lc.FetchJobs(ctx, kw, page)
				if err != nil {
					return nil, 0, err
				}
				return r.Jobs, r.TotalPages, nil
			},
			jobID: func(j linkedin.Job) string { return j.ID },
			details: func(ctx context.Context, jobs []linkedin.Job) ([]linkedin.JobDetail, error) {
				d, _, err := lc.FetchJobDetails(ctx, jobs, nil)
				return d, err
			},
			convert: func(d linkedin.JobDetail) services.AnalyzedJob {
				// LinkedIn shows a seniority level, not years of experience, and no deadline.
				return services.AnalyzedJob{
					ID: d.ID, URL: d.URL, Title: d.Title, Company: d.Company,
					Description: d.Description, Requirements: d.Requirements, Benefits: d.Benefits,
					Salary: d.Overview.Salary, PostedAt: d.Overview.DatePosted,
					Location: d.Overview.Location, Skills: d.Skills.All,
				}
			},
			blocked: linkedin.ErrBlocked,
		})
	}
	return sites
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// writeJSONFile writes to a temp file and renames it, so readers of *.json
// never see a half-written file.
func writeJSONFile(path string, v any) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
