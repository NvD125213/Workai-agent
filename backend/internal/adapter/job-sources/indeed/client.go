package indeed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"notification-job-mb/internal/adapter/job-sources/browser"
)

const (
	defaultBaseURL = "https://vn.indeed.com"
	// maxBodyBytes caps how much of a page is read; Indeed search pages are ~1MB.
	maxBodyBytes = 10 << 20
	// pageOffset is how far Indeed's ?start= moves per result page.
	pageOffset = 10
)

// cityHaNoi is the location Indeed is searched in.
const cityHaNoi = "Hà Nội"

// ErrBlocked means Cloudflare or Indeed's bot detection served a challenge (403/401)
// instead of the page. Retrying immediately only makes it worse, so callers should stop
// and try again later.
var ErrBlocked = errors.New("indeed: blocked by bot detection")

// ErrNotFound means the job no longer exists (expired or removed posting).
var ErrNotFound = errors.New("indeed: page not found")

// ErrLoginRequired means Indeed asked to sign in. Anonymous visitors only get the
// first search page of a query; ?start=10 and beyond show a login wall.
var ErrLoginRequired = errors.New("indeed: login required")

// URL_Search is the search URL template. {job_name} is the query-escaped keyword,
// {location} the query-escaped city; the result offset goes in &start=.
var URL_Search = `https://vn.indeed.com/jobs?q={job_name}&l={location}`

type Client struct {
	hc      *http.Client
	baseURL string

	// Politeness settings. Every request waits minDelay plus a random [0, jitter)
	// after the previous one, so a run never looks like a fixed-interval bot.
	minDelay   time.Duration
	jitter     time.Duration
	retries    int           // extra attempts on 429/5xx/network errors
	retryBase  time.Duration // first backoff, doubled on each retry
	batchEvery int           // FetchJobDetails takes a long pause after this many pages...
	batchPause time.Duration // ...lasting batchPause plus up to a third more

	mu   sync.Mutex
	next time.Time     // earliest time the next request may start
	gate chan struct{} // holds the slot of the request in flight
}

// NewClient creates an Indeed client. A nil hc uses browser.NewHTTPClient (Indeed is behind
// Cloudflare); an empty baseURL falls back to https://vn.indeed.com (overriding it is mainly useful for tests).
func NewClient(hc *http.Client, baseURL string) *Client {
	if hc == nil {
		hc = browser.NewHTTPClient(30 * time.Second)
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	// Same spacing as the other sites: 2-4 s between requests, 8-11 s after every batchEvery details.
	return &Client{
		hc:         hc,
		baseURL:    strings.TrimRight(baseURL, "/"),
		minDelay:   2 * time.Second,
		jitter:     2 * time.Second,
		retries:    3,
		retryBase:  10 * time.Second,
		batchEvery: 15,
		batchPause: 8 * time.Second,
		gate:       make(chan struct{}, 1),
	}
}

// SetRequestDelay changes the wait between two requests to minDelay plus a random [0, jitter).
func (c *Client) SetRequestDelay(minDelay, jitter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minDelay, c.jitter = minDelay, jitter
}

// SearchURL builds the Indeed search URL for a keyword and page (starting at 1) in Ha Noi.
func (c *Client) SearchURL(keyword string, page int) string {
	u := strings.NewReplacer(
		"{job_name}", url.QueryEscape(strings.TrimSpace(keyword)),
		"{location}", url.QueryEscape(cityHaNoi),
	).Replace(URL_Search)
	u = c.rebase(u)
	if page > 1 {
		u += "&start=" + strconv.Itoa((page-1)*pageOffset)
	}
	return u
}

// rebase points a vn.indeed.com URL at c.baseURL (a no-op outside tests).
func (c *Client) rebase(u string) string {
	return strings.Replace(u, defaultBaseURL, c.baseURL, 1)
}

// FetchJobs downloads one search result page and parses it into jobs.
// Pages after the first return ErrLoginRequired for anonymous visitors.
func (c *Client) FetchJobs(ctx context.Context, keyword string, page int) (*SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("indeed: keyword is required")
	}
	if page < 1 {
		page = 1
	}
	searchURL := c.SearchURL(keyword, page)

	body, err := c.get(ctx, searchURL, "")
	if err != nil {
		return nil, err
	}

	result, err := ParseSearchPage(strings.NewReader(body))
	if err != nil {
		if isLoginWall(body) {
			return nil, fmt.Errorf("%w: %s", ErrLoginRequired, searchURL)
		}
		return nil, fmt.Errorf("indeed: parse %s: %w", searchURL, err)
	}
	result.Keyword = keyword
	result.SourceURL = searchURL
	if result.Page == 0 {
		result.Page = page
	}
	return result, nil
}

