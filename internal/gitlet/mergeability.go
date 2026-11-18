package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// A change's mergeability: whether one revision of a live change merges
// cleanly into its target's tip. The repository's home Gitlet node merges the
// two commits in a scratch of its own (internal/gitlet/home: `git merge-tree
// --write-tree`, a function of the two commits alone); the committing data
// node decides and publishes the [Mergeability] record on the change's own
// subject ([KindChange]), as it does a revision's facts ([Revise]). A Gitlet
// node never publishes it. Every later reader (the change's page, its
// readiness) reads the row (code_mergeability), never git.

// # What it says
//
// CLEAN: no path conflicted. CONFLICT: the record names the conflicted paths
// that fit ([ConflictsHeld]) and counts the rest. UNKNOWN: the home's merge ran
// out of its deadline; such a row is read again at its home's refresh
// (changesdue.go), as a moved target is, rather than left standing.
//
// A history sharing no commit with its target merges from the empty tree, as
// its facts measure it ([Revise]'s empty base): the home merges with
// `--allow-unrelated-histories`, so paths both sides hold differently conflict
// and the rest merge, rather than a refusal no later record could move.

// # The guard: monotone in the tip
//
// A mergeability is of the change's head revision and head against its
// current target, or it is SUPERSEDED, as a revision's facts are. For that
// revision and branch it only moves forward: a tip earlier than the row's (a
// slower pass that read before the target moved) is superseded, and so is the
// same tip once the row is decided, since a merge of two commits is decided
// once. The same tip is taken only over an undecided row, and moves the row's
// instant, so the next try waits its home's refresh rather than every pass.

// # The readiness it stamps
//
// Every record carries the change's READINESS, stamped by the writer: its
// target's merge gate ([JudgeGate]) judged for no merger in particular, over
// the decide's own snapshot and chart with the record's merge in place of the
// row's ([ReadinessRows.With]), since the home may have read a record behind.
// Beside it, the versions of the change's row and its conversation's readiness
// inputs, and the position of the latest record queuing or ending an attempt
// of the head's checks (checks.go): an input of its own, on the checks
// subject, that moves neither version. The apply stores them, judging nothing
// again: what a wake that a change became ready reads, and what the listing of
// the changes waiting on a person seeks.

// A REFRESH merges nothing: the home's answer to readiness inputs that moved
// since the row's answer was judged ([ChangeDue.RefreshDue]), recording the
// readiness again over the row's merge, which the writer stamps on it. It is
// SUPERSEDED where the change holds no merge of that revision against that
// branch (the home merges first, and the merge records the readiness), and
// where nothing it reads has moved. Its apply rewrites only the answer, its
// versions and its checks position, so the merge's instant, which paces the
// home's recheck of a moved target, stays the merge's.

// # Its tables
//
// code_mergeability, one row a change (replicated 0105; reshaped by 0106, 0117
// and 0129), written by the applier (apply_mergeability.go) from the records
// on the change's own subject, its answer cleared by the repository record that
// closes or merges the change ([clearReadiness]), and removed only with the
// change by its repository's purge. Its conflicted paths' bytes count in the
// conversation's text_bytes, as revisions' facts do, so a purge part weighs
// them by that one narrow row.

// MergeState is what a change's head revision does merged into its target's
// tip (code_mergeability.state): a closed set ([MergeStates]).
//
// BASE FORMAT, as [OpMergeability] is: a value only a mergeability record
// carries, so a build that knows the op knows the set.
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
// HUNDRED, as many as a person reads down a list of; past it the record counts
// the rest ([Mergeability.ConflictsOmitted]), since a merge conflicting in
// more is one nobody resolves file by file.
const MaxConflictPaths = 100

