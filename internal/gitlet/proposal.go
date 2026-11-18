package gitlet

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A proposal carries EVERY command the client sent: those the receiver already
// refused arrive ng with their reason, the committer decides the rest, and the
// record states each, so a push row is the whole answer the client was given.

// Proposal is one push, decided by the repository's committer.
type Proposal struct {
	// PushID is minted by the receiving node (a UUIDv7) and stable across a
	// failover: it is what collapses a push two committers were asked to
	// decide onto the one row it applied as.
	PushID    string    `json:"push_id"`
	RepoID    string    `json:"repo_id"`
	Principal Principal `json:"principal"`
	// Via is how the push's pack was built (the relay's git or the engine's
	// own), stamped by its builder and carried into the entry and the push row,
	// never read from a commit. It rides the wire in the record's base format
	// ([PushEntry.Via]).
	Via PushVia `json:"via"`
	// Node is the node that received the push.
	Node   string `json:"node"`
	Atomic bool   `json:"atomic,omitempty"`
	// Commands is ALL the commands the client sent; the ones the receiver
	// refused carry ResultNG and their reason, the rest no result yet.
	Commands []Command `json:"commands"`
	// Pack is the bundle the push stored, nil when it carried no objects.
	Pack            *PackRef `json:"pack,omitempty"`
	Introduced      []string `json:"introduced,omitempty"`
	IntroducedCount int      `json:"introduced_count,omitempty"`
	// Offered is an attempt's count of the commits its refused commands offered, made by its
	// receiver ([PushEntry.Offered]); 0 on every other proposal.
	Offered    int       `json:"offered,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	// Seen is the code log position the receiving node synced to before
	// receive-pack ran, the state the client's commands and the thin pack's
	// bases were read at; zero for a receiver that read none. The committer
	// DECIDES FROM A SNAPSHOT AT OR PAST IT ([Committer]): it may be behind the
	// receiver, and from below would refuse as stale or repo_deleted a command
	// whose old value the client was shown, again after every fetch. A
	// PER-PROPOSAL floor, never the node's, and never recorded.
	Seen statelog.Position `json:"seen,omitzero"`
	// DropEpoch is the epoch of the objects of the copy receive-pack checked the
	// push over (internal/gitlet/cache's Synced.DropEpoch): its synced row's
	// ([RepoRef.DropEpoch]) once the copy holds every pack the rows place, the
	// epoch before while an expunge's begin's inputs are still among its live
	// objects. The committer refuses the push `expunging` WHOLE where the
	// repository's epoch is another ([RefusalExpunging]): an expunge begun since
	// may have left dropping an object that copy held live, which a ref landed
	// onto would name past the expunge's commit retiring it. The
	// copy's sync reads every page at one epoch (Reader.Snapshot). Never recorded.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// Change is the change the push's options ask for ([ParseChangeOptions]) as
	// its receiver read them, the title filled from the subject of the commit the
	// push sets its source to where the options named none; nil for none. It
	// rides only beside exactly one command setting a branch ([ChangeSource]).
	// Its decide opens it in the push's entry ([PushEntry.Change]), names the
	// live change its source already heads, or answers why it opened none
	// ([PushAnswer.Change]); it never refuses the push. It rides the wire in the
	// record's base format.
	Change *ChangeOption `json:"change,omitempty"`
	// WrittenFor is whom a SEAT'S push is made for (ADR-0109), as
	// [ChangeProposal.WrittenFor] is: whoever the turn asking an engine write works
	// for ([EngineWrite.WrittenFor]), or nobody stated for a seat-only push, a
	// coding box's among them; a person's and the expiry's state nothing. Held to
	// the principal by [writtenForInvalid], its builder having held the ceiling.
	//
	// A proposal is no record, so it takes no versionedFields row; the entry its
	// committer stamps does ([PushEntry.WorkingFor], [Proposal.entryWith]). A
	// build without the field, or not knowing its kind or an app, refuses a push
	// carrying somebody, naming args.written_for (.kind, .apps[]), so the walk
	// asks another ([unknownToThisBuild]); one made for nobody sends no member,
	// and an older build's arrives stated nobody ([Proposal.UnmarshalJSONFrom]).
	WrittenFor types.WrittenFor `json:"written_for,omitzero"`
}

// ProposeResult is what became of one proposal.
type ProposeResult struct {
	// Outcome is applied, pending or unknown for a push in a record, an
	// attempt's among them ([Attempted]), and empty for one answered with no
	// record of its own: every command noop or ng and no attempt, or a refusal
	// of the whole push. A push answered from an earlier record's row is
	// applied, its Answer Collapsed.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Position is where the push's record landed: the row's own for an applied
	// push, the record's for a pending one, and for a not_applied refusal, or a
	// repo_deleted one the code log made after the append, the batch's own
	// record, which applies nowhere. Never another operation's record
	// (statelog.Unavailable.Landed), which would tell a client its push was
	// durable there.
	Position statelog.Position `json:"position,omitzero"`
	// OpID is the batch's operation id, which an unknown outcome names.
	OpID   string     `json:"op_id,omitempty"`
	Answer PushAnswer `json:"answer"`
}

// PushAnswer is a push's commands as the client is told them.
type PushAnswer struct {
	PushID string `json:"push_id"`
	// Commands is AUTHORITATIVE for an applied push — read from its row —
	// and for one answered with no record; for a pending or unknown one it
	// is the decision's, tentative until the record resolves.
	Commands []Command `json:"commands"`
	// Refused is a refusal of the whole push — busy, compaction_behind,
	// push_quota, expunging, too_many_changes, unavailable, not_applied,
	// repo_deleted, repo_archived — with every command it covers ng under the
	// same code.
	Refused Refusal `json:"refused,omitempty"`
	// Detail is a whole-push refusal's one sentence of specifics, and every
	// command the refusal covers carries the SAME sentence as its Detail,
	// whichever path (the decide or the committer) formed the refusal, so a
	// reader of the answer or of a command finds it. A command the receiver had
	// already refused keeps its own. Empty when Refused is.
	Detail string `json:"detail,omitempty"`
	// Collapsed is a push answered from a row an EARLIER record applied: a
	// forwarded or failed-over proposal of a push that had already landed.
	Collapsed bool `json:"collapsed,omitempty"`
	// Change is what the push's change came to ([Proposal.Change]), nil for a
	// push that asked for none, one no record carries, and one whose answer
	// cannot say. For an applied push, the change it opened where the rows hold
	// one of its id ([PushChangeID]), the one fact they hold whichever round
	// decided the record, else the decision's: the live change its source headed
	// or why it opened none. For a pending or unknown push, the decision's,
	// tentative as its commands are.
	Change *ChangeAnswer `json:"change,omitempty"`
}

// maxDetailBytes is the longest Detail a receiver may fill in on a command it
// refused: one sentence naming NO REF (git prints it beside the reason), with
// at most one protection's pattern, as long as a ref (MaxRefNameBytes), plus
// room for the longest sentence's words ([ProtectionDetail]'s, 61 bytes). A
// test holds every sentence Gitlet writes into a proposal under it.
const maxDetailBytes = MaxRefNameBytes + 64

// Validate refuses a proposal this domain cannot decide, wrapping
// [ErrInvalidProposal] and naming the field: the receiver forms proposals, so
// a refusal here is the receiver's bug, never a client's push refused.
//
// BUT FIRST, a whom-it-is-made-for value this build does not know (a newer
// build may have written it into a run's row) is no receiver's bug: it is
// answered as the decoder answers it ([unknownToThisBuild]), never as
// ErrInvalidProposal, so a committer that knows it judges it.
//
// A PACK'S SIZES MUST LAY OUT A BUNDLE, refused here before it is published
// rather than by the first fetch; the applier, a pure function of the record,
// does not check again, since refusing there would stop the log on every node.
func (p Proposal) Validate() error {
	_, err := p.measure()
	return err
}

// measure validates p and answers [entryBound] of it — one encoding for both.
func (p Proposal) measure() (int, error) {
	if err := p.validFields(); err != nil {
		return 0, err
	}
	bound, err := entryBound(p)
	switch {
	case err != nil:
		return 0, fmt.Errorf("%w: %w", ErrInvalidProposal, err)
	case bound > EntryRoom:
		return 0, &TooWideError{Bytes: bound, Room: EntryRoom}
	}
	return bound, nil
}

// EntryRoom is the most bytes ONE push's entry may take in its record:
// [MaxRecordBytes] less a record's own overhead, so a push alone in its
// record always fits it.
const EntryRoom = MaxRecordBytes - recordOverheadBytes

// ValidateCommands holds a push's COMMANDS to what one push may carry, BEFORE
// ITS WORK: every rule [Proposal.Validate] holds commands to, and the entry to
// [EntryRoom] with everything the work may add at its largest
// ([Proposal.completed], [introducedShare]). p is as the receiver holds it
// before receive-pack: principal, whom it is made for, way, node and commands,
// MEASURED AS STAMPED, with the refusals that need no object; every other
// field is ignored but its change, which the receiver asks again with once the
// hook's options name it ([ChangeOption.measured]), leaving out one too wide.
// WHAT IT REFUSES IS THE CLIENT'S PUSH, before receive-pack, the object-store
// put and the estate round trip are spent on it; one passing here passes
// Validate's budget whatever its work adds. Only some six hundred longest refs
// of characters JSON escapes (`&`, `<`, `>`, U+2028, U+2029) reach the bound.
func (p Proposal) ValidateCommands() error {
	switch {
	case !p.Via.proposed():
		// The asker's own bug, never the client's: an unstamped way measures
		// short by up to the longest way's spelling.
		return refusedf("%w: via: %q is not one of %s — a push is measured as built", ErrInvalidProposal,
			p.Via, words.Or(proposedVias()))
	case len(p.Commands) == 0:
		return refusedf("%w: commands: a push updates at least one ref", ErrInvalidProposal)
	case len(p.Commands) > MaxCommandsPerPush:
		return refusedf("%w: commands: %d, past the %d one push may carry (MaxCommandsPerPush)",
			ErrInvalidProposal, len(p.Commands), MaxCommandsPerPush)
	}
	if err := validCommands(p.Commands); err != nil {
		return err
	}
	bound, err := entryBound(p.completed())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProposal, err)
	}
	// The introduced ids at their most are added, not encoded: they add the
	// same bytes to any entry, and encoding a thousand ids per push costs 50 KB.
	if bound += introducedShare(); bound > EntryRoom {
		return &TooWideError{Bytes: bound, Room: EntryRoom}
	}
	return nil
}

// completed is p with everything a push's work may add to its entry at its
// largest but the introduced ids ([introducedShare]), for
// [Proposal.ValidateCommands]: each command forced and refused under the
// longest recorded code (the hook's checks; [entryBound] adds
// [commandResultBound] for the decision), the largest pack, push id and
// receive instant, and --atomic. The principal, the way, whom it is made for
// and the change stay as stamped: no work adds to them, and taking them larger
// would refuse a push that fits ([entryBound] measures the change's target
// and an unfilled title at their longest).
func (p Proposal) completed() Proposal {
	w := p
	w.PushID, w.Atomic = largestEntry().PushID, true
	w.ReceivedAt = largestEntry().ReceivedAt
	w.Commands = make([]Command, len(p.Commands))
	for i, c := range p.Commands {
		c.Forced, c.Result, c.Reason = true, ResultNG, longestRecorded()
		w.Commands[i] = c
	}
	w.Pack, w.Introduced, w.IntroducedCount = largestEntry().Pack, nil, 0
	return w
}

// introducedShare is what [MaxIntroduced] commit ids and the largest count add
// to a push entry's encoding: the same bytes whatever else the entry holds.
var introducedShare = sync.OnceValue(func() int {
	ids := make([]string, MaxIntroduced)
	for i := range ids {
		ids[i] = strings.Repeat("f", 40)
	}
	with, err := json.Marshal(PushEntry{Introduced: ids, IntroducedCount: math.MaxInt})
	if err != nil {
		panic("gitlet: encode the most introduced ids: " + err.Error())
	}
	without, err := json.Marshal(PushEntry{})
	if err != nil {
		panic("gitlet: encode an empty entry: " + err.Error())
	}
	return len(with) - len(without)
})

// longestRecorded is the longest code a record's command may carry.
var longestRecorded = sync.OnceValue(func() Refusal {
	var longest Refusal
	for _, r := range Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	return longest
})

// largestEntry is the fields of a push entry the receiver's work fills in but
// the introduced ids, at their longest spellings: a UUIDv7 push id, an instant
// with nine fractional digits in a zone of its own, and a pack whose numbers
// are each the widest an int64 spells.
var largestEntry = sync.OnceValue(func() PushEntry {
	key, err := objstore.ParseKey("01ffffff-ffff-7fff-bfff-ffffffffffff")
	if err != nil {
		panic("gitlet: the largest object key: " + err.Error())
	}
	return PushEntry{
		PushID:     "01ffffff-ffff-7fff-bfff-ffffffffffff",
		ReceivedAt: time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.FixedZone("", -(23*3600+59*60))),
		Pack: &PackRef{PackID: "01ffffff-ffff-7fff-bfff-ffffffffffff", Object: key,
			Hash: objstore.Hash(strings.Repeat("f", 64)), Size: math.MaxInt64, IndexBytes: math.MaxInt64,
			ReverseBytes: math.MaxInt64, GitChecksum: strings.Repeat("f", 40), Objects: math.MaxInt64},
	}
})

// ValidateUnstored is [Proposal.Validate] of a proposal whose pack is not yet
// STORED: its sizes, count and checksum known, its key and digest not until
// the put returns, so measured at their one spelling's length. The receiver
// thus stops, mid-upload, a push no committer could take rather than store a
// bundle no row will name. A pack already stored is refused: Validate checks
// that.
func (p Proposal) ValidateUnstored() error {
	if p.Pack != nil {
		if !p.Pack.Object.IsZero() || p.Pack.Hash != "" {
			return refusedf("%w: pack: already stored as %s — Validate checks a stored pack",
				ErrInvalidProposal, p.Pack.Object)
		}
		pack := *p.Pack
		pack.Object, pack.Hash = largestEntry().Pack.Object, largestEntry().Pack.Hash
		p.Pack = &pack
	}
	return p.Validate()
}

// ValidPushID reports whether id is a push's id as the receiving node mints
// one ([Proposal.PushID]): a UUIDv7 in its canonical lowercase spelling — the
// grammar a proposal is held to, and the one a read naming a push is.
func ValidPushID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.Version() == 7 && u.String() == id
}

// validFields is every rule of [Proposal.Validate] but the record budget.
func (p Proposal) validFields() error {
	// FIRST, A VALUE THIS BUILD DOES NOT KNOW, before any rule judges the
	// push (writtenfor.go's head).
	if err := unknownToThisBuild(writtenForAt, p.WrittenFor); err != nil {
		return err
	}
	if !ValidPushID(p.PushID) {
		return refusedf("%w: push_id: %q is not a UUIDv7 in its canonical lowercase spelling",
			ErrInvalidProposal, p.PushID)
	}
	if id, err := uuid.Parse(p.RepoID); err != nil || id.String() != p.RepoID {
		return refusedf("%w: repo_id: %q is not a UUID in its canonical lowercase spelling",
			ErrInvalidProposal, p.RepoID)
	}
	// Who pushes: a person through a credential bound to their human seat, or a
	// seat by its handle and the run that made the push (a tool call's turn, or
	// its coding box's run); the decide holds a seat's push to its grant (rule
	// 2b, decide.go). A token bound to nobody fetches only, and the engine's own
	// account pushes the expiry of seats' idle branches and nothing else, the
	// two paired both ways.
	why := p.Principal.invalid()
	if p.Principal.Kind == PrincipalSystem {
		why = p.Principal.systemInvalid()
	}
	switch {
	case why != "":
		return fmt.Errorf("%w: %s", ErrInvalidProposal, why)
	case p.Principal.Kind == PrincipalSystem && p.Via != ViaExpiry:
		return refusedf("%w: via: %q — the engine's own account pushes only the ref lifecycle's expiry (%q)",
			ErrInvalidProposal, p.Via, ViaExpiry)
	case p.Via == ViaExpiry && p.Principal.Kind != PrincipalSystem:
		return refusedf("%w: principal.kind: %q — the ref lifecycle's expiry is the engine's own push, "+
			"kind %q", ErrInvalidProposal, p.Principal.Kind, PrincipalSystem)
	case p.Principal.Kind == PrincipalOperator && p.Principal.Seat == "":
		return refusedf("%w: principal.seat: a push names the person its token is bound to — a "+
			"token bound to nobody fetches only", ErrInvalidProposal)
	case p.Principal.Kind == PrincipalAgent && p.Principal.Run.IsZero():
		// The server stamps the run (its turn at least) on every seat's push,
		// so one naming none is its proposer's bug.
		return refusedf("%w: principal.run: a seat's push names the run that made it — its turn at "+
			"least, stamped by the engine from the call", ErrInvalidProposal)
	case p.Principal.Kind == PrincipalAgent && p.Via == ViaGit && p.Principal.Run.RunID == "":
		// A seat's push git received is its coding box's, whose run Gitlet
		// stamps on admitting it: one naming none is its proposer's bug, and
		// no read of that run's pushes would find its row.
		return refusedf("%w: principal.run.run_id: a seat's push git received is its coding box's, "+
			"which names the run that made it and its harness, stamped by the engine from the run's row",
			ErrInvalidProposal)
	case p.Via == ViaEngine && p.Principal.Run.RunID != "":
		// An engine-built write is a tool call's, whose run is its turn: one
		// naming an executor would list under a run that never made it.
		return refusedf("%w: principal.run.run_id: a push the engine built is a tool call's, whose run "+
			"is its turn: it names no executor", ErrInvalidProposal)
	case !p.Via.proposed():
		// A MERGE'S PUSH IS ITS RECORD'S, never a proposal's: a push naming
		// via merge would move a branch as a merge does and judge it as a
		// push, past the gate a merge is decided by (ADR-0096).
		return refusedf("%w: via: %q is not one of %s — a push names how its pack was built, and a "+
			"merge is decided through its gate, never proposed as a push", ErrInvalidProposal, p.Via,
			words.Or(proposedVias()))
	case strings.TrimSpace(p.Node) == "":
		return refusedf("%w: node: a push names the node that received it", ErrInvalidProposal)
	case p.ReceivedAt.IsZero():
		return refusedf("%w: received_at: a push names when it was received", ErrInvalidProposal)
	case !p.Seen.IsZero() && p.Seen.Stream != topics.CodeLogStream:
		return refusedf("%w: seen: %s is a position on another log than the code log (%s)",
			ErrInvalidProposal, p.Seen, topics.CodeLogStream)
	case !p.Seen.IsZero() && p.Seen.Valid() != nil:
		return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, p.Seen.Valid())
	case p.DropEpoch < 0:
		return refusedf("%w: drop_epoch: %d — a drop epoch counts the expunges begun on a "+
			"repository, from 0", ErrInvalidProposal, p.DropEpoch)
	case len(p.Commands) == 0:
		return refusedf("%w: commands: a push updates at least one ref", ErrInvalidProposal)
	case len(p.Commands) > MaxCommandsPerPush:
		return refusedf("%w: commands: %d, past the %d one push may carry (MaxCommandsPerPush)",
			ErrInvalidProposal, len(p.Commands), MaxCommandsPerPush)
	}
	// WHOM IT IS MADE FOR, held to its pusher: a seat's push states it, and
	// nobody else's carries anybody ([writtenForInvalid]).
	if err := writtenForInvalid(p.Principal, p.WrittenFor); err != nil {
		return refusedCarrier(ErrInvalidProposal, err)
	}
	if err := validCommands(p.Commands); err != nil {
		return err
	}
	if p.Via == ViaExpiry {
		if err := p.validExpiry(); err != nil {
			return err
		}
	}
	// An engine-built push is one commit on one ref, so one moving more,
	// or --atomic, is no push the engine built.
	switch {
	case p.Via == ViaEngine && len(p.Commands) != 1:
		return refusedf("%w: commands: %d, and a push the engine built moves exactly one ref",
			ErrInvalidProposal, len(p.Commands))
	case p.Via == ViaEngine && p.Atomic:
		return refusedf("%w: atomic: a push the engine built moves one ref, which --atomic has nothing "+
			"to hold together with", ErrInvalidProposal)
	case p.Via == ViaEngine && p.Pack != nil && p.Pack.PackBytes() > MaxWritePackBytes:
		// THE BUILD'S OWN BOUND: a Gitlet node answers a write whose pack is
		// past it too_large before anything is uploaded, so a proposal
		// carrying one is no build of this engine's.
		return refusedf("%w: pack: %d bytes of pack, past the %d an engine-built push may store "+
			"(MaxWritePackBytes)", ErrInvalidProposal, p.Pack.PackBytes(), MaxWritePackBytes)
	}
	if p.Pack != nil {
		if err := p.validPack(); err != nil {
			return err
		}
	}
	// A CHANGE RIDES BESIDE ONE BRANCH: its receiver answers a push of no
	// branch or of several itself, and proposes the change of none.
	if p.Change != nil {
		if err := p.Change.validFields(); err != nil {
			return err
		}
		if _, r, why := ChangeSource(p.Commands); r != "" {
			return badField("change", "%s — its receiver proposes a change only beside the one branch a "+
				"push sets", why)
		}
	}
	switch {
	case len(p.Introduced) > MaxIntroduced:
		return refusedf("%w: introduced: %d ids, past the %d a push row lists (MaxIntroduced)",
			ErrInvalidProposal, len(p.Introduced), MaxIntroduced)
	case p.IntroducedCount < len(p.Introduced):
		return refusedf("%w: introduced_count: %d, fewer than the %d ids listed",
			ErrInvalidProposal, p.IntroducedCount, len(p.Introduced))
	}
	for i, oid := range p.Introduced {
		if !gitref.ValidOID(oid) {
			return fmt.Errorf("%w: introduced[%d]: %q is not an object id", ErrInvalidProposal, i, oid)
		}
	}
	return p.validOffered()
}

// validOffered holds a count of the commits offered to an attempt's proposal: a push git received
// that its receiver refused whole, by a rule, storing and opening nothing.
func (p Proposal) validOffered() error {
	switch {
	case p.Offered < 0:
		return refusedf("%w: offered: %d is no count of commits", ErrInvalidProposal, p.Offered)
	case p.Offered == 0:
		return nil
	case p.Via != ViaGit:
		return refusedf("%w: offered: only a push git received is an attempt", ErrInvalidProposal)
	case !Attempted(p.Commands):
		return refusedf("%w: offered: only a push its receiver refused whole, by a rule, carries the "+
			"commits it offered", ErrInvalidProposal)
	case p.Pack != nil || len(p.Introduced) > 0 || p.IntroducedCount > 0:
		return refusedf("%w: offered: an attempt stores and introduces nothing", ErrInvalidProposal)
	case p.Change != nil:
		return refusedf("%w: offered: an attempt opens no change", ErrInvalidProposal)
	}
	return nil
}

// validExpiry holds the ref lifecycle's push ([ViaExpiry]) to what its step
// forms (retention_expiry.go): only undecided DELETIONS of branches in a
// seat's namespace ([Holdable]; whose is the decide's question), and no
// pack, introduced commit, --atomic or change. It names its listing page's
// position ([Proposal.Seen]), by which its decide keeps every branch held
// since (rule 4b, [ExpiryRule]); naming none would order no hold after it.
func (p Proposal) validExpiry() error {
	for i, c := range p.Commands {
		switch {
		case c.New != ZeroOID:
			return fmt.Errorf("%w: commands[%d].new: %s — the ref lifecycle's expiry only deletes a branch",
				ErrInvalidProposal, i, c.New)
		case !ValidRef(c.Ref) || !Holdable(c.Ref):
			return fmt.Errorf("%w: commands[%d].ref: %q is no seat's branch (%s<handle>/…), and the expiry "+
				"deletes only those", ErrInvalidProposal, i, c.Ref, SeatNamespaceRoot)
		case c.Result != "":
			return fmt.Errorf("%w: commands[%d].result: %q — the expiry's commands are decided by its "+
				"committer alone", ErrInvalidProposal, i, c.Result)
		}
	}
	switch {
	case p.Pack != nil:
		return refusedf("%w: pack: the ref lifecycle's expiry only deletes, and carries no objects",
			ErrInvalidProposal)
	case len(p.Introduced) > 0 || p.IntroducedCount != 0:
		return refusedf("%w: introduced: the ref lifecycle's expiry introduces no commit", ErrInvalidProposal)
	case p.Atomic:
		return refusedf("%w: atomic: each branch the expiry deletes is judged on its own, and one kept never "+
			"keeps another", ErrInvalidProposal)
	case p.Change != nil:
		return refusedf("%w: change: the ref lifecycle's expiry asks for no change", ErrInvalidProposal)
	case p.Seen.IsZero():
		return refusedf("%w: seen: the ref lifecycle's expiry names the position of the page it was listed "+
			"from, which every hold recorded since keeps a branch past", ErrInvalidProposal)
	}
	return nil
}

// validCommands holds each command to what a receiver may send: two object
// ids that differ, a ref no other command names, and either no result yet or
// a refusal the receiver decided — a code that may be recorded, and at most a
// sentence of Detail.
func validCommands(cmds []Command) error {
	seen := make(map[string]bool, len(cmds))
	for i, c := range cmds {
		switch {
		case RecordedRef(c.Ref) != c.Ref:
			return fmt.Errorf("%w: commands[%d].ref: %d bytes (valid UTF-8: %t) — a command carries its "+
				"ref in its recorded form (RecordedRef), at most %d bytes of UTF-8", ErrInvalidProposal, i,
				len(c.Ref), utf8.ValidString(c.Ref), MaxRefNameBytes)
		case seen[c.Ref]:
			return fmt.Errorf("%w: commands[%d].ref: %q is named twice — git sends one "+
				"command a ref", ErrInvalidProposal, i, c.Ref)
		case !gitref.ValidOID(c.Old):
			return fmt.Errorf("%w: commands[%d].old: %q is not an object id", ErrInvalidProposal, i, c.Old)
		case !gitref.ValidOID(c.New):
			return fmt.Errorf("%w: commands[%d].new: %q is not an object id", ErrInvalidProposal, i, c.New)
		case c.Old == c.New:
			return fmt.Errorf("%w: commands[%d]: old and new are both %s — git never sends a "+
				"command that moves nothing", ErrInvalidProposal, i, c.Old)
		case c.Result == "" && (c.Reason != "" || c.Detail != ""):
			return fmt.Errorf("%w: commands[%d]: an undecided command carries a reason or a "+
				"detail", ErrInvalidProposal, i)
		case c.Result != "" && c.Result != ResultNG:
			return fmt.Errorf("%w: commands[%d].result: %q — the receiver fills in only the "+
				"refusals it decides, and the committer everything else", ErrInvalidProposal, i, c.Result)
		case c.Result == ResultNG && !c.Reason.Recorded():
			return fmt.Errorf("%w: commands[%d].reason: %q is not a refusal a command may "+
				"carry", ErrInvalidProposal, i, c.Reason)
		case len(c.Detail) > maxDetailBytes:
			return fmt.Errorf("%w: commands[%d].detail: %d bytes, past the %d of one sentence",
				ErrInvalidProposal, i, len(c.Detail), maxDetailBytes)
		case !utf8.ValidString(c.Detail):
			return fmt.Errorf("%w: commands[%d].detail: not valid UTF-8", ErrInvalidProposal, i)
		}
		seen[c.Ref] = true
	}
	return nil
}

// validPack holds a pack to a bundle its row can describe — [PackRef.Check],
// the one rule every holder of the row applies, so a proposal is refused here
// for exactly what a Gitlet node would refuse to install — and to the push it
// travels with: it carries the push's own id, and the push moves some ref to
// a commit, since a pack carries the objects a new value needs.
func (p Proposal) validPack() error {
	k := p.Pack
	if k.PackID != p.PushID {
		return refusedf("%w: pack.pack_id: %q, and a push's pack carries the push's id %q",
			ErrInvalidProposal, k.PackID, p.PushID)
	}
	if err := k.Check(); err != nil {
		return fmt.Errorf("%w: pack.%w", ErrInvalidProposal, err)
	}
	if !slices.ContainsFunc(p.Commands, func(c Command) bool { return c.New != ZeroOID }) {
		return refusedf("%w: pack: a push that only deletes refs carries no objects",
			ErrInvalidProposal)
	}
	return nil
}

// entryWith is the record entry p becomes with cmds decided, counted against
// day: no Detail on any command (a sentence is an answer, not state) and, on
// a SEAT'S entry alone, the day ([PushEntry.Day]) and whom it was made for
// ([PushEntry.WorkingFor]), which the committer stamps. ONE BUILDER for the
// entry the decide records and the one [entryBound] measures.
func (p Proposal) entryWith(cmds []Command, day string) PushEntry {
	stored := make([]Command, len(cmds))
	for i, c := range cmds {
		c.Detail = ""
		stored[i] = c
	}
	e := PushEntry{
		PushID: p.PushID, Principal: p.Principal, Via: p.Via, Node: p.Node, Atomic: p.Atomic,
		Commands: stored, Pack: p.Pack, Introduced: p.Introduced,
		IntroducedCount: p.IntroducedCount, Offered: p.Offered, ReceivedAt: p.ReceivedAt,
	}
	if p.Principal.Kind == PrincipalAgent {
		e.Day, e.WorkingFor = day, p.WrittenFor.Carrier()
	}
	return e
}

// longestDay is a company day's label at its longest, period.Day's spelling
// of any day a label can spell ("YYYY-MM-DD", ten bytes): what a seat's entry
// is measured carrying before its decide stamps the day it is counted against.
const longestDay = "9999-12-31"

// THE BOUNDS A COMMITTER FORMS BATCHES BY, so a record's budgets are met
// before the record exists.
const (
	// recordOverheadBytes bounds a push record's encoded bytes outside its
	// entries: the envelope's keys, the subject, a one-path scope, an op id,
	// the writer and the actor (a node id config admits, 64 bytes at most)
	// and the payload's own keys — under 600 bytes measured with the longest
	// node id, so a kibibyte. The limits test holds it against a real
	// encoded record.
	recordOverheadBytes = 1 << 10

	// commandResultBound is the most deciding adds to one command's encoded
	// bytes: `"result":""` becomes `"result":"noop"` (+4), or
	// `"result":"ng","reason":"<code>"` (+2, +12 and the code). 32 holds the
	// longest code a record may carry; the limits test derives that code from
	// Refusals(), so a longer one fails it.
	commandResultBound = 32
)

// entryBound is the most bytes p's entry can take in a push record: the
// entry as [Proposal.entryWith] builds it, a seat's day at [longestDay], its
// change as its decide would open it ([ChangeOption.measured]) and an
// operator's landings at their most ([measuredLandings]), plus what deciding
// adds to each command and the comma before the next entry.
func entryBound(p Proposal) (int, error) {
	e := p.entryWith(p.Commands, longestDay)
	e.Change = p.Change.measured()
	e.Landings = measuredLandings(p.Principal, p.Commands)
	b, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("encode the push's entry: %w", err)
	}
	return len(b) + len(p.Commands)*commandResultBound + 1, nil
}

// rowBound is the most rows p's entry can make its record's apply touch of
// what its proposal alone says: its push row, its pack, its pusher's day but
// an expiry's ([pusherDayRows]), [commandRows] for every command the receiver
// did not refuse, and a seat's [seatDayRows]. The live changes its commands
// move ([changeBook]) and its landings ([PushEntry.Landings]) only the rows
// say: the decide counts them from its snapshot ([entryRows]), leading the
// next record with an entry past this one's rows and refusing one past a
// record of its own (too_many_changes). Reserved here, a person's push would
// be charged two landing rows a branch, protected or not.
func rowBound(p Proposal) int {
	rows := 1
	for _, c := range p.Commands {
		if c.Result != ResultNG {
			rows += commandRows(c)
		}
	}
	if p.Pack != nil {
		rows++
	}
	if p.Principal.Kind == PrincipalAgent {
		rows += seatDayRows
	}
	if p.Via != ViaExpiry {
		rows += pusherDayRows
	}
	return rows
}

// landingRows is the most rows one landing a push makes on a protected branch
// touches ([PushEntry.Landings]): its landing row, the branch's green row and
// its row of tip work — owed by a landing nothing proved, cleared by a green
// one ([writeLandings], [advanceGreen]).
const landingRows = 3

// measuredLandings is the landings an entry of principal's commands can carry
// at its most, as [entryBound] encodes them: one for every command the
// receiver did not refuse that moves a branch onto a commit, green — an
// OPERATOR'S alone, since no seat's push moves a protected branch (a
// protection takes a direct push of a person's at most, [ProtectionRefusal]),
// so a seat's entry carries none. A few bytes a command, so the byte bound
// reserves them where the row bound does not ([rowBound]).
func measuredLandings(principal Principal, cmds []Command) []PushLanding {
	if principal.Kind != PrincipalOperator {
		return nil
	}
	var out []PushLanding
	for i, c := range cmds {
		if c.Result != ResultNG && lands(c) {
			out = append(out, PushLanding{Command: i, Green: true})
		}
	}
	return out
}

// lands reports a command that would land on its ref were a protection rule to
// match it: one moving a branch onto a commit — never a deletion, which lands
// nothing, nor a tag, which a merge gate never holds.
func lands(c Command) bool { return strings.HasPrefix(c.Ref, branchPrefix) && c.New != ZeroOID }

// entryRows is the most rows a DECIDED entry makes its record's apply touch
// besides the changes its commands move: [rowBound] of the commands its
// decide found ok, plus [landingRows] for each landing it stamped. The decide
// admits an entry by it and [withinRecord] holds the record to it.
func entryRows(e PushEntry) int {
	rows := 1
	for _, c := range e.Commands {
		if c.Result == ResultOK {
			rows += commandRows(c)
		}
	}
	if e.Pack != nil {
		rows++
	}
	if e.Principal.Kind == PrincipalAgent {
		rows += seatDayRows
	}
	if e.Via != ViaExpiry {
		rows += pusherDayRows
	}
	return rows + landingRows*len(e.Landings)
}

// seatDayRows is the most rows a seat's entry makes its record's apply touch
// in code_seat_days ([countSeatDays]): the seat's row of its day, and the one
// day beyond the seat's two newest the count forgets. Counted once an entry,
// it overstates a record carrying one seat's entries twice, which only closes
// a batch earlier. The ONE count [rowBound] and [withinRecord] take.
const seatDayRows = 2

// commandRows is the most rows one applying command makes its record's apply
// touch: its ref; for a forced update or a deletion, the object it moved off
// ([leavesObject], a code_ref_drops row, the record's own write, unlike a
// horizon's); and for a deletion a row kept for the sweep. The ONE count [rowBound] and
// [withinRecord] take, so a batch is formed by what its record is held to.
//
// The kept row lets the applier's sweep always match a record's new
// tombstones within MaxRecordRows (MaxTombstonesPerRepo), so it never writes
// deletions as REMOVALS and raises the repository's mark (apply_push.go),
// which sends every synced cache on every Gitlet node to read it whole and
// stops their maintenance on it for a round. The cost is batching alone: one
// maximal deletion push a record (doc.go's measured section).
func commandRows(c Command) int {
	rows := 1
	if c.New == ZeroOID {
		rows++
	}
	if leavesObject(c) {
		rows++
	}
	return rows
}

// recordRows is the row bound of a record carrying a batch whose entries
// bound rows rows between them ([rowBound], the tombstone sweep's room for the
// entries' own deletions among them): those, the repository's counters, the
// day row its pushes are counted in (apply_activity.go), the push history's
// share of the horizon and the tombstone mark. Whatever else
// the record leaves the sweep takes for tombstones past their horizon, and
// the removal of the ref drops past PruneGrace what the sweep leaves.
func recordRows(rows int) int { return rows + 1 + pushDayRows + HorizonDeletesPerRecord + 1 }

// refuseAll is p answered with a refusal of the whole push: every command the
// receiver did not already refuse becomes ng under code with detail, and
// detail is the answer's one sentence as well ([PushAnswer.Detail]).
func refuseAll(p Proposal, code Refusal, detail string) PushAnswer {
	return PushAnswer{PushID: p.PushID, Commands: refuseEvery(p.Commands, code, detail),
		Refused: code, Detail: detail}
}
