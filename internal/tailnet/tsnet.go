package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"tailscale.com/tsnet"
)

// AuthKeyEnv names the variable the node reads its auth key from on first
// login. The key never reaches a log.
const AuthKeyEnv = "TS_AUTHKEY"

// JoinTailscale joins through embedded Tailscale. Its own log lines reach log
// at debug, with the auth key redacted, except the certificate lines, which
// reach it at info so that a slow or failed issuance shows its progress.
func JoinTailscale(log *slog.Logger) Join {
	return func(hostname, dir string) (Backend, error) {
		server := &tsnet.Server{
			Dir:      dir,
			Hostname: hostname,
			AuthKey:  os.Getenv(AuthKeyEnv),
			UserLogf: logf(log, slog.LevelDebug, ""),
			Logf:     logf(log, slog.LevelInfo, "cert("),
		}
		if err := server.Start(); err != nil {
			_ = server.Close()
			return nil, err
		}
		return tsnetBackend{server: server}, nil
	}
}

func logf(log *slog.Logger, level slog.Level, only string) func(string, ...any) {
	return func(format string, args ...any) {
		if only != "" && !strings.HasPrefix(format, only) {
			return
		}
		line := strings.TrimSpace(fmt.Sprintf(format, args...))
		if line == "" {
			return
		}
		if key := os.Getenv(AuthKeyEnv); key != "" {
			line = strings.ReplaceAll(line, key, "[redacted]")
		}
		log.Log(context.Background(), level, "tailnet message", "component", "tailnet", "message", line)
	}
}

type tsnetBackend struct {
	server *tsnet.Server
}

func (b tsnetBackend) Listen(addr string) (net.Listener, error) {
	return b.server.Listen("tcp", addr)
}

func (b tsnetBackend) Status(ctx context.Context) (Status, error) {
	client, err := b.server.LocalClient()
	if err != nil {
		return Status{}, err
	}
	st, err := client.StatusWithoutPeers(ctx)
	if err != nil {
		return Status{}, err
	}
	out := Status{Backend: st.BackendState, AuthURL: st.AuthURL}
	if st.Self != nil {
		out.DNSName = st.Self.DNSName
	}
	return out, nil
}

func (b tsnetBackend) TLSConfig(ctx context.Context, name string) (*tls.Config, error) {
	client, err := b.server.LocalClient()
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := client.CertPair(ctx, name)
	if err == nil {
		_, err = tls.X509KeyPair(certPEM, keyPEM)
	}
	if err != nil {
		return nil, err
	}
	return &tls.Config{GetCertificate: client.GetCertificate}, nil
}

func (b tsnetBackend) Close() error {
	if err := b.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
