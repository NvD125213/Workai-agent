package wgtunnel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func newKeyPair(t *testing.T) (priv, pub Key) {
	t.Helper()
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatal(err)
	}
	priv[0] &= 248
	priv[31] = (priv[31] & 127) | 64
	p, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	copy(pub[:], p)
	return priv, pub
}

func b64(k Key) string { return base64.StdEncoding.EncodeToString(k[:]) }

func TestParseConfig(t *testing.T) {
	priv, _ := newKeyPair(t)
	_, peerPub := newKeyPair(t)
	psk, _ := newKeyPair(t)
	conf := fmt.Sprintf(`
# from a VPN provider
[Interface]
PrivateKey = %s
Address = 10.2.0.2/32, fd00::2/128
DNS = 10.2.0.1, example.internal
MTU = 1280
PostUp = iptables -A ...   # ignored

[Peer]
PublicKey = %s
PresharedKey = %s
Endpoint = vpn.example.com:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
`, b64(priv), b64(peerPub), b64(psk))

	cfg, err := ParseConfig(strings.NewReader(conf))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrivateKey != priv || cfg.MTU != 1280 {
		t.Errorf("interface = %+v", cfg)
	}
	wantAddrs := []netip.Addr{netip.MustParseAddr("10.2.0.2"), netip.MustParseAddr("fd00::2")}
	if fmt.Sprint(cfg.Addresses) != fmt.Sprint(wantAddrs) {
		t.Errorf("Addresses = %v", cfg.Addresses)
	}
	if fmt.Sprint(cfg.DNS) != "[10.2.0.1]" {
		t.Errorf("DNS = %v (search domains must be skipped)", cfg.DNS)
	}
	if len(cfg.Peers) != 1 {
		t.Fatalf("peers = %d", len(cfg.Peers))
	}
	p := cfg.Peers[0]
	if p.PublicKey != peerPub || p.PresharedKey == nil || *p.PresharedKey != psk ||
		p.Endpoint != "vpn.example.com:51820" || p.Keepalive != 25 || fmt.Sprint(p.AllowedIPs) != "[0.0.0.0/0 ::/0]" {
		t.Errorf("peer = %+v", p)
	}
}

func TestParseConfigDefaultsAndErrors(t *testing.T) {
	priv, pub := newKeyPair(t)
	ok := fmt.Sprintf("[Interface]\nPrivateKey=%s\nAddress=10.0.0.2\n[Peer]\nPublicKey=%s\nEndpoint=1.2.3.4:51820\nAllowedIPs=0.0.0.0/0\n",
		b64(priv), b64(pub))
	cfg, err := ParseConfig(strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MTU != defaultMTU || fmt.Sprint(cfg.DNS) != fmt.Sprint(defaultDNS) || cfg.Addresses[0].String() != "10.0.0.2" {
		t.Errorf("defaults = %+v", cfg)
	}

	bad := map[string]string{
		"no private key": strings.Replace(ok, "PrivateKey="+b64(priv)+"\n", "", 1),
		"bad key":        strings.Replace(ok, b64(pub), "not-a-key", 1),
		"no endpoint":    strings.Replace(ok, "Endpoint=1.2.3.4:51820\n", "", 1),
		"no peer":        ok[:strings.Index(ok, "[Peer]")],
		"bad section":    strings.Replace(ok, "[Peer]", "[Pear]", 1),
		"bad endpoint":   strings.Replace(ok, "1.2.3.4:51820", "1.2.3.4", 1),
	}
	for name, conf := range bad {
		if _, err := ParseConfig(strings.NewReader(conf)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// TestTunnelEndToEnd starts a second, in-process WireGuard peer acting as the
// VPN server, serves HTTP on its tunnel address and fetches it through Up.
func TestTunnelEndToEnd(t *testing.T) {
	serverPriv, serverPub := newKeyPair(t)
	clientPriv, clientPub := newKeyPair(t)
	serverIP, clientIP := netip.MustParseAddr("10.66.0.1"), netip.MustParseAddr("10.66.0.2")

	// Server: listens on a free UDP port of 127.0.0.1.
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	udp.Close()

	stun, snet, err := netstack.CreateNetTUN([]netip.Addr{serverIP}, nil, defaultMTU)
	if err != nil {
		t.Fatal(err)
	}
	sdev := device.NewDevice(stun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "server: "))
	defer sdev.Close()
	if err := sdev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\npublic_key=%s\nallowed_ip=%s/32\n",
		serverPriv.hex(), port, clientPub.hex(), clientIP)); err != nil {
		t.Fatal(err)
	}
	if err := sdev.Up(); err != nil {
		t.Fatal(err)
	}
	ln, err := snet.ListenTCP(&net.TCPAddr{IP: serverIP.AsSlice(), Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s", r.Host)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// Client: the code under test.
	conf := fmt.Sprintf("[Interface]\nPrivateKey=%s\nAddress=%s/32\n[Peer]\nPublicKey=%s\nEndpoint=127.0.0.1:%d\nAllowedIPs=%s/32\nPersistentKeepalive=25\n",
		b64(clientPriv), clientIP, b64(serverPub), port, serverIP)
	cfg, err := ParseConfig(strings.NewReader(conf))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tun, err := Up(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	hc := &http.Client{Transport: &http.Transport{DialContext: tun.DialContext}, Timeout: 15 * time.Second}
	resp, err := hc.Get("http://" + serverIP.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from "+serverIP.String() {
		t.Errorf("body = %q", body)
	}
}

func TestLazyDialerRetriesBadConfig(t *testing.T) {
	d := NewLazyDialer(t.TempDir() + "/missing.conf")
	if _, err := d.DialContext(context.Background(), "tcp", "10.0.0.1:80"); err == nil {
		t.Fatal("want error for a missing config")
	}
	// The error is cached until retryAfter; with 0 the file is read again.
	d.retryAfter = 0
	if _, err := d.DialContext(context.Background(), "tcp", "10.0.0.1:80"); err == nil {
		t.Fatal("want error again")
	}
}
