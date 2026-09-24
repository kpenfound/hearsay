package discord_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/connector/discord"
)

func TestReactionEmojis(t *testing.T) {
	for _, tc := range []struct {
		name, ratify, demote, wantRatify, wantDemote string
		bad                                          bool
	}{
		{"defaults", "", "", "✅", "👎", false},
		{"custom", "👍", "👎🏽", "👍", "👎🏽", false},
		{"same", "👍", "👍", "", "", true},
		{"whitespace", " 👍", "👎", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d, err := (discord.Settings{RatifyEmoji: tc.ratify, DemoteEmoji: tc.demote}).ReactionEmojis()
			if (err != nil) != tc.bad || (!tc.bad && (r != tc.wantRatify || d != tc.wantDemote)) {
				t.Fatalf("ReactionEmojis() = %q, %q, %v", r, d, err)
			}
		})
	}
}
