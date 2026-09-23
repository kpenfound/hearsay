// Package obsidian backfills a locally mounted Obsidian vault. The connectors
// process must run with the vault mounted at settings.root; a path inside a
// different container or host is not accessible. Live polling is separate work.
package obsidian

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Type is the registry key for the Obsidian connector.
const Type = "obsidian"
const pageSize = 100

// Settings describes the mounted vault and its source-native owner identity.
// Public must be explicitly true; an omitted value is private to Owner.
type Settings struct {
	Root              string             `json:"root"`
	Owner             connector.Identity `json:"owner"`
	Public            bool               `json:"public"`
	PermissionVersion string             `json:"permission_version"`
	Templates         []string           `json:"templates"`
}

// Connector walks an allowlisted mounted vault into L0.
type Connector struct {
	source     string
	root       *os.Root
	folders    []string
	templates  []string
	owner      connector.Identity
	acl        connector.ACL
	permission string
}

var _ connector.Poller = (*Connector)(nil)
var _ connector.Backfiller = (*Connector)(nil)

// Factory builds a connector for the binary registry.
func Factory(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
	return New(src)
}

// New validates a source and opens its mounted vault.
func New(src connector.SourceConfig) (*Connector, error) {
	var s Settings
	if err := src.DecodeSettings(&s); err != nil {
		return nil, err
	}
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return nil, errors.New("settings.root must be an absolute path to a mounted vault")
	}
	if s.Owner.Source == "" || s.Owner.NativeID == "" || s.Owner.Kind != connector.IdentityUser || !connector.ValidSourceID(s.Owner.Source) {
		return nil, errors.New("settings.owner must name a user with source and native_id")
	}
	if len(src.Secrets) != 0 {
		return nil, errors.New("obsidian does not use secrets")
	}
	if len(src.Containers) == 0 {
		return nil, errors.New("containers must list allowed vault-relative folders")
	}
	folders := slices.Clone(src.Containers)
	for _, f := range folders {
		if err := validFolder(f); err != nil {
			return nil, fmt.Errorf("container %q: %w", f, err)
		}
	}
	slices.Sort(folders)
	if len(slices.Compact(slices.Clone(folders))) != len(folders) {
		return nil, errors.New("containers contains duplicate folders")
	}
	for _, f := range s.Templates {
		if err := validFolder(f); err != nil {
			return nil, fmt.Errorf("template %q: %w", f, err)
		}
	}
	info, err := os.Lstat(s.Root)
	if err != nil {
		return nil, fmt.Errorf("vault root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("settings.root must be a directory, not a symlink")
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return nil, fmt.Errorf("opening vault root: %w", err)
	}
	for _, f := range folders {
		if err := safeDirectory(root, f); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("container %q: %w", f, err)
		}
	}
	acl := connector.ACL{{Kind: connector.ACLIdentity, Source: s.Owner.Source, NativeID: s.Owner.NativeID}}
	if s.Public {
		acl = connector.ACL{{Kind: connector.ACLPublic}}
	}
	permissionJSON, _ := json.Marshal(struct {
		ACL     connector.ACL      `json:"acl"`
		Owner   connector.Identity `json:"owner"`
		Version string             `json:"version"`
	}{acl, s.Owner, s.PermissionVersion})
	permissionHash := sha256.Sum256(permissionJSON)
	return &Connector{source: src.ID, root: root, folders: folders, templates: slices.Clone(s.Templates), owner: s.Owner, acl: acl, permission: hex.EncodeToString(permissionHash[:])[:16]}, nil
}

func validFolder(f string) error {
	if f == "" || f == "." || f == "*" || path.IsAbs(f) || path.Clean(f) != f || strings.Contains(f, "\\") || strings.Contains(f, "@") || strings.ContainsFunc(f, func(r rune) bool { return r <= 32 || r == 127 }) {
		return errors.New("must be a clean vault-relative folder path")
	}
	for _, part := range strings.Split(f, "/") {
		if part == ".obsidian" || part == ".trash" || part == ".." {
			return errors.New("excluded or parent directory")
		}
	}
	return nil
}

func safeDirectory(root *os.Root, f string) error {
	current := ""
	for _, part := range strings.Split(f, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("must be a real directory beneath the root")
		}
	}
	return nil
}

// Describe declares the document events this connector emits.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindDocument}}
}

// Health reports the state of this local source.
func (c *Connector) Health(context.Context) connector.Health {
	return connector.Health{Status: connector.HealthOK}
}

// Close releases the mounted vault handle.
func (c *Connector) Close(context.Context) error { return c.root.Close() }

// Poll is intentionally idle until the separate live-sync work lands.
func (c *Connector) Poll(context.Context, connector.Sink) error { return nil }

type frame struct {
	Dir   string `json:"dir"`
	After string `json:"after,omitempty"`
}
type position struct {
	Stack []frame `json:"stack"`
}

