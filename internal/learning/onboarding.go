package learning

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/toolname"
)

// ChainHash is the stable identity of where a seat sits in the org: the
// company name, the KEY of every unit above the seat outermost-first, then the
// seat's own HANDLE.
//
// It stamps an onboarding marker and is the whole re-onboarding trigger; content
// is deliberately NOT in it (a typo fix would re-run a full pass).
// IDENTITIES, NOT DISPLAY NAMES, so relabelling a division does not re-onboard
// its seats: unit keys and seat handles are immutable (ADR-0013). THE COMPANY
// NAME STAYS AS WRITTEN, being an input to [org.Organization.AgentIDFor] too.
//
// Empty for a nil org or seat; entry points taking a chain hash refuse an empty
// one, so a nil never becomes a marker nothing matches.
func ChainHash(o *org.Organization, r *org.Role) string {
	if o == nil || r == nil {
		return ""
	}
	// A slice walk, not a map range: this value is compared against one an
	// EARLIER PROCESS wrote, so anything whose order or seed varies per run
	// reads as a company-wide restructure on every restart. Same reason it
	// is sha256 and not maphash.
	parts := make([]string, 0, 4)
	parts = append(parts, o.Name)
	for _, u := range o.UnitChainFor(r) {
		parts = append(parts, u.Key())
	}
	parts = append(parts, r.Handle())
	return hashChain(parts)
}

// hashChain hashes the components as netstrings — each part's byte length, a
// colon, then the bytes.
//
// A separator-joined form (sha256 over strings.Join(parts, "\n")) is AMBIGUOUS:
// company "Acme\nOps" with a root seat joins to the same bytes as company
// "Acme" with unit "Ops" holding that seat, so a seat lifted out of a unit
// would keep a marker claiming it already onboarded there. Organization.Validate
// accepts a newline in a name (TestJoiningTheChainWithASeparatorWouldCollide
// skips itself if that changes), and a rarer separator only hides the
// collision; length prefixes are injective over arbitrary bytes.
//
// Changing this construction re-onboards every seat once.
func hashChain(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		// hash.Hash documents Write as never returning an error, and
		// there would be nothing useful to do with one here anyway.
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte(":"))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Marker is one seat's onboarding row: the org chain it has read itself
// into, and the lease state of any pass running over it.
type Marker struct {
	// AgentID is the DERIVED uuid over (org name, handle) — the same
	// identity the diary keys on.
	AgentID string

	// ChainHash is the chain the seat onboarded for, or empty on a row that
	// exists only because a pass was claimed. Empty never equals a real
	// [ChainHash], which is what makes a claimed-but-unmarked row read
	// correctly as "not onboarded".
	ChainHash string

	// Handle and Role are what the seat was called when it marked itself,
	// for an operator reading the row. Nothing looks a marker up by them,
	// which is why a corrected role name that leaves Role stale costs
	// nothing.
	Handle string
	Role   string

	// Summary is what the seat said it took away from onboarding.
	// Operator-facing only — nothing branches on it — so the table answers
	// "what did this seat actually learn", not just "yes".
	Summary string

	// CreatedAt is when the row first appeared and never moves again;
	// UpdatedAt moves on every write, claims and releases included, so it
	// measures activity on the row rather than the age of the onboarding.
	//
	// Both are read-side: [Onboarding.Mark] takes its write time as an
	// argument instead, because a caller filling these in would have to
	// know which of the two the upsert ignores.
	CreatedAt time.Time
	UpdatedAt time.Time

	// LeaseUntil is the deadline of the pass lease, zero when no pass is
	// claimed. Read it through [Marker.LeaseHeld] rather than comparing it
	// directly — the boundary has to match the one the claim uses.
	LeaseUntil time.Time
}

// LeaseHeld reports whether a pass lease is still holding at now.
//
// The boundary is `now < until`: AT the deadline the lease is already free,
// which is the same boundary the diary's ttl_until uses. It has to be the
// same answer [Onboarding.Claim] reaches in SQL, or a reader sees a pass in
// flight that any turn can steal, or — worse — a free lease no claimant can
// take.
func (m Marker) LeaseHeld(now time.Time) bool { return leaseHeld(m.LeaseUntil, now) }

