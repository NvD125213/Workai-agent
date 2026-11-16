package vieclam24h

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// apiCodeTokenRejected is what the API answers (with HTTP 200) when the bearer token is
// invalid, signed with another key or expired: "Mã bảo mật không còn hợp lệ".
const apiCodeTokenRejected = 1009

// tokenRefreshCooldown is the least time between two lookups of the site for a new token,
// so a token that keeps being rejected does not make every request reload the site.
const tokenRefreshCooldown = 5 * time.Minute

// errTokenRejected marks an API answer that refused the token; get refreshes the token
// and retries once.
var errTokenRejected = errors.New("vieclam24h: token rejected")

var (
	// appScriptRe finds the Next.js app bundle, where the site keeps its guest token:
	// <script src="https://static-cdn.vieclam24h.vn/seeker/<build>/_next/static/chunks/pages/_app-<hash>.js">
	appScriptRe = regexp.MustCompile(`<script[^>]+src="([^"]*/_next/static/chunks/pages/_app-[^"]+\.js)"`)
	jwtRe       = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
)

// refreshToken looks up the site's current guest token after the API rejected `rejected`.
// The site's JavaScript hard-codes it (JWT_GUEST || "eyJ..."): the home page links the
// app bundle, the bundle holds the token.
//
// Concurrent callers share one lookup: whoever comes second finds the token already
// changed and just uses it. Lookups are at most one per tokenRefreshCooldown; inside it,
// or when the site still has the rejected token, the API is considered blocked.
func (c *Client) refreshToken(ctx context.Context, rejected string) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	current, last := c.token, c.lastRefresh
	c.mu.Unlock()
	if current != rejected {
		return nil // another request refreshed it meanwhile
	}
	if !last.IsZero() && time.Since(last) < c.refreshCooldown {
		return fmt.Errorf("%w: token rejected again within %s of the last refresh", ErrBlocked, c.refreshCooldown)
	}
	c.mu.Lock()
	c.lastRefresh = time.Now()
	c.mu.Unlock()

	token, err := c.discoverToken(ctx)
	if err != nil {
		return fmt.Errorf("%w: token rejected and no new one found: %v", ErrBlocked, err)
	}
	if token == rejected {
		return fmt.Errorf("%w: token rejected but the site still uses the same one", ErrBlocked)
	}
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	slog.Info("vieclam24h: guest token refreshed from the site")
	return nil
}

// discoverToken reads the guest token from the site: home page -> app bundle -> JWT.
func (c *Client) discoverToken(ctx context.Context) (string, error) {
	home, err := c.fetchSite(ctx, c.siteBase+"/")
	if err != nil {
		return "", err
	}
	m := appScriptRe.FindSubmatch(home)
	if m == nil {
		return "", errors.New("app bundle not found on the home page")
	}
	base, _ := url.Parse(c.siteBase + "/")
	ref, err := url.Parse(string(m[1]))
	if err != nil {
		return "", fmt.Errorf("app bundle link %q: %w", m[1], err)
	}
	bundle, err := c.fetchSite(ctx, base.ResolveReference(ref).String())
	if err != nil {
		return "", err
	}
	token := guestTokenIn(bundle)
	if token == "" {
		return "", errors.New("no guest token in the app bundle")
	}
	return token, nil
}

// guestTokenIn picks the guest token among the JWTs of a script: one whose payload has a
// channel_code and no signed-in user, the most frequent if several.
func guestTokenIn(script []byte) string {
	counts := map[string]int{}
	best := ""
	for _, raw := range jwtRe.FindAll(script, -1) {
		tok := string(raw)
		if !isGuestJWT(tok) {
			continue
		}
		counts[tok]++
		if counts[tok] > counts[best] {
			best = tok
		}
	}
	return best
}

func isGuestJWT(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return false
	}
	_, hasChannel := claims["channel_code"]
	return hasChannel && claims["user"] == nil
}

// fetchSite downloads a page or script of the site, through the same rate limiter.
func (c *Client) fetchSite(ctx context.Context, pageURL string) ([]byte, error) {
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/javascript,*/*;q=0.8")
	req.Header.Set("Accept-Language", "vi-VN,vi;q=0.9,en;q=0.8")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", pageURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", pageURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
}

// isTokenRejection reports whether an API body (HTTP 200) refuses the token.
func isTokenRejection(body []byte) bool {
	var env struct {
		Code int `json:"code"`
	}
	return json.Unmarshal(body, &env) == nil && env.Code == apiCodeTokenRejected
}
