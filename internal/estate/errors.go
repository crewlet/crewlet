package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ErrOutcomeUnknown is a write that was sent and never answered. It may have
// landed.
//
// TWO CLASSES REACH IT, and they differ in what makes the retry safe, which is
// why the sentence names neither. A write a repeat is not safe for
// ([opOnceWrite]) is reported at the first node that did not answer, and is
// read back before it is written again. A write carrying its caller's
// operation id ([opIdempotentWrite]) is asked of every other data node first,
// and reported only when none of them ran it either — where repeating it
// under that SAME id is the safe retry, and a fresh one is a second write.
var ErrOutcomeUnknown = errors.New("estate: the write was sent to a data node " +
	"that did not answer, so whether it landed is unknown")

// sentinel is one error value whose identity crosses the wire.
type sentinel struct {
	code string
	err  error
}

// sentinels are every error value a serving node's answer may carry and a
// caller may compare with errors.Is.
//
// EVERY exported Err* of the packages whose errors cross — the gate in
// errors_test.go parses them and fails on one missing here — rather than the
// ones a tool compares today, because the list of what a tool compares grows
// in a different package from this one and nothing would tell this list.
//
// The code is the Go identifier, so the list is its own documentation and a
// renamed sentinel is a gate failure rather than a silently dead code.
var sentinels = []sentinel{
	{"context.Canceled", context.Canceled},
	{"context.DeadlineExceeded", context.DeadlineExceeded},
	{"store.ErrNoEstate", store.ErrNoEstate},
	{"queue.ErrTooLarge", queue.ErrTooLarge},
	{"queue.ErrNotLive", queue.ErrNotLive},

	{"worklet.ErrAlreadyAnswered", worklet.ErrAlreadyAnswered},
	{"worklet.ErrBadQuery", worklet.ErrBadQuery},
	{"worklet.ErrForbidden", worklet.ErrForbidden},
	{"worklet.ErrInboxFull", worklet.ErrInboxFull},
	{"worklet.ErrInvalid", worklet.ErrInvalid},
	{"worklet.ErrIndexBuilding", worklet.ErrIndexBuilding},
	{"worklet.ErrNoChartActivation", worklet.ErrNoChartActivation},
	{"worklet.ErrNoComment", worklet.ErrNoComment},
	{"worklet.ErrNoFile", worklet.ErrNoFile},
	{"worklet.ErrNoProject", worklet.ErrNoProject},
	{"worklet.ErrNoTask", worklet.ErrNoTask},
	{"worklet.ErrNoType", worklet.ErrNoType},
	{"worklet.ErrNotAuthor", worklet.ErrNotAuthor},
	{"worklet.ErrNothingToRestore", worklet.ErrNothingToRestore},
	{"worklet.ErrReassignmentBudget", worklet.ErrReassignmentBudget},
	{"worklet.ErrReparentAcrossProjects", worklet.ErrReparentAcrossProjects},
	{"worklet.ErrRestoresOthers", worklet.ErrRestoresOthers},
	{"worklet.ErrStaleVersion", worklet.ErrStaleVersion},
	{"worklet.ErrState", worklet.ErrState},
	{"worklet.ErrStepUnresolved", worklet.ErrStepUnresolved},
	{"worklet.ErrStepUnvouched", worklet.ErrStepUnvouched},
	{"worklet.ErrTaken", worklet.ErrTaken},
	{"worklet.ErrTooBroad", worklet.ErrTooBroad},
	{"worklet.ErrUploadStale", worklet.ErrUploadStale},

	{"doclet.ErrBadCursor", doclet.ErrBadCursor},
	{"doclet.ErrConflict", doclet.ErrConflict},
	{"doclet.ErrInvalid", doclet.ErrInvalid},
	{"doclet.ErrNoActivation", doclet.ErrNoActivation},
	{"doclet.ErrNotFound", doclet.ErrNotFound},
	{"doclet.ErrNotInTrash", doclet.ErrNotInTrash},
	{"doclet.ErrParent", doclet.ErrParent},
	{"doclet.ErrReserved", doclet.ErrReserved},
	{"doclet.ErrStaleVersion", doclet.ErrStaleVersion},
	{"doclet.ErrTitleTaken", doclet.ErrTitleTaken},
	{"doclet.ErrUnservedOrder", doclet.ErrUnservedOrder},

	{"chatlet.ErrArchived", chatlet.ErrArchived},
	{"chatlet.ErrConflict", chatlet.ErrConflict},
	{"chatlet.ErrForbidden", chatlet.ErrForbidden},
	{"chatlet.ErrInvalid", chatlet.ErrInvalid},
	{"chatlet.ErrNameTaken", chatlet.ErrNameTaken},
	{"chatlet.ErrNoActivation", chatlet.ErrNoActivation},
	{"chatlet.ErrNotFound", chatlet.ErrNotFound},
	{"chatlet.ErrReactionsFull", chatlet.ErrReactionsFull},
	{"chatlet.ErrRoomExists", chatlet.ErrRoomExists},
	{"chatlet.ErrUnknownKind", chatlet.ErrUnknownKind},

	{"statelog.ErrAdoptionUnrecorded", statelog.ErrAdoptionUnrecorded},
	{"statelog.ErrAheadOfLog", statelog.ErrAheadOfLog},
	{"statelog.ErrConflict", statelog.ErrConflict},
	{"statelog.ErrDonorConnectionClosed", statelog.ErrDonorConnectionClosed},
	{"statelog.ErrEstateNotRestored", statelog.ErrEstateNotRestored},
	{"statelog.ErrExists", statelog.ErrExists},
	{"statelog.ErrForeignPosition", statelog.ErrForeignPosition},
	{"statelog.ErrGenerationPassed", statelog.ErrGenerationPassed},
	{"statelog.ErrJoinerFault", statelog.ErrJoinerFault},
	{"statelog.ErrLogDiverged", statelog.ErrLogDiverged},
	{"statelog.ErrLogTruncated", statelog.ErrLogTruncated},
	{"statelog.ErrNoDecision", statelog.ErrNoDecision},
	{"statelog.ErrNoOffer", statelog.ErrNoOffer},
	{"statelog.ErrPositionRange", statelog.ErrPositionRange},
	{"statelog.ErrReanchorRefused", statelog.ErrReanchorRefused},
	{"statelog.ErrRecordTampered", statelog.ErrRecordTampered},
	{"statelog.ErrRecordUnverified", statelog.ErrRecordUnverified},
	{"statelog.ErrStopped", statelog.ErrStopped},
	{"statelog.ErrStreamRecreated", statelog.ErrStreamRecreated},
	{"statelog.ErrUnavailable", statelog.ErrUnavailable},
	{"statelog.ErrUnsigned", statelog.ErrUnsigned},
	{"statelog.ErrWaitAbandoned", statelog.ErrWaitAbandoned},
	{"statelog.ErrWrongStream", statelog.ErrWrongStream},

	// THE IDENTITY DIRECTORY'S, which a node holding no data reads through
	// a data node ([opIdentityBinding], [opSeatHolders]) — and the identity
	// vocabulary's and the session's, which the questions a person verb is
	// decided on answer in ([iam.Holders], [session.ResolveSeat]).
	{"iamdomain.ErrAssistRefused", iamdomain.ErrAssistRefused},
	{"iamdomain.ErrCollapsed", iamdomain.ErrCollapsed},
	{"iamdomain.ErrInvalid", iamdomain.ErrInvalid},
	{"iamdomain.ErrInvalidLogin", iamdomain.ErrInvalidLogin},
	{"iamdomain.ErrInvalidToken", iamdomain.ErrInvalidToken},
	{"iamdomain.ErrNoBlindKey", iamdomain.ErrNoBlindKey},
	{"iamdomain.ErrNoCompany", iamdomain.ErrNoCompany},
	{"iamdomain.ErrNoInvitation", iamdomain.ErrNoInvitation},
	{"iamdomain.ErrNotCurrent", iamdomain.ErrNotCurrent},
	{"iamdomain.ErrNotEnrollable", iamdomain.ErrNotEnrollable},
	{"iamdomain.ErrNotFindable", iamdomain.ErrNotFindable},
	{"iamdomain.ErrNotFound", iamdomain.ErrNotFound},
	{"iamdomain.ErrOperationReused", iamdomain.ErrOperationReused},
	{"iamdomain.ErrRefused", iamdomain.ErrRefused},
	{"iamdomain.ErrSeatRequired", iamdomain.ErrSeatRequired},
	{"iamdomain.ErrTokenRefused", iamdomain.ErrTokenRefused},

	{"iam.ErrHolderUnseated", iam.ErrHolderUnseated},
	{"iam.ErrInvalidPrincipal", iam.ErrInvalidPrincipal},
	{"iam.ErrNoHolder", iam.ErrNoHolder},
	{"iam.ErrUnresolved", iam.ErrUnresolved},

	{"session.ErrMalformed", session.ErrMalformed},
	{"session.ErrNoCompany", session.ErrNoCompany},
	{"session.ErrNoKeyring", session.ErrNoKeyring},

	// THE ARTIFACTS ESTATE'S, which every data node answers for a node
	// holding no data ([Artifacts]), and the vocabulary's beneath it: an
	// audience, an id or a label a write refused, and the manifest grammar's
	// and the media table's refusals a publish wraps.
	{"artifactsdomain.ErrAboveOwnRole", artifactsdomain.ErrAboveOwnRole},
	{"artifactsdomain.ErrAgentCompanyShare", artifactsdomain.ErrAgentCompanyShare},
	{"artifactsdomain.ErrBadCursor", artifactsdomain.ErrBadCursor},
	{"artifactsdomain.ErrBrandContrast", artifactsdomain.ErrBrandContrast},
	{"artifactsdomain.ErrBrandIncomplete", artifactsdomain.ErrBrandIncomplete},
	{"artifactsdomain.ErrConflict", artifactsdomain.ErrConflict},
	{"artifactsdomain.ErrExists", artifactsdomain.ErrExists},
	{"artifactsdomain.ErrInvalid", artifactsdomain.ErrInvalid},
	{"artifactsdomain.ErrFolderCycle", artifactsdomain.ErrFolderCycle},
	{"artifactsdomain.ErrFolderDepth", artifactsdomain.ErrFolderDepth},
	{"artifactsdomain.ErrFolderLimit", artifactsdomain.ErrFolderLimit},
	{"artifactsdomain.ErrFolderNameTaken", artifactsdomain.ErrFolderNameTaken},
	{"artifactsdomain.ErrFolderNotFound", artifactsdomain.ErrFolderNotFound},
	{"artifactsdomain.ErrLabelTaken", artifactsdomain.ErrLabelTaken},
	{"artifactsdomain.ErrLabelsFull", artifactsdomain.ErrLabelsFull},
	{"artifactsdomain.ErrNotFound", artifactsdomain.ErrNotFound},
	{"artifactsdomain.ErrNotInTrash", artifactsdomain.ErrNotInTrash},
	{"artifactsdomain.ErrPurged", artifactsdomain.ErrPurged},
	{"artifactsdomain.ErrPurging", artifactsdomain.ErrPurging},
	{"artifactsdomain.ErrSourceTaken", artifactsdomain.ErrSourceTaken},
	{"artifactsdomain.ErrTrashed", artifactsdomain.ErrTrashed},
	{"artifactsdomain.ErrUploadStale", artifactsdomain.ErrUploadStale},

	{"artifacts.ErrInvalidAudience", artifacts.ErrInvalidAudience},
	{"artifacts.ErrInvalidID", artifacts.ErrInvalidID},
	{"artifacts.ErrInvalidLabel", artifacts.ErrInvalidLabel},
	{"manifest.ErrInvalid", manifest.ErrInvalid},
	{"manifest.ErrReserved", manifest.ErrReserved},
	{"mediatype.ErrRefused", mediatype.ErrRefused},

	// THE DATASTORE'S, answered by a data node for a node holding no data
	// ([ArtifactData]), and its encoder's and query compiler's beneath it.
	{"artifactdata.ErrBadCursor", artifactdata.ErrBadCursor},
	{"artifactdata.ErrConflict", artifactdata.ErrConflict},
	{"artifactdata.ErrForbidden", artifactdata.ErrForbidden},
	{"artifactdata.ErrGone", artifactdata.ErrGone},
	{"artifactdata.ErrInvalid", artifactdata.ErrInvalid},
	{"artifactdata.ErrMigrationTooLarge", artifactdata.ErrMigrationTooLarge},
	{"artifactdata.ErrNotFound", artifactdata.ErrNotFound},
	{"artifactdata.ErrNotUndoable", artifactdata.ErrNotUndoable},
	{"canon.ErrDuplicateKey", canon.ErrDuplicateKey},
	{"canon.ErrInvalid", canon.ErrInvalid},
	{"canon.ErrTooDeep", canon.ErrTooDeep},
	{"query.ErrInvalid", query.ErrInvalid},
	{"query.ErrNotIndexed", query.ErrNotIndexed},

	{"estate.ErrOutcomeUnknown", ErrOutcomeUnknown},
}

