package codescan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("atlascodescan")

// Rules is the version of what the parsers and tables read out of a file. A
// build that reads more, or reads it differently, raises it, and every
// repository is read again at its next pass though its tip has not moved.
const Rules = 1

// Status is one repository's standing in the coordination store: the tip
// last read whole, and what that reading listed, read and refused.
type Status struct {
	// Repo is the repository's id, the record's key; Name the name its
	// claims are made under, which is the voice a rename has to withdraw.
	Repo string `json:"repo"`
	Name string `json:"name"`

	// Tip is the default branch's commit last read whole and recorded
	// whole, "" for a repository whose default branch held none; Rules is
	// the rules it was read by.
	Tip   string `json:"tip,omitempty"`
	Rules int    `json:"rules,omitempty"`

	// Stamp is what a forge's listing said of the repository when it was
	// last read whole ([collect.CodeRepository.Stamp]); a Gitlet repository
	// has none. TooLarge is a tip whose tree the forge does not list whole,
	// so none of its files is read.
	Stamp    string `json:"stamp,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`

	// LastAttemptAt is when the repository was last read, Completed whether
	// that pass read every listed file and published every record, and
	// LastCompleteAt when one last did.
	LastAttemptAt  time.Time `json:"last_attempt_at,omitzero"`
	Completed      bool      `json:"completed,omitempty"`
	LastCompleteAt time.Time `json:"last_complete_at,omitzero"`

	// Listed is how many files of the tip are on the closed list, and
	// Unlisted how many more it holds past the bound, none of which is read.
	Listed   int `json:"listed,omitempty"`
	Unlisted int `json:"unlisted,omitempty"`

	// Calls is how many reads the last pass made at the repository's host,
	// Reads how many of them read a file and Bytes how many bytes of files
	// they read. Pending is how many listed files a later pass still has to
	// read.
	Calls   int `json:"calls,omitempty"`
	Reads   int `json:"reads,omitempty"`
	Bytes   int `json:"bytes,omitempty"`
	Pending int `json:"pending,omitempty"`

	// What the reading refused: Oversize files past the byte bound, Unparsed
	// files that are not their format, Withheld values a grammar refused,
	// and Truncated claims past one file's bound.
	Oversize  int `json:"oversize,omitempty"`
	Unparsed  int `json:"unparsed,omitempty"`
	Withheld  int `json:"withheld,omitempty"`
	Truncated int `json:"truncated,omitempty"`

	// Entries is how many entries the reading names; Recorded how many
	// records the last pass published, and Purged how many the log refused
	// because an administrator purged what they named.
	Entries  int `json:"entries,omitempty"`
	Recorded int `json:"recorded,omitempty"`
	Purged   int `json:"purged,omitempty"`

	// LastError is the fault of the last pass that returned one, cleared by
	// any pass that does not.
	LastError string `json:"last_error,omitempty"`
}

// RetryAfter is how soon a repository whose last pass failed is read again:
// five minutes, the collect duty's own retry, so a Gitlet node that cannot
// answer is asked a dozen times an hour rather than once a backstop.
const RetryAfter = 5 * time.Minute

// due reports whether the repository at tip owes a pass at now: never read,
// read at another tip or by other rules, or left unfinished. A pass that
// FAILED waits out [RetryAfter]; one that only ran out of its read bound
// does not.
func (s Status) due(tip string, now time.Time) bool {
	if s.Completed && s.Tip == tip && s.Rules == Rules {
		return false
	}
	return s.LastError == "" || !now.Before(s.LastAttemptAt.Add(RetryAfter))
}

// Scans is the coordination store's bucket of scan standings
// (coord.AtlasCodeScans), as this package reads it.
type Scans interface {
	AtlasCodeScans(ctx context.Context) (map[string][]byte, error)
	PutAtlasCodeScan(ctx context.Context, repo string, value []byte) error
	DeleteAtlasCodeScan(ctx context.Context, repo string) error
}

// CoordStore is each repository's standing on the fleet's coordination store.
type CoordStore struct{ scans Scans }

// NewCoordStore wraps the fleet's standings, refusing a nil backend: a nil
// wrapped is a crash on the first pass.
func NewCoordStore(scans Scans) (*CoordStore, error) {
	if scans == nil {
		return nil, errors.New("codescan: NewCoordStore needs the fleet's coordination backend (scans is nil), " +
			"which is where each repository's standing is recorded")
	}
	return &CoordStore{scans: scans}, nil
}

// Load reads every recorded standing, by repository id. One this build
// cannot decode is skipped: the repository is read again and its standing
// rewritten.
func (s *CoordStore) Load(ctx context.Context) (map[string]Status, error) {
	raw, err := s.scans.AtlasCodeScans(ctx)
	if err != nil {
		return nil, fmt.Errorf("codescan: read the scan standings: %w", err)
	}
	out := make(map[string]Status, len(raw))
	for repo, value := range raw {
		// A FORGE'S KEY IS NOT A GITLET REPOSITORY'S ([ForgeStore]): read as
		// one, it would be a repository Gitlet no longer lists, and released.
		if strings.HasPrefix(repo, forgePrefix) {
			continue
		}
		var st Status
		if err := json.Unmarshal(value, &st); err != nil {
			log.WarnContext(ctx, "atlas_code_standing_undecodable", "repo", repo, "error", err,
				"detail", "this repository is read again and its standing rewritten")
			continue
		}
		// THE KEY IS AUTHORITATIVE, not the field inside the document.
		st.Repo = repo
		out[repo] = st
	}
	return out, nil
}

// Save records one repository's standing.
func (s *CoordStore) Save(ctx context.Context, st Status) error {
	if st.Repo == "" {
		return errors.New("codescan: cannot record a standing for no repository")
	}
	value, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("codescan: encode the standing of %s: %w", st.Repo, err)
	}
	if err := s.scans.PutAtlasCodeScan(ctx, st.Repo, value); err != nil {
		return fmt.Errorf("codescan: record the standing of %s: %w", st.Repo, err)
	}
	return nil
}

// Forget drops a repository's standing.
func (s *CoordStore) Forget(ctx context.Context, repo string) error {
	if err := s.scans.DeleteAtlasCodeScan(ctx, repo); err != nil {
		return fmt.Errorf("codescan: forget the standing of %s: %w", repo, err)
	}
	return nil
}