// leaseHeld compares at the resolution the COLUMN holds, not the resolution
// time.Time holds. now.Before(until) is a nanosecond comparison, and a
// deadline is truncated to microseconds on its way into the row — so a
// claimant still holding the untruncated deadline [Claim] handed it back
// (Pass.Until, minted from a wall clock and carrying nanoseconds) would
// believe its lease outlives the instant at which the SQL predicate, which
// only ever sees the truncated value, hands the lease to somebody else.
//
// A zero deadline needs no special case: it encodes to -62135596800000000,
// year 1, which is before every instant a running company can produce. Saying
// so here rather than branching on it is deliberate — the branch was
// unreachable through any input this package can be given.
func leaseHeld(until, now time.Time) bool {
	return store.EncodeTime(now) < store.EncodeTime(until)
}

// Pass is a held onboarding-pass lease. The zero value means the lease was
// not taken, so `if !pass.Held() { skip }` is the whole caller-side protocol.
type Pass struct {
	AgentID string

	// Until is the deadline the claim wrote, and it doubles as the fencing
	// token [Onboarding.Release] clears under. See Release for why an
	// unfenced clear is a bug rather than a simplification.
	Until time.Time
}

// Held reports whether this value is a lease that was actually taken.
func (p Pass) Held() bool { return p.AgentID != "" }

// Onboarding is the per-seat onboarding bookkeeping: one row per agent
// saying which org chain that seat has already read itself into, plus the
// TTL lease that keeps two turns from running the pass at once.
//
// The table needs no retention sweep, unlike everything else in this
// package: it holds one row per seat, so it is bounded by the size of the
// company rather than by how long the company has been running. Seats that
// leave the org orphan a row each, which is a handful of bytes and a name an
// operator can still look up.
type Onboarding struct{ db *store.DB }

// NewOnboarding wraps a database handle.
func NewOnboarding(db *store.DB) *Onboarding { return &Onboarding{db: db} }

// Onboarded reports whether this seat has already onboarded for this chain.
//
// Three answers, the third is the point:
//
//	(true, nil)   — a marker stamped with chainHash. Skip the pass.
//	(false, nil)  — definitively no matching marker (never marked, or the
//	                chain moved). Run the pass.
//	(false, err)  — UNKNOWN: the store did not answer. Skip, ask next turn.
//
// Collapsing the error into "not onboarded" would re-run a whole pass (minutes
// of LLM rounds) for every marked seat on any store blip, against a one-turn
// delay of a hint if it skips. That asymmetry is why this returns an error.
func (o *Onboarding) Onboarded(ctx context.Context, agentID, chainHash string) (bool, error) {
	if agentID == "" || chainHash == "" {
		// Deliberately not "definitively not onboarded". A pass run under
		// a blank chain hash could never mark — Mark refuses one — so it
		// would re-run every single turn forever. Unknown routes the
		// caller to skip, which is the only safe answer to a question
		// this malformed.
		return false, fmt.Errorf("learning: an onboarding check needs an agent id and a chain hash")
	}
	var stored string
	err := o.db.SQL().QueryRowContext(ctx,
		`SELECT chain_hash FROM agent_onboarding_markers WHERE agent_id = ?`,
		agentID).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("learning: read onboarding marker for %s: %w", agentID, err)
	}
	return stored == chainHash, nil
}

