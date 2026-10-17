//go:build integration

package bundle_test

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestAssemblerHidesWorkEntitiesBeforeBudgeting(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	source := "bundle" + strconv.FormatInt(time.Now().UnixNano(), 36)
	hidden := "work:" + source + ":private"
	subject := "tracker:" + source + ":public"
	code := "code:" + source
	graph, docs := l2.New(pool), l1.New(pool)
	for _, e := range []l2.Entity{
		{ID: hidden, Type: l2.TypeProject, Name: strings.Repeat("Private project name ", 100), Origin: l2.OriginSource},
		{ID: subject, Type: l2.TypeTrackerItem, Origin: l2.OriginReference},
		{ID: code, Type: l2.TypeModule, Name: "Visible module", Origin: l2.OriginConfig},
	} {
		if err := graph.PutEntity(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().UTC()
	doc := l1.Document{ID: l1.DocID(source, "public"), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue, Source: l1.Source{System: source, NativeID: "public"}, L0Refs: []string{"evt:" + source + ":public"}, Time: l1.Times{Created: at, Updated: at, LastActivity: at}, Scope: []string{subject, code}, ACL: connector.ACL{{Kind: connector.ACLPublic}}, Text: "Public evidence", RawText: "Public evidence", Body: l1.Body{Summary: "Public evidence", OutcomeKind: l1.OutcomeNone}}
	reader := l1.Reader{Effective: principal.Effective{Human: "reader", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
	for _, budget := range []int{1, 100, bundle.DefaultBudget} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			assemble := bundle.New(pool).WithBudget(budget)
			doc.Scope = []string{subject, code}
			if _, err := docs.Put(t.Context(), doc); err != nil {
				t.Fatal(err)
			}
			baseline, report, err := assemble.Assemble(t.Context(), reader, subject)
			if err != nil {
				t.Fatal(err)
			}
			doc.Scope = append(doc.Scope, hidden)
			if _, err := docs.Put(t.Context(), doc); err != nil {
				t.Fatal(err)
			}
			actual, actualReport, err := assemble.Assemble(t.Context(), reader, subject)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := bundle.Encode(baseline)
			after, _ := bundle.Encode(actual)
			if !bytes.Equal(before, after) || report != actualReport {
				t.Fatalf("budget %d: hidden related entity changed bundle/report: %s %+v; %s %+v", budget, before, report, after, actualReport)
			}
			missing, _, err := assemble.Assemble(t.Context(), reader, "work:"+source+":absent")
			if err != nil {
				t.Fatal(err)
			}
			private, _, err := assemble.Assemble(t.Context(), reader, hidden)
			if err != nil {
				t.Fatal(err)
			}
			missing.Scope.ID = hidden
			missingBytes, _ := bundle.Encode(missing)
			hiddenBytes, _ := bundle.Encode(private)
			if !bytes.Equal(missingBytes, hiddenBytes) {
				t.Fatalf("hidden scope = %s; missing = %s", hiddenBytes, missingBytes)
			}
		})
	}
}
