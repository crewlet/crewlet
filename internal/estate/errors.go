package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ErrOutcomeUnknown is a write that was sent and never answered, of a kind a
// repeat is not safe for — see [opOnceWrite]. It may have landed.
var ErrOutcomeUnknown = errors.New("estate: the write was sent to a data node " +
	"that did not answer, so whether it landed is unknown — read it back " +
	"before writing again")

// ErrNoDataNode is an operation no data node could run: none is live, none
// answered, or every one that answered was not ready for it.
var ErrNoDataNode = errors.New("estate: no data node answered")

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

	{"tracker.ErrBulkInFlight", tracker.ErrBulkInFlight},
	{"tracker.ErrIndexBuilding", tracker.ErrIndexBuilding},
	{"tracker.ErrNoChartActivation", tracker.ErrNoChartActivation},
	{"tracker.ErrNoComment", tracker.ErrNoComment},
	{"tracker.ErrNoProject", tracker.ErrNoProject},
	{"tracker.ErrNoTask", tracker.ErrNoTask},
	{"tracker.ErrNothingToRestore", tracker.ErrNothingToRestore},
	{"tracker.ErrReassignmentBudget", tracker.ErrReassignmentBudget},
	{"tracker.ErrReparentAcrossProjects", tracker.ErrReparentAcrossProjects},
	{"tracker.ErrStaleVersion", tracker.ErrStaleVersion},
	{"tracker.ErrStepUnresolved", tracker.ErrStepUnresolved},
	{"tracker.ErrStepUnvouched", tracker.ErrStepUnvouched},
	{"tracker.ErrTooBroad", tracker.ErrTooBroad},

	{"pages.ErrConflict", pages.ErrConflict},
	{"pages.ErrInvalid", pages.ErrInvalid},
	{"pages.ErrNoActivation", pages.ErrNoActivation},
	{"pages.ErrNotFound", pages.ErrNotFound},
	{"pages.ErrReserved", pages.ErrReserved},
	{"pages.ErrStaleVersion", pages.ErrStaleVersion},
	{"pages.ErrTitleTaken", pages.ErrTitleTaken},

	{"statelog.ErrAheadOfLog", statelog.ErrAheadOfLog},
	{"statelog.ErrConflict", statelog.ErrConflict},
	{"statelog.ErrEstateNotRestored", statelog.ErrEstateNotRestored},
	{"statelog.ErrExists", statelog.ErrExists},
	{"statelog.ErrGenerationPassed", statelog.ErrGenerationPassed},
	{"statelog.ErrLogDiverged", statelog.ErrLogDiverged},
	{"statelog.ErrLogTruncated", statelog.ErrLogTruncated},
	{"statelog.ErrNoDecision", statelog.ErrNoDecision},
	{"statelog.ErrNoOffer", statelog.ErrNoOffer},
	{"statelog.ErrPositionRange", statelog.ErrPositionRange},
	{"statelog.ErrReanchorRefused", statelog.ErrReanchorRefused},
	{"statelog.ErrStopped", statelog.ErrStopped},
	{"statelog.ErrStreamRecreated", statelog.ErrStreamRecreated},
	{"statelog.ErrUnavailable", statelog.ErrUnavailable},
	{"statelog.ErrWaitAbandoned", statelog.ErrWaitAbandoned},
	{"statelog.ErrWrongStream", statelog.ErrWrongStream},

	{"estate.ErrOutcomeUnknown", ErrOutcomeUnknown},
	{"estate.ErrNoDataNode", ErrNoDataNode},
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
	{"tracker.ErrAmbiguous", reflect.TypeFor[*tracker.ErrAmbiguous]()},
	{"tracker.ErrAmbiguousAnswer", reflect.TypeFor[*tracker.ErrAmbiguousAnswer]()},
	{"tracker.ErrFutureVersion", reflect.TypeFor[*tracker.ErrFutureVersion]()},
	{"tracker.SubtreeStopped", reflect.TypeFor[*tracker.SubtreeStopped]()},
	{"tracker.TagClash", reflect.TypeFor[*tracker.TagClash]()},
	{"tracker.TagsFull", reflect.TypeFor[*tracker.TagsFull]()},

	{"pages.ErrFutureVersion", reflect.TypeFor[*pages.ErrFutureVersion]()},
	{"pages.ErrUnknownVersion", reflect.TypeFor[pages.ErrUnknownVersion]()},

	{"statelog.ErrSkipped", reflect.TypeFor[*statelog.ErrSkipped]()},
	{"statelog.EvictionRefusal", reflect.TypeFor[*statelog.EvictionRefusal]()},
	{"statelog.ReadmissionRefusal", reflect.TypeFor[*statelog.ReadmissionRefusal]()},
	{"statelog.Refused", reflect.TypeFor[*statelog.Refused]()},
	{"statelog.Unavailable", reflect.TypeFor[*statelog.Unavailable]()},
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