// MaxConflictBytes bounds the conflicted paths AS JSON in a mergeability
// ([ConflictsCost]); past it, as past [MaxConflictPaths], the rest are counted.
//
// 16 KiB: a hundred paths of 160 bytes, twice what a source tree's paths run
// to, so a hundred ordinary paths are never cut, while pathological ones (up
// to MaxGitPathBytes each) keep what fits. A third of [MaxTouchedBytes],
// because conflicts are written again at every recheck while the target moves,
// where the touched list is written once a revision; and a merge preview
// answers the same list inside one git answer ([MaxGitAnswerBytes]).
const MaxConflictBytes = 16 << 10

// ConflictsCost is what a list of conflicted paths takes as JSON, as
// [MaxConflictBytes] bounds it: an empty list is its two brackets.
func ConflictsCost(paths []string) int {
	if paths == nil {
		paths = []string{}
	}
	out, err := json.Marshal(paths)
	if err != nil {
		// A list of strings always encodes; only a broken encoder lands here.
		panic(fmt.Sprintf("gitlet: encoding a conflict list: %v", err))
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

// ConflictsHeld is a merge's conflicted paths as one record holds them, in
// git's order: the first that fit ([MaxConflictPaths], [MaxConflictBytes],
// [ConflictPathCarried]) and how many more there were. The home's merge and a
// merge preview both hold it, so the two answer one list.
type ConflictsHeld struct {
	Paths   []string
	Omitted int64
	// cost is Paths' JSON size ([ConflictsCost]), kept as paths are added.
	cost int
}

// Add holds p where it fits, and counts it where it does not or cannot be
// carried: a list is cut where it fills, never sorted to fit more.
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
// its target's tip, as the repository's home Gitlet node merged the two commits.
//
// Every field is in the base format, as [OpMergeability] is.
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
	// Node is the Gitlet node that merged, the home as it took itself to be;
	// on a refresh, the home that asked, and the apply leaves the row naming
	// the node that merged.
	Node string `json:"node"`
	// Refresh marks a record of the change's READINESS alone over the row's
	// merge (this file's head), running no merge. Its revision, head and
	// target name the merge refreshed; the writer STAMPS the tip, its position
	// and the state from the row, and the row keeps its conflicted paths.
	//
	// BASE FORMAT, as [OpMergeability] is.
	Refresh bool `json:"refresh,omitempty"`
	// ChangeVersion and ReadinessVersion are the versions of the change's row
	// (code_changes.version) and its conversation's readiness inputs
	// (code_conversations.readiness_version) the decide judged at, and Ready
	// and WaitsPerson its answer ([Readiness.Ready], [Readiness.WaitsPerson])
	// over its own snapshot and chart with this record's merge. STAMPED BY THE
	// WRITER (the home sends none), so the apply judges nothing again.
	//
	// BASE FORMAT, as [OpMergeability] is.
	ChangeVersion    int64 `json:"change_version,omitempty"`
	ReadinessVersion int64 `json:"readiness_version,omitempty"`
	Ready            bool  `json:"ready,omitempty"`
	WaitsPerson      bool  `json:"waits_person,omitempty"`
	// ChecksPosition is the latest position at which a record queued or ended
	// an attempt of the head's checks for the change's target, as the decide
	// read code_checks ([headChecksPositionIn]), zero where none was queued:
	// the third input M4-D24's guard holds a refresh to, since the checks
	// subject moves neither version. STAMPED BY THE WRITER.
	//
	// BASE FORMAT, as [OpMergeability] is.
	ChecksPosition statelog.Position `json:"checks_position,omitzero"`
	// ChartEpoch is the activation of the chart the decide judged by
	// (configplane.ActivationStamp: [SeatRules.Epoch]), 0 for none known: the
	// fourth input M4-D24's guard holds a refresh to, since the chart decides
	// who may approve and clear a blocker and no log record moves it; a home
	// holding a later chart refreshes the change ([ChangesDueQuery.ChartEpoch]).
	// STAMPED BY THE WRITER.
	//
	// BASE FORMAT, as [OpMergeability] is.
	ChartEpoch int64 `json:"chart_epoch,omitempty"`
	// Routing is whom the record concerns ([ChangeRouting]): the authors where
	// it TURNED the merge into a conflict, the owner where the answer turned
	// ready. STAMPED BY THE WRITER from the row it replaces.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// MergeabilityKept is a re-evaluation whose OUTCOME the row holds already
// (state, conflicted paths and their count, readiness, waits on a person),
// recorded by what it MOVED alone: a recheck's new tip, and the versions,
// checks position and chart a readiness was judged at. decideMergeability
// forms it in place of the [Mergeability] the home sent, so a recheck of a
// fast-moving target, or a refresh that changed nothing, does not rewrite the
// whole record, conflicts up to [MaxConflictBytes] included. A TRANSITION is
// always the full record, since it is what somebody is told of. It turns
// nothing, so it concerns nobody: no routing, and [RoutingsOf] names none.
//
// BASE FORMAT, as [OpMergeability] is.
type MergeabilityKept struct {
	// RepoID stays on the record because the deletion gate reads a change's
	// repository off it once a purge has removed the change's row
	// (apply_conversation.go's conversationNotApplied).
	RepoID   string `json:"repo_id"`
	ChangeID string `json:"change_id"`
	// Revision is the revision of the row's merge that was re-evaluated.
	Revision int64 `json:"revision"`
	// TargetOID and TargetPosition are the tip a RECHECK merged into and the
	// position that moved the target there; both empty on a refresh's.
	TargetOID      string            `json:"target_oid,omitempty"`
	TargetPosition statelog.Position `json:"target_position,omitzero"`
	// ChangeVersion, ReadinessVersion, ChecksPosition and ChartEpoch are what
	// the readiness was judged at ([Mergeability.ChangeVersion],
	// [Mergeability.ChartEpoch]): all four set where any moved past the row's,
	// and ChangeVersion zero where none did (a row is at version 1 from its
	// open, so only its zero names no judgement). THE CHART RIDES WITH THE
	// VERSIONS because a configuration apply alone makes a change due
	// (changes_due's chart term): a kept answer under a later chart must move
	// the row's chart, or the change stays due at every pass; and moved alone,
	// with ChangeVersion zero, it would read as no judgement.
	//
	// ChartEpoch is BASE FORMAT, as [OpMergeability] is.
	ChangeVersion    int64             `json:"change_version,omitempty"`
	ReadinessVersion int64             `json:"readiness_version,omitempty"`
	ChecksPosition   statelog.Position `json:"checks_position,omitzero"`
	ChartEpoch       int64             `json:"chart_epoch,omitempty"`
}

// keptOutcome reports whether m, stamped with its readiness, says what prior
// says: the outcome a [MergeabilityKept] keeps. A refresh's merge is the
// row's by construction, so only its readiness is compared.
func keptOutcome(m Mergeability, prior mergeRow) bool {
	if m.Ready != prior.ready || m.WaitsPerson != prior.waitsPerson {
		return false
	}
	return m.Refresh || (m.State == prior.state && m.ConflictsOmitted == prior.conflictsOmitted &&
		slices.Equal(m.Conflicts, prior.conflicts))
}

// validFields holds a mergeability to [ConversationWrite.Validate]'s rules: a
// revision from 1, a whole head id and a target branch a change may name; for
// a merge, a whole tip id at a code-log position, a known state, and conflicts
// only beside a conflict, each carried, named once and within both bounds; for
// a refresh, no tip, position, state or conflict (the row stamps or keeps
// them); never the readiness the writer stamps; and a node id config admits.
func (m *Mergeability) validFields() error {
	if m.Revision < 1 {
		return badField("mergeability.revision", "%d; a change's revisions count from 1", m.Revision)
	}
	if !gitref.ValidOID(m.HeadOID) || m.HeadOID == ZeroOID {
		return badField("mergeability.head_oid", "%q is not a whole object id", m.HeadOID)
	}
	if why := branchProblem(m.TargetRef); why != "" {
		return badField("mergeability.target_ref", "%s", why)
	}
	if m.ChangeVersion != 0 || m.ReadinessVersion != 0 || m.Ready || m.WaitsPerson || !m.ChecksPosition.IsZero() ||
		m.ChartEpoch != 0 {
		return badField("mergeability.ready", "the writer stamps the change's readiness and the versions, the "+
			"checks position and the chart it judged it at, from its own snapshot and epoch, and the home sends "+
			"none of them")
	}
	if err := stampedRouting("mergeability", m.Routing); err != nil {
		return err
	}
	if m.Refresh {
		switch {
		case m.TargetOID != "" || !m.TargetPosition.IsZero() || m.State != "":
			return badField("mergeability.refresh", "a refresh names the revision, the head and the target of "+
				"the merge it refreshes, and the writer stamps the tip, its position and the state from the "+
				"change's row: the home sends none of them")
		case len(m.Conflicts) > 0 || m.ConflictsOmitted != 0:
			return badField("mergeability.conflicts", "a refresh names no conflicted path: the change's row "+
				"keeps its own")
		case !config.ValidNodeID(m.Node):
			return badField("mergeability.node", "%q is not a node id config admits", m.Node)
		}
		return nil
	}
	if !gitref.ValidOID(m.TargetOID) || m.TargetOID == ZeroOID {
		return badField("mergeability.target_oid", "%q is not a whole object id", m.TargetOID)
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
		return badField("mergeability.state", "%q is not one of %s", m.State, words.Or(MergeStates()))
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

// stampedMergeabilityBytes is what the writer stamps on m at its widest: the
// readiness, versions, checks position and chart, and for a refresh the row's
// tip, position and state. EXACT (the stamped encoding less m's own), so a
// record is held to [MaxConversationBytes] as the writer will publish it.
func stampedMergeabilityBytes(m Mergeability) int {
	widest := m
	widest.ChangeVersion, widest.ReadinessVersion, widest.ChartEpoch = math.MaxInt64, math.MaxInt64, math.MaxInt64
	widest.Ready, widest.WaitsPerson = true, true
	widest.ChecksPosition = statelog.Position{Stream: topics.CodeLogStream, Generation: math.MaxUint32,
		Seq: math.MaxUint64}
	if m.Refresh {
		widest.TargetOID = strings.Repeat("f", gitref.OIDHexLen)
		widest.TargetPosition = statelog.Position{Stream: topics.CodeLogStream, Generation: math.MaxUint32,
			Seq: math.MaxUint64}
		for _, s := range MergeStates() {
			if len(s) > len(widest.State) {
				widest.State = s
			}
		}
	}
	stamped, err := json.Marshal(widest)
	if err != nil {
		panic(fmt.Sprintf("gitlet: encoding a mergeability: %v", err))
	}
	sent, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("gitlet: encoding a mergeability: %v", err))
	}
	return len(stamped) - len(sent)
}

// mergeRow is what a mergeability's decide and apply read of a change's row.
type mergeRow struct {
	revision         int64
	targetRef        string
	targetOID        string
	targetPosition   int64
	state            MergeState
	bytes            int64
	changeVersion    int64
	readinessVersion int64
	checksPosition   int64
	chartEpoch       int64
	// ready is the stored answer the record's own is announced against: not
	// ready to ready is the one transition that tells anybody
	// ([ReasonReadyToMerge]). The rest is the outcome [MergeabilityKept] keeps.
	ready, waitsPerson bool
	conflictsOmitted   int64
	conflicts          []string
}

// mergeabilityIn reads changeID's mergeability, and false where it holds none.
func mergeabilityIn(ctx context.Context, tx *sql.Tx, changeID string) (mergeRow, bool, error) {
	var (
		m                mergeRow
		state, conflicts string
	)
	err := tx.QueryRowContext(ctx, mergeabilitySQL, changeID).Scan(&m.revision, &m.targetRef, &m.targetOID,
		&m.targetPosition, &state, &m.bytes, &m.changeVersion, &m.readinessVersion, &m.checksPosition,
		&m.chartEpoch, &m.ready, &m.waitsPerson, &m.conflictsOmitted, &conflicts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return mergeRow{}, false, nil
	case err != nil:
		return mergeRow{}, false, fmt.Errorf("gitlet: read %s's mergeability: %w", changeID, err)
	}
	m.state = MergeState(state)
	if conflicts != "" {
		m.conflicts = strings.Split(conflicts, "\x00")
	}
	return m, true, nil
}

// decideMergeability is a mergeability's rules over row (this file's head): a
// LIVE change (change_closed, change_merged); its head revision, head and
// current target (superseded); for a merge, a tip no earlier than the row's
// for that revision and branch, and an undecided row where it is the same
// (superseded); for a refresh, the row's merge of that revision and branch,
// and readiness inputs moved or a chart later than the row's (superseded).
// Then the readiness, judged over the snapshot with the record's merge and the
// chart roster, stamped with what it was judged at. Pure over (the rows, w,
// roster, epoch).
func decideMergeability(ctx context.Context, tx *sql.Tx, w ConversationWrite, roster Roster, epoch int64,
	repo RepoRef, row changeRow, named string, d conversationDecision) (conversationDecision, error) {

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
	// THE HEAD'S CHECKS AS THIS SNAPSHOT HOLDS THEM, which is why the record's
	// scope names the repository's checks (changewriter.go): a node holding a
	// check record it could not decode decides no mergeability.
	checksAt, err := headChecksPositionIn(ctx, tx, repo.ID, row.head, row.target)
	if err != nil {
		return conversationDecision{}, err
	}
	of := held && prior.revision == m.Revision && prior.targetRef == m.TargetRef
	switch at := m.TargetPosition.Packed(); {
	case m.Refresh && !of:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s holds no merge of revision %d into %s to "+
			"refresh: its home merges it, and the merge records its readiness", named, m.Revision, m.TargetRef))
	case m.Refresh && prior.changeVersion == row.version && prior.readinessVersion == d.readiness &&
		prior.checksPosition == checksAt && prior.chartEpoch >= epoch:
		// A CHART NO LATER THAN THE ROW'S moves nothing either: an older one
		// (a node behind the activation) would store an answer the later
		// chart does not give.
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("%s's readiness is judged at its row's "+
			"version %d, its conversation's readiness version %d, its head's checks at %s and the chart of "+
			"activation %d already, and nothing it reads has moved since", named, row.version, d.readiness,
			unpacked(checksAt), prior.chartEpoch))
	case m.Refresh:
		// THE MERGE IT REFRESHES, stamped from the row: the record names what
		// it judged over, and its apply touches no merge.
		m.TargetOID, m.TargetPosition, m.State = prior.targetOID, unpacked(prior.targetPosition), prior.state
	case of && prior.targetPosition > at:
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("revision %d of %s is merged into %s at %s "+
			"already, a later tip than %s", m.Revision, named, m.TargetRef, unpacked(prior.targetPosition),
			m.TargetPosition))
	case of && prior.targetPosition == at && prior.state.Decided():
		return d.refuse(ConversationRefusalSuperseded, fmt.Sprintf("revision %d of %s is merged into %s at %s "+
			"already (%s), and a merge of two commits is decided once", m.Revision, named, m.TargetRef,
			m.TargetPosition, prior.state))
	}
	// THE READINESS, from THIS snapshot with the record's merge: the home may
	// have read a record behind, and an answer the record's own apply would
	// not leave true is no answer to store. Judged for NO MERGER, since a wake
	// that the change became ready reads it for whoever merges.
	facts, err := gateFactsIn(ctx, tx, []readinessSubject{{repo: repo, row: row}})
	if err != nil {
		return conversationDecision{}, err
	}
	f := facts[0]
	f.Rows, f.Roster = f.Rows.With(m), roster
	gate, err := f.Gate()
	if err != nil {
		return conversationDecision{}, err
	}
	readiness := JudgeGate(f, gate)
	m.ChangeVersion, m.ReadinessVersion, m.ChartEpoch = row.version, d.readiness, epoch
	if checksAt > 0 {
		m.ChecksPosition = unpacked(checksAt)
	}
	m.Ready, m.WaitsPerson = readiness.Ready(), readiness.WaitsPerson()
	if of && keptOutcome(m, prior) {
		// THE OUTCOME THE ROW HOLDS ALREADY ([MergeabilityKept]): recorded by
		// what moved, a chart alone included, since only a moved chart ends
		// the due a configuration apply makes. It turns nothing, so
		// routeConversation stamps it no routing.
		kept := MergeabilityKept{RepoID: m.RepoID, ChangeID: m.ChangeID, Revision: m.Revision}
		if !m.Refresh {
			kept.TargetOID, kept.TargetPosition = m.TargetOID, m.TargetPosition
		}
		if m.ChangeVersion != prior.changeVersion || m.ReadinessVersion != prior.readinessVersion ||
			checksAt != prior.checksPosition || m.ChartEpoch != prior.chartEpoch {
			kept.ChangeVersion, kept.ReadinessVersion, kept.ChecksPosition, kept.ChartEpoch = m.ChangeVersion,
				m.ReadinessVersion, m.ChecksPosition, m.ChartEpoch
		}
		d.op, d.payload = OpMergeabilityKept, kept
		// THE MERGEABILITY'S ROW, and the conversation's own.
		d.rows = 2
		return d, nil
	}
	// WHOM THE RECORD CONCERNS, by what it turns on the row it replaces: a
	// merge newly in conflict tells the authors once, not at every recheck;
	// an answer gone from not ready to ready tells the authors who may merge
	// it, else the lead of the repository's owner. THIS RECORD ALONE announces
	// the change ready (concerns.go's head), since its decide holds the stored
	// answer it replaces: a passed check reaches it through the home's refresh.
	d.concern = concerning{conflicted: m.State == MergeConflict && (!held || prior.state != MergeConflict)}
	d.concern.authors = d.concern.conflicted
	if m.Ready && (!held || !prior.ready) {
		d.concern.readyFor = mergingAuthors(f, gate)
		if len(d.concern.readyFor) == 0 {
			if lead := ownersLead(f.Rows.Repo, roster); lead.Handle != "" {
				d.concern.readyFor = []Reviewer{lead}
			}
		}
	}
	d.payload = m
	// THE MERGEABILITY'S ROW, and the conversation's own.
	d.rows = 2
	return d, nil
}

