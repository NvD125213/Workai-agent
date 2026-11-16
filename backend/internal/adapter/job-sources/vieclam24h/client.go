// Package vieclam24h is the job-source adapter for Việc Làm 24h (vieclam24h.vn).
//
// Unlike the HTML adapters it reads the site's JSON API: one search request returns
// up to perPage jobs with the job requirements included, so no job page is fetched.
package vieclam24h

import (
	"context"
	"encoding/json"
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
)

const (
	defaultBaseURL = "https://apiv2.vieclam24h.vn"
	siteURL        = "https://vieclam24h.vn"
	searchPath     = "/employer/fe/job/get-job-list"

	// GuestToken is the token the site's own JavaScript sends when nobody is signed in
	// (its payload is {"channel_code":"vl24h","user":null}). The API refuses requests
	// without a token. When the API rejects it, the client reads the current one from the
	// site (see refreshToken); SetToken replaces it by hand.
	GuestToken = "eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.eyJjaGFubmVsX2NvZGUiOiJ2bDI0aCIsInVzZXIiOm51bGx9.a0POm2ZVRwetYs2QsMj0sRg8lZSSbKufX4sewqhAM5o"

	// perPage is how many jobs one search request asks for (the site itself uses 30;
	// the API allows more).
	perPage = 50

	// maxBodyBytes caps how much of a response is read; 50 jobs are ~200KB.
	maxBodyBytes = 16 << 20

	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

// defaultProvince is searched unless SetProvince says otherwise, like the other sources.
const defaultProvince = "ha-noi"

// ErrBlocked means the API keeps refusing the request: the token was rejected and no
// working one could be read from the site. Retrying immediately does not help.
var ErrBlocked = errors.New("vieclam24h: blocked (token rejected, no new token on the site)")

type Client struct {
	hc       *http.Client
	baseURL  string
	siteBase string // where refreshToken reads the guest token (the site, or a test server)

	mu          sync.Mutex
	token       string
	province    Province  // zero ID = every province
	lastRefresh time.Time // last lookup of the site for a token

	refreshMu       sync.Mutex // one token lookup at a time
	refreshCooldown time.Duration

	// Politeness settings. Every request waits minDelay plus a random [0, jitter)
	// after the previous one, so a run never looks like a fixed-interval bot.
	minDelay  time.Duration
	jitter    time.Duration
	retries   int           // extra attempts on 429/5xx/network errors
	retryBase time.Duration // first backoff, doubled on each retry

	next time.Time     // earliest time the next request may start
	gate chan struct{} // holds the slot of the request in flight
}

// NewClient creates a client searching Ha Noi. A nil hc uses a plain client with a 30s
// timeout; an empty baseURL falls back to https://apiv2.vieclam24h.vn (overriding it is
// mainly useful for tests).
func NewClient(hc *http.Client, baseURL string) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	p, _ := LookupProvince(defaultProvince)
	return &Client{
		hc:              hc,
		baseURL:         strings.TrimRight(baseURL, "/"),
		siteBase:        siteURL,
		token:           GuestToken,
		province:        p,
		refreshCooldown: tokenRefreshCooldown,
		minDelay:        2 * time.Second,
		jitter:          2 * time.Second,
		retries:         3,
		retryBase:       10 * time.Second,
		gate:            make(chan struct{}, 1),
	}
}

// SetToken replaces the bearer token (default GuestToken).
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
}

