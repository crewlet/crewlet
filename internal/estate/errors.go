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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// ErrOutcomeUnknown is a write that was sent and never answered, of a kind a
// repeat is not safe for — see [opOnceWrite]. It may have landed.
var ErrOutcomeUnknown = errors.New("estate: the write was sent to a data node " +
	"that did not answer, so whether it landed is unknown — read it back " +
	"before writing again")

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

	{"tracker.ErrAlreadyAnswered", tracker.ErrAlreadyAnswered},
	{"tracker.ErrBadQuery", tracker.ErrBadQuery},
	{"tracker.ErrBulkInFlight", tracker.ErrBulkInFlight},
	{"tracker.ErrForbidden", tracker.ErrForbidden},
	{"tracker.ErrInvalid", tracker.ErrInvalid},
	{"tracker.ErrIndexBuilding", tracker.ErrIndexBuilding},
	{"tracker.ErrNoChartActivation", tracker.ErrNoChartActivation},
	{"tracker.ErrNoComment", tracker.ErrNoComment},
	{"tracker.ErrNoFile", tracker.ErrNoFile},
	{"tracker.ErrNoProject", tracker.ErrNoProject},
	{"tracker.ErrNoTask", tracker.ErrNoTask},
	{"tracker.ErrNoType", tracker.ErrNoType},
	{"tracker.ErrNotAuthor", tracker.ErrNotAuthor},
	{"tracker.ErrNothingToRestore", tracker.ErrNothingToRestore},
	{"tracker.ErrReassignmentBudget", tracker.ErrReassignmentBudget},
	{"tracker.ErrReparentAcrossProjects", tracker.ErrReparentAcrossProjects},
	{"tracker.ErrRestoresOthers", tracker.ErrRestoresOthers},
	{"tracker.ErrStaleVersion", tracker.ErrStaleVersion},
	{"tracker.ErrStepUnresolved", tracker.ErrStepUnresolved},
	{"tracker.ErrStepUnvouched", tracker.ErrStepUnvouched},
	{"tracker.ErrTooBroad", tracker.ErrTooBroad},
	{"tracker.ErrUploadStale", tracker.ErrUploadStale},

	// THE INBOX RULES', which a person's gesture on a domain's inbox is
	// decided by on the data node that writes it: a list at its ceiling is
	// answered as itself, since its remedy is another gesture. And a composed
	// cursor this build did not issue, refused where the inbox is composed:
	// registered with the rest, so the day a composition is asked across a
	// hop its refusal arrives as itself rather than as a bare message.
	{"inbox.ErrBadCursor", inbox.ErrBadCursor},
	{"inbox.ErrFull", inbox.ErrFull},
	{"inbox.ErrInvalid", inbox.ErrInvalid},

	{"pages.ErrBadCursor", pages.ErrBadCursor},
	{"pages.ErrConflict", pages.ErrConflict},
	{"pages.ErrInvalid", pages.ErrInvalid},
	{"pages.ErrNoActivation", pages.ErrNoActivation},
	{"pages.ErrNotFound", pages.ErrNotFound},
	{"pages.ErrNotInTrash", pages.ErrNotInTrash},
	{"pages.ErrParent", pages.ErrParent},
	{"pages.ErrReserved", pages.ErrReserved},
	{"pages.ErrStaleVersion", pages.ErrStaleVersion},
	{"pages.ErrTitleTaken", pages.ErrTitleTaken},

	{"search.ErrIndexBuilding", search.ErrIndexBuilding},
	{"search.ErrNoMethods", search.ErrNoMethods},

	// A REPOSITORY'S READERS refuse a cursor or a range their asker formed —
	// the asker's mistake, answered by name whichever data node read it.
	{"usage.ErrBadCursor", usage.ErrBadCursor},
	{"usage.ErrBadRange", usage.ErrBadRange},

	// THE CODE HOST'S, every one of them — ErrBadBundle, ErrBadLayer,
	// ErrSuperseded and ErrAnswerDue included, though none is answered across
	// the wire today (a bad bundle never leaves the receiving node, nor a bad
	// layer the git node that checked it; a superseded reconcile is the
	// deciding node's own; a read whose answer fell due is answered unready
	// by the git node it fell due on) — for the reason the list is complete
	// at all.
	{"code.ErrAboveCeiling", code.ErrAboveCeiling},
	{"code.ErrAnswerDue", code.ErrAnswerDue},
	{"code.ErrBadBundle", code.ErrBadBundle},
	{"code.ErrBadCursor", code.ErrBadCursor},
	{"code.ErrBadLayer", code.ErrBadLayer},
	{"code.ErrBadLevel", code.ErrBadLevel},
	{"code.ErrChangeNotFound", code.ErrChangeNotFound},
	{"code.ErrCheckNotFound", code.ErrCheckNotFound},
	{"code.ErrConfigManaged", code.ErrConfigManaged},
	{"code.ErrInvalid", code.ErrInvalid},
	{"code.ErrInvalidLayer", code.ErrInvalidLayer},
	{"code.ErrInvalidProposal", code.ErrInvalidProposal},
	{"code.ErrNoActivation", code.ErrNoActivation},
	{"code.ErrNotABlob", code.ErrNotABlob},
	{"code.ErrNotATree", code.ErrNotATree},
	{"code.ErrNotConfigManaged", code.ErrNotConfigManaged},
	{"code.ErrNotDeleted", code.ErrNotDeleted},
	{"code.ErrNotPermitted", code.ErrNotPermitted},
	{"code.ErrNotServing", code.ErrNotServing},
	{"code.ErrNotYet", code.ErrNotYet},
	{"code.ErrObjectNotFound", code.ErrObjectNotFound},
	{"code.ErrPurging", code.ErrPurging},
	{"code.ErrRecordCap", code.ErrRecordCap},
	{"code.ErrRefNotFound", code.ErrRefNotFound},
	{"code.ErrRepoNotFound", code.ErrRepoNotFound},
	{"code.ErrSliceTooLarge", code.ErrSliceTooLarge},
	{"code.ErrSnapshotMoved", code.ErrSnapshotMoved},
	{"code.ErrSuperseded", code.ErrSuperseded},

	{"statelog.ErrAheadOfLog", statelog.ErrAheadOfLog},
	{"statelog.ErrConflict", statelog.ErrConflict},
	{"statelog.ErrDonorConnectionClosed", statelog.ErrDonorConnectionClosed},
	{"statelog.ErrEstateNotRestored", statelog.ErrEstateNotRestored},
	{"statelog.ErrExists", statelog.ErrExists},
	{"statelog.ErrForeignPosition", statelog.ErrForeignPosition},
	{"statelog.ErrGenerationPassed", statelog.ErrGenerationPassed},
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
	{"artifactsdomain.ErrInvalid", artifactsdomain.ErrInvalid},
	{"artifactsdomain.ErrFolderCycle", artifactsdomain.ErrFolderCycle},
	{"artifactsdomain.ErrFolderDepth", artifactsdomain.ErrFolderDepth},
	{"artifactsdomain.ErrFolderLimit", artifactsdomain.ErrFolderLimit},
	{"artifactsdomain.ErrFolderNameTaken", artifactsdomain.ErrFolderNameTaken},
	{"artifactsdomain.ErrFolderNotFound", artifactsdomain.ErrFolderNotFound},
	{"artifactsdomain.ErrLabelTaken", artifactsdomain.ErrLabelTaken},
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
	{"estate.ErrNoGitNode", ErrNoGitNode},
}