// MergeabilityRow is a change's mergeability as its home recorded it
// (code_mergeability, [Mergeability]): the merge, the node that merged and the
// record that merged with its broker instant, and the readiness the last
// record judged with what it was judged at, which a read judges again rather
// than trusts ([ChangeSummary.Readiness]). A missing row, or one of an earlier
// revision, is a head its home has not merged yet; a tip the target has moved
// past is one its home merges again at its refresh.
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
	ChangeVersion    int64             `json:"change_version"`
	ReadinessVersion int64             `json:"readiness_version"`
	ChecksPosition   statelog.Position `json:"checks_position,omitzero"`
	ChartEpoch       int64             `json:"chart_epoch,omitempty"`
	Ready            bool              `json:"ready,omitempty"`
	WaitsPerson      bool              `json:"waits_person,omitempty"`
}

// mergeabilityRowIn is changeID's mergeability as a read answers it, nil where
// it holds none — one seek of its key.
func mergeabilityRowIn(ctx context.Context, tx *sql.Tx, changeID string) (*MergeabilityRow, error) {
	var (
		m                                   MergeabilityRow
		state, conflicts                    string
		targetPosition, position, t, checks int64
	)
	err := tx.QueryRowContext(ctx, mergeabilityRowSQL, changeID).Scan(&m.Revision, &m.TargetRef, &m.TargetOID,
		&targetPosition, &state, &m.ConflictsOmitted, &m.Node, &position, &t, &m.ChangeVersion,
		&m.ReadinessVersion, &checks, &m.ChartEpoch, &m.Ready, &m.WaitsPerson, &conflicts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("gitlet: read %s's mergeability: %w", changeID, err)
	}
	m.State, m.TargetPosition, m.Position, m.At = MergeState(state), unpacked(targetPosition), unpacked(position),
		store.DecodeTime(t)
	if checks > 0 {
		m.ChecksPosition = unpacked(checks)
	}
	if conflicts != "" {
		m.Conflicts = strings.Split(conflicts, "\x00")
	}
	return &m, nil
}