// SetProvince changes the searched province, by name or slug ("TP.HCM", "da-nang"...;
// "" = every province). See LookupProvince.
func (c *Client) SetProvince(nameOrSlug string) error {
	var p Province
	if strings.TrimSpace(nameOrSlug) != "" {
		var ok bool
		if p, ok = LookupProvince(nameOrSlug); !ok {
			return fmt.Errorf("vieclam24h: unknown province %q", nameOrSlug)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.province = p
	return nil
}

// Province is the province being searched (zero value = every province).
func (c *Client) Province() Province {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.province
}

// SetRequestDelay changes the wait between two requests to minDelay plus a random [0, jitter).
func (c *Client) SetRequestDelay(minDelay, jitter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minDelay, c.jitter = minDelay, jitter
}

// SearchURL builds the API URL for a keyword and page (starting at 1).
func (c *Client) SearchURL(keyword string, page int) string {
	q := url.Values{
		"q":        {strings.TrimSpace(keyword)},
		"page":     {strconv.Itoa(max(page, 1))},
		"per_page": {strconv.Itoa(perPage)},
	}
	if p := c.Province(); p.ID != 0 {
		q.Set("provinces", strconv.Itoa(p.ID))
	}
	return c.baseURL + searchPath + "?" + q.Encode()
}

// FetchJobs downloads one search page. Every job comes complete (requirements, salary,
// experience, deadline...), ready for AI matching.
func (c *Client) FetchJobs(ctx context.Context, keyword string, page int) (*SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("vieclam24h: keyword is required")
	}
	page = max(page, 1)
	apiURL := c.SearchURL(keyword, page)

	body, err := c.get(ctx, apiURL)
	if err != nil {
		return nil, err
	}
	result, err := ParseJobList(body)
	if err != nil {
		return nil, fmt.Errorf("vieclam24h: parse %s: %w", apiURL, err)
	}
	result.Keyword = keyword
	result.SourceURL = apiURL
	result.Page = page
	return result, nil
}

// FetchAllJobs walks the search pages from 1 up to maxPages (or the last page) and
// returns the jobs deduplicated by ID. maxPages <= 0 means every page.
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

// get fetches an API response after waiting for the rate limiter. It retries 429/5xx and
// network errors with exponential backoff. When the API rejects the token it reads a new
// one from the site and retries once; ErrBlocked (wrapped) when that does not help.
func (c *Client) get(ctx context.Context, apiURL string) ([]byte, error) {
	var lastErr error
	refreshed := false
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 && lastErr != nil {
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
		body, err := c.do(ctx, apiURL)
		release()
		if err == nil {
			return body, nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrBlocked) {
			return nil, err
		}
		var te *tokenError
		if errors.As(err, &te) {
			if refreshed {
				return nil, fmt.Errorf("%w: the new token was rejected too", ErrBlocked)
			}
			refreshed = true
			if rerr := c.refreshToken(ctx, te.token); rerr != nil {
				return nil, rerr
			}
			attempt-- // the retry with the new token is not one of the error retries
			lastErr = nil
			continue
		}
		var se *statusError
		if errors.As(err, &se) && !se.retryable() {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Client) do(ctx context.Context, apiURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("vieclam24h: build request: %w", err)
	}
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	// The headers the site's own fetch sends (tracking ids left out).
	h := req.Header
	h.Set("User-Agent", browserUserAgent)
	h.Set("Accept", "application/json")
	h.Set("Accept-Language", "vi-VN,vi;q=0.9,en;q=0.8")
	h.Set("Authorization", "Bearer "+token)
	h.Set("Origin", siteURL)
	h.Set("Referer", siteURL+"/")
	h.Set("X-Lang", "vi")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-site")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vieclam24h: fetch %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, &tokenError{token: token, reason: fmt.Sprintf("status %d", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, &statusError{url: apiURL, code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("vieclam24h: read %s: %w", apiURL, err)
	}
	// A bad or expired token comes back as HTTP 200 with code 1009.
	if isTokenRejection(b) {
		return nil, &tokenError{token: token, reason: "api code 1009"}
	}
	return b, nil
}

// tokenError is an API answer refusing token; get looks for a new one and retries.
type tokenError struct {
	token  string
	reason string
}

func (e *tokenError) Error() string { return "vieclam24h: token rejected (" + e.reason + ")" }

func (e *tokenError) Unwrap() error { return errTokenRejected }

// apiEnvelope is the wrapper of every API answer. Errors come back as HTTP 200 with
// code != 200 (e.g. code 0 and "Đã có lỗi xảy ra" without a token).
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
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
	return fmt.Sprintf("vieclam24h: fetch %s: unexpected status %d", e.url, e.code)
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
