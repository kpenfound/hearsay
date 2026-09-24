package discord_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/connector/discord"
)

func TestProfileUserID(t *testing.T) {
	tests := []struct {
		link string
		want string
	}{
		{"https://discord.com/users/302100000000000003", "302100000000000003"},
		{"https://discordapp.com/users/302100000000000003/", "302100000000000003"},
		{"  https://www.discord.com/users/302100000000000003 ", "302100000000000003"},
		{"https://discord.com/users/kyle", ""},
		{"https://discord.com/users/302100000000000003/profile", ""},
		{"https://discord.com/users/302100000000000003?x=1", ""},
		{"https://discord.gg/invite", ""},
		{"https://evil.example/users/302100000000000003", ""},
		{"https://discord.com.evil.example/users/302100000000000003", ""},
		{"discord.com/users/302100000000000003", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got, ok := discord.ProfileUserID(tt.link)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("ProfileUserID(%q) = %q, %v, want %q", tt.link, got, ok, tt.want)
		}
	}
}
