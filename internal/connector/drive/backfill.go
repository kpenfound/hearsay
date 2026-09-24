package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/connector"
)

const pageSize = "100"

type position struct {
	Folder    string `json:"folder"`
	PageToken string `json:"page_token,omitempty"`
}
type user struct {
	ID    string `json:"permissionId"`
	Email string `json:"emailAddress"`
	Name  string `json:"displayName"`
}
type file struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	MIME       string            `json:"mimeType"`
	Parents    []string          `json:"parents"`
	URL        string            `json:"webViewLink"`
	Created    time.Time         `json:"createdTime"`
	Modified   time.Time         `json:"modifiedTime"`
	Head       string            `json:"headRevisionId"`
	Owners     []user            `json:"owners"`
	Modifier   *user             `json:"lastModifyingUser"`
	Properties map[string]string `json:"properties"`
	Trashed    bool              `json:"trashed"`
}
type fileList struct {
	Files []file `json:"files"`
	Next  string `json:"nextPageToken"`
}
type permission struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Email   string `json:"emailAddress"`
	Domain  string `json:"domain"`
	Name    string `json:"displayName"`
	Deleted bool   `json:"deleted"`
}

// Backfill reads at most one file-list page of one configured folder. A failure
// returns no next cursor, so the runtime retries the page and L0 deduplicates it.
func (c *Connector) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	pos := position{Folder: c.folders[0]}
	if from != "" {
		if err := json.Unmarshal([]byte(from), &pos); err != nil {
			return connector.BackfillResult{}, fmt.Errorf("decoding Drive cursor: %w", err)
		}
		if !slices.Contains(c.folders, pos.Folder) {
			return connector.BackfillResult{}, fmt.Errorf("drive cursor folder %q is not configured", pos.Folder)
		}
	}
	q := url.Values{"q": {"'" + strings.ReplaceAll(pos.Folder, "'", "\\'") + "' in parents and trashed = false"}, "pageSize": {pageSize}, "fields": {"nextPageToken,files(id,name,mimeType,parents,webViewLink,createdTime,modifiedTime,headRevisionId,owners(permissionId,emailAddress,displayName),lastModifyingUser(permissionId,emailAddress,displayName),properties)"}, "supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"}}
	if pos.PageToken != "" {
		q.Set("pageToken", pos.PageToken)
	}
	var page fileList
	if err := c.api.getJSON(ctx, "/files", q, &page); err != nil {
		var status *statusError
		if pos.PageToken != "" && errors.As(err, &status) && status.status == http.StatusBadRequest {
			// An expired Drive token is not durable. Rewalk the folder; event IDs
			// make this safe and the replacement cursor contains no process state.
			raw, _ := json.Marshal(position{Folder: pos.Folder})
			return connector.BackfillResult{Next: connector.Cursor(raw)}, nil
		}
		return connector.BackfillResult{}, fmt.Errorf("listing Drive folder %s: %w", pos.Folder, err)
	}
	n := 0
	for _, f := range page.Files {
		// The server query narrows the walk; this direct-parent check is the
		// authorization boundary when a fixture or API response is surprising.
		if len(f.Parents) != 1 || f.Parents[0] != pos.Folder || f.ID == "" || f.MIME == "application/vnd.google-apps.folder" {
			continue
		}
		kind := connector.KindDocument
		if c.candidates[pos.Folder] {
			match, err := c.tagged(ctx, f.ID)
			if err != nil {
				return connector.BackfillResult{}, fmt.Errorf("reading labels on %s: %w", f.ID, err)
			}
			if !match {
				continue
			}
			kind = connector.KindTranscript
		}
		if f.MIME != "text/plain" && f.MIME != "text/markdown" && f.MIME != "application/vnd.google-apps.document" {
			continue
		}
		ev, ok, err := c.event(ctx, f, pos.Folder, kind)
		if err != nil {
			return connector.BackfillResult{}, fmt.Errorf("reading Drive file %s: %w", f.ID, err)
		}
		if !ok {
			continue
		}
		if err := sink.Emit(ctx, ev); err != nil {
			return connector.BackfillResult{}, fmt.Errorf("emitting Drive file %s: %w", f.ID, err)
		}
		c.mu.Lock()
		c.lastEventAt = time.Now()
		c.mu.Unlock()
		n++
	}
	if page.Next != "" {
		pos.PageToken = page.Next
	} else if i := slices.Index(c.folders, pos.Folder); i+1 < len(c.folders) {
		pos = position{Folder: c.folders[i+1]}
	} else {
		return connector.BackfillResult{Done: true, Events: n}, nil
	}
	raw, err := json.Marshal(pos)
	if err != nil {
		return connector.BackfillResult{}, fmt.Errorf("encoding Drive cursor: %w", err)
	}
	if len(raw) > connector.MaxCursorLen {
		return connector.BackfillResult{}, fmt.Errorf("drive cursor exceeds %d bytes", connector.MaxCursorLen)
	}
	return connector.BackfillResult{Next: connector.Cursor(raw), Events: n}, nil
}

