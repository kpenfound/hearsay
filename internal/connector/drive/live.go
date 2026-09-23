package drive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

const fileFields = "id,name,mimeType,parents,webViewLink,createdTime,modifiedTime,headRevisionId,owners(permissionId,emailAddress,displayName),lastModifyingUser(permissionId,emailAddress,displayName),properties,trashed"

type driveChange struct {
	FileID  string `json:"fileId"`
	Type    string `json:"changeType"`
	Removed bool   `json:"removed"`
}

// PollFrom consumes one Drive change page. The runtime persists the returned
// token only after all writes on that page succeed, so failures replay safely.
func (c *Connector) PollFrom(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.Cursor, error) {
	if from == "" {
		var start struct {
			Token string `json:"startPageToken"`
		}
		if err := c.api.getJSON(ctx, "/changes/startPageToken", url.Values{"supportsAllDrives": {"true"}}, &start); err != nil {
			return "", fmt.Errorf("starting Drive changes: %w", err)
		}
		if start.Token == "" {
			return "", errors.New("drive returned no start page token")
		}
		if err := c.reconcile(ctx, sink, start.Token); err != nil {
			return "", err
		}
		return connector.Cursor(start.Token), nil
	}
	q := url.Values{"pageToken": {string(from)}, "pageSize": {pageSize}, "fields": {"nextPageToken,newStartPageToken,changes(fileId,changeType,removed)"}, "includeItemsFromAllDrives": {"true"}, "supportsAllDrives": {"true"}, "includeRemoved": {"true"}}
	var page struct {
		Changes []driveChange `json:"changes"`
		Next    string        `json:"nextPageToken"`
		Start   string        `json:"newStartPageToken"`
	}
	if err := c.api.getJSON(ctx, "/changes", q, &page); err != nil {
		var status *statusError
		if errors.As(err, &status) && status.status == http.StatusGone {
			var start struct {
				Token string `json:"startPageToken"`
			}
			if err := c.api.getJSON(ctx, "/changes/startPageToken", url.Values{"supportsAllDrives": {"true"}}, &start); err != nil {
				return from, err
			}
			if start.Token == "" {
				return from, errors.New("drive returned no recovery token")
			}
			if err := c.reconcile(ctx, sink, start.Token); err != nil {
				return from, err
			}
			return connector.Cursor(start.Token), nil
		}
		return from, fmt.Errorf("listing Drive changes: %w", err)
	}
	for _, change := range page.Changes {
		if change.Type != "file" || change.FileID == "" {
			continue
		}
		if err := c.syncFile(ctx, sink, change.FileID, string(from)); err != nil {
			return from, fmt.Errorf("syncing Drive file %s: %w", change.FileID, err)
		}
	}
	next := page.Next
	if next == "" {
		next = page.Start
	}
	if next == "" {
		return from, errors.New("drive change page has no successor token")
	}
	if len(next) > connector.MaxCursorLen {
		return from, errors.New("drive change token is too long")
	}
	// A folder or account that cannot register push channels still has the
	// durable polling feed. A failed watch must never hold its cursor back.
	if err := c.ensureWatch(ctx, next); err != nil {
		telemetry.Logger(ctx).WarnContext(ctx, "Drive notification watch unavailable; continuing to poll", "error", err)
	}
	return connector.Cursor(next), nil
}

