package tailnet_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/tailnet"
)

// fakeBackend is a node that listens on loopback for whatever port it is
// asked for, and reports the states it is scripted with.
type fakeBackend struct {
	mu       sync.Mutex
	states   []tailnet.Status
	tls      *tls.Config
	tlsErr   error
	opened   map[string]net.Listener
	closed   bool
	listenOn func(addr string) error
}

func (b *fakeBackend) Listen(addr string) (net.Listener, error) {
	if b.listenOn != nil {
		if err := b.listenOn(addr); err != nil {
			return nil, err
		}
	}
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.opened[addr] = l
	b.mu.Unlock()
	return l, nil
}

func (b *fakeBackend) Status(context.Context) (tailnet.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.states[0]
	if len(b.states) > 1 {
		b.states = b.states[1:]
	}
	return st, nil
}

func (b *fakeBackend) TLSConfig(context.Context, string) (*tls.Config, error) {
	return b.tls, b.tlsErr
}

func (b *fakeBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, l := range b.opened {
		_ = l.Close()
	}
	return nil
}

func (b *fakeBackend) listener(addr string) net.Listener {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.opened[addr]
}

var running = tailnet.Status{Backend: "Running", DNSName: "hearsay.tail1234.ts.net."}

func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"hearsay.tail1234.ts.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

var hello = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") })

// await polls until the node reports want, failing after a few seconds.
func await(t *testing.T, n *tailnet.Node, want string) tailnet.State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st := n.State(); st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %+v, want %s", n.State(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func get(t *testing.T, scheme string, l net.Listener) string {
	t.Helper()
	client := http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, scheme+"://"+l.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func run(t *testing.T, cfg tailnet.Config) (*tailnet.Node, func()) {
	t.Helper()
	n := tailnet.New(cfg)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { n.Run(ctx, slog.New(slog.DiscardHandler), hello); close(done) }()
	return n, func() { cancel(); <-done }
}

func TestServesOnTheTailnet(t *testing.T) {
	for _, tc := range []struct {
		name         string
		http         bool
		tls          bool
		tlsErr       error
		addr, scheme string
		url          string
	}{
		{name: "HTTPS with the node's certificate", tls: true, addr: ":443", scheme: "https", url: "https://hearsay.tail1234.ts.net"},
		{name: "no certificate falls back to HTTP", tlsErr: errors.New("HTTPS is disabled for this tailnet"), addr: ":80", scheme: "http", url: "http://hearsay.tail1234.ts.net"},
		{name: "plain HTTP when asked", http: true, addr: ":80", scheme: "http", url: "http://hearsay.tail1234.ts.net"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{states: []tailnet.Status{running}, tlsErr: tc.tlsErr, opened: map[string]net.Listener{}}
			if tc.tls {
				b.tls = selfSigned(t)
			}
			var joined string
			n, stop := run(t, tailnet.Config{Hostname: "hearsay", Dir: t.TempDir(), HTTP: tc.http, Join: func(hostname, _ string) (tailnet.Backend, error) {
				joined = hostname
				return b, nil
			}})
			st := await(t, n, tailnet.StateServing)
			if st.URL != tc.url || joined != "hearsay" {
				t.Fatalf("state=%+v joined=%q", st, joined)
			}
			if body := get(t, tc.scheme, b.listener(tc.addr)); body != "hello" {
				t.Fatalf("body=%q", body)
			}
			stop()
			if !b.closed {
				t.Fatal("the node did not leave the tailnet on stop")
			}
		})
	}
}

func TestWaitsForApprovalAndRetriesAfterFailure(t *testing.T) {
	b := &fakeBackend{states: []tailnet.Status{{Backend: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc"}, {Backend: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc"}, running}, opened: map[string]net.Listener{}}
	var mu sync.Mutex
	joins := 0
	n, stop := run(t, tailnet.Config{Hostname: "hearsay", HTTP: true, RetryInterval: 10 * time.Millisecond, PollInterval: 20 * time.Millisecond,
		Join: func(string, string) (tailnet.Backend, error) {
			mu.Lock()
			defer mu.Unlock()
			joins++
			if joins == 1 {
				return nil, errors.New("control plane unreachable")
			}
			return b, nil
		}})
	defer stop()
	await(t, n, tailnet.StateLogin)
	await(t, n, tailnet.StateServing)
	mu.Lock()
	defer mu.Unlock()
	if joins != 2 {
		t.Fatalf("joins=%d", joins)
	}
}

func TestAListenerFailureIsDegradedNotFatal(t *testing.T) {
	b := &fakeBackend{states: []tailnet.Status{running}, opened: map[string]net.Listener{}, listenOn: func(string) error { return errors.New("port in use") }}
	n, stop := run(t, tailnet.Config{Hostname: "hearsay", HTTP: true, RetryInterval: time.Hour, Join: func(string, string) (tailnet.Backend, error) { return b, nil }})
	st := await(t, n, tailnet.StateDegraded)
	if st.Reason == "" {
		t.Fatalf("state=%+v", st)
	}
	stop()
}

func TestDisabledNodeDoesNothing(t *testing.T) {
	n := tailnet.New(tailnet.Config{Join: func(string, string) (tailnet.Backend, error) {
		t.Fatal("a disabled node joined")
		return nil, nil
	}})
	n.Run(t.Context(), slog.New(slog.DiscardHandler), hello)
	if n.State().State != tailnet.StateDisabled {
		t.Fatalf("state=%+v", n.State())
	}
	var none *tailnet.Node
	if none.State().State != tailnet.StateDisabled {
		t.Fatal("a nil node is not disabled")
	}
}

func TestValidHostname(t *testing.T) {
	for name, want := range map[string]bool{"hearsay": true, "hearsay-api": true, "a": true, "Hearsay": false, "hearsay.api": false, "-hearsay": false, "hearsay-": false, "": false} {
		if got := tailnet.ValidHostname(name); got != want {
			t.Errorf("ValidHostname(%q) = %v", name, got)
		}
	}
}
