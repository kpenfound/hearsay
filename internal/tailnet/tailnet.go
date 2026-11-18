// Package tailnet serves a Hearsay service on the owner's tailnet through an
// embedded Tailscale node, so agents on other machines reach it at
// https://<hostname>.<tailnet>.ts.net without a proxy in front.
//
// The node only listens: nothing in Hearsay dials out through it, and model,
// Jev and database traffic uses the host's network. The node is an addition to
// the service's own listener, never a replacement, so a tailnet failure never
// stops the service: [Node.Run] logs it and tries again while the rest runs.
//
// The node reads TS_AUTHKEY on its first login; without one it logs a login
// URL until someone approves it. Later starts use the node's state in its
// directory, which must persist.
package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Status is what a backend knows about its node.
type Status struct {
	// Backend is Tailscale's backend state: Running, NeedsLogin,
	// NeedsMachineAuth, Starting and so on.
	Backend string
	// AuthURL is the login URL shown while the node waits for approval.
	AuthURL string
	// DNSName is the node's full MagicDNS name, such as
	// hearsay.tail1234.ts.net., once it has one.
	DNSName string
}

// Backend is one joined tailnet node. It only listens.
type Backend interface {
	// Listen accepts connections on addr, such as ":443", from the tailnet.
	Listen(addr string) (net.Listener, error)
	// Status reports the node's state.
	Status(context.Context) (Status, error)
	// TLSConfig fetches the certificate for name ahead of serving, so that an
	// issuance failure is an error rather than a failed handshake, and returns
	// a configuration that serves it.
	TLSConfig(ctx context.Context, name string) (*tls.Config, error)
	// Close leaves the tailnet and ends every open connection.
	Close() error
}

// Join joins the tailnet as hostname, keeping the node's state in dir.
type Join func(hostname, dir string) (Backend, error)

// Config configures a [Node].
type Config struct {
	// Hostname is the node's name on the tailnet, one DNS label. Empty
	// disables the node.
	Hostname string
	// Dir is the node's state directory. It must persist across restarts, or
	// every start is a new node that needs approving.
	Dir string
	// HTTP serves plain HTTP on port 80 instead of HTTPS on 443. Without it,
	// a certificate the tailnet will not issue (HTTPS is off in its admin
	// settings) falls back to HTTP on 80, with a warning.
	HTTP bool
	// Join starts the node: [JoinTailscale] for the real one.
	Join Join
	// RetryInterval is how long Run waits before it joins again after a
	// failure. Zero means 30 seconds.
	RetryInterval time.Duration
	// PollInterval is how often Run checks a joining node's state. Zero means
	// two seconds.
	PollInterval time.Duration
}

// The states a node reports.
const (
	StateDisabled = "disabled"
	StateStarting = "starting"
	StateLogin    = "needs_login"
	StateServing  = "serving"
	StateDegraded = "degraded"
)

// State is a node's state as the service reports it. It carries no secret.
type State struct {
	State string `json:"state"`
	// URL is where the service is served, once it is.
	URL string `json:"url,omitempty"`
	// Reason says why the node is degraded.
	Reason string `json:"reason,omitempty"`
}

// Node is a service's hold on its tailnet node.
type Node struct {
	cfg Config

	mu    sync.Mutex
	state State
}

// ValidHostname reports whether name is one DNS label of lowercase letters,
// digits and hyphens, which is what a tailnet node may be called.
func ValidHostname(name string) bool { return hostname.MatchString(name) }

var hostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// New returns a Node. With an empty hostname it is disabled, and Run returns
// at once.
func New(cfg Config) *Node {
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	n := &Node{cfg: cfg, state: State{State: StateStarting}}
	if cfg.Hostname == "" {
		n.state = State{State: StateDisabled}
	}
	return n
}

