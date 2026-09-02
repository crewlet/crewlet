package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// Subject identifies who a profile is about.
//
// The three fields are ” when absent rather than NULL — the opposite of the
// nullable keys elsewhere in this package, and for a reason worth keeping:
// they are PRIMARY KEY columns whose emptiness is meaningful. The composite is
// what lets a resolved agent (Handle set) and an unmapped external human
// (ExternalID plus Platform) coexist under one observer without colliding.
// NULL here would make every such row distinct from every other, which is the
// exact opposite of what the key is for.
type Subject struct {
	// Handle is the seat, when the counterparty resolved to one.
	Handle string
	// ExternalID and Platform identify an unmapped external human.
	ExternalID string
	Platform   string
	// Name is display only and is not part of the identity: a person
	// renaming themselves on a chat surface must not orphan their profile.
	Name string
}

// Resolved reports whether the subject is a known seat.
func (s Subject) Resolved() bool { return s.Handle != "" }

// Valid reports whether the subject identifies anyone at all.
func (s Subject) Valid() bool {
	return s.Handle != "" || (s.ExternalID != "" && s.Platform != "")
}

// Profile is what one observer has learned about one subject.
type Profile struct {
	Observer string
	Subject  Subject

	// Traits is a flexible bag whose keys the model invents. Lookups are
	// exact, never vector: a profile is fetched for a known counterparty,
	// not searched for.
	Traits map[string]any

	FirstSeenAt time.Time

	// LastUpdatedAt moves on every upsert, no-ops included, so it measures
	// INTERACTION cadence.
	LastUpdatedAt time.Time

	// LastCorroboratedAt moves only when the traits patch is non-empty, so
	// it measures trait-CHANGE cadence. The Plan-phase prefetch demotes
	// stale traits by it — a counterparty seen daily whose traits have not
	// moved in months is one the observer has stopped learning about, and
	// that is a different fact from not having seen them.
	LastCorroboratedAt time.Time

	InteractionCount int

	// LastWorkKey is the last unit of work counted into InteractionCount.
	// One column rather than a side table: the duplicate this guards
	// against is always the IMMEDIATELY preceding write — two nodes racing,
	// or a redelivery — never one from last week, so the last key is
	// exactly as much history as the guard can use.
	LastWorkKey string
}

// Counterparties is the observer-scoped profile store.
type Counterparties struct{ db *store.DB }

// NewCounterparties wraps a database handle.
func NewCounterparties(db *store.DB) *Counterparties { return &Counterparties{db: db} }

// Observation is one interaction's worth of what was learned.
type Observation struct {
	Observer string
	Subject  Subject

	// Traits is a PATCH, merged over what is stored. An empty patch is an
	// ordinary observation — the observer interacted and learned nothing
	// new, which is itself a fact the two timestamps distinguish.
	Traits map[string]any

	// WorkKey identifies the unit of work this observation came from, and
	// is what the redelivery guard remembers: an observation carrying the
	// key of the immediately preceding one does not move the counter.
	//
	// Empty means UNGUARDED, not uncounted. Without a key a redelivery is
	// indistinguishable from a second interaction, and the two errors are
	// not symmetric — suppressing real interactions makes a seat believe
	// it has never met someone it works with daily — so an unkeyed
	// observation counts. What it must not do is overwrite the remembered
	// key: that would disarm the guard for the NEXT observation too. See
	// TestUnkeyedObservationsAlwaysCount.
	WorkKey string

	At time.Time
}

