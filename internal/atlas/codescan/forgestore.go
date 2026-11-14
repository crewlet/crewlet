package codescan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// forgePrefix starts every key of the scan standings that is a forge's: the
// pass's own standing at forge/<kind>, and each repository's at
// forge/<kind>/<voice>. A Gitlet repository's standing is keyed by its id,
// which holds no slash, so the two readers of the bucket never take each
// other's keys for their own.
const forgePrefix = "forge/"

// ForgeStatus is one forge's standing: what its last pass over the forge's
// repositories listed, read and left for a later pass.
type ForgeStatus struct {
	Kind integration.Kind `json:"kind"`

	// LastAttemptAt is when a pass last ran, Completed whether it read
	// every repository that had moved, and LastCompleteAt when one last did.
	LastAttemptAt  time.Time `json:"last_attempt_at,omitzero"`
	Completed      bool      `json:"completed,omitempty"`
	LastCompleteAt time.Time `json:"last_complete_at,omitzero"`

	// Repositories is how many the forge listed, and Whole whether that is
	// all it holds. Mirrored is how many of them Gitlet mirrors, which
	// Gitlet's own scan reads; Refused how many the forge refused the code
	// of; Unnamed how many have a key no voice can be named for; TooLarge
	// how many hold a tree the forge does not list whole.
	Repositories int  `json:"repositories,omitempty"`
	Whole        bool `json:"whole,omitempty"`
	Mirrored     int  `json:"mirrored,omitempty"`
	Refused      int  `json:"refused,omitempty"`
	Unnamed      int  `json:"unnamed,omitempty"`
	TooLarge     int  `json:"too_large,omitempty"`

	// Waiting is how many repositories a later pass still has to read,
	// because this one spent its bounds.
	Waiting int `json:"waiting,omitempty"`

	// Calls is how many requests the pass made at the forge, Trees how many
	// trees it listed, Reads how many files it read and Bytes how many
	// bytes of them. Released is how many repositories it stopped reading.
	Calls    int `json:"calls,omitempty"`
	Trees    int `json:"trees,omitempty"`
	Reads    int `json:"reads,omitempty"`
	Bytes    int `json:"bytes,omitempty"`
	Released int `json:"released,omitempty"`

	// LastError is the fault of the last pass that ended on one.
	LastError string `json:"last_error,omitempty"`
}

// due reports whether a pass is owed at now: none was made, the last left
// work or a fault behind and [RetryAfter] has passed, or the interval has.
func (s ForgeStatus) due(interval time.Duration, now time.Time) bool {
	wait := interval
	if s.LastError != "" || s.Waiting > 0 {
		wait = min(interval, RetryAfter)
	}
	return s.LastAttemptAt.IsZero() || !now.Before(s.LastAttemptAt.Add(wait))
}

// ForgeStore is the forges' half of the scan standings, on the same bucket
// of the coordination store as Gitlet's ([CoordStore]) under keys of its own.
type ForgeStore struct{ scans Scans }

// NewForgeStore wraps the fleet's standings, refusing a nil backend.
func NewForgeStore(scans Scans) (*ForgeStore, error) {
	if scans == nil {
		return nil, errors.New("codescan: NewForgeStore needs the fleet's coordination backend (scans is nil), " +
			"which is where each forge's and each repository's standing is recorded")
	}
	return &ForgeStore{scans: scans}, nil
}

func forgeKey(kind integration.Kind) string { return forgePrefix + kind.String() }

func forgeRepoKey(kind integration.Kind, voice string) string { return forgeKey(kind) + "/" + voice }

// Load reads one forge's standing and each of its repositories', by voice.
// A record this build cannot decode is skipped, and read again.
func (s *ForgeStore) Load(ctx context.Context, kind integration.Kind) (ForgeStatus, map[string]Status, error) {
	raw, err := s.scans.AtlasCodeScans(ctx)
	if err != nil {
		return ForgeStatus{}, nil, fmt.Errorf("codescan: read the scan standings: %w", err)
	}
	fs := ForgeStatus{Kind: kind}
	if value, ok := raw[forgeKey(kind)]; ok {
		if err := json.Unmarshal(value, &fs); err != nil {
			log.WarnContext(ctx, "atlas_forge_standing_undecodable", "forge", kind.String(), "error", err,
				"detail", "the forge's repositories are read again and its standing rewritten")
			fs = ForgeStatus{}
		}
		// THE KEY IS AUTHORITATIVE, not the field inside the document.
		fs.Kind = kind
	}
	repos := map[string]Status{}
	for key, value := range raw {
		voice, ok := strings.CutPrefix(key, forgeKey(kind)+"/")
		if !ok || voice == "" {
			continue
		}
		var st Status
		if err := json.Unmarshal(value, &st); err != nil {
			log.WarnContext(ctx, "atlas_code_standing_undecodable", "repo", voice, "error", err,
				"detail", "this repository is read again and its standing rewritten")
			// KEPT AS A STANDING THAT OWES A READ, so a repository that is
			// gone still has its claims withdrawn.
			st = Status{}
		}
		st.Repo, st.Name = voice, voice
		repos[voice] = st
	}
	return fs, repos, nil
}

// SaveForge records a forge's standing.
func (s *ForgeStore) SaveForge(ctx context.Context, fs ForgeStatus) error {
	value, err := json.Marshal(fs)
	if err != nil {
		return fmt.Errorf("codescan: encode the standing of %s: %w", fs.Kind, err)
	}
	if err := s.scans.PutAtlasCodeScan(ctx, forgeKey(fs.Kind), value); err != nil {
		return fmt.Errorf("codescan: record the standing of %s: %w", fs.Kind, err)
	}
	return nil
}

// Save records one repository's standing.
func (s *ForgeStore) Save(ctx context.Context, kind integration.Kind, st Status) error {
	value, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("codescan: encode the standing of %s: %w", st.Name, err)
	}
	if err := s.scans.PutAtlasCodeScan(ctx, forgeRepoKey(kind, st.Name), value); err != nil {
		return fmt.Errorf("codescan: record the standing of %s: %w", st.Name, err)
	}
	return nil
}

// Forget drops one repository's standing.
func (s *ForgeStore) Forget(ctx context.Context, kind integration.Kind, voice string) error {
	if err := s.scans.DeleteAtlasCodeScan(ctx, forgeRepoKey(kind, voice)); err != nil {
		return fmt.Errorf("codescan: forget the standing of %s: %w", voice, err)
	}
	return nil
}

// ForgetForge drops a forge's own standing.
func (s *ForgeStore) ForgetForge(ctx context.Context, kind integration.Kind) error {
	if err := s.scans.DeleteAtlasCodeScan(ctx, forgeKey(kind)); err != nil {
		return fmt.Errorf("codescan: forget the standing of %s: %w", kind, err)
	}
	return nil
}
