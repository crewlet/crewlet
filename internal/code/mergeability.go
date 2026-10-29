package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S MERGEABILITY, RECORDED: whether one revision of a live change
// merges cleanly into the tip of the branch it targets — read by the
// repository's HOME git node, which merges the two commits in a scratch of its
// own (internal/code/home: `git merge-tree --write-tree`, whose answer is a
// function of the two commits alone) — and recorded on the change as a
// [Mergeability]: a record on the change's own subject ([KindChange]), which
// the repository's committing data node decides and publishes for the home,
// as it does a revision's facts ([Revise]). A git node never publishes it.
// Every reader of a change's mergeability after that — its page, its
// readiness — reads the row (code_mergeability), never git.
//
// # What it says
//
// CLEAN: the merge writes a tree and leaves no path conflicted. CONFLICT: it
// leaves paths conflicted, which the record names — the first that fit its
// bounds, [MaxConflictPaths] of them and [MaxConflictBytes] as JSON, each a
// path a record can carry ([ConflictPathCarried]) — and counts the rest.
// UNKNOWN: the home's merge ran out of its deadline, so nothing was decided;
// such a row is read again at its home's refresh (changesdue.go), as a moved
// target is, rather than left standing for a merge nobody tried again.
//
// A HISTORY THAT SHARES NO COMMIT WITH ITS TARGET'S merges from the empty tree,
// as its facts measure it ([Revise]'s empty base): its home merges with
// `--allow-unrelated-histories`, so every path both sides hold differently is
// a conflict and every other path merges, rather than a refusal no later
// record could ever move — a change is what its facts say it is.
//
// # The guard: monotone in the tip
//
// A mergeability is of the change's HEAD revision and head against its
// CURRENT target, or it is SUPERSEDED, recorded nowhere, as a revision's facts
// are. And for that revision against that branch it moves only forward: a
// read of a tip before the one the row holds — a home's pass that read before
// the target moved, slower than the one that read after — is superseded, and
// so is the same tip read again once the row is decided, since a merge of two
// commits is decided once. The one read of the same tip that is taken is an
// undecided row's: decided at last, or still undecided, when the record moves
// the instant the row was read at — so the next try waits its home's refresh,
// as a target's move does, rather than every pass.
//
// # Its tables
//
// code_mergeability, one row a change (replicated 0075), written by the
// records on the change's own subject and removed only with the change by its
// repository's purge. Its conflicted paths' bytes count in the change's
// conversation's text_bytes, as its revisions' facts do, so a purge part
// weighs them by that one narrow row.

// MergeState is what a change's head revision does merged into its target's
// tip (code_mergeability.state): a closed set ([MergeStates]).
//
// BASE FORMAT, under the condition [OpMergeability] states: a value only a
// mergeability record carries, so a build that knows the op knows the set.
type MergeState string

const (
	// MergeClean is a merge that leaves no path conflicted.
	MergeClean MergeState = "clean"
	// MergeConflict is a merge that leaves paths conflicted.
	MergeConflict MergeState = "conflict"
	// MergeUnknown is a merge its home could not decide within its
	// deadline: read again at its home's refresh.
	MergeUnknown MergeState = "unknown"
)

// MergeStates is the closed set, in this order.
func MergeStates() []MergeState { return []MergeState{MergeClean, MergeConflict, MergeUnknown} }

// Valid reports a state this build knows.
func (s MergeState) Valid() bool { return slices.Contains(MergeStates(), s) }

// Decided reports a merge that was decided — clean or in conflict — and so is
// the answer for its two commits once and for all.
func (s MergeState) Decided() bool { return s == MergeClean || s == MergeConflict }

// MaxConflictPaths is the most conflicted paths one mergeability names: a
// HUNDRED, as many as a person resolving a merge reads down a list of; past
// it the record counts the rest ([Mergeability.ConflictsOmitted]), since a
// merge that conflicts in more is one nobody resolves file by file.
const MaxConflictPaths = 100

// MaxConflictBytes is the most the conflicted paths may take AS JSON in a
// mergeability — the list's own encoding, as [ConflictsCost] measures it —
// past which, as past [MaxConflictPaths], the rest are counted.
//
// 16 KiB: a hundred paths of 160 bytes, twice what a source tree's paths run
// to, so a list of a hundred ordinary paths is never cut, and one of
// pathological ones — up to MaxGitPathBytes each, 400 KiB a hundred — keeps
// what fits. A third of [MaxTouchedBytes], because a change's conflicts are
// written again at every recheck its moving target asks, a record each of its
// home's refreshes while it conflicts, where its touched list is written once
// a revision; and a merge preview answers the same list, inside one git
// answer ([MaxGitAnswerBytes]).
const MaxConflictBytes = 16 << 10

