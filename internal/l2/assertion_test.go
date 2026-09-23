package l2_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestAssertionNativeID(t *testing.T) {
	base := l2.Assertion{Topic: "topic:1", Position: "take the lock first", Evidence: []string{"l1:a", "l1:b"}, Agent: "shed", Principal: "kyle"}
	tests := []struct {
		name   string
		change func(a *l2.Assertion)
		same   bool
	}{
		{"the same request", func(*l2.Assertion) {}, true},
		{"another topic", func(a *l2.Assertion) { a.Topic = "topic:2" }, false},
		{"another position", func(a *l2.Assertion) { a.Position = "take the lock last" }, false},
		{"other evidence", func(a *l2.Assertion) { a.Evidence = []string{"l1:a"} }, false},
		{"evidence that runs into the position", func(a *l2.Assertion) {
			a.Position, a.Evidence = "take the lock firstl1:a", []string{"l1:b"}
		}, false},
		{"another agent", func(a *l2.Assertion) { a.Agent = "crane" }, false},
		{"for another person", func(a *l2.Assertion) { a.Principal = "sam" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := base
			other.Evidence = append([]string(nil), base.Evidence...)
			tt.change(&other)
			if got := other.NativeID() == base.NativeID(); got != tt.same {
				t.Errorf("NativeID() equal = %v, want %v", got, tt.same)
			}
		})
	}
	id := connector.EventID(connector.SelfSource, base.NativeID())
	if _, _, err := connector.ParseEventID(id); err != nil {
		t.Errorf("the native id does not make an event id: %v", err)
	}
}

func TestAssertionOf(t *testing.T) {
	native := func(a l2.Assertion) json.RawMessage { b, _ := json.Marshal(a); return b }
	good := l2.Assertion{Topic: "topic:1", Position: "p", Evidence: []string{"l1:a"}, Agent: "shed", Principal: "kyle"}
	many := good
	many.Evidence = make([]string, l2.MaxAssertionEvidence+1)
	noAgent := good
	noAgent.Agent = ""
	tests := []struct {
		name    string
		kind    connector.Kind
		native  json.RawMessage
		wantErr bool
	}{
		{"an assertion", connector.KindAssertion, native(good), false},
		{"another kind", connector.KindAudit, native(good), true},
		{"not JSON", connector.KindAssertion, json.RawMessage(`"x"`), true},
		{"no evidence", connector.KindAssertion, native(l2.Assertion{Topic: "t", Position: "p", Agent: "shed"}), true},
		{"too much evidence", connector.KindAssertion, native(many), true},
		{"no agent", connector.KindAssertion, native(noAgent), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l2.AssertionOf(connector.Event{Kind: tt.kind, Payload: connector.Payload{Native: tt.native}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("AssertionOf() error = %v, want error %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, l2.ErrInvalid) {
				t.Errorf("AssertionOf() error = %v, want ErrInvalid", err)
			}
			if err == nil && got.Topic != good.Topic {
				t.Errorf("AssertionOf() = %+v", got)
			}
		})
	}
}

func TestAssertedTier(t *testing.T) {
	event := connector.EventID(connector.SelfSource, "assertion:1")
	agents := config.DefaultPolicy()
	agents.RatifiedBy.Artifacts = []config.ArtifactClass{config.ArtifactAgent}
	fromGitHub := agents
	fromGitHub.RatifiedBy.Sources = []string{"github"}
	fromHearsay := agents
	fromHearsay.RatifiedBy.Sources = []string{connector.SelfSource}
	tests := []struct {
		name   string
		policy config.Policy
		want   l2.Tier
	}{
		{"the default: an agent never ratifies", config.DefaultPolicy(), l2.TierInferred},
		{"a policy where agents ratify", agents, l2.TierRatified},
		{"agents ratify from another source only", fromGitHub, l2.TierInferred},
		{"agents ratify from hearsay", fromHearsay, l2.TierRatified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l2.AssertedTier(tt.policy, event); got != tt.want {
				t.Errorf("AssertedTier() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestAnAssertedStanceNeedsAValidEvent(t *testing.T) {
	st := l2.Stance{ID: "s", TopicID: "t", Position: "p", StatedAt: time.Now(), Evidence: []string{"l1:a"},
		Tier: l2.TierInferred, ACL: connector.ACL{{Kind: connector.ACLPublic}}}
	for _, tc := range []struct {
		name, assertion string
		wantErr         bool
	}{
		{"read from a document", "", false},
		{"asserted", connector.EventID(connector.SelfSource, "assertion:1"), false},
		{"not an event id", "l1:a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st.Assertion = tc.assertion
			if err := st.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
