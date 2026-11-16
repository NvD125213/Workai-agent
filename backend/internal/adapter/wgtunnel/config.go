// Package wgtunnel runs a WireGuard tunnel inside the process (wireguard-go with a
// gVisor netstack) and dials TCP connections through it.
//
// It needs no TUN driver, no admin rights and changes no system routes: only the
// connections made with Tunnel.DialContext go through the VPN, and their DNS
// lookups are sent to the tunnel's DNS servers too. It is used for job sites that
// Vietnamese ISPs block (LinkedIn) while every other request goes out directly.
package wgtunnel

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

const defaultMTU = 1420 // wg-quick default

// defaultDNS is used when the config has no DNS line (Cloudflare, queried through the tunnel).
var defaultDNS = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("1.0.0.1")}

// Config is a WireGuard configuration in the wg-quick format, the .conf file VPN
// providers (Mullvad, ProtonVPN, Cloudflare WARP via wgcf, your own server...) hand out:
//
//	[Interface]
//	PrivateKey = <base64>
//	Address = 10.2.0.2/32, fd00::2/128
//	DNS = 10.2.0.1
//
//	[Peer]
//	PublicKey = <base64>
//	Endpoint = vpn.example.com:51820
//	AllowedIPs = 0.0.0.0/0, ::/0
//	PersistentKeepalive = 25
type Config struct {
	PrivateKey Key
	Addresses  []netip.Addr // interface addresses, without the prefix length
	DNS        []netip.Addr
	MTU        int
	Peers      []Peer
}

type Peer struct {
	PublicKey    Key
	PresharedKey *Key
	Endpoint     string // host:port; the host may be a name, resolved when the tunnel starts
	AllowedIPs   []netip.Prefix
	Keepalive    int // seconds, 0 = off
}

// Key is a Curve25519 key (or preshared key).
type Key [32]byte

func (k Key) hex() string { return hex.EncodeToString(k[:]) }

func parseKey(s string) (Key, error) {
	var k Key
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != len(k) {
		return k, errors.New("invalid key, want 32 bytes in base64")
	}
	copy(k[:], b)
	return k, nil
}

// LoadConfig reads a wg-quick .conf file.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("wgtunnel: %w", err)
	}
	defer f.Close()
	cfg, err := ParseConfig(f)
	if err != nil {
		return nil, fmt.Errorf("wgtunnel: %s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig parses a wg-quick configuration. wg-quick-only keys (PostUp, Table,
// SaveConfig, ...) are ignored since no system interface is created.
func ParseConfig(r io.Reader) (*Config, error) {
	cfg := &Config{MTU: defaultMTU}
	var (
		section    string
		peer       *Peer
		havePriv   bool
		havePubKey bool
	)
	finishPeer := func() error {
		if peer == nil {
			return nil
		}
		if !havePubKey {
			return errors.New("[Peer] without PublicKey")
		}
		if peer.Endpoint == "" {
			return errors.New("[Peer] without Endpoint")
		}
		if len(peer.AllowedIPs) == 0 {
			return errors.New("[Peer] without AllowedIPs")
		}
		cfg.Peers = append(cfg.Peers, *peer)
		peer, havePubKey = nil, false
		return nil
	}

	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			if err := finishPeer(); err != nil {
				return nil, err
			}
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch section {
			case "interface":
			case "peer":
				peer = &Peer{}
			default:
				return nil, fmt.Errorf("line %d: unknown section %s", n, line)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: want Key = Value", n)
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)

		var err error
		switch section {
		case "interface":
			err = cfg.set(key, value)
			if key == "privatekey" && err == nil {
				havePriv = true
			}
		case "peer":
			err = peer.set(key, value)
			if key == "publickey" && err == nil {
				havePubKey = true
			}
		default:
			err = errors.New("key outside of [Interface] or [Peer]")
		}
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): %w", n, key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := finishPeer(); err != nil {
		return nil, err
	}

	switch {
	case !havePriv:
		return nil, errors.New("[Interface] without PrivateKey")
	case len(cfg.Addresses) == 0:
		return nil, errors.New("[Interface] without Address")
	case len(cfg.Peers) == 0:
		return nil, errors.New("no [Peer]")
	}
	if len(cfg.DNS) == 0 {
		cfg.DNS = defaultDNS
	}
	return cfg, nil
}

func (c *Config) set(key, value string) error {
	switch key {
	case "privatekey":
		k, err := parseKey(value)
		c.PrivateKey = k
		return err
	case "address":
		for _, v := range splitList(value) {
			p, err := netip.ParsePrefix(v)
			if err != nil {
				a, aerr := netip.ParseAddr(v)
				if aerr != nil {
					return err
				}
				p = netip.PrefixFrom(a, a.BitLen())
			}
			c.Addresses = append(c.Addresses, p.Addr())
		}
	case "dns":
		// wg-quick also allows search domains here; only the server addresses matter.
		for _, v := range splitList(value) {
			if a, err := netip.ParseAddr(v); err == nil {
				c.DNS = append(c.DNS, a)
			}
		}
	case "mtu":
		mtu, err := strconv.Atoi(value)
		if err != nil || mtu < 576 || mtu > 9000 {
			return errors.New("invalid MTU")
		}
		c.MTU = mtu
	}
	return nil
}

func (p *Peer) set(key, value string) error {
	switch key {
	case "publickey":
		k, err := parseKey(value)
		p.PublicKey = k
		return err
	case "presharedkey":
		k, err := parseKey(value)
		p.PresharedKey = &k
		return err
	case "endpoint":
		if _, _, err := net.SplitHostPort(value); err != nil {
			return err
		}
		p.Endpoint = value
	case "allowedips":
		for _, v := range splitList(value) {
			pr, err := netip.ParsePrefix(v)
			if err != nil {
				return err
			}
			p.AllowedIPs = append(p.AllowedIPs, pr.Masked())
		}
	case "persistentkeepalive":
		if strings.EqualFold(value, "off") {
			return nil
		}
		s, err := strconv.Atoi(value)
		if err != nil || s < 0 || s > 65535 {
			return errors.New("invalid PersistentKeepalive")
		}
		p.Keepalive = s
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
