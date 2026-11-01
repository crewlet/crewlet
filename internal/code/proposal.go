package code

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

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// THE PROPOSAL: one push as the receiving node hands it to a committer, and
// the answer it gets back.
//
// A proposal carries EVERY command the client sent. The receiver may already
// have refused some of them — a blob past the limit, a rule no state decides,
// a protection — and those arrive ng with their reason; the committer decides
// the rest against the rows and the record states the result of each, so a
// push row is the whole answer the client was given.

// Proposal is one push, decided by the repository's committer.
type Proposal struct {
	// PushID is minted by the receiving node (a UUIDv7) and stable across a
	// failover: it is what collapses a push two committers were asked to
	// decide onto the one row it applied as.
	PushID    string    `json:"push_id"`
	RepoID    string    `json:"repo_id"`
	Principal Principal `json:"principal"`
	// Via is how the push's pack was built, stamped by its builder — the
	// relay's git, or the engine's own — and carried whole into the record's
	// entry and the push row: a push's history says how it was made as the
	// code that made it saw it, never as a commit says. It rides the estate's
	// wire as it rides the record, under [PushEntry.Via]'s stated condition:
	// a committer of a build without it would decide the push into an entry
	// naming no way.
	Via PushVia `json:"via"`
	// Node is the node that received the push.
	Node   string `json:"node"`
	Atomic bool   `json:"atomic,omitempty"`
	// Commands is ALL the commands the client sent; the ones the receiver
	// refused carry ResultNG and their reason, the rest no result yet.
	Commands []Command `json:"commands"`
	// Pack is the bundle the push stored, nil when it carried no objects.
	Pack            *PackRef  `json:"pack,omitempty"`
	Introduced      []string  `json:"introduced,omitempty"`
	IntroducedCount int       `json:"introduced_count,omitempty"`
	ReceivedAt      time.Time `json:"received_at"`
	// Seen is the code log's position the receiving node checked the push
	// against — its linearizable sync before receive-pack ran, the state the
	// client's commands were formed from and the thin pack's bases were read
	// at — zero for a receiver that read none. The committer DECIDES FROM A
	// SNAPSHOT AT OR PAST IT ([Committer]): the repository's committer is
	// usually another node, which may be behind what the receiver saw — a
	// node that just became the committer, or one a few records short of a
	// repository an activation has just created — and from below it the
	// decide would refuse a command whose old value the client was shown
	// (stale, "fetch first") or the repository itself (repo_deleted), and
	// refuse it again after the client fetched, until it caught up. It is a
	// PER-PROPOSAL floor, never this node's: it is somebody else's writes,
	// and only the decide that answers this push has to see them. Never
	// recorded — a push row says what the push did, not what was read to
	// receive it.
	Seen statelog.Position `json:"seen,omitzero"`
	// DropEpoch is the repository's drop epoch as the receiving node checked
	// the push against it — the epoch the objects of the copy receive-pack
	// checked the push's objects over hold (internal/code/cache's
	// Synced.DropEpoch): its synced row's ([RepoRef.DropEpoch]) once the copy
	// holds every pack where the rows place it, and the epoch before while an
	// expunge's begin's inputs, holding what it made dropping, are still among
	// the copy's live objects. The committer refuses the push `expunging`
	// WHOLE where the repository's epoch is another ([RefusalExpunging]): an
	// expunge's begin since has left dropping objects that copy held live, and
	// a ref landed onto one would name an object the expunge's commit retires.
	// The snapshot that synced the copy reads every page at one epoch
	// (Reader.Snapshot). Never recorded, as Seen is not.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// Change is the change the push's options ask for ([ParseChangeOptions]),
	// as its receiver read them — its title filled in from the subject of
	// the commit the push sets its source to where the options named none —
	// or nil for a push that asks for none. A proposal carries one only
	// beside exactly one command setting a branch ([ChangeSource]), which its
	// receiver judges first. Its decide opens it in the push's own entry
	// ([PushEntry.Change]), names the live change its source already heads,
	// or answers why it opened none ([PushAnswer.Change]); it never refuses
	// the push. It rides the estate's wire as it rides the record, under
	// [PushEntry.Change]'s stated condition: a committer of a build without
	// it would decide the push and open nothing.
	Change *ChangeOption `json:"change,omitempty"`
}

