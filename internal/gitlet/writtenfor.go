package gitlet

import (
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/events/types"
)

// WHOM A CODE WRITE IS MADE FOR, AS ITS REQUEST CROSSES THE WIRE (ADR-0109).
//
// Six requests carry it, each composed where a seat's write is made — a
// change's lifecycle ([ChangeProposal]) and its conversation
// ([ConversationWrite]), an engine write ([EngineWrite]), a write of a merge
// queue ([QueueProposal]), a merge ([MergeProposal]) and a push ([Proposal]) —
// and every one of them decodes ITSELF, to state on arrival whom it is made for
// whatever arrived ([types.WrittenFor.Received]), as internal/estate's Actor
// does for a tracker write's author.
//
// THE NODE THAT DECIDES A REQUEST CANNOT TELL an absent carrier a current
// node stated from one an older node, knowing no carrier, never sent: a
// carrier of nobody is left off the wire (`omitzero`), and so is the whole
// member on a node of a build before it. So what arrives is what the node
// that formed the request stated: an absent carrier is nobody, the writing
// seat's own work or, until the roll completes, an older node's write, which
// is decided by its ceiling as before the carrier. A request that states
// nothing is never asked of any node: the asking router refuses an agent's
// before it asks anything, its own copy in process included
// (internal/estate's codeWrites), and each request's own Validate refuses it
// where it reaches a writer another way ([writtenForInvalid]).
//
// THROUGH THE CALLER'S OWN DECODER, and so under the caller's options: on the
// wire (internal/estate's decodeInput) a member the serving build does not
// declare, anywhere inside the request, refuses it, named at its path, as it
// would in any request. An UnmarshalJSON would be handed bytes and decode them
// again under encoding/json's own defaults, which drop such a member without a
// word.
//
// A KIND OR AN APP THIS BUILD DOES NOT KNOW IS REFUSED THE SAME WAY
// ([unknownToThisBuild]): a carrier names its kind — a seat, a credential —
// and a credential's the apps its turn was held to, and no sender composes a
// kind or an app its own build does not know, so one the serving build does
// not know is a newer build's, in the roll that adds it. The serving node runs
// nothing of it, the walk asks the next node — the committer's runner-up, the
// next Gitlet node — and where none serves it the tool answers that the
// fleet's builds differ, with nothing written: what an older node does with a
// request carrying written_for at all. It is never refused as malformed, which
// would end the walk and tell a newer build's valid request it is.
//
// NO ENCODER OF ITS OWN: what crosses is the plain encoding of each request's
// fields under the caller's options, which every gate over what an operation
// takes reads it as (internal/estate's plainlyEncoded walks into each).

// UnmarshalJSONFrom decodes a change's lifecycle write as the node it is
// asked of reads it, states whom it is made for whatever arrived, and refuses
// a kind or an app its carrier names that this build does not know as a name
// the build does not declare (this file's head).
func (p *ChangeProposal) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire ChangeProposal
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(p)); err != nil {
		// UNWRAPPED, against the rule that an error wraps with %w: json/v2
		// hands its caller a *jsonv2.SemanticError a method returns as it
		// is, and wraps any other in a second one, typed as this request and
		// placed wherever the decoder stopped. internal/estate's decodeInput
		// names the member it refuses from the first SemanticError it finds,
		// so returned bare that is the decoder's own, at the member's path as
		// the decoder recorded it, never a position json/v2 inferred.
		return err
	}
	p.WrittenFor = p.WrittenFor.Received()
	return unknownToThisBuild(dec.StackPointer().AppendToken("written_for"), p.WrittenFor)
}

// UnmarshalJSONFrom decodes a write of a change's conversation as
// [ChangeProposal.UnmarshalJSONFrom] decodes a lifecycle write.
func (w *ConversationWrite) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire ConversationWrite
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(w)); err != nil {
		return err // UNWRAPPED: [ChangeProposal.UnmarshalJSONFrom] says why.
	}
	w.WrittenFor = w.WrittenFor.Received()
	return unknownToThisBuild(dec.StackPointer().AppendToken("written_for"), w.WrittenFor)
}

// UnmarshalJSONFrom decodes an engine write as
// [ChangeProposal.UnmarshalJSONFrom] decodes a lifecycle write.
func (w *EngineWrite) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire EngineWrite
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(w)); err != nil {
		return err // UNWRAPPED: [ChangeProposal.UnmarshalJSONFrom] says why.
	}
	w.WrittenFor = w.WrittenFor.Received()
	return unknownToThisBuild(dec.StackPointer().AppendToken("written_for"), w.WrittenFor)
}

// UnmarshalJSONFrom decodes a write of a merge queue as
// [ChangeProposal.UnmarshalJSONFrom] decodes a lifecycle write.
func (p *QueueProposal) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire QueueProposal
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(p)); err != nil {
		return err // UNWRAPPED: [ChangeProposal.UnmarshalJSONFrom] says why.
	}
	p.WrittenFor = p.WrittenFor.Received()
	return unknownToThisBuild(dec.StackPointer().AppendToken("written_for"), p.WrittenFor)
}

// UnmarshalJSONFrom decodes a merge as [ChangeProposal.UnmarshalJSONFrom]
// decodes a lifecycle write.
func (p *MergeProposal) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire MergeProposal
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(p)); err != nil {
		return err // UNWRAPPED: [ChangeProposal.UnmarshalJSONFrom] says why.
	}
	p.WrittenFor = p.WrittenFor.Received()
	return unknownToThisBuild(dec.StackPointer().AppendToken("written_for"), p.WrittenFor)
}