// FetchAllJobs walks the search pages from 1 up to maxPages (or the last page reported
// by Indeed, whichever comes first) and returns the jobs deduplicated by ID.
// maxPages <= 0 means no limit besides the reported total. Hitting the login wall
// after the first page ends the walk without an error.
func (c *Client) FetchAllJobs(ctx context.Context, keyword string, maxPages int) ([]Job, error) {
	seen := make(map[string]struct{})
	var jobs []Job

	for page := 1; maxPages <= 0 || page <= maxPages; page++ {
		result, err := c.FetchJobs(ctx, keyword, page)
		if page > 1 && errors.Is(err, ErrLoginRequired) {
			break
		}
		if err != nil {
			return jobs, err
		}

		added := 0
		for _, job := range result.Jobs {
			if _, ok := seen[job.ID]; ok {
				continue
			}
			seen[job.ID] = struct{}{}
			jobs = append(jobs, job)
			added++
		}

		// Stop on the last reported page, or when a page brings nothing new.
		if added == 0 || result.TotalPages == 0 || page >= result.TotalPages {
			break
		}
	}
	return jobs, nil
}

// get fetches a page as a browser navigation would, after waiting for the rate limiter.
// It retries 429/5xx and network errors with exponential backoff, and returns
// ErrBlocked or ErrNotFound (wrapped) without retrying.
func (c *Client) get(ctx context.Context, pageURL, referer string) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			backoff := c.retryBase << (attempt - 1)
			if ra, ok := retryAfter(lastErr); ok && ra > backoff {
				backoff = ra
			}
			if err := sleep(ctx, backoff+randDuration(c.retryBase)); err != nil {
				return "", err
			}
		}
		release, err := c.acquire(ctx)
		if err != nil {
			return "", err
		}
		body, err := c.do(ctx, pageURL, referer)
		release()
		if err == nil {
			return body, nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrBlocked) || errors.Is(err, ErrNotFound) {
			return "", err
		}
		var se *statusError
		if errors.As(err, &se) && !se.retryable() {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

func (c *Client) do(ctx context.Context, pageURL, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("indeed: build request: %w", err)
	}
	browser.SetNavigationHeaders(req)
	if referer != "" {
		req.Header.Set("Referer", referer)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("indeed: fetch %s: %w", pageURL, err)
	}
	defer resp.Body.Close()

	// 403 is Cloudflare's "Additional Verification Required"; 401 is Indeed's
	// "Authenticating..." bot detection page (served e.g. for /viewjob).
	if resp.Header.Get("Cf-Mitigated") == "challenge" || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("%w: %s (status %d)", ErrBlocked, pageURL, resp.StatusCode)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return "", fmt.Errorf("%w: %s", ErrNotFound, pageURL)
	case resp.StatusCode != http.StatusOK:
		return "", &statusError{url: pageURL, code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("indeed: read %s: %w", pageURL, err)
	}
	return string(b), nil
}

// acquire waits for this client's request slot: one request in flight at a
// time, starting minDelay plus a random [0, jitter) after the previous request
// finished (not started), so a slow response never lets two requests overlap.
// Slots are shared by all goroutines using the client. The returned release
// must be called once the request is done.
func (c *Client) acquire(ctx context.Context) (release func(), err error) {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	wait := time.Until(c.next)
	c.mu.Unlock()
	if err := sleep(ctx, wait); err != nil {
		<-c.gate
		return nil, err
	}
	return func() {
		c.mu.Lock()
		c.next = time.Now().Add(c.minDelay + randDuration(c.jitter))
		c.mu.Unlock()
		<-c.gate
	}, nil
}

// isLoginWall reports whether a page is Indeed's "sign in to see more than one page" wall.
func isLoginWall(body string) bool {
	return strings.Contains(body, "auth-page-header--page-two-signin")
}

type statusError struct {
	url        string
	code       int
	retryAfter string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("indeed: fetch %s: unexpected status %d", e.url, e.code)
}

func (e *statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}

// retryAfter reads a Retry-After header given in seconds from a statusError.
func retryAfter(err error) (time.Duration, bool) {
	var se *statusError
	if !errors.As(err, &se) || se.retryAfter == "" {
		return 0, false
	}
	secs, convErr := strconv.Atoi(se.retryAfter)
	if convErr != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

func randDuration(maxD time.Duration) time.Duration {
	if maxD <= 0 {
		return 0
	}
	return rand.N(maxD)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// absURL resolves an Indeed link (often relative, with tracking parameters such as
// ?campaignid=...) to an absolute https://vn.indeed.com URL without query or fragment.
func absURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	base, _ := url.Parse(defaultBaseURL)
	u = base.ResolveReference(u)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
