package code

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
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
	// Refused is a refusal of the whole push — busy, unavailable,
	// not_applied, repo_deleted, repo_archived — with every command it
	// covers ng under the same code.
	Refused Refusal `json:"refused,omitempty"`
	// Detail is a whole-push refusal's one sentence of specifics, and every
	// command the refusal covers carries the SAME sentence as its own
	// Detail — one placement whichever path formed the refusal (the decide,
	// for a repository deleted or archived; the committer, for busy,
	// unavailable, not_applied and a deletion the log refused), so a reader
	// of the answer or of a command finds it. A command the receiver had
	// already refused keeps its own. Empty when Refused is.
	Detail string `json:"detail,omitempty"`
	// Collapsed is a push answered from a row an EARLIER record applied: a
	// forwarded or failed-over proposal of a push that had already landed.
	Collapsed bool `json:"collapsed,omitempty"`
}

// maxDetailBytes is the longest Detail a receiver may fill in on a command it
// refused: one sentence, which names NO REF — git prints the ref beside the
// reason anyway — and at most one protection's pattern, which may be as long
// as a ref (MaxRefNameBytes, gitref's pattern grammar), beside its words. So
// the bound is that pattern and room for the words of the longest sentence a
// receiver forms ([ProtectionDetail]'s, 52 bytes); every sentence the code
// host writes into a proposal is held under it by a test, at its longest.
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
	case bound > MaxRecordBytes-recordOverheadBytes:
		return 0, fmt.Errorf("%w: commands: the push's entry can reach %d bytes in its record, "+
			"past the %d one entry may take (MaxRecordBytes less a record's own overhead) — "+
			"a push this wide is pushed in parts", ErrInvalidProposal, bound,
			MaxRecordBytes-recordOverheadBytes)
	}
	return bound, nil
}

// validFields is every rule of [Proposal.Validate] but the record budget.
func (p Proposal) validFields() error {
	if id, err := uuid.Parse(p.PushID); err != nil || id.Version() != 7 || id.String() != p.PushID {
		return fmt.Errorf("%w: push_id: %q is not a UUIDv7 in its canonical lowercase spelling",
			ErrInvalidProposal, p.PushID)
	}
	if id, err := uuid.Parse(p.RepoID); err != nil || id.String() != p.RepoID {
		return fmt.Errorf("%w: repo_id: %q is not a UUID in its canonical lowercase spelling",
			ErrInvalidProposal, p.RepoID)
	}
	switch {
	case p.Principal.Kind != PrincipalOperator:
		return fmt.Errorf("%w: principal.kind: %q — a push is a person's, through the token "+
			"bound to them (%s)", ErrInvalidProposal, p.Principal.Kind, PrincipalOperator)
	case strings.TrimSpace(p.Principal.OperatorID) == "":
		return fmt.Errorf("%w: principal.operator_id: a push names the credential it was made with",
			ErrInvalidProposal)
	case strings.TrimSpace(p.Principal.Seat) == "":
		return fmt.Errorf("%w: principal.seat: a push names the person its token is bound to",
			ErrInvalidProposal)
	case strings.TrimSpace(p.Node) == "":
		return fmt.Errorf("%w: node: a push names the node that received it", ErrInvalidProposal)
	case p.ReceivedAt.IsZero():
		return fmt.Errorf("%w: received_at: a push names when it was received", ErrInvalidProposal)
	case !p.Seen.IsZero() && p.Seen.Stream != topics.CodeLogStream:
		return fmt.Errorf("%w: seen: %s is a position on another log than the code log (%s)",
			ErrInvalidProposal, p.Seen, topics.CodeLogStream)
	case !p.Seen.IsZero() && p.Seen.Valid() != nil:
		return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, p.Seen.Valid())
	case len(p.Commands) == 0:
		return fmt.Errorf("%w: commands: a push updates at least one ref", ErrInvalidProposal)
	case len(p.Commands) > MaxCommandsPerPush:
		return fmt.Errorf("%w: commands: %d, past the %d one push may carry (MaxCommandsPerPush)",
			ErrInvalidProposal, len(p.Commands), MaxCommandsPerPush)
	}
	if err := validCommands(p.Commands); err != nil {
		return err
	}
	if p.Pack != nil {
		if err := p.validPack(); err != nil {
			return err
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

// validCommands holds each command to what a receiver may send: two object
// ids that differ, a ref no other command names, and either no result yet or
// a refusal the receiver decided — a code that may be recorded, and at most a
// sentence of Detail.
func validCommands(cmds []Command) error {
	seen := make(map[string]bool, len(cmds))
	for i, c := range cmds {
		switch {
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

// entryWith is the record entry p becomes with cmds as its decided commands:
// every field of the proposal, and no Detail on any command — a sentence is
// an answer, not state.
func (p Proposal) entryWith(cmds []Command) PushEntry {
	stored := make([]Command, len(cmds))
	for i, c := range cmds {
		c.Detail = ""
		stored[i] = c
	}
	return PushEntry{
		PushID: p.PushID, Principal: p.Principal, Node: p.Node, Atomic: p.Atomic,
		Commands: stored, Pack: p.Pack, Introduced: p.Introduced,
		IntroducedCount: p.IntroducedCount, ReceivedAt: p.ReceivedAt,
	}
}

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
// entry encoded as proposed, plus what deciding can add to each command, plus
// the comma between it and the next entry.
func entryBound(p Proposal) (int, error) {
	b, err := json.Marshal(p.entryWith(p.Commands))
	if err != nil {
		return 0, fmt.Errorf("encode the push's entry: %w", err)
	}
	return len(b) + len(p.Commands)*commandResultBound + 1, nil
}

// rowBound is the most rows p's entry can make its record's apply write: its
// push row, a ref for every command the receiver did not refuse, and its pack.
func rowBound(p Proposal) int {
	rows := 1
	for _, c := range p.Commands {
		if c.Result != ResultNG {
			rows++
		}
	}
	if p.Pack != nil {
		rows++
	}
	return rows
}

// recordRows is the row bound of a record carrying a batch whose entries
// bound rows rows between them: those, the repository's counters, the push
// history's share of the horizon and the tombstone mark. The tombstone sweep
// takes what the record leaves and is never in the bound (apply_push.go).
func recordRows(rows int) int { return rows + 1 + HorizonDeletesPerRecord + 1 }

// refuseAll is p answered with a refusal of the whole push: every command the
// receiver did not already refuse becomes ng under code with detail, and
// detail is the answer's one sentence as well ([PushAnswer.Detail]).
func refuseAll(p Proposal, code Refusal, detail string) PushAnswer {
	return PushAnswer{PushID: p.PushID, Commands: refuseEvery(p.Commands, code, detail),
		Refused: code, Detail: detail}
}