// typedKind is one error type whose value crosses the wire.
type typedKind struct {
	code string
	typ  reflect.Type
}

// rebuiltByPeer are the typed kinds that cannot be set field by field, each
// with what rebuilds it. A refusal's person phrasing is the one: its words are
// rebuilt only here ([caveat.SaidFromPeer]).
var rebuiltByPeer = map[string]func(typedError) (reflect.Value, bool){
	"caveat.Said": saidFromPeer,
}

// typedKinds are every error TYPE a caller may unwrap with errors.As, keyed
// by the Go identifier for the reason [sentinels] is.
//
// Each is carried field by field: a plain field as JSON, and a field of type
// error — the cause a refusal was concluded from — as a nested wire error, so
// the cause still answers errors.Is on the far side.
var typedKinds = []typedKind{
	{"worklet.AlreadyAnsweredError", reflect.TypeFor[*worklet.AlreadyAnsweredError]()},
	{"worklet.ErrAmbiguous", reflect.TypeFor[*worklet.ErrAmbiguous]()},
	{"worklet.ErrAmbiguousAnswer", reflect.TypeFor[*worklet.ErrAmbiguousAnswer]()},
	{"worklet.ErrFutureVersion", reflect.TypeFor[*worklet.ErrFutureVersion]()},
	{"worklet.MoveStopped", reflect.TypeFor[*worklet.MoveStopped]()},
	{"worklet.Refusal", reflect.TypeFor[*worklet.Refusal]()},
	{"worklet.SubtreeStopped", reflect.TypeFor[*worklet.SubtreeStopped]()},
	{"worklet.TagClash", reflect.TypeFor[*worklet.TagClash]()},
	{"worklet.TagsFull", reflect.TypeFor[*worklet.TagsFull]()},
	{"worklet.TextCapError", reflect.TypeFor[*worklet.TextCapError]()},

	{"doclet.ErrFutureVersion", reflect.TypeFor[*doclet.ErrFutureVersion]()},
	{"doclet.ErrUnknownVersion", reflect.TypeFor[doclet.ErrUnknownVersion]()},
	{"doclet.InvalidError", reflect.TypeFor[*doclet.InvalidError]()},

	{"chatlet.ErrFutureVersion", reflect.TypeFor[*chatlet.ErrFutureVersion]()},
	{"chatlet.ErrUnknownVersion", reflect.TypeFor[chatlet.ErrUnknownVersion]()},
	{"chatlet.InvalidError", reflect.TypeFor[*chatlet.InvalidError]()},

	{"statelog.CallerOpIDError", reflect.TypeFor[*statelog.CallerOpIDError]()},
	{"statelog.ErrSkipped", reflect.TypeFor[*statelog.ErrSkipped]()},
	{"statelog.FreshnessError", reflect.TypeFor[*statelog.FreshnessError]()},
	{"statelog.EvictionRefusal", reflect.TypeFor[*statelog.EvictionRefusal]()},
	{"statelog.ReadmissionRefusal", reflect.TypeFor[*statelog.ReadmissionRefusal]()},
	{"statelog.ReanchorNotApplied", reflect.TypeFor[*statelog.ReanchorNotApplied]()},
	{"statelog.Refused", reflect.TypeFor[*statelog.Refused]()},
	{"statelog.Telling", reflect.TypeFor[*statelog.Telling]()},
	{"statelog.Unavailable", reflect.TypeFor[*statelog.Unavailable]()},

	{"iamdomain.ErrFutureVersion", reflect.TypeFor[*iamdomain.ErrFutureVersion]()},
	{"iamdomain.ErrTaken", reflect.TypeFor[*iamdomain.ErrTaken]()},
	{"iamdomain.ErrUnknownVersion", reflect.TypeFor[iamdomain.ErrUnknownVersion]()},
	{"iamdomain.InvitationRedeemed", reflect.TypeFor[*iamdomain.InvitationRedeemed]()},
	{"iamdomain.NotReadBack", reflect.TypeFor[*iamdomain.NotReadBack]()},
	{"iamdomain.Refusal", reflect.TypeFor[*iamdomain.Refusal]()},

	{"iam.LookRefused", reflect.TypeFor[*iam.LookRefused]()},

	{"artifactsdomain.AlreadyExists", reflect.TypeFor[*artifactsdomain.AlreadyExists]()},
	{"artifactsdomain.BrandContrast", reflect.TypeFor[*artifactsdomain.BrandContrast]()},
	{"artifactsdomain.BrandIncomplete", reflect.TypeFor[*artifactsdomain.BrandIncomplete]()},
	{"artifactsdomain.CarriedObjectGone", reflect.TypeFor[*artifactsdomain.CarriedObjectGone]()},
	{"artifactsdomain.ErrFutureVersion", reflect.TypeFor[*artifactsdomain.ErrFutureVersion]()},
	{"artifactsdomain.FolderAmbiguous", reflect.TypeFor[*artifactsdomain.FolderAmbiguous]()},
	{"artifactsdomain.QuotaExceeded", reflect.TypeFor[*artifactsdomain.QuotaExceeded]()},
	{"artifactsdomain.VersionMoved", reflect.TypeFor[*artifactsdomain.VersionMoved]()},

	{"artifactdata.ErrFutureVersion", reflect.TypeFor[*artifactdata.ErrFutureVersion]()},
	{"artifactdata.RateLimited", reflect.TypeFor[*artifactdata.RateLimited]()},

	{"estate.ErrUnserved", reflect.TypeFor[*ErrUnserved]()},

	// A REFUSAL'S WORDS FOR A PERSON, which a writer puts beside its
	// error (ADR-0060): carried so a write refused on a data node answers
	// a person on this one with them. A peer that does not send it is
	// answered with the refusal code's own sentence, never a model's.
	{"caveat.Said", reflect.TypeFor[*caveat.Said]()},
}

