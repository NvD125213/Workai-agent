package vietnamworks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://ms.vietnamworks.com"
	siteURL        = "https://www.vietnamworks.com"
	// maxBodyBytes caps how much of a response is read; a 50-job search page is ~0.5MB.
	maxBodyBytes = 10 << 20

	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

// URL_JOB_SEARCH is the search API (POST, JSON body built by newSearchPayload).
var URL_JOB_SEARCH = `https://ms.vietnamworks.com/job-search/v1.0/search`

// ErrBlocked means VietnamWorks refused the request (403). Retrying immediately only
// makes it worse, so callers should stop and try again later.
var ErrBlocked = errors.New("vietnamworks: blocked (403)")

// ErrNotFound means the job no longer exists (expired or removed posting).
var ErrNotFound = errors.New("vietnamworks: job not found")

type Client struct {
	hc      *http.Client
	baseURL string

	// Politeness settings. Every request waits minDelay plus a random [0, jitter)
	// after the previous one, so a run never looks like a fixed-interval bot.
	minDelay    time.Duration
	jitter      time.Duration
	retries     int           // extra attempts on 429/5xx/network errors
	retryBase   time.Duration // first backoff, doubled on each retry
	batchEvery  int           // FetchJobDetails takes a long pause after this many jobs...
	batchPause  time.Duration // ...lasting batchPause plus up to a third more
	hitsPerPage int

	mu   sync.Mutex
	next time.Time     // earliest time the next request may start
	gate chan struct{} // holds the slot of the request in flight
}

// NewClient creates a VietnamWorks client. A nil hc uses a plain http.Client (the JSON API
// is not behind a bot check); an empty baseURL falls back to https://ms.vietnamworks.com
// (overriding it is mainly useful for tests).
func NewClient(hc *http.Client, baseURL string) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		hc:          hc,
		baseURL:     strings.TrimRight(baseURL, "/"),
		minDelay:    2 * time.Second,
		jitter:      2 * time.Second,
		retries:     3,
		retryBase:   10 * time.Second,
		batchEvery:  25,
		batchPause:  8 * time.Second,
		gate:        make(chan struct{}, 1),
		hitsPerPage: 50,
	}
}

// SetRequestDelay changes the wait between two requests to minDelay plus a random [0, jitter).
func (c *Client) SetRequestDelay(minDelay, jitter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minDelay, c.jitter = minDelay, jitter
}

// rebase points a ms.vietnamworks.com URL at c.baseURL (a no-op outside tests).
func (c *Client) rebase(u string) string {
	return strings.Replace(u, defaultBaseURL, c.baseURL, 1)
}

// FetchJobs downloads one search result page (page starts at 1, like TopCV).
func (c *Client) FetchJobs(ctx context.Context, keyword string, page int) (*SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("vietnamworks: keyword is required")
	}
	if page < 1 {
		page = 1
	}
	payload, err := json.Marshal(newSearchPayload(keyword, page-1, c.hitsPerPage))
	if err != nil {
		return nil, fmt.Errorf("vietnamworks: encode search payload: %w", err)
	}

	searchURL := c.rebase(URL_JOB_SEARCH)
	body, err := c.do(ctx, http.MethodPost, searchURL, payload)
	if err != nil {
		return nil, err
	}

	result, err := ParseSearchResponse(body)
	if err != nil {
		return nil, fmt.Errorf("vietnamworks: parse search %q page %d: %w", keyword, page, err)
	}
	result.Keyword = keyword
	result.SourceURL = URL_JOB_SEARCH
	if result.Page == 0 {
		result.Page = page
	}
	return result, nil
}

// FetchAllJobs walks the search pages from 1 up to maxPages (or the last page reported
// by VietnamWorks, whichever comes first) and returns the jobs deduplicated by ID.
// maxPages <= 0 means no limit besides the reported total.
func (c *Client) FetchAllJobs(ctx context.Context, keyword string, maxPages int) ([]Job, error) {
	seen := make(map[string]struct{})
	var jobs []Job

	for page := 1; maxPages <= 0 || page <= maxPages; page++ {
		result, err := c.FetchJobs(ctx, keyword, page)
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

// do sends a request to the API after waiting for the rate limiter. It retries 429/5xx
// and network errors with exponential backoff, and returns ErrBlocked or ErrNotFound
// (wrapped) without retrying. body is sent as JSON when non-nil.
func (c *Client) do(ctx context.Context, method, apiURL string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			backoff := c.retryBase << (attempt - 1)
			if ra, ok := retryAfter(lastErr); ok && ra > backoff {
				backoff = ra
			}
			if err := sleep(ctx, backoff+randDuration(c.retryBase)); err != nil {
				return nil, err
			}
		}
		release, err := c.acquire(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := c.doOnce(ctx, method, apiURL, body)
		release()
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrBlocked) || errors.Is(err, ErrNotFound) {
			return nil, err
		}
		var se *statusError
		if errors.As(err, &se) && !se.retryable() {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Client) doOnce(ctx context.Context, method, apiURL string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, r)
	if err != nil {
		return nil, fmt.Errorf("vietnamworks: build request: %w", err)
	}
	setAPIHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vietnamworks: fetch %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", ErrBlocked, apiURL)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, apiURL)
	case resp.StatusCode != http.StatusOK:
		return nil, &statusError{url: apiURL, code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("vietnamworks: read %s: %w", apiURL, err)
	}
	return b, nil
}

// setAPIHeaders makes a request look like the XHR the vietnamworks.com front end sends.
func setAPIHeaders(req *http.Request) {
	h := req.Header
	h.Set("User-Agent", browserUserAgent)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "vi")
	h.Set("Origin", siteURL)
	h.Set("Referer", siteURL+"/")
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

type statusError struct {
	url        string
	code       int
	retryAfter string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("vietnamworks: fetch %s: unexpected status %d", e.url, e.code)
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
