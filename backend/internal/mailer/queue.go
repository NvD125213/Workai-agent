package mailer

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"net/textproto"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrQueueFull   = errors.New("mailer: queue full")
	ErrQueueClosed = errors.New("mailer: queue closed")
)

// Sender delivers one message; *SMTP and Log implement it.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

type QueueConfig struct {
	// Workers is the number of concurrent deliveries. Sending is network-bound,
	// so this may exceed the CPU count; it is capped by what the SMTP relay accepts.
	Workers int
	// Buffer is how many messages may wait for a worker before Enqueue rejects.
	Buffer      int
	MaxAttempts int
	SendTimeout time.Duration // per attempt
	RetryBase   time.Duration // first retry delay, doubled each attempt
}

func (c *QueueConfig) applyDefaults() {
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.Buffer <= 0 {
		c.Buffer = 1000
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 4
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = 15 * time.Second
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 2 * time.Second
	}
}

type job struct {
	msg       Message
	expiresAt time.Time // zero means no expiry
}

// Queue sends email in the background on a fixed pool of workers fed by a
// bounded channel, so request handlers never wait on the SMTP server.
//
// Messages live only in memory: anything still queued when the process dies
// is lost. That suits short-lived, user-retryable mail such as OTPs; it is not
// a durable queue.
type Queue struct {
	sender Sender
	cfg    QueueConfig
	now    func() time.Time

	jobs   chan job
	mu     sync.RWMutex // guards closed against a send on the closed channel
	closed bool
	wg     sync.WaitGroup

	// stop is cancelled when Shutdown's deadline passes: in-flight sends and
	// retry waits are abandoned and remaining jobs are dropped.
	stop    context.Context
	abort   context.CancelFunc
	dropped atomic.Int64
}

// NewQueue starts cfg.Workers delivery goroutines. Call Shutdown to stop them.
func NewQueue(sender Sender, cfg QueueConfig) *Queue {
	cfg.applyDefaults()
	stop, abort := context.WithCancel(context.Background())
	q := &Queue{
		sender: sender,
		cfg:    cfg,
		now:    time.Now,
		jobs:   make(chan job, cfg.Buffer),
		stop:   stop,
		abort:  abort,
	}
	q.wg.Add(cfg.Workers)
	for range cfg.Workers {
		go q.worker()
	}
	return q
}

// Enqueue hands msg to the workers without blocking. A message still undelivered
// at expiresAt is dropped (pass the zero time for no expiry). It returns
// ErrQueueFull when the buffer is full and ErrQueueClosed after Shutdown.
func (q *Queue) Enqueue(msg Message, expiresAt time.Time) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ErrQueueClosed
	}
	select {
	case q.jobs <- job{msg: msg, expiresAt: expiresAt}:
		return nil
	default:
		return ErrQueueFull
	}
}

// Shutdown stops accepting messages and waits for queued ones to be delivered.
// If ctx ends first, pending work is abandoned and ctx.Err() is returned.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.jobs)
	}
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	var err error
	select {
	case <-done:
	case <-ctx.Done():
		q.abort()
		<-done // workers return promptly once stop is cancelled
		err = ctx.Err()
	}
	q.abort() // release the context's resources
	if n := q.dropped.Load(); n > 0 {
		log.Printf("mailer: %d message(s) dropped at shutdown", n)
	}
	return err
}

func (q *Queue) worker() {
	defer q.wg.Done()
	for j := range q.jobs {
		if q.stop.Err() != nil {
			q.dropped.Add(1)
			continue // drain without sending
		}
		q.deliver(j)
	}
}

// deliver retries transient failures with backoff. The worker is held during
// the wait; the wait is short and bounded by MaxAttempts, and it keeps the
// number of concurrent SMTP connections at Workers.
func (q *Queue) deliver(j job) {
	defer func() {
		// A bug in one delivery must not take down the API process.
		if r := recover(); r != nil {
			log.Printf("mailer: panic delivering %s: %v", j.msg.ID, r)
		}
	}()

	for attempt := 1; ; attempt++ {
		if !j.expiresAt.IsZero() && !q.now().Before(j.expiresAt) {
			log.Printf("mailer: %s expired before delivery", j.msg.ID)
			return
		}

		ctx, cancel := context.WithTimeout(q.stop, q.cfg.SendTimeout)
		err := q.sender.Send(ctx, j.msg)
		cancel()
		if err == nil {
			return
		}
		if q.stop.Err() != nil {
			q.dropped.Add(1)
			return
		}
		// Logs carry the message ID, never the recipient or body (an OTP).
		if isPermanent(err) || attempt >= q.cfg.MaxAttempts {
			log.Printf("mailer: giving up on %s after %d attempt(s): %v", j.msg.ID, attempt, err)
			return
		}

		t := time.NewTimer(backoff(q.cfg.RetryBase, attempt))
		select {
		case <-t.C:
		case <-q.stop.Done():
			t.Stop()
			q.dropped.Add(1)
			return
		}
	}
}

// isPermanent reports failures a retry cannot fix: a malformed message, or an
// SMTP 5xx reply (e.g. unknown mailbox, rejected sender).
func isPermanent(err error) bool {
	if errors.Is(err, ErrInvalidMessage) {
		return true
	}
	var tpErr *textproto.Error
	return errors.As(err, &tpErr) && tpErr.Code >= 500
}

// backoff doubles from base with ±20% jitter so retries after an SMTP outage
// do not arrive in lock step.
func backoff(base time.Duration, attempt int) time.Duration {
	d := base << min(attempt-1, 10)
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}