// Backfill visits at most pageSize filesystem entries per call. The cursor
// carries path names, never process-local offsets or open file handles.
func (c *Connector) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	p := position{Stack: []frame{{Dir: "."}}}
	if from != "" {
		if err := json.Unmarshal([]byte(from), &p); err != nil {
			return connector.BackfillResult{}, fmt.Errorf("decoding obsidian cursor: %w", err)
		}
		if len(p.Stack) == 0 || len(p.Stack) > 128 || p.Stack[0].Dir != "." {
			return connector.BackfillResult{}, errors.New("invalid obsidian cursor")
		}
		for i, f := range p.Stack {
			if f.Dir != "." {
				if err := validFolder(f.Dir); err != nil {
					return connector.BackfillResult{}, fmt.Errorf("invalid cursor directory: %w", err)
				}
			}
			if i > 0 && path.Dir(f.Dir) != p.Stack[i-1].Dir {
				return connector.BackfillResult{}, errors.New("invalid cursor ancestry")
			}
			if f.After != "" && (path.Base(f.After) != f.After || f.After == "." || f.After == ".." || strings.Contains(f.After, "\\")) {
				return connector.BackfillResult{}, errors.New("invalid cursor entry")
			}
		}
	}
	count, visited := 0, 0
	for len(p.Stack) > 0 && visited < pageSize {
		if err := ctx.Err(); err != nil {
			return connector.BackfillResult{}, err
		}
		top := &p.Stack[len(p.Stack)-1]
		// A directory replaced by a symlink cannot become a route out of the root.
		if top.Dir != "." {
			if err := safeDirectory(c.root, top.Dir); err != nil {
				return connector.BackfillResult{}, fmt.Errorf("resuming %q: %w", top.Dir, err)
			}
		}
		dir, err := c.root.Open(top.Dir)
		if err != nil {
			return connector.BackfillResult{}, fmt.Errorf("opening %q: %w", top.Dir, err)
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if readErr != nil {
			return connector.BackfillResult{}, fmt.Errorf("listing %q: %w", top.Dir, readErr)
		}
		if closeErr != nil {
			return connector.BackfillResult{}, closeErr
		}
		slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		next := -1
		for i, e := range entries {
			if e.Name() > top.After {
				next = i
				break
			}
		}
		if next < 0 {
			p.Stack = p.Stack[:len(p.Stack)-1]
			continue
		}
		entry := entries[next]
		top.After = entry.Name()
		visited++
		rel := path.Join(top.Dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 || excluded(rel, c.templates) {
			continue
		}
		if entry.IsDir() {
			if c.intersects(rel) {
				p.Stack = append(p.Stack, frame{Dir: rel})
			}
			continue
		}
		if !c.allowed(path.Dir(rel)) || !strings.EqualFold(path.Ext(rel), ".md") || !entry.Type().IsRegular() {
			continue
		}
		ev, err := c.event(rel)
		if err != nil {
			return connector.BackfillResult{}, err
		}
		if err := sink.Emit(ctx, ev); err != nil {
			return connector.BackfillResult{}, fmt.Errorf("emitting %q: %w", rel, err)
		}
		count++
	}
	if len(p.Stack) == 0 {
		return connector.BackfillResult{Done: true, Events: count}, nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	if len(raw) > connector.MaxCursorLen {
		return connector.BackfillResult{}, errors.New("obsidian cursor exceeds storage limit")
	}
	return connector.BackfillResult{Next: connector.Cursor(raw), Events: count}, nil
}

func excluded(rel string, templates []string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == ".obsidian" || part == ".trash" {
			return true
		}
	}
	for _, t := range templates {
		if rel == t || strings.HasPrefix(rel, t+"/") {
			return true
		}
	}
	return false
}
func (c *Connector) allowed(dir string) bool {
	for _, f := range c.folders {
		if dir == f || strings.HasPrefix(dir, f+"/") {
			return true
		}
	}
	return false
}
func (c *Connector) intersects(dir string) bool {
	for _, f := range c.folders {
		if dir == f || strings.HasPrefix(dir, f+"/") || strings.HasPrefix(f, dir+"/") || dir == "." {
			return true
		}
	}
	return false
}

func (c *Connector) event(rel string) (connector.Event, error) {
	info, err := c.root.Lstat(rel)
	if err != nil {
		return connector.Event{}, fmt.Errorf("stat %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return connector.Event{}, fmt.Errorf("%q is no longer a regular file", rel)
	}
	file, err := c.root.Open(rel)
	if err != nil {
		return connector.Event{}, fmt.Errorf("opening %q: %w", rel, err)
	}
	defer func() { _ = file.Close() }()
	// A changed path must not make a symlink into a device or outside-root read.
	if stat, err := file.Stat(); err != nil || !stat.Mode().IsRegular() {
		return connector.Event{}, fmt.Errorf("%q is no longer a regular file", rel)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return connector.Event{}, fmt.Errorf("reading %q: %w", rel, err)
	}
	if !utf8.Valid(data) {
		return connector.Event{}, fmt.Errorf("%q is not UTF-8 markdown", rel)
	}
	native, err := frontmatter(data)
	if err != nil {
		return connector.Event{}, fmt.Errorf("frontmatter in %q: %w", rel, err)
	}
	content := sha256.Sum256(data)
	token := hex.EncodeToString(content[:]) + "+" + c.permission
	artifact := escapedPath(rel)
	return connector.Event{Source: c.source, NativeID: artifact + "@" + token, Kind: connector.KindDocument, Time: info.ModTime().UTC(), ACL: slices.Clone(c.acl), Payload: connector.Payload{Artifact: artifact, Container: connector.Container{Kind: connector.ContainerFolder, NativeID: path.Dir(rel)}, Title: path.Base(rel), Text: string(data), Author: &c.owner, Native: native, Revision: &connector.Revision{Token: token}}}, nil
}

// escapedPath retains the vault-relative directory structure while making
// names with spaces or other unsafe bytes valid contract native ids.
func escapedPath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func frontmatter(data []byte) (json.RawMessage, error) {
	if !strings.HasPrefix(string(data), "---\n") {
		return nil, nil
	}
	end := strings.Index(string(data[4:]), "\n---\n")
	if end < 0 {
		return nil, nil
	}
	var value map[string]any
	if err := yaml.Unmarshal(data[4:4+end], &value); err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return json.Marshal(value)
}
