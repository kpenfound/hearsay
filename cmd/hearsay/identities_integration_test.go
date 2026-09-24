//go:build integration

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestIdentitiesCommandReadsHintsOnly(t *testing.T) {
	url, pool := deleteDatabase(t, "identities_cli_")
	configPath := writeConfig(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github]\n",
		"principals/p.yaml":   "- id: alice\n  name: Alice Smith\n  identities: [{source: github, native_id: known, handle: alice}]\n",
	})
	put := func(source, artifact, native, kind string) {
		t.Helper()
		payload := `{"artifact":"` + artifact + `","text":"PAYLOAD_SENTINEL","author":{"source":"` + source + `","kind":"` + kind + `","native_id":"` + native + `","handle":"missing","display_name":"Alice Smith"},"participants":[{"identity":{"source":"github","kind":"bot","native_id":"bot"}}],"mentions":[{"source":"github","kind":"user","native_id":"mentioned"}]}`
		_, err := pool.Exec(t.Context(), `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl) VALUES($1,$2,$3,'message',$4,$5,$6,'[{"kind":"public"}]')`, source+":"+artifact, source, artifact, artifact, time.Now().UTC(), payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	put("github", "one", "u1", "user")
	put("github", "two", "u1", "user")
	put("github", "three", "bot-author", "bot")
	put("hearsay", "own", "own", "user")
	for _, format := range []string{"--json", "--yaml"} {
		var out, errout bytes.Buffer
		if err := run(t.Context(), []string{"identities", "list", "--config", configPath, "--database-url", url, format}, &out, &errout); err != nil {
			t.Fatalf("%s: %v: %s", format, err, errout.String())
		}
		got := out.String()
		if strings.Contains(got, "PAYLOAD_SENTINEL") || strings.Contains(got, "bot-author") || strings.Contains(got, "hearsay") {
			t.Fatalf("%s leaked excluded data: %s", format, got)
		}
		if !strings.Contains(got, "u1") || !strings.Contains(got, "mentioned") || !strings.Contains(got, "alice") {
			t.Fatalf("%s missing hints: %s", format, got)
		}
		var repeat bytes.Buffer
		if err := run(t.Context(), []string{"identities", "list", "--config", configPath, "--database-url", url, format}, &repeat, &errout); err != nil {
			t.Fatal(err)
		}
		if repeat.String() != got {
			t.Fatalf("%s changed across runs", format)
		}
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM l0_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("L0 mutated: %d", count)
	}
}