func (c *Connector) syncFile(ctx context.Context, sink connector.Sink, id, observation string) error {
	reader, ok := sink.(connector.ArtifactReader)
	if !ok {
		return errors.New("drive sync needs an artifact reader")
	}
	previous, had, err := reader.CurrentArtifact(ctx, c.source, id)
	if err != nil {
		return err
	}
	// Change pages describe a past transition. Always fetch the current file:
	// a replayed removal may now refer to an eligible file again.
	var f file
	removed := false
	err = c.api.getJSON(ctx, "/files/"+url.PathEscape(id), url.Values{"fields": {fileFields}, "supportsAllDrives": {"true"}}, &f)
	if err != nil {
		var status *statusError
		if !errors.As(err, &status) || (status.status != http.StatusNotFound && status.status != http.StatusGone) {
			return err
		}
		removed = true
	}
	if !removed && !f.Trashed && f.ID == id && len(f.Parents) == 1 && slices.Contains(c.folders, f.Parents[0]) && f.MIME != "application/vnd.google-apps.folder" {
		folder := f.Parents[0]
		kind := connector.KindDocument
		if c.candidates[folder] {
			tagged, err := c.tagged(ctx, id)
			if err != nil {
				return err
			}
			if !tagged {
				return c.tombstone(ctx, sink, previous, had)
			}
			kind = connector.KindTranscript
		}
		if f.MIME != "text/plain" && f.MIME != "text/markdown" && f.MIME != "application/vnd.google-apps.document" {
			return c.tombstone(ctx, sink, previous, had)
		}
		ev, emit, err := c.event(ctx, f, folder, kind)
		if err != nil {
			return err
		}
		if !emit {
			return c.tombstone(ctx, sink, previous, had)
		}
		if had && sameDriveState(previous, ev) {
			return nil
		}
		var retractionID string
		if !had {
			r, ok := sink.(connector.RetractionReader)
			if !ok {
				return errors.New("drive sync needs a retraction reader")
			}
			tomb, found, err := r.LastRetraction(ctx, c.source, id)
			if err != nil {
				return err
			}
			if found {
				retractionID = tomb.ID
			}
		}
		// Drive has no permission version. Hash the normalized ACL with the
		// metadata whose change must also produce a revision (notably folder).
		acl := slices.Clone(ev.ACL)
		slices.SortFunc(acl, func(a, b connector.ACLEntry) int {
			aj, _ := json.Marshal(a)
			bj, _ := json.Marshal(b)
			return strings.Compare(string(aj), string(bj))
		})
		versionInput, _ := json.Marshal(struct {
			ACL               connector.ACL
			Folder, Name, URL string
			Properties        map[string]string
			Observation       string
			Kind              connector.Kind
			Retraction        string
		}{acl, folder, f.Name, f.URL, f.Properties, observation, kind, retractionID})
		hash := sha256.Sum256(versionInput)
		token := ev.Payload.Revision.Token + "+perm:" + hex.EncodeToString(hash[:12])
		ev.NativeID = id + "@" + token
		ev.Payload.Revision.Token = token
		// The source gives no timestamp for permission changes. A stable
		// successor to modifiedTime lets L0 order this after backfill, while
		// arrival order resolves multiple sharing changes on the same head.
		modified := f.Modified
		if modified.IsZero() || modified.Before(f.Created) {
			modified = f.Created
		}
		ev.Payload.Revision.EditedAt = modified.Add(time.Nanosecond)
		if err := sink.Emit(ctx, ev); err != nil {
			return err
		}
		c.mu.Lock()
		c.lastEventAt = time.Now()
		c.mu.Unlock()
		return nil
	}
	return c.tombstone(ctx, sink, previous, had)
}

func sameDriveState(a, b connector.Event) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Payload.Revision == nil || b.Payload.Revision == nil {
		return false
	}
	if !strings.HasPrefix(a.Payload.Revision.Token, b.Payload.Revision.Token+"+perm:") && a.Payload.Revision.Token != b.Payload.Revision.Token {
		return false
	}
	leftPayload, rightPayload := a.Payload, b.Payload
	leftPayload.Revision, rightPayload.Revision = nil, nil
	left, _ := json.Marshal(leftPayload)
	right, _ := json.Marshal(rightPayload)
	if string(left) != string(right) {
		return false
	}
	return sameACL(a.ACL, b.ACL)
}

func sameACL(a, b connector.ACL) bool {
	if len(a) != len(b) {
		return false
	}
	left, right := slices.Clone(a), slices.Clone(b)
	order := func(a, b connector.ACLEntry) int {
		aj, _ := json.Marshal(a)
		bj, _ := json.Marshal(b)
		return strings.Compare(string(aj), string(bj))
	}
	slices.SortFunc(left, order)
	slices.SortFunc(right, order)
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return string(l) == string(r)
}

