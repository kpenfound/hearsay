// Package drive backfills and tracks explicitly configured Google Drive folders.
package drive

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

const (
	// Type is the registry key for this connector.
	Type = "drive"
	// SecretCredentials contains the service account JSON.
	SecretCredentials = "credentials"
	defaultAPI        = "https://www.googleapis.com/drive/v3"
)

var driveID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Settings is the source's Drive-specific configuration.
type Settings struct {
	TranscriptCandidateFolderIDs []string `json:"transcript_candidate_folder_ids"`
	MeetingTranscriptLabelID     string   `json:"meeting_transcript_label_id"`
	APIURL                       string   `json:"api_url"`
	NotificationURL              string   `json:"notification_url"`
}

type credentials struct {
	Type        string `json:"type"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// Connector walks configured Drive folders into L0.
type Connector struct {
	source          string
	folders         []string
	candidates      map[string]bool
	label           string
	api             *client
	mu              sync.Mutex
	lastEventAt     time.Time
	notificationURL string
	watchID         string
	watchToken      string
	watchExpires    time.Time
}

var (
	_ connector.Poller       = (*Connector)(nil)
	_ connector.CursorPoller = (*Connector)(nil)
	_ connector.Pusher       = (*Connector)(nil)
	_ connector.Backfiller   = (*Connector)(nil)
)

// Factory is registered under Type by the binary.
func Factory(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
	return New(src)
}

// New validates a Drive source and its service account credential.
func New(src connector.SourceConfig) (*Connector, error) {
	var settings Settings
	if err := src.DecodeSettings(&settings); err != nil {
		return nil, err
	}
	if len(src.Containers) == 0 {
		return nil, errors.New("containers is empty: list explicit Drive folder IDs")
	}
	folders := slices.Clone(src.Containers)
	for _, id := range folders {
		if !driveID.MatchString(id) || id == connector.AllowAll {
			return nil, fmt.Errorf("container %q is not an explicit Drive folder ID", id)
		}
	}
	slices.Sort(folders)
	if len(slices.Compact(slices.Clone(folders))) != len(folders) {
		return nil, errors.New("containers contains duplicate folder IDs")
	}
	candidates := make(map[string]bool)
	for _, id := range settings.TranscriptCandidateFolderIDs {
		if !driveID.MatchString(id) || !slices.Contains(folders, id) {
			return nil, fmt.Errorf("transcript candidate folder %q is not an explicit container", id)
		}
		if candidates[id] {
			return nil, fmt.Errorf("transcript candidate folder %q is duplicated", id)
		}
		candidates[id] = true
	}
	if len(candidates) > 0 && !driveID.MatchString(settings.MeetingTranscriptLabelID) {
		return nil, errors.New("settings.meeting_transcript_label_id must be a published Drive label ID when transcript candidates are configured")
	}
	if len(candidates) == 0 && settings.MeetingTranscriptLabelID != "" {
		return nil, errors.New("settings.meeting_transcript_label_id has no transcript candidate folders")
	}
	for name := range src.Secrets {
		if name != SecretCredentials {
			return nil, fmt.Errorf("secret %q is not read by the drive connector", name)
		}
	}
	var cred credentials
	if err := json.Unmarshal([]byte(src.Secrets[SecretCredentials]), &cred); err != nil {
		return nil, fmt.Errorf("secret %q is not service account JSON: %w", SecretCredentials, err)
	}
	if cred.Type != "service_account" || cred.ClientEmail == "" || cred.TokenURI == "" || cred.PrivateKey == "" {
		return nil, fmt.Errorf("secret %q must be service account JSON with type, client_email, private_key and token_uri", SecretCredentials)
	}
	block, _ := pem.Decode([]byte(cred.PrivateKey))
	if block == nil {
		return nil, errors.New("credentials private_key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("credentials private_key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("credentials private_key is not RSA")
	}
	tokenURI, err := url.Parse(cred.TokenURI)
	if err != nil {
		return nil, fmt.Errorf("credentials token_uri: %w", err)
	}
	localTokenURI := tokenURI.Scheme == "http" && (strings.HasPrefix(tokenURI.Host, "127.0.0.1:") || strings.HasPrefix(tokenURI.Host, "localhost:"))
	if tokenURI.Host == "" || (tokenURI.Scheme != "https" && !localTokenURI) {
		return nil, errors.New("credentials token_uri must be an HTTPS URL")
	}
	base := settings.APIURL
	if base == "" {
		base = defaultAPI
	}
	apiURL, err := url.Parse(base)
	if err != nil || apiURL.Host == "" || (apiURL.Scheme != "https" && apiURL.Scheme != "http") || apiURL.RawQuery != "" || apiURL.Fragment != "" {
		return nil, fmt.Errorf("settings.api_url %q is not an HTTP(S) URL with a host", base)
	}
	if apiURL.Scheme == "http" && !strings.HasPrefix(apiURL.Host, "127.0.0.1:") && !strings.HasPrefix(apiURL.Host, "localhost:") {
		return nil, errors.New("settings.api_url must use HTTPS except for a local fixture")
	}
	if settings.NotificationURL != "" {
		notificationURL, err := url.Parse(settings.NotificationURL)
		if err != nil {
			return nil, fmt.Errorf("settings.notification_url: %w", err)
		}
		localNotification := notificationURL.Scheme == "http" && (strings.HasPrefix(notificationURL.Host, "127.0.0.1:") || strings.HasPrefix(notificationURL.Host, "localhost:"))
		if notificationURL.Host == "" || (notificationURL.Scheme != "https" && !localNotification) {
			return nil, errors.New("settings.notification_url must be HTTPS or a local fixture URL")
		}
	}
	return &Connector{source: src.ID, folders: folders, candidates: candidates, label: settings.MeetingTranscriptLabelID,
		notificationURL: settings.NotificationURL,
		api:             &client{base: strings.TrimRight(apiURL.String(), "/"), tokenURI: cred.TokenURI, email: cred.ClientEmail, key: rsaKey, http: &http.Client{Timeout: 30 * time.Second}}}, nil
}

// Describe declares the two L0 kinds Drive can emit.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindDocument, connector.KindTranscript, connector.KindTombstone}}
}

// Health reports the last emitted event without doing IO.
func (c *Connector) Health(context.Context) connector.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return connector.Health{Status: connector.HealthOK, LastEventAt: c.lastEventAt}
}

// Close has nothing to release; the connector starts no goroutines.
func (c *Connector) Close(context.Context) error { return nil }

// Poll is unused when the runtime offers the durable CursorPoller path.
func (c *Connector) Poll(context.Context, connector.Sink) error {
	return errors.New("drive polling needs a durable cursor")
}