// MergePreviewQuery asks a repository's Gitlet nodes what merging one commit
// into another would do (a comparison's merge preview), answered by the same
// merge a change's home records (internal/gitlet/home), in a scratch of the
// answering node's, never a cache. A principal's read, held by the answering
// node to the asker's grant and the seat it reads for, as [GitQuery] is.
//
// THE TWO COMMITS ARE WHOLE IDS the asker resolved from the refs it was shown,
// so the node merges exactly what was shown and no revision grammar reaches git.
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

// Validate refuses a preview no Gitlet node could answer ([ErrInvalid], naming
// the field): a repository id that is not one, an asker that is no principal
// or a ceiling its rule refuses, two ids that are not whole object ids, and a
// floor on another log than the code log.
func (q MergePreviewQuery) Validate() error {
	if !canonicalUUID(q.RepoID) {
		return refusedf("%w: repo_id: %q is not a repository's id", ErrInvalid, q.RepoID)
	}
	if why := q.Asker.invalid(); why != "" {
		return refusedf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	for _, id := range []struct{ field, id string }{{"base", q.Base}, {"head", q.Head}} {
		if !gitref.ValidOID(id.id) || id.id == ZeroOID {
			return refusedf("%w: %s: %q is not a whole commit id", ErrInvalid, id.field, id.id)
		}
	}
	if err := q.Fresh.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("%w: fresh: %w", ErrInvalid, err)
	}
	return nil
}

