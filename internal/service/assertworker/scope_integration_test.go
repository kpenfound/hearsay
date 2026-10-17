//go:build integration

package assertworker_test

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func TestControlEventsUseTheirRootsTouches(t *testing.T) {
	for _, kind := range []string{"command", "reaction"} {
		t.Run(kind, func(t *testing.T) {
			c := newCommands(t, scratchPool(t))
			root := c.f.issue
			root.NativeID = root.Payload.Artifact + "@touches"
			root.Payload.Revision = &connector.Revision{Token: "touches", EditedAt: at(12)}
			root.Payload.Touches = []connector.Touch{{Source: "covered", Project: "acme/api"}}
			c.append(root)
			c.repo.Scopes = []config.Scope{{ID: "touched", Sources: []config.ScopeSource{{Source: "covered", Containers: []string{"acme/api"}}}}}
			var control connector.Event
			var once func() (int, error)
			if kind == "command" {
				control = c.comment(12, "u1", "/hearsay pin")
				follower := assertworker.NewCommandFollower(c.pool, c.repo, 0, 0)
				once = func() (int, error) { return follower.Once(t.Context()) }
			} else {
				c.repo.Sources[0].Type = "discord"
				c.repo.Sources[0].Settings = json.RawMessage(`{"guild":"123456789012345677"}`)
				control = event(c.src, connector.KindReaction, root.Payload.Artifact+":reaction:u1", at(10), "", "u1", "", "")
				control.Payload.Parent = root.Payload.Artifact
				control.Payload.Native = json.RawMessage(`{"emoji":"✅"}`)
				control = c.append(control)
				follower := assertworker.NewGestureFollower(c.pool, c.repo, gestureRegistry(t))
				once = func() (int, error) { return follower.Once(t.Context()) }
			}
			check := func(ev connector.Event) {
				t.Helper()
				waitFor(t, "control event to be enqueued", func() bool {
					if _, err := once(); err != nil {
						t.Fatal(err)
					}
					var key string
					if err := c.pool.QueryRow(t.Context(), `SELECT serial_key FROM queue_job WHERE target_id = $1`, l2.GestureTarget+ev.ID).Scan(&key); err != nil {
						return false
					}
					if key != "touched" {
						t.Fatalf("control serial key = %q", key)
					}
					return true
				})
			}
			check(control)
			check(c.delete(control))
		})
	}
}
