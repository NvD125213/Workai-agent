package mailer_test

import (
	"context"
	"errors"
	"net/textproto"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"notification-job-mb/internal/mailer"
)

// scriptedSender fails the first failN calls per message with failErr, and
// can block every call until release is closed.
type scriptedSender struct {
	mu      sync.Mutex
	calls   map[string]int
	sent    []string
	failN   int
	failErr error
	release chan struct{}

	active, peak atomic.Int32
}

func newScripted() *scriptedSender { return &scriptedSender{calls: map[string]int{}} }

func (s *scriptedSender) Send(ctx context.Context, m mailer.Message) error {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[m.ID]++
	if s.calls[m.ID] <= s.failN {
		return s.failErr
	}
	s.sent = append(s.sent, m.ID)
	return nil
}

func (s *scriptedSender) callsFor(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[id]
}

func fastConfig() mailer.QueueConfig {
	return mailer.QueueConfig{Workers: 2, Buffer: 10, MaxAttempts: 3, SendTimeout: time.Second, RetryBase: time.Millisecond}
}

func shutdown(t *testing.T, q *mailer.Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestQueueRetriesTransientFailure(t *testing.T) {
	s := newScripted()
	s.failN, s.failErr = 2, errors.New("connection reset")
	q := mailer.NewQueue(s, fastConfig())

	if err := q.Enqueue(mailer.Message{ID: "m1"}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	shutdown(t, q)

	if got := s.callsFor("m1"); got != 3 {
		t.Fatalf("attempts = %d, want 3 (2 failures then success)", got)
	}
	if len(s.sent) != 1 {
		t.Fatalf("sent = %v", s.sent)
	}
}

func TestQueueStopsAfterMaxAttempts(t *testing.T) {
	s := newScripted()
	s.failN, s.failErr = 100, errors.New("timeout")
	q := mailer.NewQueue(s, fastConfig())
	_ = q.Enqueue(mailer.Message{ID: "m1"}, time.Time{})
	shutdown(t, q)

	if got := s.callsFor("m1"); got != 3 {
		t.Fatalf("attempts = %d, want MaxAttempts=3", got)
	}
}

func TestQueueDoesNotRetryPermanentErrors(t *testing.T) {
	for name, err := range map[string]error{
		"smtp 5xx":        &textproto.Error{Code: 550, Msg: "mailbox unavailable"},
		"invalid message": mailer.ErrInvalidMessage,
	} {
		t.Run(name, func(t *testing.T) {
			s := newScripted()
			s.failN, s.failErr = 100, err
			q := mailer.NewQueue(s, fastConfig())
			_ = q.Enqueue(mailer.Message{ID: "m1"}, time.Time{})
			shutdown(t, q)
			if got := s.callsFor("m1"); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
		})
	}
}

func TestQueueSkipsExpiredMessages(t *testing.T) {
	s := newScripted()
	q := mailer.NewQueue(s, fastConfig())
	_ = q.Enqueue(mailer.Message{ID: "old"}, time.Now().Add(-time.Second))
	shutdown(t, q)
	if got := s.callsFor("old"); got != 0 {
		t.Fatal("expired message was sent")
	}
}

func TestQueueBoundsConcurrencyAndRejectsWhenFull(t *testing.T) {
	s := newScripted()
	s.release = make(chan struct{})
	cfg := fastConfig()
	cfg.Workers, cfg.Buffer = 2, 3
	q := mailer.NewQueue(s, cfg)

	// 2 messages occupy the workers, 3 fill the buffer.
	accepted := 0
	deadline := time.Now().Add(2 * time.Second)
	for accepted < 5 && time.Now().Before(deadline) {
		if err := q.Enqueue(mailer.Message{ID: string(rune('a' + accepted))}, time.Time{}); err == nil {
			accepted++
		} else {
			time.Sleep(time.Millisecond) // workers not yet holding their message
		}
	}
	for s.active.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := q.Enqueue(mailer.Message{ID: "overflow"}, time.Time{}); !errors.Is(err, mailer.ErrQueueFull) {
		t.Fatalf("enqueue beyond capacity: %v, want mailer.ErrQueueFull", err)
	}

	close(s.release)
	shutdown(t, q)
	if p := s.peak.Load(); p > 2 {
		t.Fatalf("peak concurrent sends = %d, want <= Workers (2)", p)
	}
	if len(s.sent) != 5 {
		t.Fatalf("delivered %d, want all 5 accepted messages", len(s.sent))
	}
}

func TestQueueRejectsAfterShutdown(t *testing.T) {
	q := mailer.NewQueue(newScripted(), fastConfig())
	shutdown(t, q)
	if err := q.Enqueue(mailer.Message{ID: "late"}, time.Time{}); !errors.Is(err, mailer.ErrQueueClosed) {
		t.Fatalf("got %v, want mailer.ErrQueueClosed", err)
	}
	shutdown(t, q) // idempotent
}

func TestQueueShutdownDeadlineAbandonsWork(t *testing.T) {
	s := newScripted()
	s.release = make(chan struct{}) // never closed: sends hang until cancelled
	q := mailer.NewQueue(s, fastConfig())
	for i := range 4 {
		_ = q.Enqueue(mailer.Message{ID: string(rune('a' + i))}, time.Time{})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := q.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown did not return promptly after its deadline")
	}
	if len(s.sent) != 0 {
		t.Fatal("abandoned messages were reported sent")
	}
}