// MergePreviewAnswer is what merging Head into Base would do, as one Gitlet node
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

// headChecksPositionIn is the latest position at which a record queued or
// ended an attempt of a check of commit oid in repoID FOR TARGET, 0 where
// none: what a stored readiness was judged at ([Mergeability.ChecksPosition]).
// Only the target's attempts count, the ones its merge gate judges
// (gatejudge.go): another branch's attempts at a shared head move nothing.
func headChecksPositionIn(ctx context.Context, tx *sql.Tx, repoID, oid, target string) (int64, error) {
	var at int64
	if err := tx.QueryRowContext(ctx, headChecksPositionSQL, repoID, oid, target).Scan(&at); err != nil {
		return 0, fmt.Errorf("gitlet: read the position of %s's checks for %s: %w", oid, target, err)
	}
	return at, nil
}

// A mergeability's reads, each registered in plan_test.go: the row by its key
// (a decide's and an apply's, and a read's whole), and a head's checks
// position, a range of code_checks' key on the commit and target bounded by
// MaxChecksPerFile checks of MaxCheckAttempts each. Its write is
// apply_mergeability.go's.
const (
	mergeabilitySQL = `SELECT revision, target_ref, target_oid, target_position, state, bytes, change_version,
			readiness_version, checks_position, chart_epoch, ready, waits_person, conflicts_omitted, conflicts
		FROM code_mergeability WHERE change_id = ?`
	mergeabilityRowSQL = `SELECT revision, target_ref, target_oid, target_position, state, conflicts_omitted, node,
			position, at, change_version, readiness_version, checks_position, chart_epoch, ready, waits_person,
			conflicts
		FROM code_mergeability WHERE change_id = ?`
	headChecksPositionSQL = `SELECT COALESCE(MAX(CASE WHEN queued_position > finished_position
			THEN queued_position ELSE finished_position END), 0)
		FROM code_checks INDEXED BY ` + checksKey + ` WHERE repo_id = ? AND commit_oid = ? AND target_ref = ?`
)
