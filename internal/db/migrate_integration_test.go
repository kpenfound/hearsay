//go:build integration

package db_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/db"
)

// scratchDatabase makes an empty database of its own and returns its URL, so
// that a test can migrate from nothing — and roll back — without touching the
// database every other test is reading.
func scratchDatabase(t *testing.T) string {
	t.Helper()
	adminURL := os.Getenv("HEARSAY_DATABASE_URL")
	if adminURL == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}

	name := "hearsay_scratch_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.FormatInt(scratches.Add(1), 36)
	// A context of its own: the cleanup runs after the test's is cancelled.
	ctx := context.Background()
	admin, err := db.Open(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer admin.Close()

	// CREATE DATABASE cannot run inside a transaction, and the name is one this
	// function built out of digits, so there is nothing here to interpolate.
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("creating the scratch database: %v", err)
	}
	t.Cleanup(func() {
		dropper, err := db.Open(ctx, adminURL)
		if err != nil {
			t.Errorf("connecting to drop the scratch database: %v", err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("dropping the scratch database: %v", err)
		}
	})
	return swapDatabaseName(adminURL, name)
}

var scratches atomic.Int64

// swapDatabaseName points a connection URL at another database on the same
// server.
func swapDatabaseName(url, name string) string {
	base, query, hasQuery := strings.Cut(url, "?")
	slash := strings.LastIndex(base, "/")
	swapped := base[:slash+1] + name
	if hasQuery {
		return swapped + "?" + query
	}
	return swapped
}

