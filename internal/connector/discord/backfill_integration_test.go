//go:build integration

package discord_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// A durable re-sync resumes at its saved message snowflake in a new runtime,
// then replaces the public current ACL for every artifact in the channel.
func TestDiscordResyncResumesThroughSQLStore(t *testing.T) {
	database := os.Getenv("HEARSAY_DATABASE_URL")
	if database == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &restFixture{gatewaySilent: true}
	for i := 0; i < 101; i++ {
		f.messages = append(f.messages, f.message(testChannel, fmt.Sprint(1551744840499200200-i)))
	}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := discordSource(server.URL)
	src.ID = "disc" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	events := l0.New(pool)
	gate := connector.NewGate(events, src.ID, c.Describe(), connector.NewAllowlist(src))
	walkDiscord(t, c, gate, "")
	f.mu.Lock()
	f.private = true
	hold := make(chan struct{})
	f.holdPage = hold
	f.pageEntered = make(chan struct{}, 1)
	f.mu.Unlock()
	resyncs := l0.NewResyncs(pool)
	if err := resyncs.Owe(t.Context(), src.ID, testChannel); err != nil {
		t.Fatal(err)
	}
	registry := connector.NewRegistry()
	if err := registry.Register(discord.Type, discord.Factory); err != nil {
		t.Fatal(err)
	}
	start := func() func() {
		t.Helper()
		runtime, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{src}, Registry: registry, Sink: events, Resyncs: resyncs, Lookup: func(string) (string, bool) { return "bot-token", true }, Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runtime.Run(ctx) }()
		return func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("runtime did not stop")
			}
		}
	}
	state := func() connector.Resync {
		t.Helper()
		all, err := resyncs.Resyncs(t.Context(), src.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 {
			t.Fatalf("re-sync rows = %+v", all)
		}
		return all[0]
	}
	stop := start()
	select {
	case <-f.pageEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second page was not requested")
	}
	deadline := time.Now().Add(5 * time.Second)
	for state().Cursor == "" {
		if time.Now().After(deadline) {
			t.Fatal("first page cursor not stored")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if r := state(); !r.Owed || r.Cursor == "" {
		t.Fatalf("interrupted re-sync = %+v", r)
	}
	f.mu.Lock()
	calls := len(f.pages)
	f.mu.Unlock()
	stop = start()
	close(hold)
	deadline = time.Now().Add(5 * time.Second)
	for state().Owed {
		if time.Now().After(deadline) {
			t.Fatal("resumed re-sync did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	f.mu.Lock()
	resumed := append([]string(nil), f.pages[calls:]...)
	f.mu.Unlock()
	for _, before := range resumed {
		if before == "" {
			t.Errorf("restart returned to first message page: %v", resumed)
		}
	}
	current, err := events.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 101 {
		t.Fatalf("current messages = %d, want 101", len(current))
	}
	for _, ev := range current {
		if len(ev.ACL) != 1 || ev.ACL[0].Kind != connector.ACLGroup || ev.ACL[0].NativeID != testChannel {
			t.Errorf("current ACL of %s = %+v", ev.NativeID, ev.ACL)
		}
	}
}
