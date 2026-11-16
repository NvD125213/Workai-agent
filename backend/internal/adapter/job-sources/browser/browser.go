// Package browser makes HTTP requests look like Chrome, to get past the Cloudflare
// bot check in front of job sites (TopCV, JobsGO, ...).
package browser

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/cookiejar"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Keep the User-Agent and Sec-Ch-Ua versions in line with the Chrome version
// utls.HelloChrome_Auto mimics, otherwise Cloudflare sees the mismatch.
const (
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
	SecChUa   = `"Chromium";v="133", "Not(A:Brand";v="99", "Google Chrome";v="133"`
)

// NewHTTPClient returns an HTTP client that gets past the Cloudflare bot check.
// Cloudflare challenges Go's crypto/tls fingerprint (403 "Just a moment..."), so the TLS
// handshake is done by utls with a Chrome ClientHello, over HTTP/2 like a real browser.
func NewHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	t := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			raw, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			conn := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
			if err := conn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return conn, nil
		},
		ReadIdleTimeout: 30 * time.Second,
	}
	// Keep cookies (Cloudflare __cf_bm, session) between requests like a browser does.
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: timeout, Transport: t, Jar: jar}
}

// SetNavigationHeaders makes a request look like a top-level Chrome navigation.
func SetNavigationHeaders(req *http.Request) {
	h := req.Header
	h.Set("User-Agent", UserAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", "vi-VN,vi;q=0.9,en;q=0.8")
	h.Set("Sec-Ch-Ua", SecChUa)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Upgrade-Insecure-Requests", "1")
}
