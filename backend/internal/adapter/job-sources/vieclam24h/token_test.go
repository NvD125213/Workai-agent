package vieclam24h

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeJWT builds a token with the given payload (the signature is not checked here).
func fakeJWT(t *testing.T, payload map[string]any, sig string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"typ": "JWT", "alg": "HS256"}) + "." + enc(payload) + "." + sig
}

// fakeSite serves the API, which accepts only `valid`, plus a home page and an app bundle
// that hand out `published` the way vieclam24h.vn does.
type fakeSite struct {
	t         *testing.T
	valid     string
	published string
	apiCalls  atomic.Int32
	homeCalls atomic.Int32
	jsCalls   atomic.Int32
	rejectAs  string // "1009" (what the real API does) or "401"
}

func (f *fakeSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == searchPath:
		f.apiCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.valid {
			if f.rejectAs == "401" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"code":1009,"msg":"Mã bảo mật không còn hợp lệ","data":[]}`))
			return
		}
		w.Write(readFixture(f.t, "job-list.json"))
	case r.URL.Path == "/":
		f.homeCalls.Add(1)
		// As on the real home page: the bundle sits on a CDN path with a build id.
		fmt.Fprint(w, `<html><head><script src="/seeker/261110081416/_next/static/chunks/main-b8d2.js" defer=""></script>`+
			`<script src="/seeker/261110081416/_next/static/chunks/pages/_app-d2132bac70d2aeb7.js" defer=""></script></head></html>`)
	case strings.HasSuffix(r.URL.Path, "/pages/_app-d2132bac70d2aeb7.js"):
		f.jsCalls.Add(1)
		// Shapes of the real bundle: a signed-in user's token must not be picked.
		user := fakeJWT(f.t, map[string]any{"channel_code": "vl24h", "user": map[string]any{"id": 1}}, "usr")
		fmt.Fprintf(w, `var a=(i=fs.V.get("access_token"))||"%s",v={Authorization:"Bearer ".concat(g)};`+
			`v=function(){return h.env.JWT_GUEST||"%s"};f=s.GK.getAccessToken()||"%s";x="%s";`,
			f.published, f.published, f.published, user)
	default:
		f.t.Errorf("unexpected request %s", r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestTokenRefreshedFromSite(t *testing.T) {
	for _, rejectAs := range []string{"1009", "401"} {
		t.Run(rejectAs, func(t *testing.T) {
			newTok := fakeJWT(t, map[string]any{"channel_code": "vl24h", "user": nil}, "new")
			site := &fakeSite{t: t, valid: newTok, published: newTok, rejectAs: rejectAs}
			c := newTestClient(t, site)

			res, err := c.FetchJobs(context.Background(), "developer", 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Jobs) != 2 {
				t.Errorf("jobs = %d", len(res.Jobs))
			}
			if c.token != newTok {
				t.Errorf("token not replaced")
			}
			if site.apiCalls.Load() != 2 || site.homeCalls.Load() != 1 || site.jsCalls.Load() != 1 {
				t.Errorf("calls: api %d, home %d, js %d; want 2, 1, 1",
					site.apiCalls.Load(), site.homeCalls.Load(), site.jsCalls.Load())
			}

			// Later requests use the new token directly.
			if _, err := c.FetchJobs(context.Background(), "developer", 1); err != nil {
				t.Fatal(err)
			}
			if site.apiCalls.Load() != 3 || site.homeCalls.Load() != 1 {
				t.Errorf("second search: api %d, home %d", site.apiCalls.Load(), site.homeCalls.Load())
			}
		})
	}
}

// Concurrent searches (find-job runs several) share one lookup of the site.
func TestTokenRefreshSharedByConcurrentRequests(t *testing.T) {
	newTok := fakeJWT(t, map[string]any{"channel_code": "vl24h", "user": nil}, "new")
	site := &fakeSite{t: t, valid: newTok, published: newTok, rejectAs: "1009"}
	c := newTestClient(t, site)

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Go(func() {
			if _, err := c.FetchJobs(context.Background(), "developer", 1); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if site.homeCalls.Load() != 1 {
		t.Errorf("home page fetched %d times, want 1", site.homeCalls.Load())
	}
}

// When the site still publishes the rejected token, there is nothing to switch to.
func TestTokenRefreshSameTokenIsBlocked(t *testing.T) {
	site := &fakeSite{t: t, valid: "something-else", published: GuestToken, rejectAs: "1009"}
	c := newTestClient(t, site)

	_, err := c.FetchJobs(context.Background(), "developer", 1)
	if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "same one") {
		t.Fatalf("err = %v, want ErrBlocked (same token)", err)
	}
	if site.apiCalls.Load() != 1 {
		t.Errorf("api calls = %d, want 1 (no retry with the same token)", site.apiCalls.Load())
	}

	// Within the cooldown the site is not asked again.
	if _, err := c.FetchJobs(context.Background(), "developer", 1); !errors.Is(err, ErrBlocked) {
		t.Fatalf("second err = %v", err)
	}
	if site.homeCalls.Load() != 1 {
		t.Errorf("home page fetched %d times within the cooldown, want 1", site.homeCalls.Load())
	}

	// After the cooldown it is tried again.
	c.refreshCooldown = 0
	_, _ = c.FetchJobs(context.Background(), "developer", 1)
	if site.homeCalls.Load() != 2 {
		t.Errorf("home page fetched %d times after the cooldown, want 2", site.homeCalls.Load())
	}
}

// A new token that the API also rejects is tried once, never in a loop.
func TestTokenRefreshNewTokenRejectedToo(t *testing.T) {
	published := fakeJWT(t, map[string]any{"channel_code": "vl24h", "user": nil}, "pub")
	site := &fakeSite{t: t, valid: "nothing-works", published: published, rejectAs: "1009"}
	c := newTestClient(t, site)

	_, err := c.FetchJobs(context.Background(), "developer", 1)
	if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "rejected too") {
		t.Fatalf("err = %v, want ErrBlocked (new token rejected too)", err)
	}
	if site.apiCalls.Load() != 2 {
		t.Errorf("api calls = %d, want 2", site.apiCalls.Load())
	}
}

func TestGuestTokenIn(t *testing.T) {
	guest := fakeJWT(t, map[string]any{"channel_code": "vl24h", "user": nil}, "g")
	user := fakeJWT(t, map[string]any{"channel_code": "vl24h", "user": map[string]any{"id": 9}}, "u")
	other := fakeJWT(t, map[string]any{"sub": "analytics"}, "o")
	script := fmt.Sprintf(`a="%s";b="%s";c="%s";d="%s"`, user, other, guest, guest)
	if got := guestTokenIn([]byte(script)); got != guest {
		t.Errorf("guestTokenIn = %q, want the guest token", got)
	}
	if got := guestTokenIn([]byte(`no tokens here`)); got != "" {
		t.Errorf("guestTokenIn = %q, want empty", got)
	}
	if !isGuestJWT(GuestToken) {
		t.Error("the current GuestToken must look like a guest token")
	}
}