// wireError is an error as it crosses: what it says, and what it is.
type wireError struct {
	Message   string       `json:"message"`
	Sentinels []string     `json:"sentinels,omitempty"`
	Typed     []typedError `json:"typed,omitempty"`
}

// typedError is one typed value in an error's chain.
type typedError struct {
	Code   string                     `json:"code"`
	Fields map[string]json.RawMessage `json:"fields,omitempty"`
	Causes map[string]*wireError      `json:"causes,omitempty"`
}

var errorType = reflect.TypeFor[error]()

// encodeError is err as it crosses, or nil.
func encodeError(err error) *wireError {
	if err == nil {
		return nil
	}
	w := &wireError{Message: err.Error()}
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			w.Sentinels = append(w.Sentinels, s.code)
		}
	}
	for _, k := range typedKinds {
		target := reflect.New(k.typ)
		if !errors.As(err, target.Interface()) {
			continue
		}
		typed, encErr := encodeTyped(k.code, target.Elem())
		if encErr != nil {
			// THE MESSAGE STILL CROSSES, and the identities the
			// sentinels carry; only this type's fields are lost, and
			// the gate is what keeps that from ever happening.
			continue
		}
		w.Typed = append(w.Typed, typed)
	}
	return w
}

// encodeTyped carries one typed error's exported fields.
func encodeTyped(code string, v reflect.Value) (typedError, error) {
	out := typedError{Code: code}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return out, nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return out, fmt.Errorf("estate: %s is not a struct", code)
	}
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		value := v.Field(i)
		if field.Type == errorType {
			if value.IsNil() {
				continue
			}
			if out.Causes == nil {
				out.Causes = map[string]*wireError{}
			}
			out.Causes[field.Name] = encodeError(value.Interface().(error))
			continue
		}
		raw, err := json.Marshal(value.Interface())
		if err != nil {
			return out, fmt.Errorf("estate: %s.%s: %w", code, field.Name, err)
		}
		if out.Fields == nil {
			out.Fields = map[string]json.RawMessage{}
		}
		out.Fields[field.Name] = raw
	}
	return out, nil
}

