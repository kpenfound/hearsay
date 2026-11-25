//go:build integration

package discord_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/poll"
)

func TestDiscordResyncResumesThroughSQLStore(t *testing.T) {
	pool := dbtest.Pool(t)
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
	start := func() func() {
		t.Helper()
		_, stop := runRuntime(t, src, func(o *connector.RuntimeOptions) {
			o.Sink = events
			o.Resyncs = resyncs
		})
		return stop
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
	poll.Until(t, "the first page cursor to be stored", func() bool { return state().Cursor != "" })
	stop()
	if r := state(); !r.Owed || r.Cursor == "" {
		t.Fatalf("interrupted re-sync = %+v", r)
	}
	f.mu.Lock()
	calls := len(f.pages)
	f.mu.Unlock()
	stop = start()
	close(hold)
	poll.Until(t, "the resumed re-sync to finish", func() bool { return !state().Owed })
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