// ConflictsCost is what a list of conflicted paths takes as JSON, as
// [MaxConflictBytes] bounds it: the list's own encoding, an empty list its two
// brackets.
func ConflictsCost(paths []string) int {
	if paths == nil {
		paths = []string{}
	}
	out, err := json.Marshal(paths)
	if err != nil {
		// A list of strings always encodes; reaching here is a broken
		// encoder, against which no list could be measured.
		panic(fmt.Sprintf("code: encoding a conflict list: %v", err))
	}
	return len(out)
}

// ConflictPathCarried reports whether a record can carry p as a conflicted
// path: valid UTF-8, which a record's JSON carries exactly, and a path a tree
// entry is named by ([GitPath]'s rule). A path git reports that is neither —
// a file name in another encoding — is counted, never listed.
func ConflictPathCarried(p string) bool {
	return utf8.ValidString(p) && validGitPath(p, false) == ""
}

// ConflictsHeld is a merge's conflicted paths as one record holds them, read
// as git reports them: the first that fit — at most [MaxConflictPaths], at
// most [MaxConflictBytes] as JSON, each one a record can carry
// ([ConflictPathCarried]) — and how many more there were. What the home's
// merge and a merge preview both hold, so the two answer one list.
type ConflictsHeld struct {
	Paths   []string
	Omitted int64
	// cost is what Paths takes as JSON, as ConflictsCost measures it, kept
	// as they are added.
	cost int
}

// Add holds p where it fits, and counts it where it does not. A path past
// the bounds is counted however it is spelled: a list is cut where it fills,
// never sorted to fit more.
func (h *ConflictsHeld) Add(p string) {
	if len(h.Paths) >= MaxConflictPaths || !ConflictPathCarried(p) {
		h.Omitted++
		return
	}
	quoted, err := json.Marshal(p)
	if err != nil {
		h.Omitted++
		return
	}
	cost := h.cost + len(quoted) + 1 // its comma, or a first path's closing bracket
	if h.cost == 0 {
		cost = 2 + len(quoted)
	}
	if cost > MaxConflictBytes {
		h.Omitted++
		return
	}
	h.Paths, h.cost = append(h.Paths, p), cost
}

// Mergeability records whether one revision of a change merges cleanly into
// its target's tip, as the repository's home git node merged the two commits.
//
// Every field is in the base format, under the condition [OpMergeability]
// states.
type Mergeability struct {
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// Revision is the revision merged, and HeadOID its head: the change's
	// head revision as the home read it, which the decide holds it to.
	Revision int64  `json:"revision"`
	HeadOID  string `json:"head_oid"`
	// TargetRef is the change's target branch, TargetOID the tip the head
	// was merged into and TargetPosition the position of the record that
	// moved the branch there.
	TargetRef      string            `json:"target_ref"`
	TargetOID      string            `json:"target_oid"`
	TargetPosition statelog.Position `json:"target_position"`
	// State is what the merge did. Conflicts is the paths it left
	// conflicted that the record holds, and ConflictsOmitted how many more
	// it left — each only on a conflict ([ConflictsHeld]).
	State            MergeState `json:"state"`
	Conflicts        []string   `json:"conflicts,omitempty"`
	ConflictsOmitted int64      `json:"conflicts_omitted,omitempty"`
	// Node is the git node that merged: the repository's home as it took
	// itself to be.
	Node string `json:"node"`
}

