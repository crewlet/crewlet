// Package origin records where a failure left the engine's own code: one line,
// `defer origin.Stamp(&err)`, at a function a failure passes through on its way
// out, and the failure carries that function's name from then on.
//
// # Why a function name, and why stamped on the way out
//
// A failure that nothing classifies (an engine defect, until shown otherwise)
// reaches a turn's record as one generic kind, so every such defect looked the
// same: one report for every bug on the turn path. What tells two of them apart
// without reading the company's words is WHERE in the engine they surfaced.
// The alternatives each fail. An error's dynamic type is almost always
// *fmt.wrapError, and a type is not a defect. A message's `pkg:` prefix is an
// unenforced convention that covers some errors and is text a company's words
// can reach. A program counter captured in every error constructor is a change
// to thousands of sites. A deferred stamp is one line where it is worth having,
// costs nothing on success, and reads nothing the error says.
//
// # The rules
//
//   - FIRST NON-NIL ERROR ONLY. [Stamp] does nothing when the function returns
//     nil, and nothing when the error already carries a stamp.
//   - THE DEEPEST STAMP WINS. A stamp made further down the chain is kept, so a
//     function's own stamp is a fallback for failures born where nobody stamps.
//   - NOTHING BUT A FUNCTION OF THIS MODULE. The name comes from the runtime's
//     own frame, never the error, and only a function under the module's
//     `internal/` tree is recorded: anything else leaves the error untouched.
//     The file is its base name, so a build that kept its paths leaks no
//     directory.
//   - TRANSPARENT. The wrapper's message is the error's own, and errors.Is and
//     errors.As see through it. A caller that compares an error with `==`
//     or asserts its type directly would not, so a stamp goes only where every
//     caller uses errors.Is and errors.As.
package origin

import (
	"errors"
	"path"
	"reflect"
	"regexp"
	"runtime"
	"strings"
)

// Place is where a failure left the engine's own code: the function as the
// runtime spells it, and the base name of its source file.
type Place struct {
	Function string
	File     string
}

// stamped is an error and where it was stamped.
type stamped struct {
	err   error
	place Place
}

func (s *stamped) Error() string { return s.err.Error() }
func (s *stamped) Unwrap() error { return s.err }

// Stamp records, on a non-nil *err with no stamp yet, the function that
// deferred it. Deferred as `defer origin.Stamp(&err)` over a named result.
func Stamp(err *error) {
	if err == nil || *err == nil {
		return
	}
	var held *stamped
	if errors.As(*err, &held) {
		return
	}
	if place, ok := caller(); ok {
		*err = &stamped{err: *err, place: place}
	}
}

// Of is the place stamped on err's chain, and false where none is.
func Of(err error) (Place, bool) {
	var held *stamped
	if !errors.As(err, &held) {
		return Place{}, false
	}
	return held.place, true
}

// caller is the function that deferred [Stamp]: the first frame above it that
// is neither Stamp nor the runtime's own.
func caller() (Place, bool) {
	pcs := make([]uintptr, 8)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if frame.Function != stampFunction && !strings.HasPrefix(frame.Function, "runtime.") {
			return placeOf(frame.Function, frame.File)
		}
		if !more {
			return Place{}, false
		}
	}
}

// placeOf is the place a runtime frame names, and false for a function outside
// this module's `internal/` tree. A deferred call's own wrapper is its
// function's.
func placeOf(function, file string) (Place, bool) {
	function = deferWrapper.ReplaceAllString(function, "")
	if !strings.HasPrefix(function, enginePrefix) || !strings.HasSuffix(file, ".go") {
		return Place{}, false
	}
	return Place{Function: function, File: path.Base(file)}, true
}

// deferWrapper is the suffix the compiler gives a deferred call it wraps.
var deferWrapper = regexp.MustCompile(`\.deferwrap[0-9]+$`)

// enginePrefix is the module's `internal/` tree and stampFunction [Stamp] as
// the runtime spells it, both read off this package's own symbol ([Of], which
// refers to neither), so a fork under another module path records its own.
var enginePrefix = strings.TrimSuffix(
	runtime.FuncForPC(reflect.ValueOf(Of).Pointer()).Name(), "origin.Of")

var stampFunction = enginePrefix + "origin.Stamp"
