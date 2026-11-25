package tailnet

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func capture(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})), &buf
}

func TestTailscaleLogLinesAtDebugCarryNoAuthKey(t *testing.T) {
	const key = "tskey-auth-kQ3z9example"
	t.Setenv(AuthKeyEnv, key)
	log, buf := capture(slog.LevelDebug)

	logf(log, slog.LevelDebug, "")("login with %s failed", key)

	if strings.Contains(buf.String(), key) {
		t.Errorf("debug output carries the auth key: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Errorf("debug output = %q, want the line kept with the key redacted", buf.String())
	}
}

func TestTailscaleLogLinesAtInfoAreCertificateProgressOnly(t *testing.T) {
	log, buf := capture(slog.LevelInfo)
	certLines := logf(log, slog.LevelInfo, "cert(")

	certLines("cert(%q): waiting for issuance", "hearsay.ts.net")
	certLines("magicsock: connected to %s", "derp-1")

	out := buf.String()
	if !strings.Contains(out, "waiting for issuance") {
		t.Errorf("output = %q, want the certificate line", out)
	}
	if strings.Contains(out, "magicsock") {
		t.Errorf("output = %q, want only certificate lines at info level", out)
	}
}