// Record merges an observation into a profile, creating it if needed.
//
// Reports whether the INTERACTION COUNTER moved. False means the same work key
// was already counted — a redelivery, or two nodes racing the same turn —
// which is the guard working rather than a failure.
func (c *Counterparties) Record(ctx context.Context, o Observation) (bool, error) {
	if o.Observer == "" || !o.Subject.Valid() {
		return false, fmt.Errorf("learning: an observation needs an observer and a subject")
	}
	counted := false
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		existing, err := c.load(ctx, tx, o.Observer, o.Subject)
		switch {
		case err == nil:
		case sql.ErrNoRows == err: //nolint:errorlint // exact sentinel from load
			existing = Profile{}
		default:
			return err
		}

		merged := maps.Clone(existing.Traits)
		if merged == nil {
			merged = map[string]any{}
		}
		changed := false
		for k, v := range o.Traits {
			// A patch key whose value is UNCHANGED is not a
			// corroboration. Treating it as one makes every interaction
			// look like fresh learning and the staleness signal stops
			// meaning anything.
			if before, had := merged[k]; !had || !sameValue(before, v) {
				changed = true
			}
			merged[k] = v
		}

		// first_seen_at is deliberately absent from the DO UPDATE clause
		// below, so this value is only ever used by the INSERT — the
		// column is written once and never moves. Computing it from
		// `existing` as well would be a second guard on a property the
		// upsert already holds; it was written, mutated away, and nothing
		// noticed.
		firstSeen := o.At
		corroborated := existing.LastCorroboratedAt
		if changed || corroborated.IsZero() {
			corroborated = o.At
		}
		count := existing.InteractionCount
		workKey := existing.LastWorkKey
		// An UNKEYED observation counts — see
		// TestUnkeyedObservationsAlwaysCount for why that asymmetry is
		// deliberate — but it must not TOUCH the guard. Assigning
		// o.WorkKey unconditionally wrote "" into last_work_key, which
		// disarmed the dedupe for the next observation as well: the
		// redelivery of a keyed interaction that arrived after an unkeyed
		// one compared its real key against "", differed, and counted a
		// second time. So the column keeps the last KEYED unit of work,
		// which is the only thing it can usefully remember.
		if o.WorkKey == "" || o.WorkKey != existing.LastWorkKey {
			count++
			counted = true
			// ONLY A REAL KEY MOVES THE GUARD. Assigning o.WorkKey
			// unconditionally wrote "" into last_work_key, which disarmed
			// the dedupe for the next observation as well: a redelivery
			// arriving after an unkeyed observation compared its real key
			// against "", differed, and counted a second time. The column
			// keeps the last KEYED unit of work, which is the only thing
			// it can usefully remember.
			if o.WorkKey != "" {
				workKey = o.WorkKey
			}
		}
		name := o.Subject.Name
		if name == "" {
			// A display name absent from THIS observation must not erase
			// the one already known: a chat payload that omits it is
			// common, and blanking the profile makes every later prompt
			// say "someone" about a person the seat has met a dozen times.
			name = existing.Subject.Name
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO counterparty_profiles (
				observer_handle, subject_handle, subject_external_id, subject_platform,
				subject_name, traits, first_seen_at, last_updated_at,
				last_corroborated_at, interaction_count, last_work_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (observer_handle, subject_handle, subject_external_id, subject_platform)
			DO UPDATE SET
				subject_name = excluded.subject_name,
				traits = excluded.traits,
				last_updated_at = excluded.last_updated_at,
				last_corroborated_at = excluded.last_corroborated_at,
				interaction_count = excluded.interaction_count,
				last_work_key = excluded.last_work_key`,
			o.Observer, o.Subject.Handle, o.Subject.ExternalID, o.Subject.Platform,
			name, jsonObject(merged), store.EncodeTime(firstSeen), store.EncodeTime(o.At),
			store.EncodeTime(corroborated), count, workKey)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("learning: record observation for %s: %w", o.Observer, err)
	}
	return counted, nil
}

// Purge drops profiles nobody has interacted with since cutoff, reporting how
// many went.
//
// A RETENTION AT ALL, which this table had none of: one row per distinct
// human or seat a seat has ever messaged, republished to every peer for every
// held seat on each memory-sync cycle, growing for the life of the
// deployment. Every other table with a documented horizon ships a sweep; this
// one was `wholeEachCycle` in memsync with no cap, no TTL and no maintenance
// entry.
//
// Keyed on last_updated_at rather than last_corroborated_at: this drops a
// profile nobody has INTERACTED with, and last_updated_at moves on every
// upsert including the no-ops, so it is the true last-contact stamp.
// Corroboration measures trait CHANGE, which a long steady relationship can
// go a year without.
//
// Losing one is cheap and self-healing: the next interaction re-creates the
// row and the seat re-learns from what it observes. What it costs is the
// interaction count, which is a cadence signal rather than a fact.
func (c *Counterparties) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := c.db.SQL().ExecContext(ctx,
		`DELETE FROM counterparty_profiles WHERE last_updated_at < ?`,
		store.EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("learning: purge counterparty profiles: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("learning: purge counterparty profiles: %w", err)
	}
	return n, nil
}

// Get returns one profile, and false when the observer has never met the
// subject.
func (c *Counterparties) Get(ctx context.Context, observer string, subject Subject) (Profile, bool, error) {
	p, err := c.load(ctx, c.db.SQL(), observer, subject)
	if err == sql.ErrNoRows { //nolint:errorlint // exact sentinel from load
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, fmt.Errorf("learning: read profile for %s: %w", observer, err)
	}
	return p, true, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (c *Counterparties) load(ctx context.Context, q querier, observer string, s Subject) (Profile, error) {
	row := q.QueryRowContext(ctx, `
		SELECT observer_handle, subject_handle, subject_external_id, subject_platform,
			subject_name, traits, first_seen_at, last_updated_at,
			last_corroborated_at, interaction_count, last_work_key
		FROM counterparty_profiles
		WHERE observer_handle = ? AND subject_handle = ?
		  AND subject_external_id = ? AND subject_platform = ?`,
		observer, s.Handle, s.ExternalID, s.Platform)

	var (
		p                                Profile
		traits                           string
		firstSeen, updated, corroborated int64
	)
	if err := row.Scan(&p.Observer, &p.Subject.Handle, &p.Subject.ExternalID,
		&p.Subject.Platform, &p.Subject.Name, &traits, &firstSeen, &updated,
		&corroborated, &p.InteractionCount, &p.LastWorkKey); err != nil {
		return Profile{}, err
	}
	p.FirstSeenAt = store.DecodeTime(firstSeen)
	p.LastUpdatedAt = store.DecodeTime(updated)
	p.LastCorroboratedAt = store.DecodeTime(corroborated)
	if err := json.Unmarshal([]byte(traits), &p.Traits); err != nil {
		log.WarnContext(ctx, "counterparty_traits_undecodable", "observer", observer, "error", err)
		p.Traits = map[string]any{}
	}
	return p, nil
}

// sameValue compares two trait values for corroboration purposes.
//
// Via their JSON rendering, because the values come off a model as arbitrary
// decoded JSON — nested maps and slices included — and Go's == panics on
// those. Comparing renderings also makes 1 and 1.0 the same trait, which is
// what a reader means by unchanged.
func sameValue(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		// One of them cannot be rendered, so it cannot be stored either.
		// Calling it CHANGED is the safe direction: it moves the
		// corroboration stamp, which at worst makes a profile look fresher
		// than it is, where the other direction hides a real update.
		return false
	}
	return string(ja) == string(jb)
}
