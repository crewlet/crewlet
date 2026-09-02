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
)

// ChainHash is the stable identity of where a seat sits in the org: the
// company name, every unit above the seat outermost-first, then the seat's
// own role name.
//
// It is what an onboarding marker is stamped with, and it is the entire
// re-onboarding trigger. A seat that moves between teams, gains an ancestor
// unit, or whose division is renamed hashes differently; its stored marker
// stops matching, and the first-turn pass fires again for the structure the
// seat is actually in now. Content changes are deliberately NOT in it — an
// edited Onboarding page is the seat's own business to re-read, and hashing
// page content would re-run a full pass every time somebody fixed a typo.
//
// Renaming the seat itself needs no help from this hash: the agent id is
// derived from (org name, handle) through [org.DeriveAgentID], so a rename
// lands on a different row entirely and the old marker is simply orphaned.
//
// Empty for a nil org or seat. Both entry points that take a chain hash
// refuse an empty one, so a nil can never become a marker nothing matches.
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
		parts = append(parts, u.Name)
	}
	parts = append(parts, r.Name)
	return hashChain(parts)
}

// hashChain hashes the components as netstrings — each part's byte length, a
// colon, then the bytes.
//
// The separator-joined form (sha256 over strings.Join(parts, "\n")) is
// AMBIGUOUS, and not in theory: a company named "Acme\nOps" holding a
// root-level seat joins to exactly the same bytes as company "Acme" holding
// unit "Ops" holding that seat. Two different structures, one hash — so a
// seat lifted out of a unit keeps a marker claiming it already onboarded
// there and never re-reads. Nothing in the config layer forbids a newline in
// a name — measured: Organization.Validate accepts one, and
// TestJoiningTheChainWithASeparatorWouldCollide skips itself if that ever
// changes — and picking a rarer separator only moves the collision somewhere
// less likely to be found. Length prefixes are injective over arbitrary
// bytes, which retires the question instead of narrowing it.
//
// Changing this construction re-onboards every seat in the company once,
// exactly as a restructure would. That is the price of ever touching it.
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
	// AgentID is the DERIVED uuid (org name plus handle) — the same
	// identity the diary and episodes key on, not the handle.
	AgentID string

	// ChainHash is the chain the seat onboarded for, or empty on a row that
	// exists only because a pass was claimed. Empty never equals a real
	// [ChainHash], which is what makes a claimed-but-unmarked row read
	// correctly as "not onboarded".
	ChainHash string

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
// THREE answers, and the third one is the point:
//
//	(true, nil)   — a marker stamped with chainHash. Skip the pass.
//	(false, nil)  — definitively no matching marker: never marked, or the
//	                seat moved and the stamped chain no longer matches.
//	                Run the pass.
//	(false, err)  — UNKNOWN. The store did not answer, which says nothing
//	                at all about whether this seat onboarded. Skip the pass
//	                and ask again next turn.
//
// A caller that collapses the error into "not onboarded" re-runs a whole
// pass — minutes of LLM rounds re-reading pages the seat has already read —
// for every already-marked seat in the company on any store blip, against a
// one-turn delay of a hint if it skips instead. That asymmetry is the entire
// reason this returns an error rather than a bool.
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
// A held Pass means run the pass; the zero Pass means somebody else already
// is, so skip. FAILS CLOSED: an error yields the zero
// Pass, so a caller that only reads Held() still skips. Running a
// possibly-duplicate onboarding is the expensive mistake here — two engines
// re-reading the same pages and writing the same conventions — and not
// running one costs a turn.
//
// The claim is ONE statement, and that is a decision about what the LOSER is
// told. A read-then-write inside store.Tx is not unsafe — store.Tx begins
// DEFERRED, both claimants take their snapshot before either writes, and the
// second commit is refused rather than silently overwriting the first
// (measured, TestWhyTheClaimIsOneStatement: turso answers "database snapshot
// is stale"). But the loser
// learns it lost through an ERROR, and an error is the one thing this package
// keeps distinct from a definite answer. Both shapes skip the pass, so the
// difference never surfaces as a duplicate onboarding; it surfaces as a store
// that looks broken every time two turns race one seat. The conditional
// upsert evaluates its predicate under the write lock instead, so the loser
// gets (Pass{}, nil) — definitely somebody else's — and an error keeps
// meaning what it means everywhere else here.
//
// A claim for a never-marked seat INSERTS the row with an empty chain_hash,
// which Onboarded reads correctly as "not onboarded" — the lease and the
// marker share a row, and the row existing is not the same fact as the seat
// having onboarded.
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
// marking (the agent ran out of rounds, the turn failed) does not hold
// re-onboarding hostage for the rest of the TTL.
//
// FENCED to the lease it took, which is not decoration. The TTL exists to
// bound a claimant that DIED mid-pass — but a claimant that merely ran LONG
// is indistinguishable from a dead one, and it comes back: its lease lapses,
// a second engine claims and starts a pass, and then the first one finishes
// and releases. An unfenced clear frees the second engine's live lease and a
// third pass starts on top of it (measured — TestReleaseIsFencedToTheClaimItTook).
// Under a forward-moving clock the deadlines strictly increase, so the token
// is unique per claim: the second claim can only happen at or after the
// first deadline, which puts its own deadline a whole TTL later.
//
// A Pass whose Until has been mangled by the caller simply matches nothing
// and the lease runs to its TTL. That is the safe direction to fail.
//
// The zero Pass is a no-op, so a caller that skipped can still release
// unconditionally.
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

// Hint is the instruction set an unmarked agent is shown.
//
// It enumerates the SCOPES on the seat's chain — the org, then each unit from
// the outermost down to its own — and leaves finding each page to the agent's
// knowledge-base search. Naming scopes rather than page ids is what keeps the
// hint valid across a knowledge base that gets reorganised: an id goes stale
// silently and sends the agent to a 404, a scope name is what a search is for.
func Hint(o *org.Organization, r *org.Role) string {
	if o == nil || r == nil {
		return ""
	}
	var bullets []string
	add := func(scope string) {
		bullets = append(bullets, fmt.Sprintf("- '%s' page in the **%s** area of "+
			"the knowledge base", OnboardingPageTitle, scope))
	}
	add(o.Name)
	for _, unit := range o.UnitChainFor(r) {
		add(unit.Name)
	}

	return "You have not yet completed onboarding for this team configuration.\n\n" +
		"Before doing other work, read the following pages (each is a page in " +
		"your team knowledge base, literally titled '" + OnboardingPageTitle + "'):\n" +
		strings.Join(bullets, "\n") + "\n\n" +
		"How:\n" +
		"- Use your knowledge-base search tool (or a get-page tool if you know " +
		"the id) to locate each page. If a page is missing for some scope, " +
		"that's fine — skip it.\n" +
		"- For each page, capture conventions you should apply going forward " +
		"via `reflect_and_persist`. Write declarative facts, not instructions " +
		"to yourself.\n" +
		"- When you have read all available pages and persisted what matters, " +
		"call `mark_onboarded` with a one-line summary. After that, this " +
		"section disappears from your prompt.\n\n" +
		"If a page changes later you can re-read it any time — the onboarding " +
		"marker only re-fires when the org structure itself changes."
}