// validFields holds a mergeability to [ConversationWrite.Validate]'s rules: a
// revision from 1; whole object ids for the head and the target's tip; the
// target a branch a change may name, and its tip's position one on the code
// log; a state this build knows, with conflicts only beside a conflict — each
// a path a record carries, named once, within both bounds — and none counted
// beside any other state; and the node a node id config admits.
func (m *Mergeability) validFields() error {
	if m.Revision < 1 {
		return badField("mergeability.revision", "%d; a change's revisions count from 1", m.Revision)
	}
	for _, id := range []struct{ field, id string }{
		{"mergeability.head_oid", m.HeadOID}, {"mergeability.target_oid", m.TargetOID},
	} {
		if !gitref.ValidOID(id.id) || id.id == ZeroOID {
			return badField(id.field, "%q is not a whole object id", id.id)
		}
	}
	if why := branchProblem(m.TargetRef); why != "" {
		return badField("mergeability.target_ref", "%s", why)
	}
	switch p := m.TargetPosition; {
	case p.IsZero():
		return badField("mergeability.target_position", "the merge names the record that moved the target to the "+
			"tip it merged into")
	case p.Stream != topics.CodeLogStream:
		return badField("mergeability.target_position", "%s is a position on another log than the code log (%s)",
			p, topics.CodeLogStream)
	case p.Valid() != nil:
		return fmt.Errorf("%w: mergeability.target_position: %w", ErrInvalidProposal, p.Valid())
	}
	switch {
	case !m.State.Valid():
		return badField("mergeability.state", "%q is not one of %v", m.State, MergeStates())
	case m.State != MergeConflict && (len(m.Conflicts) > 0 || m.ConflictsOmitted != 0):
		return badField("mergeability.conflicts", "a %s merge leaves no path conflicted", m.State)
	case m.ConflictsOmitted < 0:
		return badField("mergeability.conflicts_omitted", "%d; a count of paths is never negative",
			m.ConflictsOmitted)
	case len(m.Conflicts) > MaxConflictPaths:
		return badField("mergeability.conflicts", "%d paths, past the %d one mergeability names "+
			"(MaxConflictPaths)", len(m.Conflicts), MaxConflictPaths)
	}
	if cost := ConflictsCost(m.Conflicts); cost > MaxConflictBytes {
		return badField("mergeability.conflicts", "the list is %d bytes of JSON, past the %d one mergeability "+
			"names (MaxConflictBytes)", cost, MaxConflictBytes)
	}
	named := make(map[string]bool, len(m.Conflicts))
	for i, p := range m.Conflicts {
		field := fmt.Sprintf("mergeability.conflicts[%d]", i)
		switch {
		case !ConflictPathCarried(p):
			return badField(field, "%q is no path a record carries: valid UTF-8 naming a tree entry", p)
		case named[p]:
			return badField(field, "%q is named twice", p)
		}
		named[p] = true
	}
	if !config.ValidNodeID(m.Node) {
		return badField("mergeability.node", "%q is not a node id config admits", m.Node)
	}
	return nil
}

// mergeRow is what a mergeability's decide and apply read of a change's row:
// the revision, branch and tip's position it is of, what it says, and what its
// wide column weighs.
type mergeRow struct {
	revision       int64
	targetRef      string
	targetPosition int64
	state          MergeState
	bytes          int64
}

// mergeabilityIn reads changeID's mergeability, and false where it holds none.
func mergeabilityIn(ctx context.Context, tx *sql.Tx, changeID string) (mergeRow, bool, error) {
	var (
		m     mergeRow
		state string
	)
	err := tx.QueryRowContext(ctx, mergeabilitySQL, changeID).Scan(&m.revision, &m.targetRef, &m.targetPosition,
		&state, &m.bytes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return mergeRow{}, false, nil
	case err != nil:
		return mergeRow{}, false, fmt.Errorf("code: read %s's mergeability: %w", changeID, err)
	}
	m.state = MergeState(state)
	return m, true, nil
}

// decideMergeability is a mergeability's rules over row — see this file's
// head: a LIVE change (change_closed, change_merged); its head revision and
// head and its current target (superseded); and, for that revision against
// that branch, a tip no earlier than the row's, and a later one or an
// undecided row where it is the same (superseded). Pure over the rows: it
// reads no chart.
func decideMergeability(ctx context.Context, tx *sql.Tx, w ConversationWrite, row changeRow, named string,
	d conversationDecision) (conversationDecision, error) {

	m := *w.Mergeability
	switch row.state {
	case ChangeStateClosed:
		return d.refuse(ConversationRefusalChangeClosed, fmt.Sprintf("%s is closed, and a closed change's "+
			"revisions are merged by nobody", named))
	case ChangeStateMerged:
		return d.refuse(ConversationRefusalChangeMerged, fmt.Sprintf("%s is merged, and a merged change's "+
			"revisions are merged no further", named))
	}
	switch {
	case m.Revision != row.revision || m.HeadOID != row.head:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s is at revision %d, its head %s, and the "+
			"merge is of revision %d at %s: the home merges the head revision", named, row.revision,
			short(row.head), m.Revision, short(m.HeadOID)))
	case m.TargetRef != row.target:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s merges into %s, and the merge is into %s: "+
			"the home merges into the change's target", named, row.target, m.TargetRef))
	}
	prior, held, err := mergeabilityIn(ctx, tx, row.id)
	if err != nil {
		return conversationDecision{}, err
	}
	if held && prior.revision == m.Revision && prior.targetRef == m.TargetRef {
		switch at := m.TargetPosition.Packed(); {
		case prior.targetPosition > at:
			return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("revision %d of %s is merged into %s at %s "+
				"already, a later tip than %s", m.Revision, named, m.TargetRef, unpacked(prior.targetPosition),
				m.TargetPosition))
		case prior.targetPosition == at && prior.state.Decided():
			return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("revision %d of %s is merged into %s at %s "+
				"already (%s), and a merge of two commits is decided once", m.Revision, named, m.TargetRef,
				m.TargetPosition, prior.state))
		}
	}
	d.payload = m
	// THE MERGEABILITY'S ROW, and the conversation's own.
	d.rows = 2
	return d, nil
}

