package linkedin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://www.linkedin.com"
	// maxBodyBytes caps how much of a page is read; guest pages are well under 1MB.
	maxBodyBytes = 10 << 20

	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

// Search filters, taken from a LinkedIn search URL:
// geoId 105790653 is Ha Noi (like the other sources, which search Ha Noi) and
// f_TPR=r2592000 keeps jobs posted in the last 30 days.
const (
	geoHaNoi     = "105790653"
	postedWithin = "r2592000"
)

// ErrBlocked means LinkedIn refused the request (status 999/403, or a redirect to the
// sign-in wall). Retrying immediately only makes it worse, so callers should stop and
// try again later.
var ErrBlocked = errors.New("linkedin: blocked (999/403 or sign-in wall)")

// ErrNotFound means the job no longer exists (expired or removed posting).
var ErrNotFound = errors.New("linkedin: page not found")

// URL_FIND_JOB is LinkedIn's public (signed-out) job search endpoint. It returns an HTML
// fragment of pageSize job cards starting at the start-th result.
var URL_FIND_JOB = defaultBaseURL + "/jobs-guest/jobs/api/seeMoreJobPostings/search"

// DialFunc dials a network connection, e.g. wgtunnel.LazyDialer.DialContext to go
// through a WireGuard VPN where the ISP blocks LinkedIn.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type Client struct {
	hc      *http.Client
	baseURL string

	// Politeness settings. Every request waits minDelay plus a random [0, jitter)
	// after the previous one, so a run never looks like a fixed-interval bot.
	// LinkedIn answers 429 quickly to signed-out clients, hence slower than other sources.
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

// NewClient creates a LinkedIn client. A nil hc uses NewHTTPClient with a direct
// connection; an empty baseURL falls back to https://www.linkedin.com (overriding it is
// mainly useful for tests).
func NewClient(hc *http.Client, baseURL string) *Client {
	if hc == nil {
		hc = NewHTTPClient(30*time.Second, nil)
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		hc:         hc,
		baseURL:    strings.TrimRight(baseURL, "/"),
		minDelay:   3 * time.Second,
		jitter:     3 * time.Second,
		retries:    3,
		retryBase:  10 * time.Second,
		batchEvery: 15,
		batchPause: 10 * time.Second,
		gate:       make(chan struct{}, 1),
	}
}

// NewHTTPClient returns an HTTP client that keeps cookies between requests like a
// browser. dial (optional) makes every connection, so passing a VPN dialer sends all
// LinkedIn traffic, DNS included when the dialer resolves names itself, through it.
func NewHTTPClient(timeout time.Duration, dial DialFunc) *http.Client {
	jar, _ := cookiejar.New(nil)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if dial != nil {
		tr.DialContext = dial
		tr.Proxy = nil // the tunnel is the way out; an HTTP(S)_PROXY would bypass it
	}
	return &http.Client{Timeout: timeout, Jar: jar, Transport: tr}
}

// SetRequestDelay changes the wait between two requests to minDelay plus a random [0, jitter).
func (c *Client) SetRequestDelay(minDelay, jitter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minDelay, c.jitter = minDelay, jitter
}

// SearchURL builds the search URL for a keyword and page (starting at 1) in Ha Noi.
func (c *Client) SearchURL(keyword string, page int) string {
	q := url.Values{
		"keywords": {strings.TrimSpace(keyword)},
		"geoId":    {geoHaNoi},
		"f_TPR":    {postedWithin},
	}
	if page > 1 {
		q.Set("start", strconv.Itoa((page-1)*pageSize))
	}
	return c.rebase(URL_FIND_JOB) + "?" + q.Encode()
}

// rebase points a www.linkedin.com URL at c.baseURL (a no-op outside tests).
func (c *Client) rebase(u string) string {
	return strings.Replace(u, defaultBaseURL, c.baseURL, 1)
}

// FetchJobs downloads one search result page and parses it into jobs.
//
// LinkedIn does not report the number of results to signed-out clients, so TotalPages
// is page+1 while pages are full and page once a short page shows the end.
func (c *Client) FetchJobs(ctx context.Context, keyword string, page int) (*SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("linkedin: keyword is required")
	}
	if page < 1 {
		page = 1
	}
	searchURL := c.SearchURL(keyword, page)

	body, err := c.get(ctx, searchURL, c.baseURL+"/jobs/search")
	if err != nil {
		// Past the last result LinkedIn answers 400 or 404 instead of an empty page.
		var se *statusError
		if page > 1 && (errors.Is(err, ErrNotFound) || errors.As(err, &se) && se.code == http.StatusBadRequest) {
			return &SearchResult{Keyword: keyword, SourceURL: searchURL, Page: page, TotalPages: page - 1, Jobs: []Job{}}, nil
		}
		return nil, err
	}

	result, err := ParseSearchPage(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("linkedin: parse %s: %w", searchURL, err)
	}
	result.Keyword = keyword
	result.SourceURL = searchURL
	result.Page = page
	result.TotalPages = page
	if result.cards >= pageSize {
		result.TotalPages = page + 1
	}
	return result, nil
}

// FetchAllJobs walks the search pages from 1 up to maxPages (or until a page is short
// or brings nothing new) and returns the jobs deduplicated by ID.
// maxPages <= 0 means no limit besides the end of the results.
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
		if added == 0 || page >= result.TotalPages {
			break
		}
	}
	return jobs, nil
}

// get fetches a page after waiting for the rate limiter. It retries 429/5xx and network
// errors with exponential backoff, and returns ErrBlocked or ErrNotFound (wrapped) without retrying.
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
		return "", fmt.Errorf("linkedin: build request: %w", err)
	}
	setBrowserHeaders(req)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("linkedin: fetch %s: %w", pageURL, err)
	}
	defer resp.Body.Close()

	// 999 is LinkedIn's "request denied"; a redirect to the auth wall means the
	// signed-out quota is used up.
	if resp.StatusCode == 999 || resp.StatusCode == http.StatusForbidden || isSignInWall(resp.Request.URL) {
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
		return "", fmt.Errorf("linkedin: read %s: %w", pageURL, err)
	}
	return string(b), nil
}

func isSignInWall(u *url.URL) bool {
	if u == nil {
		return false
	}
	p := u.Path
	return strings.HasPrefix(p, "/authwall") || strings.HasPrefix(p, "/checkpoint") ||
		strings.HasPrefix(p, "/login") || strings.HasPrefix(p, "/signup")
}

// setBrowserHeaders makes a request look like Chrome's own fetch of the fragment.
// English labels keep the job criteria ("Seniority level"...) stable for the parser.
func setBrowserHeaders(req *http.Request) {
	h := req.Header
	h.Set("User-Agent", browserUserAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	h.Set("Accept-Language", "en-US,en;q=0.9,vi;q=0.8")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
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
	return fmt.Sprintf("linkedin: fetch %s: unexpected status %d", e.url, e.code)
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