// reconcile runs after acquiring a fresh change token. A token captured before
// this scan lets the ordinary feed replay any mutation concurrent with it.
func (c *Connector) reconcile(ctx context.Context, sink connector.Sink, token string) error {
	r, ok := sink.(connector.ArtifactReader)
	if !ok {
		return errors.New("drive recovery needs an artifact reader")
	}
	prior, err := r.CurrentArtifacts(ctx, c.source)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, folder := range c.folders {
		pageToken := ""
		for {
			q := url.Values{"q": {"'" + folder + "' in parents and trashed = false"}, "pageSize": {pageSize}, "fields": {"nextPageToken,files(" + fileFields + ")"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
			if pageToken != "" {
				q.Set("pageToken", pageToken)
			}
			var page fileList
			if err := c.api.getJSON(ctx, "/files", q, &page); err != nil {
				return fmt.Errorf("recovering folder %s: %w", folder, err)
			}
			for _, f := range page.Files {
				if f.ID == "" || len(f.Parents) != 1 || f.Parents[0] != folder {
					continue
				}
				seen[f.ID] = true
				if err := c.syncFile(ctx, sink, f.ID, token); err != nil {
					return err
				}
			}
			if page.Next == "" {
				break
			}
			if page.Next == pageToken {
				return fmt.Errorf("folder %s recovery page did not advance", folder)
			}
			pageToken = page.Next
		}
	}
	for _, ev := range prior {
		if !seen[ev.Payload.Artifact] && slices.Contains(c.folders, ev.Payload.Container.NativeID) {
			if err := c.syncFile(ctx, sink, ev.Payload.Artifact, token); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Connector) tombstone(ctx context.Context, sink connector.Sink, previous connector.Event, had bool) error {
	if !had {
		return nil
	}
	id := previous.Payload.Artifact
	// The current revision's identity makes each eligible → ineligible cycle
	// distinct while a replay of that cycle retains the same tombstone ID.
	hash := sha256.Sum256([]byte(previous.NativeID))
	name := id + ":tombstone:" + hex.EncodeToString(hash[:12])
	ev := connector.Event{Source: c.source, NativeID: name, Kind: connector.KindTombstone, Time: previous.Time,
		Payload: connector.Payload{Artifact: name, Target: id, Container: previous.Payload.Container}, ACL: previous.ACL}
	if err := sink.Emit(ctx, ev); err != nil {
		return err
	}
	c.mu.Lock()
	c.lastEventAt = time.Now()
	c.mu.Unlock()
	return nil
}

// Handler accepts only messages from the active channel token. The webhook
// contains no file state; it merely wakes the durable change-feed poller.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		c.mu.Lock()
		valid := c.watchID != "" && r.Header.Get("X-Goog-Channel-ID") == c.watchID && r.Header.Get("X-Goog-Channel-Token") == c.watchToken
		c.mu.Unlock()
		if !valid {
			http.Error(w, "channel", http.StatusUnauthorized)
			return
		}
		state := r.Header.Get("X-Goog-Resource-State")
		if state != "change" && state != "sync" {
			http.Error(w, "state", http.StatusBadRequest)
			return
		}
		if wake, ok := sink.(connector.PollRequester); ok {
			wake.RequestPoll()
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (c *Connector) ensureWatch(ctx context.Context, pageToken string) error {
	if c.notificationURL == "" {
		return nil
	}
	c.mu.Lock()
	if time.Until(c.watchExpires) > time.Hour {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(random[:8])
	token := hex.EncodeToString(random[8:])
	var channel struct {
		Expiration string `json:"expiration"`
	}
	q := url.Values{"pageToken": {pageToken}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	request := map[string]string{"id": id, "type": "web_hook", "address": c.notificationURL, "token": token,
		"expiration": strconv.FormatInt(time.Now().Add(24*time.Hour).UnixMilli(), 10)}
	if err := c.api.postJSON(ctx, "/changes/watch", q, request, &channel); err != nil {
		return fmt.Errorf("watching Drive changes: %w", err)
	}
	expires := time.Now().Add(24 * time.Hour)
	if millis, err := strconv.ParseInt(channel.Expiration, 10, 64); err == nil {
		expires = time.UnixMilli(millis)
	}
	c.mu.Lock()
	c.watchID, c.watchToken, c.watchExpires = id, token, expires
	c.mu.Unlock()
	return nil
}