// applyMergeability writes a change's mergeability ([Mergeability]) — its one
// row, replaced whole by every record the decide took — moving the change's
// conversation's text_bytes by what the conflicted paths weigh, less what the
// row they replace weighed, so a purge weighs the change's subject's wide rows
// by its one narrow row. The row's instant is the record's broker instant,
// which its home's next recheck is paced by (changesdue.go).
//
// PURE IN THE RECORD, its position, its broker instant and the prior row.
func applyMergeability(ctx context.Context, tx *sql.Tx, at applyContext, m Mergeability) (int, conversationCounts,
	error) {

	if !m.State.Valid() {
		return 0, conversationCounts{}, fmt.Errorf("code: the mergeability at %s says %q, which no writer forms",
			at.position, m.State)
	}
	prior, held, err := mergeabilityIn(ctx, tx, m.ChangeID)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	conflicts := strings.Join(m.Conflicts, "\x00")
	weight := int64(len(conflicts))
	if _, err := tx.ExecContext(ctx, writeMergeabilitySQL, m.ChangeID, m.Revision, m.TargetRef, m.TargetOID,
		m.TargetPosition.Packed(), string(m.State), len(m.Conflicts), m.ConflictsOmitted, m.Node, weight,
		at.packed, at.at(), conflicts); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: write %s's mergeability at %s: %w", m.ChangeID,
			at.position, err)
	}
	counts := conversationCounts{textBytes: weight}
	if held {
		counts.textBytes -= prior.bytes
	}
	return 1, counts, nil
}

// MergeabilityRow is a change's mergeability as its home recorded it
// (code_mergeability, [Mergeability]): the revision and the target's tip it
// merged — the branch, the tip and the position that moved it there — what
// the merge did, the conflicted paths it names and how many more there were,
// the git node that merged, and the record that wrote it with its broker
// instant. A change whose head revision's row is missing, or is of an earlier
// revision, is one its home has not merged yet; one whose target has moved
// since says so by its tip, which its home merges again at its refresh.
type MergeabilityRow struct {
	Revision         int64             `json:"revision"`
	TargetRef        string            `json:"target_ref"`
	TargetOID        string            `json:"target_oid"`
	TargetPosition   statelog.Position `json:"target_position"`
	State            MergeState        `json:"state"`
	Conflicts        []string          `json:"conflicts,omitempty"`
	ConflictsOmitted int64             `json:"conflicts_omitted,omitempty"`
	Node             string            `json:"node"`
	Position         statelog.Position `json:"position"`
	At               time.Time         `json:"at"`
}

// mergeabilityRowIn is changeID's mergeability as a read answers it, nil where
// it holds none — one seek of its key.
func mergeabilityRowIn(ctx context.Context, tx *sql.Tx, changeID string) (*MergeabilityRow, error) {
	var (
		m                           MergeabilityRow
		state, conflicts            string
		targetPosition, position, t int64
	)
	err := tx.QueryRowContext(ctx, mergeabilityRowSQL, changeID).Scan(&m.Revision, &m.TargetRef, &m.TargetOID,
		&targetPosition, &state, &m.ConflictsOmitted, &m.Node, &position, &t, &conflicts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("code: read %s's mergeability: %w", changeID, err)
	}
	m.State, m.TargetPosition, m.Position, m.At = MergeState(state), unpacked(targetPosition), unpacked(position),
		store.DecodeTime(t)
	if conflicts != "" {
		m.Conflicts = strings.Split(conflicts, "\x00")
	}
	return &m, nil
}

