package topcv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// Concurrent callers must still reach TopCV one request at a time, each one
// starting the configured pause after the previous one finished.
func TestRequestsAreSerializedWithPause(t *testing.T) {
	const (
		pause    = 40 * time.Millisecond
		respTime = 30 * time.Millisecond // slower than nothing, so overlaps would show
		callers  = 5
	)
	var (
		mu       sync.Mutex
		inFlight int
		maxIn    int
		spans    [][2]time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mu.Lock()
		inFlight++
		maxIn = max(maxIn, inFlight)
		mu.Unlock()

		time.Sleep(respTime)
		http.NotFound(w, r) // ErrNotFound: no retry, one request per call

		mu.Lock()
		inFlight--
		spans = append(spans, [2]time.Time{start, time.Now()})
		mu.Unlock()
	}))
	defer srv.Close()

	c := newTestClient(srv)
	c.SetRequestDelay(pause, 0)

	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() { _, _ = c.FetchJobDetail(context.Background(), srv.URL+"/viec-lam/x/1.html") })
	}
	wg.Wait()

	if maxIn != 1 {
		t.Fatalf("max requests in flight = %d, want 1", maxIn)
	}
	if len(spans) != callers {
		t.Fatalf("requests = %d, want %d", len(spans), callers)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0].Before(spans[j][0]) })
	for i := 1; i < len(spans); i++ {
		// Measured server side; allow a little for scheduling and the response write.
		if gap := spans[i][0].Sub(spans[i-1][1]); gap < pause-5*time.Millisecond {
			t.Errorf("request %d started %v after the previous one ended, want >= %v", i, gap, pause)
		}
	}
}

// A caller waiting for the slot gives up when its context ends, without
// holding the slot for the others.
func TestAcquireHonorsContext(t *testing.T) {
	c := NewClient(nil, "http://unused", "")
	c.SetRequestDelay(time.Hour, 0)
	release, err := c.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release() // the next slot is now an hour away

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.acquire(ctx); err == nil {
		t.Fatal("acquire during the pause: want context error")
	}
	select {
	case c.gate <- struct{}{}: // the slot was released
		<-c.gate
	default:
		t.Fatal("slot still held after a cancelled acquire")
	}
}
