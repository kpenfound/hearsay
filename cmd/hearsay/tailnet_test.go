package main

import (
	"strings"
	"testing"
)

func TestAPITailnetFlags(t *testing.T) {
	for _, tc := range []struct {
		name, hostname, dir, wantErr string
		wantNode                     bool
	}{
		{name: "no hostname serves only --listen"},
		{name: "a node", hostname: "hearsay", dir: t.TempDir(), wantNode: true},
		{name: "not a DNS label", hostname: "Hearsay.API", dir: t.TempDir(), wantErr: "not a tailnet hostname"},
		{name: "no state directory", hostname: "hearsay", wantErr: "--tailnet-dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, err := apiTailnet(t.Context(), tc.hostname, tc.dir, false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || (node != nil) != tc.wantNode {
				t.Fatalf("node=%v err=%v", node, err)
			}
		})
	}
}