// tagged accepts only a single, directly applied exact ID. A 403 on this
// file's labels is unreadable metadata and skips it; server failures retry.
func (c *Connector) tagged(ctx context.Context, id string) (bool, error) {
	var found bool
	var token string
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"pageSize": {"100"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var result struct {
			Labels []struct {
				ID string `json:"id"`
			} `json:"labels"`
			Next string `json:"nextPageToken"`
		}
		body, err := c.api.get(ctx, "/files/"+url.PathEscape(id)+"/listLabels", q)
		if err != nil {
			var status *statusError
			if errors.As(err, &status) && status.status == http.StatusForbidden {
				return false, nil
			}
			return false, err
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return false, nil
		}
		for _, label := range result.Labels {
			if label.ID == "" {
				return false, nil
			}
			if label.ID == c.label {
				if found {
					return false, nil
				}
				found = true
			}
		}
		if result.Next == "" {
			return found, nil
		}
		if result.Next == token {
			return false, nil
		}
		token = result.Next
	}
	return false, nil
}

func (c *Connector) event(ctx context.Context, f file, folder string, kind connector.Kind) (connector.Event, bool, error) {
	if f.Created.IsZero() || f.Name == "" {
		return connector.Event{}, false, nil
	}
	var author *connector.Identity
	for _, u := range f.Owners {
		if author = identity(c.source, u); author != nil {
			break
		}
	}
	if author == nil && f.Modifier != nil {
		author = identity(c.source, *f.Modifier)
	}
	if author == nil && kind == connector.KindDocument {
		return connector.Event{}, false, nil
	}
	if f.Head == "" && f.MIME == "application/vnd.google-apps.document" {
		head, err := c.revision(ctx, f.ID)
		if err != nil {
			return connector.Event{}, false, err
		}
		f.Head = head
	}
	if f.Head == "" {
		return connector.Event{}, false, nil
	}
	q := url.Values{}
	path := "/files/" + url.PathEscape(f.ID)
	if f.MIME == "application/vnd.google-apps.document" {
		path += "/export"
		q.Set("mimeType", "text/plain")
	} else {
		q.Set("alt", "media")
	}
	body, err := c.api.get(ctx, path, q)
	if err != nil {
		return connector.Event{}, false, err
	}
	if !utf8.Valid(body) {
		return connector.Event{}, false, nil
	}
	acl, err := c.permissions(ctx, f.ID)
	if err != nil {
		return connector.Event{}, false, err
	}
	if len(acl) == 0 {
		return connector.Event{}, false, nil
	}
	link := f.URL
	if link == "" {
		link = "https://drive.google.com/file/d/" + url.PathEscape(f.ID) + "/view"
	}
	participants := []connector.Participant{}
	for _, email := range strings.Split(f.Properties["calendar_attendee_emails"], ",") {
		email = strings.TrimSpace(email)
		if email != "" {
			participants = append(participants, connector.Participant{Identity: connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: email, Email: email}, Role: connector.RoleAttendee})
		}
	}
	return connector.Event{Source: c.source, NativeID: f.ID + "@" + f.Head, Kind: kind, Time: f.Created,
		Payload: connector.Payload{Artifact: f.ID, Container: connector.Container{Kind: connector.ContainerFolder, NativeID: folder}, URL: link, Title: f.Name, Text: string(body), Author: author, Participants: participants, Revision: &connector.Revision{Token: f.Head, EditedAt: f.Modified}}, ACL: acl}, true, nil
}