// remoteError is an error rebuilt from the wire: the message the serving node
// wrote, answering errors.Is and errors.As for every identity it carried.
type remoteError struct {
	msg       string
	sentinels []error
	typed     []reflect.Value
}

func (e *remoteError) Error() string { return e.msg }

// Is answers for every sentinel the original matched, anywhere in its chain.
func (e *remoteError) Is(target error) bool {
	for _, s := range e.sentinels {
		if s == target {
			return true
		}
	}
	return false
}

// As assigns the first carried value of the target's type.
func (e *remoteError) As(target any) bool {
	tv := reflect.ValueOf(target)
	if tv.Kind() != reflect.Pointer || tv.IsNil() {
		return false
	}
	want := tv.Type().Elem()
	for _, v := range e.typed {
		if v.Type().AssignableTo(want) {
			tv.Elem().Set(v)
			return true
		}
	}
	return false
}

// decodeError rebuilds an error from the wire, or nil.
//
// An identity this build does not know — a newer peer's sentinel — is
// DROPPED and its message kept: the caller reads what went wrong in words,
// and branches on nothing it could not have branched on anyway.
func decodeError(w *wireError) error {
	if w == nil {
		return nil
	}
	out := &remoteError{msg: w.Message}
	for _, code := range w.Sentinels {
		for _, s := range sentinels {
			if s.code == code {
				out.sentinels = append(out.sentinels, s.err)
				break
			}
		}
	}
	for _, t := range w.Typed {
		if v, ok := decodeTyped(t); ok {
			out.typed = append(out.typed, v)
		}
	}
	return out
}