// ProposeResult is what became of one proposal.
type ProposeResult struct {
	// Outcome is applied, pending or unknown for a push in a record, and
	// empty for one answered with no record of its own: every command noop
	// or ng, or a refusal of the whole push. A push answered from the row an
	// earlier record applied it as is applied, and its Answer says
	// Collapsed.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Position is where the push's record landed — the row's own for an
	// applied push, the record's for a pending one, and for a not_applied
	// refusal, or a repo_deleted one the code log made after the append, the
	// position of the batch's own record, which applies nowhere. Never
	// another operation's record: a refusal the log made at one of those
	// (statelog.Unavailable.Landed) names no record of the push, and a
	// position read as this one told a client its push was durable there.
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
	// command the refusal covers carries the SAME sentence as its own
	// Detail — one placement whichever path formed the refusal (the decide,
	// for a repository deleted or archived, at its pack bound, at a seat's
	// day's quota, at another drop epoch than the push was checked at, or for
	// the live changes it moves; the
	// committer, for busy, unavailable, not_applied and a deletion the log
	// refused), so a reader of the answer or of a command finds it. A
	// command the receiver had already refused keeps its own. Empty when
	// Refused is.
	Detail string `json:"detail,omitempty"`
	// Collapsed is a push answered from a row an EARLIER record applied: a
	// forwarded or failed-over proposal of a push that had already landed.
	Collapsed bool `json:"collapsed,omitempty"`
	// Change is what the push's change came to ([Proposal.Change]), nil for
	// a push that asked for none, one no record carries, and one whose
	// answer cannot say: for an applied push, the change it opened where the
	// rows hold one of its id ([PushChangeID]) — the one fact they hold
	// whichever round of the batch decided the record — and otherwise the
	// decision's, the live change its source headed (numbered from its row)
	// or why it opened none; for a pending or unknown push, the decision's,
	// tentative until the record resolves, as its commands are.
	Change *ChangeAnswer `json:"change,omitempty"`
}

// maxDetailBytes is the longest Detail a receiver may fill in on a command it
// refused: one sentence, which names NO REF — git prints the ref beside the
// reason anyway — and at most one protection's pattern, which may be as long
// as a ref (MaxRefNameBytes, gitref's pattern grammar), beside its words. So
// the bound is that pattern and room for the words of the longest sentence a
// receiver forms ([ProtectionDetail]'s, 61 bytes: a seat's push under a
// direct_push persons rule); every sentence the code host writes into a
// proposal is held under it by a test, at its longest.
const maxDetailBytes = MaxRefNameBytes + 64

// Validate refuses a proposal this domain cannot decide, wrapping
// [ErrInvalidProposal] and naming the field. The receiver forms proposals, so
// a refusal here is the receiver's bug — never a client's push refused.
//
// A PACK'S SIZES MUST LAY OUT A BUNDLE: a row whose sizes describe no bundle
// names an object no cache can install, so it is refused here, before it is
// published, rather than by the first fetch. The applier does not check them
// again — it is a pure function of the record, and refusing there would stop
// the log on every node.
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
// ITS WORK: every rule [Proposal.Validate] holds a proposal's commands to —
// at least one and at most [MaxCommandsPerPush], each ref in its
// [RecordedRef] form and named once, two ids that differ, undecided or
// refused under a code a record carries — and the push's entry to
// [EntryRoom] with everything the work may yet add at its largest
// ([Proposal.completed]): every command forced and decided, a pack, the most
// introduced ids a row lists, the push's id and the receive instant at their
// longest spellings, the push --atomic, and for a seat's push the company day
// its committer stamps ([entryBound]). p is the proposal as the receiver
// holds it once it has read the client's commands, before receive-pack runs:
// its principal — a seat's run with it — how it is being built ([Proposal.Via]), its
// node and its commands, with the receiver's refusals that need no object;
// every other field is ignored but its change ([Proposal.Change]), which the
// receiver learns only from the options receive-pack hands its hook and asks
// this again with before it proposes one: a change too wide for the entry
// beside the commands at their largest is answered to its pusher and left
// out, so the push lands without it. The principal and the way are MEASURED
// AS STAMPED, since every proposer stamps both before its work: no work adds
// to them, so taking them at a largest that is not theirs would refuse a push
// that fits.
//
// WHAT IT REFUSES IS THE CLIENT'S PUSH, never the receiver's bug: the client
// chose the commands, and every other field is taken at its largest. A push
// that passes here passes [Proposal.Validate]'s budget whatever its work
// adds, so a push too wide for its record is refused before receive-pack, the
// object-store put and the estate's round trip are spent on it — where it was
// once refused by its committer, with the receiver's own proposal, after all
// three. Plain refs fit easily: [MaxCommandsPerPush] commands at
// [MaxRefNameBytes] take under half the room. What reaches it is a ref JSON
// spells long — `&`, `<` and `>` six bytes each, U+2028 and U+2029 two bytes
// for each of their three — of which some six hundred refs of the longest are
// enough.
func (p Proposal) ValidateCommands() error {
	switch {
	case !p.Via.proposed():
		// THE ASKER'S OWN BUG, never the client's: a measure of an entry
		// whose way is unstamped is short of the entry that will be
		// proposed, by up to the longest way's spelling.
		return fmt.Errorf("%w: via: %q is not one of %s — a push is measured as built", ErrInvalidProposal,
			p.Via, words.Or(proposedVias()))
	case len(p.Commands) == 0:
		return fmt.Errorf("%w: commands: a push updates at least one ref", ErrInvalidProposal)
	case len(p.Commands) > MaxCommandsPerPush:
		return fmt.Errorf("%w: commands: %d, past the %d one push may carry (MaxCommandsPerPush)",
			ErrInvalidProposal, len(p.Commands), MaxCommandsPerPush)
	}
	if err := validCommands(p.Commands); err != nil {
		return err
	}
	bound, err := entryBound(p.completed())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProposal, err)
	}
	// THE INTRODUCED IDS AT THEIR MOST ARE ADDED, NOT ENCODED: JSON spells
	// an object's members one after another, so what they add to an entry is
	// the same bytes whatever else it holds — and encoding the thousand ids
	// for every push read cost each one 50 KB and a tenth of a millisecond
	// to learn one number.
	if bound += introducedShare(); bound > EntryRoom {
		return &TooWideError{Bytes: bound, Room: EntryRoom}
	}
	return nil
}

