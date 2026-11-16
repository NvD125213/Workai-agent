package wgtunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Tunnel is a running WireGuard device whose network stack lives in the process.
type Tunnel struct {
	dev  *device.Device
	tnet *netstack.Net
}

// Up starts a tunnel. Peer endpoints given as host names are resolved here with
// the system resolver (the VPN server itself is expected to be reachable).
//
// WireGuard has no connection setup: Up succeeds even if the server is down or
// the keys are wrong, and the problem shows up as dial timeouts.
func Up(ctx context.Context, cfg *Config) (*Tunnel, error) {
	uapi, err := ipcConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tun, tnet, err := netstack.CreateNetTUN(cfg.Addresses, cfg.DNS, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("wgtunnel: create netstack: %w", err)
	}
	logger := &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf: func(format string, args ...any) {
			slog.Warn("wgtunnel: " + fmt.Sprintf(format, args...))
		},
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), logger)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgtunnel: configure device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgtunnel: bring device up: %w", err)
	}
	return &Tunnel{dev: dev, tnet: tnet}, nil
}

// DialContext dials through the tunnel. Host names are resolved by the tunnel's
// DNS servers, so DNS-level blocking by the ISP does not apply.
func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.tnet.DialContext(ctx, network, addr)
}

func (t *Tunnel) Close() {
	t.dev.Close()
}

// ipcConfig renders cfg in the wireguard-go UAPI text format (keys in hex,
// endpoints as ip:port).
func ipcConfig(ctx context.Context, cfg *Config) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", cfg.PrivateKey.hex())
	b.WriteString("replace_peers=true\n")
	for _, p := range cfg.Peers {
		endpoint, err := resolveEndpoint(ctx, p.Endpoint)
		if err != nil {
			return "", err
		}
		// public_key starts the peer's section, so it must come first.
		fmt.Fprintf(&b, "public_key=%s\n", p.PublicKey.hex())
		if p.PresharedKey != nil {
			fmt.Fprintf(&b, "preshared_key=%s\n", p.PresharedKey.hex())
		}
		fmt.Fprintf(&b, "endpoint=%s\n", endpoint)
		if p.Keepalive > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.Keepalive)
		}
		b.WriteString("replace_allowed_ips=true\n")
		for _, a := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", a)
		}
	}
	return b.String(), nil
}

// resolveEndpoint turns "host:port" into "ip:port", preferring IPv4 since many
// home networks have no IPv6 route.
func resolveEndpoint(ctx context.Context, endpoint string) (string, error) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("wgtunnel: endpoint %q: %w", endpoint, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", fmt.Errorf("wgtunnel: endpoint %q: invalid port", endpoint)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(ip.Unmap(), uint16(port)).String(), nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return "", fmt.Errorf("wgtunnel: resolve endpoint %s: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("wgtunnel: resolve endpoint %s: no address", host)
	}
	best := ips[0].Unmap()
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			best = ip.Unmap()
			break
		}
	}
	return netip.AddrPortFrom(best, uint16(port)).String(), nil
}

// LazyDialer starts the tunnel from a config file on the first dial and reuses
// it afterwards, so a process that never needs the VPN never starts it. When
// starting fails, the next dial tries again (after retryAfter), so a VPN server
// that comes back later is picked up without a restart.
type LazyDialer struct {
	path       string
	retryAfter time.Duration

	mu       sync.Mutex
	tunnel   *Tunnel
	lastErr  error
	failedAt time.Time
}

// NewLazyDialer returns a dialer for the wg-quick config at path. The file is
// read on the first dial; call CheckConfig at startup to fail early on a bad file.
func NewLazyDialer(path string) *LazyDialer {
	return &LazyDialer{path: path, retryAfter: 30 * time.Second}
}

// CheckConfig parses the config file without starting the tunnel.
func CheckConfig(path string) error {
	_, err := LoadConfig(path)
	return err
}

func (d *LazyDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	t, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	return t.DialContext(ctx, network, addr)
}

func (d *LazyDialer) get(ctx context.Context) (*Tunnel, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tunnel != nil {
		return d.tunnel, nil
	}
	if d.lastErr != nil && time.Since(d.failedAt) < d.retryAfter {
		return nil, d.lastErr
	}
	cfg, err := LoadConfig(d.path)
	if err == nil {
		d.tunnel, err = Up(ctx, cfg)
	}
	if err != nil {
		d.lastErr, d.failedAt = err, time.Now()
		return nil, err
	}
	d.lastErr = nil
	slog.Info("wgtunnel: tunnel up", "config", d.path, "peers", len(cfg.Peers))
	return d.tunnel, nil
}

// Close stops the tunnel if it was started.
func (d *LazyDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tunnel != nil {
		d.tunnel.Close()
		d.tunnel = nil
	}
}
