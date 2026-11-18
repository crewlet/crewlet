package gitlet

import (
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/events/types"
)

// WHOM A CODE WRITE IS MADE FOR, AS ITS REQUEST CROSSES THE WIRE (ADR-0109).
// [ChangeProposal], [ConversationWrite], [EngineWrite], [QueueProposal],
// [MergeProposal] and [Proposal] each decode ITSELF, with no encoder of its own
// (internal/estate's plainlyEncoded), through the caller's decoder and options
// (an UnmarshalJSON would re-decode under encoding/json's defaults, dropping
// undeclared members), and state whom it is made for whatever arrived
// ([types.WrittenFor.Received]): an absent carrier (`omitzero`) is nobody, or an
// older node's write, decided by its ceiling. A request stating nothing is
// refused by the router (internal/estate's codeWrites) or its Validate
// ([writtenForInvalid]). A value this build does not know (wire, process or
// stored row) is an undeclared name ([unknownToThisBuild]), FIRST in Validate:
// the walk moves on as from a peer that refused it unread (estate's unreadHere).

// UnmarshalJSONFrom decodes a change's lifecycle write as the node it is
// asked of reads it, states whom it is made for whatever arrived, and refuses
// a kind or an app its carrier names that this build does not know as a name
// the build does not declare (this file's head).
func (p *ChangeProposal) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire ChangeProposal
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(p)); err != nil {
		// UNWRAPPED, against the %w rule: json/v2 returns a method's
		// *jsonv2.SemanticError as is and wraps any other at wherever the
		// decoder stopped, and internal/estate's decodeInput names the refused
		// member from the first SemanticError: only the decoder's own holds the
		// member's path, not a position json/v2 inferred.
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
// the building Gitlet node's chart (the half of [writtenForUnder] only a chart
// decides: whether a seat's carrier names a person's seat or a colleague's),
// wrapping [ErrInvalid] and naming written_for; nil where it may. The Gitlet
// node asks it before reading or building anything of a seat's write
// (internal/gitlet/host).
func (w EngineWrite) WrittenForOn(roster Roster) error {
	if err := writtenForUnder(w.Principal, w.WrittenFor, w.Ceiling, roster); err != nil {
		return refusedCarrier(ErrInvalid, err)
	}
	return nil
}

// unknownToThisBuild is the refusal of the first value f's carrier names that
// this build does not know — its kind, or a credential's app — at its place
// under at (`kind`, `apps/<i>`); nil where it knows them all. The one place
// this is decided: each request asks it as it decodes and FIRST in Validate,
// as [writtenForInvalid] does. It is the decoder's own refusal of an
// undeclared name (jsonv2.ErrUnknownName): a kind (types.WorkingForKind) or an
// app (iam.AllGrants) is vocabulary a newer build adds as it adds a member, so
// the estate asks the next node; refusing it as malformed would end that walk.
// Never a known name, nor the zero: the zero kind and app, and admin (no app),
// are no build's carrier, refused malformed by [writtenForInvalid], as is a
// seat's carrier naming apps.
func unknownToThisBuild(at jsontext.Pointer, f types.WrittenFor) error {
	carrier := f.Carrier()
	switch {
	case carrier == nil:
		return nil
	case KindUnknownToThisBuild(carrier):
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

// KindUnknownToThisBuild reports whether f names a kind this build does not
// know — a newer build's: the kind half of [unknownToThisBuild], which decides
// it. Never the zero kind, which [writtenForInvalid] refuses as malformed.
// Exported for the Gitlet node's box admission (internal/gitlet/host's
// runCeiling), which bounds a run's box by its row's kind and so tells a newer
// build's run from one no build launched by this rule alone.
func KindUnknownToThisBuild(f *types.WorkingFor) bool {
	return f != nil && f.Kind != "" && !f.Kind.Valid()
}

// RunCarrierInvalid is why a write a run of seat makes may not state f — whom
// the run's row says its turn works for (internal/sandbox's
// PendingRun.CodeWorkingFor), and so every push of its box — or nil:
// [writtenForInvalid] of an agent of handle seat. A value this build does not
// know is [unknownToThisBuild]'s refusal ([IsUnknownToThisBuild]); any other
// refusal names written_for. Exported for the Gitlet node's box admission
// (internal/gitlet/host's admitRun): one rule for the row and its box's pushes.
func RunCarrierInvalid(seat string, f *types.WorkingFor) error {
	return writtenForInvalid(Principal{Kind: PrincipalAgent, Seat: seat}, types.ForTurn(f))
}

// writtenForAt is the written_for member, where each request's Validate names
// a carrier value this build does not know, as its decoder does on the wire.
var writtenForAt = jsontext.Pointer("").AppendToken("written_for")

// refusedCarrier is err ([writtenForInvalid], [writtenForUnder]) as the
// request's own refusal: wrapped in invalid where the carrier breaks the rule
// — its sender's bug, or a chart that moved under the turn ([ChartRefusal],
// kept in the chain) — and AS IT IS for a value this build does not know
// ([unknownToThisBuild]): a newer build's request, never classed malformed, so
// the estate's walk asks a node that knows it.
func refusedCarrier(invalid, err error) error {
	if IsUnknownToThisBuild(err) {
		return err
	}
	return fmt.Errorf("%w: %w", invalid, err)
}

// IsUnknownToThisBuild reports whether err is the refusal of a carrier value
// this build does not know ([unknownToThisBuild]), on the wire or by Validate:
// no refusal as malformed, since a node whose build knows the value decides
// it. Exported for the Gitlet node's landing (internal/gitlet/host's land.go),
// which asks the push's committer such a proposal rather than refuse it.
func IsUnknownToThisBuild(err error) bool {
	var semantic *jsonv2.SemanticError
	return errors.As(err, &semantic) && errors.Is(err, jsonv2.ErrUnknownName)
}

// handedOn reports whether this node decided nothing of err's proposal for a
// reason about itself — not serving it ([ErrNotServing]), or a build that
// does not know a carrier value ([IsUnknownToThisBuild]) — so the estate asks
// the runner-up, and its committer's and change writer's counters say
// `handed_on`, never `failed`.
func handedOn(err error) bool { return errors.Is(err, ErrNotServing) || IsUnknownToThisBuild(err) }

// WHOM A CODE WRITE WAS MADE FOR, AS A ROW KEEPS IT (ADR-0109, replicated
// migration a_code_row_keeps_whom_an_agents_write_was_made_for): working_for
// on code_revisions, code_pushes and code_queue keeps the carrier of the write
// that made the row, as JSON, NULL for nobody. A later record naming an
// earlier actor reads its carrier from the actor's row ([storedCarrier]) in the
// statement reading the actor ([revisionPusher], [landingPusher]); the queue's
// records read their entry's row, a landing's apply fills its push rows from
// them ([Land.pushes]), and [QueueWorkEntry.WorkingFor] answers it, for the
// home's close of a landed item. Filled from the carrier's first build on: a
// replicated migration runs as a node opens, not at a log position, so a
// column filled later would differ across nodes for the same records (as
// internal/worklet's history row, replicated 0160).

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
// NULL, and for a value that does not decode, or decodes to nothing, a
// stranger's, who holds no app — the closed end, as internal/worklet reads its
// history rows; [carrierColumn] writes no such value.
//
// A kind this build does not know decodes, so it is read as the row holds it,
// never rewritten to a stranger's: another build may have written it, and
// only one that knows it can judge it. A holder that must judge asks
// [KindUnknownToThisBuild]; a record's wake composes it on its own node (as a
// stranger's where that build does not know the kind); and the home's close of
// a landed item leaves the close owed for a home that knows it
// (internal/engine's queue close): deferred, never dropped.
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