// completed is p with everything a push's work may add to its entry at its
// largest but the introduced ids ([introducedShare]), for
// [Proposal.ValidateCommands]: each command forced and refused by the receiver
// under the longest code a record carries — what the hook's checks (stale, a
// blob, a protection) write into the proposal, beside which [entryBound] still
// adds [commandResultBound] for the committer's decision — a pack whose every
// field is spelled at its longest, the push id and the receive instant at
// their longest spellings, and the push --atomic. The principal, its run among
// it, the way and the change stay as their proposer stamped them: no work adds
// to them, and [entryBound] measures the change's target at its longest.
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
		panic("code: encode the most introduced ids: " + err.Error())
	}
	without, err := json.Marshal(PushEntry{})
	if err != nil {
		panic("code: encode an empty entry: " + err.Error())
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
		panic("code: the largest object key: " + err.Error())
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
// STORED — its sizes, its object count and its checksum known from the push's
// own files, its object's key and digest not until the put returns: the pack
// is measured and checked as it will be once stored, a key and a digest
// having one spelling's length each. So the receiver holds its proposal to
// every rule the committer will, once the commits the push introduces are
// listed and while its bundle is still uploading, and stops the upload of a
// push no committer could take rather than store a bundle no row will name.
// A pack already stored is refused: Validate is the check of that.
func (p Proposal) ValidateUnstored() error {
	if p.Pack != nil {
		if !p.Pack.Object.IsZero() || p.Pack.Hash != "" {
			return fmt.Errorf("%w: pack: already stored as %s — Validate checks a stored pack",
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
	if !ValidPushID(p.PushID) {
		return fmt.Errorf("%w: push_id: %q is not a UUIDv7 in its canonical lowercase spelling",
			ErrInvalidProposal, p.PushID)
	}
	if id, err := uuid.Parse(p.RepoID); err != nil || id.String() != p.RepoID {
		return fmt.Errorf("%w: repo_id: %q is not a UUID in its canonical lowercase spelling",
			ErrInvalidProposal, p.RepoID)
	}
	// WHO PUSHES: a person, through a credential bound to their human seat,
	// or a seat, by its handle and the run that made the push — through a
	// tool, whose write the engine builds and whose run is the turn that
	// called it, or through the coding box of a run it launched, whose push
	// git receives and names that run and its coding agent beside the turn.
	// The decide holds a seat's push to its grant on the chart (rule 2b,
	// decide.go) before any protection, so a seat's push is a proposal like a
	// person's; a token bound to nobody fetches only. And the engine, on its
	// own account, deletes seats' idle branches — its expiry and no other
	// push, the two paired both ways.
	why := p.Principal.invalid()
	if p.Principal.Kind == PrincipalSystem {
		why = p.Principal.systemInvalid()
	}
	switch {
	case why != "":
		return fmt.Errorf("%w: %s", ErrInvalidProposal, why)
	case p.Principal.Kind == PrincipalSystem && p.Via != ViaExpiry:
		return fmt.Errorf("%w: via: %q — the engine's own account pushes only the ref lifecycle's expiry (%q)",
			ErrInvalidProposal, p.Via, ViaExpiry)
	case p.Via == ViaExpiry && p.Principal.Kind != PrincipalSystem:
		return fmt.Errorf("%w: principal.kind: %q — the ref lifecycle's expiry is the engine's own push, "+
			"kind %q", ErrInvalidProposal, p.Principal.Kind, PrincipalSystem)
	case p.Principal.Kind == PrincipalOperator && p.Principal.Seat == "":
		return fmt.Errorf("%w: principal.seat: a push names the person its token is bound to — a "+
			"token bound to nobody fetches only", ErrInvalidProposal)
	case p.Principal.Kind == PrincipalAgent && p.Principal.Run.IsZero():
		// THE SERVER STAMPS THE RUN on every seat's push — its turn, at
		// least — from the call that made it: a seat's push naming none is
		// its proposer's bug, and a row naming no run would say nothing of
		// what made it.
		return fmt.Errorf("%w: principal.run: a seat's push names the run that made it — its turn at "+
			"least, stamped by the engine from the call", ErrInvalidProposal)
	case p.Principal.Kind == PrincipalAgent && p.Via == ViaGit && p.Principal.Run.RunID == "":
		// A SEAT'S PUSH GIT RECEIVED IS ITS CODING BOX'S — a seat's own
		// writes are tool calls, which the engine builds — and the code host
		// stamps the run that pushed and its coding agent from the run's row
		// when it admits the box: one naming neither is its proposer's bug,
		// and its row would be one no read of that run's pushes finds.
		return fmt.Errorf("%w: principal.run.run_id: a seat's push git received is its coding box's, "+
			"which names the run that made it and its coding agent, stamped by the engine from the run's row",
			ErrInvalidProposal)
	case p.Via == ViaEngine && p.Principal.Run.RunID != "":
		// AN ENGINE-BUILT WRITE IS A TOOL CALL'S, whose run is the turn that
		// called it — a coding run's box pushes through git — so one naming a
		// coding run is its proposer's bug, and a read of that run's pushes
		// would answer a write the run never made.
		return fmt.Errorf("%w: principal.run.run_id: a push the engine built is a tool call's, whose run "+
			"is its turn: it names no coding run", ErrInvalidProposal)
	case !p.Via.proposed():
		// A MERGE'S PUSH IS ITS RECORD'S, never a proposal's: a push naming
		// via merge would move a branch as a merge does and judge it as a
		// push, past the gate a merge is decided by (ADR-0089).
		return fmt.Errorf("%w: via: %q is not one of %s — a push names how its pack was built, and a "+
			"merge is decided through its gate, never proposed as a push", ErrInvalidProposal, p.Via,
			words.Or(proposedVias()))
	case strings.TrimSpace(p.Node) == "":
		return fmt.Errorf("%w: node: a push names the node that received it", ErrInvalidProposal)
	case p.ReceivedAt.IsZero():
		return fmt.Errorf("%w: received_at: a push names when it was received", ErrInvalidProposal)
	case !p.Seen.IsZero() && p.Seen.Stream != topics.CodeLogStream:
		return fmt.Errorf("%w: seen: %s is a position on another log than the code log (%s)",
			ErrInvalidProposal, p.Seen, topics.CodeLogStream)
	case !p.Seen.IsZero() && p.Seen.Valid() != nil:
		return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, p.Seen.Valid())
	case p.DropEpoch < 0:
		return fmt.Errorf("%w: drop_epoch: %d — a drop epoch counts the expunges begun on a "+
			"repository, from 0", ErrInvalidProposal, p.DropEpoch)
	case len(p.Commands) == 0:
		return fmt.Errorf("%w: commands: a push updates at least one ref", ErrInvalidProposal)
	case len(p.Commands) > MaxCommandsPerPush:
		return fmt.Errorf("%w: commands: %d, past the %d one push may carry (MaxCommandsPerPush)",
			ErrInvalidProposal, len(p.Commands), MaxCommandsPerPush)
	}
	if err := validCommands(p.Commands); err != nil {
		return err
	}
	if p.Via == ViaExpiry {
		if err := p.validExpiry(); err != nil {
			return err
		}
	}
	// AN ENGINE-BUILT PUSH IS ONE COMMIT ON ONE REF: the engine builds a
	// write as one ref update, so a proposal of it moving more, or held
	// together by --atomic — which one command has nothing to hold together
	// with — is no push the engine built.
	switch {
	case p.Via == ViaEngine && len(p.Commands) != 1:
		return fmt.Errorf("%w: commands: %d, and a push the engine built moves exactly one ref",
			ErrInvalidProposal, len(p.Commands))
	case p.Via == ViaEngine && p.Atomic:
		return fmt.Errorf("%w: atomic: a push the engine built moves one ref, which --atomic has nothing "+
			"to hold together with", ErrInvalidProposal)
	case p.Via == ViaEngine && p.Pack != nil && p.Pack.PackBytes() > MaxWritePackBytes:
		// THE BUILD'S OWN BOUND: a git node answers a write whose pack is
		// past it too_large before anything is uploaded, so a proposal
		// carrying one is no build of this engine's.
		return fmt.Errorf("%w: pack: %d bytes of pack, past the %d an engine-built push may store "+
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
		return fmt.Errorf("%w: introduced: %d ids, past the %d a push row lists (MaxIntroduced)",
			ErrInvalidProposal, len(p.Introduced), MaxIntroduced)
	case p.IntroducedCount < len(p.Introduced):
		return fmt.Errorf("%w: introduced_count: %d, fewer than the %d ids listed",
			ErrInvalidProposal, p.IntroducedCount, len(p.Introduced))
	}
	for i, oid := range p.Introduced {
		if !gitref.ValidOID(oid) {
			return fmt.Errorf("%w: introduced[%d]: %q is not an object id", ErrInvalidProposal, i, oid)
		}
	}
	return nil
}

// validExpiry holds the ref lifecycle's push ([ViaExpiry]) to what its step
// forms (retention_expiry.go): only DELETIONS, each of a branch in some seat's
// namespace ([Holdable]; whose namespace it is is the decide's question) off
// the commit its listing read, none decided yet — the receivers' refusals are
// a client's, and the expiry has none — and nothing else of a push: no pack, no
// introduced commit, no --atomic, no change. And it names the position of the
// page it was listed from ([Proposal.Seen]), which its decide keeps every
// branch held since by (rule 4b, [ExpiryRule]): an expiry naming none would
// order no hold recorded after its listing.
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
		return fmt.Errorf("%w: pack: the ref lifecycle's expiry only deletes, and carries no objects",
			ErrInvalidProposal)
	case len(p.Introduced) > 0 || p.IntroducedCount != 0:
		return fmt.Errorf("%w: introduced: the ref lifecycle's expiry introduces no commit", ErrInvalidProposal)
	case p.Atomic:
		return fmt.Errorf("%w: atomic: each branch the expiry deletes is judged on its own, and one kept never "+
			"keeps another", ErrInvalidProposal)
	case p.Change != nil:
		return fmt.Errorf("%w: change: the ref lifecycle's expiry asks for no change", ErrInvalidProposal)
	case p.Seen.IsZero():
		return fmt.Errorf("%w: seen: the ref lifecycle's expiry names the position of the page it was listed "+
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
// for exactly what a git node would refuse to install — and to the push it
// travels with: it carries the push's own id, and the push moves some ref to
// a commit, since a pack carries the objects a new value needs.
func (p Proposal) validPack() error {
	k := p.Pack
	if k.PackID != p.PushID {
		return fmt.Errorf("%w: pack.pack_id: %q, and a push's pack carries the push's id %q",
			ErrInvalidProposal, k.PackID, p.PushID)
	}
	if err := k.Check(); err != nil {
		return fmt.Errorf("%w: pack.%w", ErrInvalidProposal, err)
	}
	if !slices.ContainsFunc(p.Commands, func(c Command) bool { return c.New != ZeroOID }) {
		return fmt.Errorf("%w: pack: a push that only deletes refs carries no objects",
			ErrInvalidProposal)
	}
	return nil
}

// entryWith is the record entry p becomes with cmds as its decided commands,
// counted against day: every field of the proposal, no Detail on any command
// — a sentence is an answer, not state — and, on a SEAT'S entry alone, the
// company day its decide counted it against ([PushEntry.Day]), which the
// committer stamps and no proposer sends.
func (p Proposal) entryWith(cmds []Command, day string) PushEntry {
	stored := make([]Command, len(cmds))
	for i, c := range cmds {
		c.Detail = ""
		stored[i] = c
	}
	e := PushEntry{
		PushID: p.PushID, Principal: p.Principal, Via: p.Via, Node: p.Node, Atomic: p.Atomic,
		Commands: stored, Pack: p.Pack, Introduced: p.Introduced,
		IntroducedCount: p.IntroducedCount, ReceivedAt: p.ReceivedAt,
	}
	if p.Principal.Kind == PrincipalAgent {
		e.Day = day
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
// entry encoded as proposed — a seat's carrying the day its decide stamps, at
// its longest ([longestDay]), and its change as its decide would open it,
// the target it stamps at its longest where the options named none
// ([ChangeOption.measured]) — plus what deciding can add to each command,
// plus the comma between it and the next entry.
func entryBound(p Proposal) (int, error) {
	e := p.entryWith(p.Commands, longestDay)
	e.Change = p.Change.measured()
	b, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("encode the push's entry: %w", err)
	}
	return len(b) + len(p.Commands)*commandResultBound + 1, nil
}

// rowBound is the most rows p's entry can make its record's apply touch: its
// push row, its pack, [commandRows] for every command the receiver did not
// refuse, and for a SEAT'S push the [seatDayRows] its day's count takes.
//
// # A deletion keeps a row for the sweep so no record this committer forms removes
//
// The applier holds MaxTombstonesPerRepo after every record with two moves:
// its sweep takes the oldest past the bound within what the record's rows
// leave of MaxRecordRows, and a record whose new tombstones that room cannot
// match writes its deletions as REMOVALS and raises the repository's mark to
// its own position (apply_push.go) — which sends every cache synced before the
// record, on every git node holding the repository, to read it whole, and
// stops each such node's maintenance on it for the full round. Counted as its
// ref alone, a deletion left the sweep less room than the record's own
// deletions need: three pushes of a thousand deletions shared one record whose
// sweep had 995 rows for 3 000 new tombstones, and a record filled to the row
// bound left it none. With a row kept for the sweep, a record's sweep always
// has one for every tombstone it makes, the removal is left to records no
// committer of this build forms, and what that costs is batching alone: one
// maximal deletion push a record rather than three, a record still under half
// the lane's time budget (doc.go's measured section).
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
	return rows
}

// entryRows is the most rows a DECIDED entry makes its record's apply touch
// besides the changes its commands move: its push row, [commandRows] for every
// command that applies, its pack, and for a seat's entry the [seatDayRows]
// its day's count takes — [rowBound] of the commands its decide found ok,
// which the decide admits an entry by and [withinRecord] holds the record to.
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
	return rows
}

// seatDayRows is the most rows a seat's entry makes its record's apply touch
// in code_seat_days ([countSeatDays]): the seat's row of the day its entry is
// counted against, and the one day beyond the seat's two newest that the
// count forgets. COUNTED
// ONCE AN ENTRY, which overstates a record carrying a seat's entries twice —
// they share the seat's two rows — and so only closes a batch earlier at the
// row bound, never lets its apply pass it. The ONE count the batcher
// ([rowBound]) and the decide's own check ([withinRecord]) take.
const seatDayRows = 2

// commandRows is the most rows one command that applies makes its record's
// apply touch: its ref; the object it moved its ref off ([leavesObject], a
// code_ref_drops row), for a forced update and a deletion; and for a
// deletion the old tombstone the record's sweep may have to take to keep the
// repository inside MaxTombstonesPerRepo once the deletion's own is written —
// a deletion three, a forced update two, any other one. The ONE count the
// batcher ([rowBound]) and the decide's own check ([withinRecord]) take, so
// what a batch is formed by is what its record is held to. A drop row is the
// record's own write rather than a horizon's, which takes only what is left:
// uncounted, three maximal forced pushes would share a record writing 6 007
// rows, past MaxRecordRows, and a deletion's drop would take the row kept for
// its sweep.
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
// push history's share of the horizon and the tombstone mark. Whatever else
// the record leaves the sweep takes for tombstones past their horizon, and
// the removal of the ref drops past PruneGrace what the sweep leaves.
func recordRows(rows int) int { return rows + 1 + HorizonDeletesPerRecord + 1 }

// refuseAll is p answered with a refusal of the whole push: every command the
// receiver did not already refuse becomes ng under code with detail, and
// detail is the answer's one sentence as well ([PushAnswer.Detail]).
func refuseAll(p Proposal, code Refusal, detail string) PushAnswer {
	return PushAnswer{PushID: p.PushID, Commands: refuseEvery(p.Commands, code, detail),
		Refused: code, Detail: detail}
}