// State reports the node's state. A nil Node is disabled.
func (n *Node) State() State {
	if n == nil {
		return State{State: StateDisabled}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state
}

func (n *Node) set(s State) {
	n.mu.Lock()
	n.state = s
	n.mu.Unlock()
}

// Run serves handler on the tailnet until ctx is cancelled, joining again
// after any failure. It returns nil when ctx ends; a tailnet failure is a
// degraded state, logged, never an error.
func (n *Node) Run(ctx context.Context, log *slog.Logger, handler http.Handler) {
	if n == nil || n.cfg.Hostname == "" {
		return
	}
	for {
		err := n.serve(ctx, log, handler)
		if ctx.Err() != nil {
			return
		}
		n.set(State{State: StateDegraded, Reason: reason(err)})
		log.WarnContext(ctx, "tailnet listener degraded; retrying", "hostname", n.cfg.Hostname, "reason", reason(err), "retry_in", n.cfg.RetryInterval.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.cfg.RetryInterval):
		}
	}
}

// reason is an error as a state's reason: what failed, with no secret.
func reason(err error) string {
	if err == nil {
		return "the tailnet listener stopped"
	}
	return err.Error()
}

// serve joins once and serves until the listener fails or ctx ends.
func (n *Node) serve(ctx context.Context, log *slog.Logger, handler http.Handler) error {
	n.set(State{State: StateStarting})
	backend, err := n.cfg.Join(n.cfg.Hostname, n.cfg.Dir)
	if err != nil {
		return fmt.Errorf("joining the tailnet: %w", err)
	}
	defer func() { _ = backend.Close() }()
	dnsName, err := n.await(ctx, log, backend)
	if err != nil {
		return err
	}
	listener, url, err := n.listen(ctx, log, backend, dnsName)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	n.set(State{State: StateServing, URL: url})
	log.InfoContext(ctx, "serving on the tailnet", "url", url)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		return err
	case <-ctx.Done():
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(grace)
		<-done
		return nil
	}
}

// await waits for the node to be running and returns its MagicDNS name, without
// the trailing dot. While it waits for approval the login URL is logged once.
func (n *Node) await(ctx context.Context, log *slog.Logger, backend Backend) (string, error) {
	loggedURL := ""
	for {
		st, err := backend.Status(ctx)
		if err != nil {
			return "", fmt.Errorf("reading the tailnet node's state: %w", err)
		}
		if st.Backend == "Running" && st.DNSName != "" {
			return strings.TrimSuffix(st.DNSName, "."), nil
		}
		if st.AuthURL != "" {
			n.set(State{State: StateLogin, Reason: "waiting for the node to be approved"})
			if st.AuthURL != loggedURL {
				log.WarnContext(ctx, "the tailnet node needs approving: open the login URL, or set TS_AUTHKEY and restart", "hostname", n.cfg.Hostname, "login_url", st.AuthURL)
				loggedURL = st.AuthURL
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(n.cfg.PollInterval):
		}
	}
}

// listen opens HTTPS on 443 with the node's certificate, or HTTP on 80 when the
// configuration asks for it or the certificate cannot be had.
func (n *Node) listen(ctx context.Context, log *slog.Logger, backend Backend, dnsName string) (net.Listener, string, error) {
	if !n.cfg.HTTP {
		cfg, err := backend.TLSConfig(ctx, dnsName)
		if err == nil {
			l, err := backend.Listen(":443")
			if err != nil {
				return nil, "", fmt.Errorf("listening on the tailnet's port 443: %w", err)
			}
			return tls.NewListener(l, cfg), "https://" + dnsName, nil
		}
		log.WarnContext(ctx, "no tailnet certificate; serving plain HTTP on port 80 (enable HTTPS in the tailnet's admin settings)", "hostname", n.cfg.Hostname, "reason", err.Error())
	}
	l, err := backend.Listen(":80")
	if err != nil {
		return nil, "", fmt.Errorf("listening on the tailnet's port 80: %w", err)
	}
	return l, "http://" + dnsName, nil
}