// decodeTyped rebuilds one typed value, reporting false for a code this build
// does not know or fields it cannot read.
func decodeTyped(t typedError) (reflect.Value, bool) {
	var kind *typedKind
	for i := range typedKinds {
		if typedKinds[i].code == t.Code {
			kind = &typedKinds[i]
			break
		}
	}
	if kind == nil {
		return reflect.Value{}, false
	}
	if rebuilt, ok := rebuiltByPeer[t.Code]; ok {
		return rebuilt(t)
	}
	structType := kind.typ
	if structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	ptr := reflect.New(structType)
	v := ptr.Elem()
	for name, raw := range t.Fields {
		field := v.FieldByName(name)
		if !field.IsValid() || !field.CanSet() {
			continue
		}
		if err := json.Unmarshal(raw, field.Addr().Interface()); err != nil {
			return reflect.Value{}, false
		}
	}
	for name, cause := range t.Causes {
		field := v.FieldByName(name)
		if !field.IsValid() || !field.CanSet() || field.Type() != errorType {
			continue
		}
		if err := decodeError(cause); err != nil {
			field.Set(reflect.ValueOf(err))
		}
	}
	if kind.typ.Kind() == reflect.Pointer {
		return ptr, true
	}
	return v, true
}

// saidFromPeer rebuilds a refusal's person phrasing a peer carried as
// `{"Words": {"person": "<sentence>", "remedy": {...}}}`, or reports false for
// one with no words or words it cannot read, which leave the refusal its
// code's own sentence, as every field of a typed error this router cannot
// decode does.
func saidFromPeer(t typedError) (reflect.Value, bool) {
	raw, ok := t.Fields["Words"]
	if !ok {
		return reflect.Value{}, false
	}
	said, err := caveat.SaidFromPeer(raw)
	if err != nil || said == nil {
		return reflect.Value{}, false
	}
	return reflect.ValueOf(said), true
}