// The whole of ADR-0006's command surface, against a database that starts with
// no schema at all: up, status, up-to, down, and the migration whose down
// refuses.
func TestMigrateUpAndDown(t *testing.T) {
	url := scratchDatabase(t)
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("NewMigrator() = %v, want no error", err)
	}
	defer func() { _ = migrator.Close() }()

	newest, err := db.EmbeddedVersion()
	if err != nil {
		t.Fatalf("EmbeddedVersion() = %v, want no error", err)
	}

	// A database nothing has migrated is behind every binary.
	if _, err := db.Connect(t.Context(), url); !errors.Is(err, db.ErrSchemaBehind) {
		t.Fatalf("Connect(unmigrated) = %v, want db.ErrSchemaBehind", err)
	}
	if version, err := migrator.Version(t.Context()); err != nil || version != 0 {
		t.Fatalf("Version(unmigrated) = %d, %v, want 0 and no error", version, err)
	}

	applied, err := migrator.Up(t.Context())
	if err != nil {
		t.Fatalf("Up() = %v, want no error", err)
	}
	if len(applied) != int(newest) {
		t.Errorf("Up() applied %d migrations, want %d", len(applied), newest)
	}
	if version, err := migrator.Version(t.Context()); err != nil || version != newest {
		t.Fatalf("Version(migrated) = %d, %v, want %d and no error", version, err, newest)
	}

	// A second up is a no-op, which is what makes re-running a deploy job safe.
	again, err := migrator.Up(t.Context())
	if err != nil {
		t.Fatalf("Up(again) = %v, want no error", err)
	}
	if len(again) != 0 {
		t.Errorf("Up(again) applied %d migrations, want none", len(again))
	}

	// Now the schema check passes, and the events table is there.
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("Connect(migrated) = %v, want no error", err)
	}
	defer pool.Close()
	var events int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM l0_events`).Scan(&events); err != nil {
		t.Fatalf("counting l0_events: %v", err)
	}

	status, err := migrator.Status(t.Context())
	if err != nil {
		t.Fatalf("Status() = %v, want no error", err)
	}
	for _, s := range status {
		if !s.Applied {
			t.Errorf("Status() says %s is pending after up", s.Name)
		}
	}

	// Down reverses one migration at a time, and what each one created goes
	// with it. The migrations are named here newest first, so a migration added
	// later is a line added at the top of this list; a migration that creates
	// no table of its own — an index on an existing one — has no table here and
	// is only checked for rolling back cleanly.
	for i, table := range []string{"l2_gestures", "", "l2_topic_operations", "l0_deletion_rebuilds", "l0_deletions", "", "", "bundle_changes", "l2_alias_candidates", "", "l2_pins", "", "", "", "", "l0_resyncs", "l2_stances", "l0_backfill_cursors", "", "", "", "", "l0_feed_cursors", "l1_docs", "queue_job", "l0_events"} {
		want := newest - int64(i) - 1
		if _, err := migrator.Down(t.Context()); err != nil {
			t.Fatalf("Down() = %v, want no error", err)
		}
		if version, err := migrator.Version(t.Context()); err != nil || version != want {
			t.Fatalf("Version(after down) = %d, %v, want %d", version, err, want)
		}
		if i == 3 {
			if _, err := pool.Exec(t.Context(), `SELECT redacted_by FROM l2_stances LIMIT 0`); err == nil {
				t.Error("stance redaction remains after rolling its migration back")
			}
		}
		if i == 9 {
			if _, err := pool.Exec(t.Context(), `SELECT assertion FROM l2_stances LIMIT 0`); err == nil {
				t.Error("stance assertion remains after rolling its migration back")
			}
		}
		if i == 12 {
			if _, err := pool.Exec(t.Context(), `SELECT judgement FROM l2_stances LIMIT 0`); err == nil {
				t.Error("stance judgement remains after rolling its migration back")
			}
		}
		if i == 11 {
			if _, err := pool.Exec(t.Context(), `SELECT judgement FROM l2_stances LIMIT 0`); err != nil {
				t.Errorf("stance judgement missing after rolling reconciliation back: %v", err)
			}
			if _, err := pool.Exec(t.Context(), `SELECT artifact_class FROM l1_docs LIMIT 0`); err != nil {
				t.Errorf("artifact class missing after rolling reconciliation back: %v", err)
			}
		}
		if i == 13 {
			if _, err := pool.Exec(t.Context(), `SELECT artifact_class FROM l1_docs LIMIT 0`); err == nil {
				t.Error("artifact class remains after rolling its migration back")
			}
		}
		if table == "" {
			continue
		}
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&events); err == nil {
			t.Errorf("%s is still there after rolling its migration back", table)
		}
	}

	// And up again, so that a down migration that leaves the schema in a state
	// up cannot re-apply fails here rather than in someone's afternoon.
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("Up(after down) = %v, want no error", err)
	}
	if version, err := migrator.Version(t.Context()); err != nil || version != newest {
		t.Fatalf("Version(after up again) = %d, %v, want %d", version, err, newest)
	}
}

// ADR-0006 turns goose's session-level advisory lock on, which is not its
// default, so that `migrate up` invocations racing during a flaky deploy are
// safe: one applies and the others wait.
//
// This pins the property rather than the lock. Removing the lock does not make
// it fail — goose runs each migration in a transaction, and four racing runs
// serialize on that by themselves — so it is evidence that concurrent
// migration works, not that the lock is what makes it work.
func TestTwoMigrationsAtOnceAreSafe(t *testing.T) {
	url := scratchDatabase(t)
	newest, err := db.EmbeddedVersion()
	if err != nil {
		t.Fatalf("EmbeddedVersion() = %v, want no error", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	applied := make([]int, len(errs))
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			migrator, err := db.NewMigrator(t.Context(), url, nil)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = migrator.Close() }()
			results, err := migrator.Up(t.Context())
			errs[i], applied[i] = err, len(results)
		}()
	}
	wg.Wait()

	total := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("Up() in goroutine %d = %v, want no error", i, err)
		}
		total += applied[i]
	}
	// Between them they apply each migration once, not once each.
	if total != int(newest) {
		t.Errorf("%d concurrent runs applied %d migrations between them, want %d", len(errs), total, newest)
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("Connect() after concurrent migrations = %v, want no error", err)
	}
	pool.Close()
}

// ADR-0006: a down migration that would destroy data says so and fails rather
// than pretending to reverse. Migration 1 is the one that does.
func TestTheVectorExtensionRefusesToBeRolledBack(t *testing.T) {
	url := scratchDatabase(t)
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("NewMigrator() = %v, want no error", err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("Up() = %v, want no error", err)
	}

	newest, err := db.EmbeddedVersion()
	if err != nil {
		t.Fatalf("EmbeddedVersion() = %v, want no error", err)
	}
	// Everything above migration 1 rolls back cleanly.
	for version := newest; version > 1; version-- {
		if _, err := migrator.Down(t.Context()); err != nil {
			t.Fatalf("Down(from %d) = %v, want no error", version, err)
		}
	}
	err = downErr(migrator.Down(t.Context()))
	if err == nil {
		t.Fatal("Down(to nothing) = nil, want the vector extension to refuse")
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("Down(to nothing) = %v, want an error naming the vector extension", err)
	}
}

// up-to stops where it was told, which is what a deployment rolling forward in
// steps needs.
func TestMigrateUpTo(t *testing.T) {
	url := scratchDatabase(t)
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("NewMigrator() = %v, want no error", err)
	}
	defer func() { _ = migrator.Close() }()

	if _, err := migrator.UpTo(t.Context(), 1); err != nil {
		t.Fatalf("UpTo(1) = %v, want no error", err)
	}
	if version, err := migrator.Version(t.Context()); err != nil || version != 1 {
		t.Fatalf("Version(after up-to 1) = %d, %v, want 1", version, err)
	}
	// Stopping short of the newest migration is a database this binary refuses.
	if _, err := db.Connect(t.Context(), url); !errors.Is(err, db.ErrSchemaBehind) {
		t.Errorf("Connect(behind) = %v, want db.ErrSchemaBehind", err)
	}
	status, err := migrator.Status(t.Context())
	if err != nil {
		t.Fatalf("Status() = %v, want no error", err)
	}
	if len(status) < 2 || status[1].Applied {
		t.Errorf("Status() = %v, want everything after the first migration pending", format(status))
	}
}

// A database ahead of the binary is allowed: a rollout is migrated forward
// before every replica has been replaced (ADR-0006).
func TestADatabaseAheadOfTheBinaryIsAllowed(t *testing.T) {
	url := scratchDatabase(t)
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("NewMigrator() = %v, want no error", err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("Up() = %v, want no error", err)
	}

	pool, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer pool.Close()
	newest, err := db.EmbeddedVersion()
	if err != nil {
		t.Fatalf("EmbeddedVersion() = %v, want no error", err)
	}
	// A migration from a newer binary, as a rolling deploy would leave it.
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, newest+1); err != nil {
		t.Fatalf("recording a newer migration: %v", err)
	}
	if err := db.CheckSchema(t.Context(), pool); err != nil {
		t.Errorf("CheckSchema(ahead) = %v, want no error", err)
	}
}

func downErr(_ []db.Applied, err error) error { return err }

func format(statuses []db.Status) string {
	var b strings.Builder
	for _, s := range statuses {
		fmt.Fprintf(&b, "%d:%v ", s.Version, s.Applied)
	}
	return b.String()
}

// Existing documents must acquire authority from their retained root events,
// even when the model's outcome would suggest a different PR state.
func TestArtifactClassBackfill(t *testing.T) {
	url := scratchDatabase(t)
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := migrator.UpTo(t.Context(), 13); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cases := []struct{ kind, native, container, want string }{
		{"pr", `{"merged_at":"2026-09-09T12:00:00Z","state":"closed"}`, "repository", "merged_pr"},
		{"pr", `{"state":"closed"}`, "repository", "pull_request"},
		{"chat_thread", `{}`, "dm", "dm"},
		{"chat_burst", `{}`, "channel", "chat_thread"},
		{"meeting_segment", `{}`, "folder", "meeting"},
		{"wiki_section", `{}`, "folder", "spec"},
	}
	for i, tt := range cases {
		artifact := fmt.Sprintf("artifact-%d", i)
		eventID := "evt:backfill:" + artifact
		payload := fmt.Sprintf(`{"artifact":%q,"container":{"kind":%q,"native_id":"x"},"native":%s}`, artifact, tt.container, tt.native)
		if _, err := pool.Exec(t.Context(), `INSERT INTO l0_events (id, source, native_id, kind, artifact, occurred_at, payload, acl) VALUES ($1, 'backfill', $2, 'issue', $2, now(), $3::jsonb, '[{"kind":"public"}]'::jsonb)`, eventID, artifact, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO l1_docs (id, kind, source, source_native_id, l0_refs, created_at, updated_at, last_activity_at, acl, text, raw_text, body, outcome_kind) VALUES ($1, $2, 'backfill', $3, ARRAY[$4], now(), now(), now(), '[{"kind":"public"}]'::jsonb, 'text', 'raw', '{"outcome_kind":"resolved"}'::jsonb, 'resolved')`, "l1:backfill:"+artifact, tt.kind, artifact, eventID); err != nil {
			t.Fatal(err)
		}
	}
	// A missing provenance row makes the migration fail atomically. Supplying
	// that retained event and retrying must backfill every row once.
	if _, err := pool.Exec(t.Context(), `INSERT INTO l1_docs (id, kind, source, source_native_id, l0_refs, created_at, updated_at, last_activity_at, acl, text, raw_text, body, outcome_kind) VALUES ('l1:backfill:late-root', 'issue', 'backfill', 'late-root', ARRAY['evt:backfill:late-root'], now(), now(), now(), '[{"kind":"public"}]'::jsonb, 'text', 'raw', '{"outcome_kind":"none"}'::jsonb, 'none')`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.UpTo(t.Context(), 14); err == nil {
		t.Fatal("backfill accepted missing provenance")
	}
	if version, err := migrator.Version(t.Context()); err != nil || version != 13 {
		t.Fatalf("version after failed backfill = %d, %v", version, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO l0_events (id, source, native_id, kind, artifact, occurred_at, payload, acl) VALUES ('evt:backfill:late-root', 'backfill', 'late-root', 'issue', 'late-root', now(), '{"artifact":"late-root","container":{"kind":"repository","native_id":"x"}}'::jsonb, '[{"kind":"public"}]'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.UpTo(t.Context(), 14); err != nil {
		t.Fatal(err)
	}
	if applied, err := migrator.Up(t.Context()); err != nil || len(applied) < 2 || applied[0].Version != 15 || applied[1].Version != 16 {
		t.Errorf("Up(after backfill) = %v, %v; want migrations 15 and 16, then any later ones", applied, err)
	}
	if applied, err := migrator.Up(t.Context()); err != nil || len(applied) != 0 {
		t.Errorf("Up(again) = %v, %v; want no migrations", applied, err)
	}
	for i, tt := range cases {
		var got string
		if err := pool.QueryRow(t.Context(), `SELECT artifact_class FROM l1_docs WHERE id = $1`, fmt.Sprintf("l1:backfill:artifact-%d", i)).Scan(&got); err != nil || got != tt.want {
			t.Errorf("%d: class = %q, %v; want %q", i, got, err, tt.want)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE l1_docs SET artifact_class = NULL WHERE id = 'l1:backfill:artifact-0'`); err == nil {
		t.Error("NULL class accepted")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE l1_docs SET artifact_class = 'unknown' WHERE id = 'l1:backfill:artifact-0'`); err == nil {
		t.Error("unknown class accepted")
	}
}

func TestVersion14BranchSchemasUpgrade(t *testing.T) {
	for _, branch := range []string{"artifact-class", "stance-judgement"} {
		t.Run(branch, func(t *testing.T) {
			url := scratchDatabase(t)
			migrator, err := db.NewMigrator(t.Context(), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = migrator.Close() }()
			if _, err := migrator.UpTo(t.Context(), 13); err != nil {
				t.Fatal(err)
			}
			pool, err := db.Open(t.Context(), url)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			_, err = pool.Exec(t.Context(), `INSERT INTO l0_events (id, source, native_id, kind, artifact, occurred_at, payload, acl) VALUES ('evt:legacy', 'legacy', 'pr-1', 'document', 'pr-1', now(), '{"native":{"state":"merged"}}'::jsonb, '[{"kind":"public"}]'::jsonb)`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = pool.Exec(t.Context(), `INSERT INTO l1_docs (id, kind, source, source_native_id, l0_refs, created_at, updated_at, last_activity_at, acl, text, raw_text, body, outcome_kind) VALUES ('l1:legacy:pr-1', 'pr', 'legacy', 'pr-1', ARRAY['evt:legacy'], now(), now(), now(), '[{"kind":"public"}]'::jsonb, 'retained', 'raw', '{}'::jsonb, 'none')`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = pool.Exec(t.Context(), `INSERT INTO l2_topics (id, scope, name, acl, opened_by) VALUES ('topic:legacy', 'legacy', 'topic', '[{"kind":"public"}]'::jsonb, 'l1:legacy:pr-1')`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = pool.Exec(t.Context(), `INSERT INTO l2_stances (id, topic_id, position, stated_at, evidence, tier, acl) VALUES ('stance:legacy', 'topic:legacy', 'keep', now(), ARRAY['l1:legacy:pr-1'], 'ratified', '[{"kind":"public"}]'::jsonb)`)
			if err != nil {
				t.Fatal(err)
			}

			if branch == "artifact-class" {
				if _, err := migrator.UpTo(t.Context(), 14); err != nil {
					t.Fatal(err)
				}
			} else {
				// Replay the stance branch's version-14 schema, then record the
				// version Goose recorded on that branch.
				body, err := fs.ReadFile(db.Migrations(), "00015_add_l2_stance_judgement.sql")
				if err != nil {
					t.Fatal(err)
				}
				up, _, _ := strings.Cut(string(body), "-- +goose Down")
				if _, err := pool.Exec(t.Context(), up); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(t.Context(), `INSERT INTO goose_db_version (version_id, is_applied) VALUES (14, true)`); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(t.Context(), `UPDATE l2_stances SET judgement = 'changes' WHERE id = 'stance:legacy'`); err != nil {
					t.Fatal(err)
				}
				if _, err := migrator.UpTo(t.Context(), 15); err == nil {
					t.Fatal("UpTo(15) accepted the legacy stance schema without artifact class")
				}
			}
			if _, err := migrator.Up(t.Context()); err != nil {
				t.Fatalf("Up() from %s version 14: %v", branch, err)
			}
			newest, err := db.EmbeddedVersion()
			if err != nil {
				t.Fatal(err)
			}
			if version, err := migrator.Version(t.Context()); err != nil || version != newest {
				t.Fatalf("Version() = %d, %v; want %d", version, err, newest)
			}
			var class, docText, position, judgement string
			if err := pool.QueryRow(t.Context(), `SELECT artifact_class, text FROM l1_docs WHERE id = 'l1:legacy:pr-1'`).Scan(&class, &docText); err != nil || class != "merged_pr" || docText != "retained" {
				t.Errorf("document = %q, %q, %v; want merged_pr and retained", class, docText, err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT position FROM l2_stances WHERE id = 'stance:legacy'`).Scan(&position); err != nil || position != "keep" {
				t.Errorf("stance = %q, %v; want keep", position, err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT coalesce(judgement, '') FROM l2_stances WHERE id = 'stance:legacy'`).Scan(&judgement); err != nil || (branch == "stance-judgement" && judgement != "changes") || (branch == "artifact-class" && judgement != "") {
				t.Errorf("judgement = %q, %v; existing value was not preserved", judgement, err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE l2_stances SET judgement = 'restates' WHERE id = 'stance:legacy'`); err != nil {
				t.Errorf("valid judgement rejected: %v", err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE l2_stances SET judgement = 'unknown' WHERE id = 'stance:legacy'`); err == nil {
				t.Error("unknown judgement accepted")
			}
			if _, err := pool.Exec(t.Context(), `UPDATE l1_docs SET artifact_class = NULL WHERE id = 'l1:legacy:pr-1'`); err == nil {
				t.Error("NULL artifact class accepted")
			}
			if _, err := pool.Exec(t.Context(), `UPDATE l1_docs SET artifact_class = 'unknown' WHERE id = 'l1:legacy:pr-1'`); err == nil {
				t.Error("unknown artifact class accepted")
			}
		})
	}
}
