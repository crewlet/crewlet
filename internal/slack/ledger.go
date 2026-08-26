package slack

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The provisioning ledger: which Slack app belongs to which agent.
//
// # Why there is a file at all, when every other vendor has none
//
// Slack serves an app's credentials ONCE, at apps.manifest.create, and has
// no method that reads them back. Two of the four — the client id and
// secret — are needed only to redo an install later, and no part of the
// running engine reads either. So they cannot go into the config's `${VAR}`
// contract like a bot token does (there is no field for them), and they must
// not be thrown away (losing them means deleting the app and making another).
//
// A local ledger is what is left. It also makes a re-run idempotent: a
// handle with a recorded app id gets apps.manifest.update rather than a
// second app with the same name.
//
// IT IS A SECRETS FILE. It holds client secrets, so it is written 0600
// through a temp file and a rename, and it belongs in .gitignore beside
// .env — the writer cannot enforce the second, so the report says it.

// LedgerName is the default filename, beside the company document.
const LedgerName = "slack-apps.json"

// AppRecord is one provisioned app.
type AppRecord struct {
	AppID         string `json:"app_id"`
	ClientID      string `json:"client_id,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
	SigningSecret string `json:"signing_secret,omitempty"`
	BotUserID     string `json:"bot_user_id,omitempty"`
	TeamID        string `json:"team_id,omitempty"`

	// ManifestHash is the digest of the last manifest Slack accepted.
	// A re-run whose manifest still matches skips apps.manifest.update,
	// which matters because that method is rate limited to roughly one
	// request a minute and a company of seven issues seven of them.
	ManifestHash string `json:"manifest_hash,omitempty"`
}

// Installed reports an app the workspace has actually installed.
//
// The BOT USER ID is the tell, because it is the one field that can only
// come from a completed OAuth exchange: an app can be created, updated and
// listed for ever without anybody clicking Allow.
func (r AppRecord) Installed() bool { return r.BotUserID != "" }

// Ledger is the whole file: agent handle to provisioned app, plus the
// operator's rotating app-configuration token.
type Ledger struct {
	Apps map[string]AppRecord `json:"apps"`

	// ConfigToken is the app-configuration token pair, kept because
	// Slack's rotation is single-use in both directions: rotating
	// invalidates the refresh token it was given, so a run that rotated
	// and did not persist the result has locked the operator out of their
	// own apps. Recorded the moment it is minted, before it is used.
	ConfigToken struct {
		Token        string    `json:"token,omitempty"`
		RefreshToken string    `json:"refresh_token,omitempty"`
		ExpiresAt    time.Time `json:"expires_at,omitzero"`
	} `json:"config_token,omitzero"`
}

// LoadLedger reads the ledger at path; a missing file is an empty one.
func LoadLedger(path string) (*Ledger, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Ledger{Apps: map[string]AppRecord{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("slack: read %s: %w", path, err)
	}
	var ledger Ledger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil, fmt.Errorf("slack: parse %s: %w", path, err)
	}
	if ledger.Apps == nil {
		ledger.Apps = map[string]AppRecord{}
	}
	return &ledger, nil
}

// Save writes the ledger atomically and owner-only.
//
// ATOMICITY MATTERS MORE HERE than for most files: the ledger holds client
// secrets Slack returns exactly once, so a truncate-then-write interrupted
// half way would destroy them with no way back. The temp file is 0600 by
// construction and the rename carries that mode onto the destination.
func (l *Ledger) Save(path string) error {
	encoded, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("slack: encode %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".slack-apps.*")
	if err != nil {
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.WriteString(tmp, string(encoded)+"\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	return nil
}

// Handles names the seats the ledger knows, sorted.
func (l *Ledger) Handles() []string {
	out := make([]string, 0, len(l.Apps))
	for handle := range l.Apps {
		out = append(out, handle)
	}
	sort.Strings(out)
	return out
}

// LedgerPathFor is the ledger beside a company document.
func LedgerPathFor(companyPath string) string {
	return filepath.Join(filepath.Dir(companyPath), LedgerName)
}