// typedKind is one error type whose value crosses the wire.
type typedKind struct {
	code string
	typ  reflect.Type
}

// typedKinds are every error TYPE a caller may unwrap with errors.As, keyed
// by the Go identifier for the reason [sentinels] is.
//
// Each is carried field by field: a plain field as JSON, and a field of type
// error — the cause a refusal was concluded from — as a nested wire error, so
// the cause still answers errors.Is on the far side.
var typedKinds = []typedKind{
	{"tracker.AlreadyAnsweredError", reflect.TypeFor[*tracker.AlreadyAnsweredError]()},
	{"tracker.ErrAmbiguous", reflect.TypeFor[*tracker.ErrAmbiguous]()},
	{"tracker.ErrAmbiguousAnswer", reflect.TypeFor[*tracker.ErrAmbiguousAnswer]()},
	{"tracker.ErrFutureVersion", reflect.TypeFor[*tracker.ErrFutureVersion]()},
	{"tracker.MoveStopped", reflect.TypeFor[*tracker.MoveStopped]()},
	{"tracker.SubtreeStopped", reflect.TypeFor[*tracker.SubtreeStopped]()},
	{"tracker.TagClash", reflect.TypeFor[*tracker.TagClash]()},
	{"tracker.TagsFull", reflect.TypeFor[*tracker.TagsFull]()},
	{"tracker.TextCapError", reflect.TypeFor[*tracker.TextCapError]()},

	{"pages.ErrFutureVersion", reflect.TypeFor[*pages.ErrFutureVersion]()},
	{"pages.ErrUnknownVersion", reflect.TypeFor[pages.ErrUnknownVersion]()},

	{"search.ErrFutureVersion", reflect.TypeFor[*search.ErrFutureVersion]()},

	{"usage.ErrFutureVersion", reflect.TypeFor[*usage.ErrFutureVersion]()},

	// THE CODE HOST'S, every one of them — BadBundle included, though it is
	// never answered across the wire today (a bundle its seal refused is
	// answered by the landing on the node that stored it) — for the reason
	// the list is complete at all.
	{"code.BadBundle", reflect.TypeFor[*code.BadBundle]()},
	{"code.ErrFutureVersion", reflect.TypeFor[*code.ErrFutureVersion]()},
	{"code.ErrGitUnready", reflect.TypeFor[*code.ErrGitUnready]()},
	{"code.TooWideError", reflect.TypeFor[*code.TooWideError]()},

	{"statelog.CallerOpIDError", reflect.TypeFor[*statelog.CallerOpIDError]()},
	{"statelog.ErrSkipped", reflect.TypeFor[*statelog.ErrSkipped]()},
	{"statelog.EvictionRefusal", reflect.TypeFor[*statelog.EvictionRefusal]()},
	{"statelog.ReadmissionRefusal", reflect.TypeFor[*statelog.ReadmissionRefusal]()},
	{"statelog.Refused", reflect.TypeFor[*statelog.Refused]()},
	{"statelog.Unavailable", reflect.TypeFor[*statelog.Unavailable]()},

	{"iamdomain.ErrFutureVersion", reflect.TypeFor[*iamdomain.ErrFutureVersion]()},
	{"iamdomain.ErrTaken", reflect.TypeFor[*iamdomain.ErrTaken]()},
	{"iamdomain.ErrUnknownVersion", reflect.TypeFor[iamdomain.ErrUnknownVersion]()},
	{"iamdomain.InvitationRedeemed", reflect.TypeFor[*iamdomain.InvitationRedeemed]()},

	{"iam.LookRefused", reflect.TypeFor[*iam.LookRefused]()},

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