// Mark records that this seat completed onboarding for m.ChainHash.
//
// Re-onboarding overwrites the row in place, so [Onboarding.Onboarded]
// reflects the new chain immediately and no cleanup pass has to find the
// stale marker later. CreatedAt survives the overwrite: it says when this
// seat first onboarded, which is a fact a restructure does not change.
//
// Marking also clears the pass lease, unfenced and on purpose — unlike
// Release. The lease protects the RUNNING of a pass, and a mark is a pass
// that finished: whatever claimant is still holding it, the work it was
// serialising is now done and the marker suppresses every later attempt.
func (o *Onboarding) Mark(ctx context.Context, m Marker, at time.Time) error {
	switch {
	case m.AgentID == "":
		return fmt.Errorf("learning: an onboarding marker needs an agent id")
	case m.ChainHash == "":
		// A blank hash is what a claimed-but-unmarked row carries. Storing
		// one as a MARK would make Onboarded answer false for every chain
		// forever, so the seat would re-onboard every turn and never
		// settle.
		return fmt.Errorf("learning: an onboarding marker needs a chain hash")
	}
	stamp := store.EncodeTime(at)
	if _, err := o.db.SQL().ExecContext(ctx, `
		INSERT INTO agent_onboarding_markers
			(agent_id, chain_hash, agent_handle, role, summary,
			 created_at, updated_at, in_progress_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (agent_id) DO UPDATE SET
			chain_hash        = excluded.chain_hash,
			agent_handle      = excluded.agent_handle,
			role              = excluded.role,
			summary           = excluded.summary,
			updated_at        = excluded.updated_at,
			in_progress_until = NULL`,
		m.AgentID, m.ChainHash, m.Handle, m.Role, m.Summary, stamp, stamp,
	); err != nil {
		return fmt.Errorf("learning: mark onboarding for %s: %w", m.AgentID, err)
	}
	return nil
}

// Claim takes the single-flight lease on one seat's onboarding pass.
//
// A held Pass means run; the zero Pass means skip. FAILS CLOSED: an error
// yields the zero Pass (a duplicate onboarding is costly; a skip costs a turn).
// ONE conditional upsert, for the LOCK HOLD: the write lock excludes every
// table in the file, and read-then-write would hold it across read, decision
// and write for every claimant queued. The loser still gets (Pass{}, nil); a
// write transaction takes the lock at BEGIN (internal/store/begin.go).
//
// A claim for a never-marked seat INSERTS the row with an empty chain_hash,
// which Onboarded reads as "not onboarded": lease and marker share a row.
func (o *Onboarding) Claim(ctx context.Context, agentID string, now time.Time, ttl time.Duration) (Pass, error) {
	if agentID == "" {
		return Pass{}, fmt.Errorf("learning: an onboarding claim needs an agent id")
	}
	if ttl <= 0 {
		// A lease that is already expired when it is written is not a
		// degenerate lease, it is no lease: every concurrent claimant
		// sees a free one and they all run the pass together.
		return Pass{}, fmt.Errorf("learning: an onboarding claim needs a positive ttl, got %s", ttl)
	}
	until := now.Add(ttl)
	stamp, deadline := store.EncodeTime(now), store.EncodeTime(until)
	res, err := o.db.SQL().ExecContext(ctx, `
		INSERT INTO agent_onboarding_markers
			(agent_id, chain_hash, created_at, updated_at, in_progress_until)
		VALUES (?, '', ?, ?, ?)
		ON CONFLICT (agent_id) DO UPDATE SET
			in_progress_until = excluded.in_progress_until,
			updated_at        = excluded.updated_at
		WHERE agent_onboarding_markers.in_progress_until IS NULL
		   OR agent_onboarding_markers.in_progress_until <= ?`,
		agentID, stamp, stamp, deadline, stamp)
	if err != nil {
		return Pass{}, fmt.Errorf("learning: claim onboarding pass for %s: %w", agentID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The write may well have landed. Reporting the claim as lost is
		// the fail-closed answer: at worst this seat waits a turn, where
		// claiming on an unknown result is how two passes run at once.
		return Pass{}, fmt.Errorf("learning: claim onboarding pass for %s: %w", agentID, err)
	}
	if n == 0 {
		return Pass{}, nil
	}
	return Pass{AgentID: agentID, Until: until}, nil
}

// Release gives the pass lease back early, so a pass that ended without
// marking does not hold re-onboarding hostage for the rest of the TTL.
//
// FENCED to the lease it took: a claimant that ran LONG looks dead, a second
// engine claims, and an unfenced release by the first would free the second's
// live lease (TestReleaseIsFencedToTheClaimItTook). Deadlines strictly
// increase, so Until is unique per claim.
//
// A mangled Until matches nothing and the lease runs to its TTL, the safe
// direction. The zero Pass is a no-op, so a skipper can release unconditionally.
func (o *Onboarding) Release(ctx context.Context, p Pass, at time.Time) error {
	if !p.Held() {
		return nil
	}
	if _, err := o.db.SQL().ExecContext(ctx, `
		UPDATE agent_onboarding_markers
		   SET in_progress_until = NULL, updated_at = ?
		 WHERE agent_id = ? AND in_progress_until = ?`,
		store.EncodeTime(at), p.AgentID, store.EncodeTime(p.Until),
	); err != nil {
		return fmt.Errorf("learning: release onboarding pass for %s: %w", p.AgentID, err)
	}
	return nil
}

// Get returns one seat's whole marker row, for operators and diagnostics.
//
// (Marker{}, false, nil) is "no row for this seat"; an error is the same
// unknown Onboarded returns, and callers must not read it as absence.
func (o *Onboarding) Get(ctx context.Context, agentID string) (Marker, bool, error) {
	if agentID == "" {
		return Marker{}, false, fmt.Errorf("learning: an onboarding lookup needs an agent id")
	}
	var (
		m                Marker
		created, updated int64
		lease            sql.NullInt64
	)
	err := o.db.SQL().QueryRowContext(ctx, `
		SELECT agent_id, chain_hash, agent_handle, role, summary,
		       created_at, updated_at, in_progress_until
		  FROM agent_onboarding_markers WHERE agent_id = ?`, agentID).
		Scan(&m.AgentID, &m.ChainHash, &m.Handle, &m.Role, &m.Summary,
			&created, &updated, &lease)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Marker{}, false, nil
	case err != nil:
		return Marker{}, false, fmt.Errorf("learning: read onboarding marker for %s: %w", agentID, err)
	}
	m.CreatedAt = store.DecodeTime(created)
	m.UpdatedAt = store.DecodeTime(updated)
	m.LeaseUntil = store.TimeAt(lease)
	return m, true, nil
}

