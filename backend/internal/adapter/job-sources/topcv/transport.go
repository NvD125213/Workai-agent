package topcv

import (
	"net/http"
	"time"

	"notification-job-mb/internal/adapter/job-sources/browser"
)

// NewHTTPClient returns an HTTP client that gets past the Cloudflare bot check in front of TopCV.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return browser.NewHTTPClient(timeout)
}

func setBrowserHeaders(req *http.Request) {
	browser.SetNavigationHeaders(req)
}