// MergePreviewQuery asks a repository's git nodes what merging one commit into
// another would do — a comparison's merge preview — answered by the same merge
// a change's home records its mergeability by (internal/code/home), in a
// scratch of the answering node's, never a cache. A read a principal makes:
// held to the asker's grant on the repository, and to the grant of the seat
// it reads for, by the node that answers, as a read by object id is
// ([GitQuery]).
//
// THE TWO COMMITS ARE WHOLE IDS, which their asker resolved from refs it was
// shown — the comparison's two sides — so the answering node merges exactly
// what was shown, and none of git's revision grammar reaches it.
type MergePreviewQuery struct {
	// RepoID is the repository's canonical id.
	RepoID string `json:"repo_id"`
	// Asker is who previews, REQUIRED, and CeilingSeat the seat a seat's turn
	// reads for, as on every read a principal makes ([GitQuery]).
	Asker       Principal `json:"asker"`
	CeilingSeat string    `json:"ceiling_seat,omitempty"`
	// Base is the commit merged into — the branch a change would target —
	// and Head the commit merged, both whole ids.
	Base string `json:"base"`
	Head string `json:"head"`
	// Fresh is the asker's floor on the code log, as a read by object id
	// takes it ([GitQuery.Fresh]).
	Fresh statelog.Freshness `json:"fresh"`
	// Home is stamped by the walk on the attempt it lets fill a copy, as
	// [GitQuery.Home] is.
	Home bool `json:"home,omitempty"`
}

// Validate refuses a preview no git node could answer ([ErrInvalid], naming
// the field): a repository id that is not one, an asker that is no principal
// or a ceiling its rule refuses, two ids that are not whole object ids, and a
// floor on another log than the code log.
func (q MergePreviewQuery) Validate() error {
	if !canonicalUUID(q.RepoID) {
		return fmt.Errorf("%w: repo_id: %q is not a repository's id", ErrInvalid, q.RepoID)
	}
	if why := q.Asker.invalid(); why != "" {
		return fmt.Errorf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	for _, id := range []struct{ field, id string }{{"base", q.Base}, {"head", q.Head}} {
		if !gitref.ValidOID(id.id) || id.id == ZeroOID {
			return fmt.Errorf("%w: %s: %q is not a whole commit id", ErrInvalid, id.field, id.id)
		}
	}
	if err := q.Fresh.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("%w: fresh: %w", ErrInvalid, err)
	}
	return nil
}

// MergePreviewAnswer is what merging Head into Base would do, as one git node
// merged them: what a change's mergeability records ([Mergeability]) — its
// state, the conflicted paths it holds and how many more there were — and the
// position the node's copy was read at.
type MergePreviewAnswer struct {
	RepoID           string            `json:"repo_id"`
	Base             string            `json:"base"`
	Head             string            `json:"head"`
	State            MergeState        `json:"state"`
	Conflicts        []string          `json:"conflicts,omitempty"`
	ConflictsOmitted int64             `json:"conflicts_omitted,omitempty"`
	Position         statelog.Position `json:"position"`
}

// A mergeability's statements, each registered in plan_test.go: the row by its
// key, a decide's and an apply's, and a read's whole; and its write by the
// same key, the row replaced whole by every record the decide took.
const (
	mergeabilitySQL = `SELECT revision, target_ref, target_position, state, bytes FROM code_mergeability
		WHERE change_id = ?`
	mergeabilityRowSQL = `SELECT revision, target_ref, target_oid, target_position, state, conflicts_omitted, node,
			position, at, conflicts
		FROM code_mergeability WHERE change_id = ?`
	writeMergeabilitySQL = `INSERT INTO code_mergeability (change_id, revision, target_ref, target_oid,
			target_position, state, conflicts_count, conflicts_omitted, node, bytes, position, at, conflicts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id) DO UPDATE SET revision = excluded.revision, target_ref = excluded.target_ref,
			target_oid = excluded.target_oid, target_position = excluded.target_position, state = excluded.state,
			conflicts_count = excluded.conflicts_count, conflicts_omitted = excluded.conflicts_omitted,
			node = excluded.node, bytes = excluded.bytes, position = excluded.position, at = excluded.at,
			conflicts = excluded.conflicts`
)