// OnboardingPageTitle is the page every scope's conventions live on.
//
// One literal title across every scope rather than a per-unit naming scheme:
// an agent is told to look for "Onboarding" in each area of the knowledge
// base, which is a search a model can actually perform, where "the page named
// after your unit" is a rule it has to reconstruct and will get wrong.
const OnboardingPageTitle = "Onboarding"

// Hint is the instruction set an unmarked agent's onboarding pass is shown
// (internal/agent/runner's onboarding pass, under its `## What to do`).
//
// It enumerates the SCOPES on the seat's chain (the org, then each unit
// outermost-first) and leaves finding each page to the agent's search. Scope
// names, not page ids, stay valid across a reorganised knowledge base; an id
// goes stale silently into a 404.
//
// THE STEPS AND NOTHING ELSE: how the pass reaches its tools is the onboarding
// header's, and that `mark_onboarded` ends the pass is that tool's own
// description. This names the order (read, keep, mark) and the seat's own pages.
func Hint(o *org.Organization, r *org.Role) string {
	if o == nil || r == nil {
		return ""
	}
	var bullets []string
	add := func(scope string) {
		bullets = append(bullets, "- **"+scope+"**")
	}
	add(o.Name)
	for _, unit := range o.UnitChainFor(r) {
		add(unit.Name)
	}

	return "Read the page titled '" + OnboardingPageTitle + "' in each of these " +
		"areas of the team knowledge base, searching for it with your " +
		"knowledge-base tools and skipping an area that has none:\n" +
		strings.Join(bullets, "\n") + "\n\n" +
		"Keep each convention you should apply from now on with " +
		"`" + toolname.ReflectAndPersist + "`, as a declarative fact rather than an " +
		"instruction to yourself. Then call `" + toolname.MarkOnboarded + "` with a one-line " +
		"summary."
}
