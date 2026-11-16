// Package careerlink is the job-source adapter for CareerLink (careerlink.vn).
package careerlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://www.careerlink.vn"
	// maxBodyBytes caps how much of a page is read; pages are ~200-500KB.
	maxBodyBytes = 10 << 20

	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

// defaultCity is searched unless SetCity says otherwise, like the other sources.
const defaultCity = "Hà Nội"

// ErrBlocked means CareerLink refused the request: a 403, or its "Xác nhận bạn không phải
// robot" hCaptcha page, which it serves with status 200 once an IP sends too many requests.
// Retrying immediately only makes it worse, so callers should stop and try again later.
var ErrBlocked = errors.New("careerlink: blocked (403 / captcha)")

// ErrNotFound means the job page no longer exists (expired or removed posting).
var ErrNotFound = errors.New("careerlink: page not found")

// URL is the search page for a keyword in a province: /tim-viec-lam-tai/k/{keyword}/{city
// slug}/{city code}, words of the keyword joined by "-" ("Video-Editor"), ?page=N after the
// first page. Without a province the search is /vieclam/list?keyword=….
const URL = "https://www.careerlink.vn/tim-viec-lam-tai/k/Video-Editor/ha-noi/HN"

type Client struct {
	hc      *http.Client
	baseURL string
	city    City // zero Code = every province

	// Politeness settings. Every request waits minDelay plus a random [0, jitter)
	// after the previous one, so a run never looks like a fixed-interval bot.
	// CareerLink shows a captcha quickly to fast clients, hence the long delays.
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

// NewClient creates a CareerLink client searching Ha Noi. A nil hc uses NewHTTPClient; an
// empty baseURL falls back to https://www.careerlink.vn (overriding it is mainly useful for tests).
func NewClient(hc *http.Client, baseURL string) *Client {
	if hc == nil {
		hc = NewHTTPClient(30 * time.Second)
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	city, _ := LookupCity(defaultCity)
	return &Client{
		hc:         hc,
		baseURL:    strings.TrimRight(baseURL, "/"),
		city:       city,
		minDelay:   4 * time.Second,
		jitter:     3 * time.Second,
		retries:    3,
		retryBase:  10 * time.Second,
		batchEvery: 15,
		batchPause: 20 * time.Second,
		gate:       make(chan struct{}, 1),
	}
}

// NewHTTPClient returns an HTTP client that keeps cookies between requests like a browser.
func NewHTTPClient(timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: timeout, Jar: jar}
}

// SetCity changes the searched province, by name, slug or code ("TP.HCM", "da-nang",
// "DNI"...; "" = every province). See LookupCity.
func (c *Client) SetCity(nameOrSlug string) error {
	var city City
	if strings.TrimSpace(nameOrSlug) != "" {
		var ok bool
		if city, ok = LookupCity(nameOrSlug); !ok {
			return fmt.Errorf("careerlink: unknown city %q", nameOrSlug)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.city = city
	return nil
}

// City is the province being searched (zero value = every province).
func (c *Client) City() City {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.city
}

// SetRequestDelay changes the wait between two requests to minDelay plus a random [0, jitter).
func (c *Client) SetRequestDelay(minDelay, jitter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minDelay, c.jitter = minDelay, jitter
}

// SearchURL builds the search URL for a keyword and page (starting at 1).
func (c *Client) SearchURL(keyword string, page int) string {
	keyword = strings.Join(strings.Fields(keyword), " ")
	q := url.Values{}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	var u string
	if city := c.City(); city.Code != "" {
		words := strings.Fields(keyword)
		for i, w := range words {
			words[i] = url.PathEscape(w)
		}
		u = c.baseURL + "/tim-viec-lam-tai/k/" + strings.Join(words, "-") + "/" + city.Slug + "/" + city.Code
	} else {
		u = c.baseURL + "/vieclam/list"
		q.Set("keyword", keyword)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// rebase points a careerlink.vn URL at c.baseURL (a no-op outside tests).
func (c *Client) rebase(u string) string {
	return strings.Replace(u, defaultBaseURL, c.baseURL, 1)
}

// FetchJobs downloads one search result page and parses it into jobs.
func (c *Client) FetchJobs(ctx context.Context, keyword string, page int) (*SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("careerlink: keyword is required")
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
		return nil, fmt.Errorf("careerlink: parse %s: %w", searchURL, err)
	}
	result.Keyword = keyword
	result.SourceURL = searchURL
	result.Page = page
	if result.TotalJobs > 0 {
		result.TotalPages = (result.TotalJobs + jobsPerPage - 1) / jobsPerPage
	}
	// The "next" link is what the site itself offers; trust it over the count.
	// Past the last page the site answers 200 with no cards.
	if result.hasNext && result.TotalPages <= page {
		result.TotalPages = page + 1
	}
	if !result.hasNext && result.TotalPages > page && len(result.Jobs) < jobsPerPage {
		result.TotalPages = page
	}
	return result, nil
}

// FetchAllJobs walks the search pages from 1 up to maxPages (or the last page)
// and returns the jobs deduplicated by ID. maxPages <= 0 means every page.
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
		return "", fmt.Errorf("careerlink: build request: %w", err)
	}
	setBrowserHeaders(req)
	if referer != "" {
		req.Header.Set("Referer", referer)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("careerlink: fetch %s: %w", pageURL, err)
	}
	defer resp.Body.Close()

	// The captcha page comes with status 200 and this header:
	// X-Middleware-Rewrite: /public/captcha?target_path=…
	if resp.StatusCode == http.StatusForbidden || strings.Contains(resp.Header.Get("X-Middleware-Rewrite"), "captcha") {
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
		return "", fmt.Errorf("careerlink: read %s: %w", pageURL, err)
	}
	body := string(b)
	if strings.Contains(body, `data-testid="captcha-widget"`) {
		return "", fmt.Errorf("%w: %s (captcha page)", ErrBlocked, pageURL)
	}
	return body, nil
}

// setBrowserHeaders makes a request look like a top-level Chrome navigation.
func setBrowserHeaders(req *http.Request) {
	h := req.Header
	h.Set("User-Agent", browserUserAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", "vi-VN,vi;q=0.9,en;q=0.8")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Upgrade-Insecure-Requests", "1")
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
	return fmt.Sprintf("careerlink: fetch %s: unexpected status %d", e.url, e.code)
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

// absURL resolves a CareerLink link (often relative) to an absolute
// https://www.careerlink.vn URL without query or fragment.
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
