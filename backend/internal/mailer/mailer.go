// Package mailer delivers email over SMTP.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// ErrInvalidMessage marks a message that can never be delivered as built.
var ErrInvalidMessage = errors.New("mailer: invalid message")

// Message is one outgoing plain-text email. ID must be stable across retries
// of the same message; it becomes the Message-ID so receivers can deduplicate.
type Message struct {
	ID      string
	To      string
	Subject string
	Body    string
}

type Config struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
	// Timeout bounds one whole delivery: connect, TLS, auth and transfer.
	Timeout time.Duration
}

type SMTP struct {
	cfg  Config
	from *mail.Address
}

func NewSMTP(cfg Config) (*SMTP, error) {
	if cfg.Host == "" || cfg.Port == "" {
		return nil, errors.New("mailer: SMTP_HOST and SMTP_PORT are required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	from, err := parseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("mailer: invalid SMTP_FROM: %w", err)
	}
	return &SMTP{cfg: cfg, from: from}, nil
}

// Send delivers msg. The context and cfg.Timeout both bound the delivery; on
// either, the connection is torn down so no call can hang.
func (m *SMTP) Send(ctx context.Context, msg Message) error {
	to, err := parseAddress(msg.To)
	if err != nil {
		return fmt.Errorf("%w: recipient: %v", ErrInvalidMessage, err)
	}
	data, err := m.build(msg, to)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(m.cfg.Host, m.cfg.Port))
	if err != nil {
		return fmt.Errorf("mailer: dial: %w", err)
	}
	// Unblocks any pending read/write when the context ends.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	tlsCfg := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}
	if m.cfg.Port == "465" { // implicit TLS
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("mailer: tls handshake: %w", err)
		}
		conn = tlsConn
	}

	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return fmt.Errorf("mailer: smtp greeting: %w", err)
	}
	defer c.Close()

	if _, implicit := conn.(*tls.Conn); !implicit {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("mailer: starttls: %w", err)
			}
		} else if !isLoopback(m.cfg.Host) {
			return errors.New("mailer: server does not offer STARTTLS; refusing to send in clear text")
		}
	}

	if m.cfg.Username != "" {
		// PlainAuth itself refuses to send credentials over an unencrypted
		// connection to a non-loopback host.
		auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}

	if err := c.Mail(m.from.Address); err != nil {
		return fmt.Errorf("mailer: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("mailer: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("mailer: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: end DATA: %w", err)
	}
	_ = c.Quit()
	return nil
}

func (m *SMTP) build(msg Message, to *mail.Address) ([]byte, error) {
	if strings.ContainsAny(msg.Subject, "\r\n") || strings.ContainsAny(msg.ID, "\r\n<> ") {
		return nil, fmt.Errorf("%w: header value contains forbidden characters", ErrInvalidMessage)
	}

	domain := "localhost"
	if i := strings.LastIndexByte(m.from.Address, '@'); i >= 0 {
		domain = m.from.Address[i+1:]
	}

	var buf bytes.Buffer
	hdr := func(k, v string) { buf.WriteString(k + ": " + v + "\r\n") }
	hdr("From", m.from.String())
	hdr("To", to.String())
	hdr("Subject", mime.QEncoding.Encode("UTF-8", msg.Subject))
	hdr("Date", time.Now().UTC().Format(time.RFC1123Z))
	if msg.ID != "" {
		hdr("Message-ID", "<"+msg.ID+"@"+domain+">")
	}
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", `text/plain; charset="UTF-8"`)
	hdr("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write([]byte(msg.Body)); err != nil {
		return nil, fmt.Errorf("mailer: encode body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("mailer: encode body: %w", err)
	}
	return buf.Bytes(), nil
}

// Log prints emails instead of sending them. Development only: it exposes
// message bodies, which carry one-time codes.
type Log struct{}

func (Log) Send(_ context.Context, msg Message) error {
	log.Printf("mailer(dev): to=%s subject=%q\n%s", msg.To, msg.Subject, msg.Body)
	return nil
}

// parseAddress accepts exactly one address and rejects anything that could
// smuggle extra headers or recipients.
func parseAddress(s string) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("address contains line break")
	}
	return mail.ParseAddress(s)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
