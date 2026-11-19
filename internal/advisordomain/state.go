package advisordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE ROWS A DECISION AND AN APPLY READ, in the caller's transaction: the
// writer decides from them in its snapshot, and the applier holds a record to
// them again, writing nothing where they no longer admit it.

// keyState is one finding key's row.
type keyState struct {
	found bool

	// version is the position of the newest record on the key, episodes how
	// many episodes it has opened, and current its non-closed one, if any.
	version  int64
	episodes int
	current  string

	// hold is when the key's hold lapses (zero for good), valid while one
	// is set; holdSeverity and holdEpisode its severity and the episode
	// whose dismissal set it.
	hold         sql.NullInt64
	holdSeverity Severity
	holdEpisode  string

	// ineffective is every fix, by digest, that did not take effect.
	ineffective []string
}

// loadKey reads one key's row by its id.
func loadKey(ctx context.Context, tx *sql.Tx, keyID string) (keyState, error) {
	var (
		k                          keyState
		current, severity, episode sql.NullString
		ineffective                string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT version, episodes, current_episode, hold_until, hold_severity, hold_episode, ineffective
		FROM advisor_keys WHERE key_id = ?`, keyID).Scan(&k.version, &k.episodes, &current, &k.hold,
		&severity, &episode, &ineffective)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return keyState{}, nil
	case err != nil:
		return keyState{}, fmt.Errorf("advisordomain: read key %s: %w", keyID, err)
	}
	k.found, k.current = true, current.String
	k.holdSeverity, k.holdEpisode = Severity(severity.String), episode.String
	if err := json.Unmarshal([]byte(ineffective), &k.ineffective); err != nil {
		return keyState{}, fmt.Errorf("advisordomain: key %s's ineffective fixes do not decode: %w", keyID, err)
	}
	return k, nil
}

// holds reports whether the key's hold keeps an episode of severity quiet at
// the instant at: a hold not yet lapsed, with no graver severity.
func (k keyState) holds(severity Severity, at time.Time) bool {
	if !k.hold.Valid {
		return false
	}
	lapsed := k.hold.Int64 != 0 && k.hold.Int64 <= store.EncodeTime(at)
	return !lapsed && severity.Rank() <= k.holdSeverity.Rank()
}

// withIneffective is the key's ineffective list with digest added once, as a
// row stores it.
func (k keyState) withIneffective(digest string) string {
	list := slices.Clone(k.ineffective)
	if !slices.Contains(list, digest) {
		list = append(list, digest)
	}
	return encodeList(list)
}

// encodeList is a list of words as a row stores it, `[]` for none.
func encodeList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	body, err := json.Marshal(list)
	if err != nil {
		// A list of strings encodes.
		panic(fmt.Sprintf("advisordomain: encode a list: %v", err))
	}
	return string(body)
}

// episodeState is one episode's row, as much of it as a move is judged by.
type episodeState struct {
	found bool

	keyID         string
	state         State
	revision      int
	verdictDigest string
	fixDigest     string
	appliedFix    string
}

// loadEpisode reads one episode's row by its id.
func loadEpisode(ctx context.Context, tx *sql.Tx, id string) (episodeState, error) {
	var (
		e     episodeState
		state string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT key_id, state, revision, verdict_digest, fix_digest, applied_fix
		FROM advisor_findings WHERE id = ?`, id).Scan(&e.keyID, &state, &e.revision,
		&e.verdictDigest, &e.fixDigest, &e.appliedFix)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return episodeState{}, nil
	case err != nil:
		return episodeState{}, fmt.Errorf("advisordomain: read episode %s: %w", id, err)
	}
	e.found, e.state = true, State(state)
	return e, nil
}

// legalFrom reports whether a move's event may leave state (package doc,
// "Legal moves"): closed is terminal.
func legalFrom(event Event, state State) bool {
	switch event {
	case EventApplied, EventSnoozed, EventUnsnoozed, EventUseful:
		return state == StateOpen
	case EventIneffective:
		return state == StateApplied
	case EventClosed:
		return state == StateOpen || state == StateApplied
	}
	return false
}

// encodeOptional is an optional instant as a row stores it: zero for none.
func encodeOptional(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return store.EncodeTime(t)
}

// decodeOptional is an optional instant read back: the zero time for none.
func decodeOptional(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return store.DecodeTime(v)
}