// UnmarshalJSONFrom decodes a push as [ChangeProposal.UnmarshalJSONFrom]
// decodes a lifecycle write.
func (p *Proposal) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire Proposal
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(p)); err != nil {
		return err // UNWRAPPED: [ChangeProposal.UnmarshalJSONFrom] says why.
	}
	p.WrittenFor = p.WrittenFor.Received()
	return unknownToThisBuild(dec.StackPointer().AppendToken("written_for"), p.WrittenFor)
}

// WrittenForOn is why w's carrier may not be made under its ceiling on roster,
// the chart of the Gitlet node that builds it ([writtenForUnder]'s half only a
// chart decides: whether a seat's carrier names a person's seat or a
// colleague's), wrapping [ErrInvalid] and naming written_for; nil where it may.
// The Gitlet node asks it of a seat's write before anything of the write is
// read or built (internal/gitlet/host), as Validate asks the rest everywhere.
func (w EngineWrite) WrittenForOn(roster Roster) error {
	if err := writtenForUnder(w.Principal, w.WrittenFor, w.Ceiling, roster); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// unknownToThisBuild is the refusal of the first value f's carrier names that
// this build does not know — its kind, or an app of a credential's — at its
// place under at, where the carrier is held (`kind`, `apps/<i>`), so
// internal/estate's wire names it args.written_for.kind or
// args.written_for.apps[]; nil where this build knows every value the carrier
// names.
//
// THE ONE PLACE THIS BUILD DECIDES WHETHER IT KNOWS WHAT A CARRIER NAMES, its
// kind and each app alike: every request a code write forms asks it as it
// decodes itself, so no request and no value is answered otherwise.
//
// THE DECODER'S OWN REFUSAL OF A NAME ITS BUILD DOES NOT DECLARE
// (jsonv2.ErrUnknownName), which the estate's wire makes of a member, and so the
// answer it gives one, through the same path: refused before anything runs,
// asked of the next node, and the fleet's builds said to differ where none
// serves (this file's head). A kind and an app are names of this build's
// vocabulary as a member is one of a request's (the kinds types.WorkingForKind
// declares, the grants iam.AllGrants does), and a newer build adds one as it
// adds a member.
//
// NEVER A NAME THIS BUILD KNOWS, NOR THE ZERO: the zero kind and the zero app,
// which name none, and admin, which this build knows as no app, are what no
// build composes into a carrier, so the rule every node holds the request to
// refuses each as malformed ([writtenForInvalid]), as it refuses a stranger's
// carrier, whose kind this build knows. Only a credential's carrier names
// apps; a seat's that names one is malformed too, and refused there.
func unknownToThisBuild(at jsontext.Pointer, f types.WrittenFor) error {
	carrier := f.Carrier()
	switch {
	case carrier == nil:
		return nil
	case carrier.Kind != "" && !carrier.Kind.Valid():
		return &jsonv2.SemanticError{Err: jsonv2.ErrUnknownName, JSONPointer: at.AppendToken("kind")}
	case carrier.Kind != types.WorkingForCredential:
		return nil
	}
	for i, app := range carrier.Apps {
		if app != "" && !app.Valid() {
			return &jsonv2.SemanticError{Err: jsonv2.ErrUnknownName,
				JSONPointer: at.AppendToken("apps").AppendToken(strconv.Itoa(i))}
		}
	}
	return nil
}

// WHOM A CODE WRITE WAS MADE FOR, AS A ROW KEEPS IT (ADR-0109, the replicated
// migration a_code_row_keeps_whom_an_agents_write_was_made_for): a revision's
// row (code_revisions.working_for), a push's
// (code_pushes.working_for) and a queue entry's (code_queue.working_for) each
// keep the carrier of the write that made it — an open's or a push's for a
// revision, a push's, a merge's or a landing's for a push, an enqueue's for an
// entry — as its JSON, and NULL for nobody, the writing seat's own work. The
// engine's later records name an earlier actor — a revision's facts its pusher,
// a failed check of a head its pusher, the home's landing the push that merged
// the change, a queue's landing each entry's enqueuer — and they read the
// carrier back from these rows from crewlet#798's piece 4c-ii on. Until then
// the one reader is a landing's own apply, which fills the push rows it lands
// from its entries' rows ([Land.pushes]).
//
// FILLED FROM THE CARRIER'S FIRST BUILD ON, before most of them are read: a
// replicated migration runs on each node when it opens, not at a log position,
// so a column a later build began to fill would hold NULL for the records a
// node applied before it upgraded and the carrier for the same records on a
// node that applied them after, as internal/worklet's history row is filled
// (replicated 0160).

// keptCarrier is whom a record's write was made for as the rows its apply
// writes keep it: f from a record at [WorkingForVersion] or above, and nobody
// from one below it, whatever it holds — a build before the carrier applies
// such a record with no field to read, so a node of this build that kept f
// would hold a row no older node holds for the same record.
func keptCarrier(at applyContext, f *types.WorkingFor) *types.WorkingFor {
	if at.record.V < WorkingForVersion {
		return nil
	}
	return f
}

// carrierColumn is f as a working_for column holds it: its JSON, and NULL for
// nobody.
func carrierColumn(f *types.WorkingFor) (sql.NullString, error) {
	if f == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("gitlet: encode whom a write was made for: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// storedCarrier is whom a write was made for, as column holds it: nil for
// NULL, and for a value this build cannot read — one that does not decode,
// or decodes to nothing — a stranger's, who holds no app, the closed end, as
// internal/worklet reads its history rows. No apply of this build writes such
// a value: [carrierColumn] encodes every carrier it keeps.
func storedCarrier(column sql.NullString) *types.WorkingFor {
	if !column.Valid {
		return nil
	}
	var f *types.WorkingFor
	if err := json.Unmarshal([]byte(column.String), &f); err != nil || f == nil {
		return &types.WorkingFor{Kind: types.WorkingForStranger}
	}
	return f
}