func identity(source string, u user) *connector.Identity {
	id := u.ID
	if id == "" {
		id = u.Email
	}
	if id == "" {
		return nil
	}
	return &connector.Identity{Source: source, Kind: connector.IdentityUser, NativeID: id, Email: u.Email, DisplayName: u.Name}
}

func (c *Connector) revision(ctx context.Context, id string) (string, error) {
	var token, last string
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"pageSize": {"1000"}, "fields": {"nextPageToken,revisions(id)"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var result struct {
			Revisions []struct {
				ID string `json:"id"`
			} `json:"revisions"`
			Next string `json:"nextPageToken"`
		}
		if err := c.api.getJSON(ctx, "/files/"+url.PathEscape(id)+"/revisions", q, &result); err != nil {
			return "", err
		}
		for _, r := range result.Revisions {
			if r.ID != "" {
				last = r.ID
			}
		}
		if result.Next == "" {
			return last, nil
		}
		if result.Next == token {
			return "", fmt.Errorf("revisions of %s did not advance", id)
		}
		token = result.Next
	}
	return "", fmt.Errorf("revisions of %s exceed bounded walk", id)
}

func (c *Connector) permissions(ctx context.Context, id string) (connector.ACL, error) {
	var token string
	acl := connector.ACL{}
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"pageSize": {"100"}, "fields": {"nextPageToken,permissions(id,type,emailAddress,domain,displayName,deleted)"}, "supportsAllDrives": {"true"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var result struct {
			Permissions []permission `json:"permissions"`
			Next        string       `json:"nextPageToken"`
		}
		if err := c.api.getJSON(ctx, "/files/"+url.PathEscape(id)+"/permissions", q, &result); err != nil {
			return nil, err
		}
		for _, p := range result.Permissions {
			if p.Deleted {
				continue
			}
			switch p.Type {
			case "anyone", "domain":
				acl = append(acl, connector.ACLEntry{Kind: connector.ACLPublic})
			case "group":
				if p.ID != "" {
					acl = append(acl, connector.ACLEntry{Kind: connector.ACLGroup, Source: c.source, NativeID: p.ID, Label: p.Email})
				}
			case "user":
				if p.ID != "" {
					acl = append(acl, connector.ACLEntry{Kind: connector.ACLIdentity, Source: c.source, NativeID: p.ID, Label: p.Email})
				}
			}
		}
		if result.Next == "" {
			return acl, nil
		}
		if result.Next == token {
			return nil, fmt.Errorf("permissions of %s did not advance", id)
		}
		token = result.Next
	}
	return nil, fmt.Errorf("permissions of %s exceed bounded walk", id)
}

// User is one Google account a folder is shared with directly.
type User struct {
	// PermissionID is the account's permission id, the native id the
	// connector writes on authors and access lists.
	PermissionID string
	Email        string
}

// FolderUsers lists the accounts one configured folder is shared with, from
// its sharing permissions: users only, not groups, domains or links. It is
// what `hearsay init` matches principals' email addresses against, and
// nothing else calls it.
func (c *Connector) FolderUsers(ctx context.Context, folder string) ([]User, error) {
	if !slices.Contains(c.folders, folder) {
		return nil, fmt.Errorf("folder %s is not one the source names", folder)
	}
	acl, err := c.permissions(ctx, folder)
	if err != nil {
		return nil, fmt.Errorf("reading the sharing of folder %s: %w", folder, err)
	}
	var out []User
	for _, e := range acl {
		if e.Kind == connector.ACLIdentity && e.Label != "" {
			out = append(out, User{PermissionID: e.NativeID, Email: e.Label})
		}
	}
	return out, nil
}
