package origin_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/origin"
)

const here = "github.com/crewlet/crewlet/internal/origin_test."

var errLeaf = errors.New("the company's words: rotate key sk-ant-123 for jane@example.com")

type typed struct{ code int }

func (t *typed) Error() string { return fmt.Sprintf("typed %d", t.code) }

func leaf() (err error) {
	defer origin.Stamp(&err)
	return fmt.Errorf("leaf: %w", errLeaf)
}

func middle() (err error) {
	defer origin.Stamp(&err)
	if err := leaf(); err != nil {
		return fmt.Errorf("middle: %w", err)
	}
	return nil
}

func unstampedLeaf() error { return &typed{code: 7} }

func catcher() (err error) {
	defer origin.Stamp(&err)
	return fmt.Errorf("catcher: %w", unstampedLeaf())
}

func succeeds() (err error) {
	defer origin.Stamp(&err)
	return nil
}

type receiver struct{}

func (*receiver) method() (err error) {
	defer origin.Stamp(&err)
	return func() error { return context.Canceled }()
}

func generic[T any](v T) (err error) {
	defer origin.Stamp(&err)
	return fmt.Errorf("generic %v: %w", v, fs.ErrNotExist)
}

// A STAMP NAMES THE FUNCTION THAT DEFERRED IT, by the runtime's own frame: a
// plain function, a method and a generic function, each with its file's base
// name.
//
// Mutation: read the frame one level too high or too low and every row names
// the test or the package itself.
func TestAStampNamesTheFunctionThatDeferredIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{leaf(), here + "leaf"},
		{catcher(), here + "catcher"},
		{(&receiver{}).method(), here + "(*receiver).method"},
		{generic(3), here + "generic[...]"},
	} {
		place, ok := origin.Of(tc.err)
		if !ok || place.Function != tc.want || place.File != "origin_test.go" {
			t.Errorf("%v is stamped %+v (%v), want %s in origin_test.go", tc.err, place, ok, tc.want)
		}
	}
}

// THE DEEPEST STAMP WINS: a function that wraps an error already stamped below
// it keeps the stamp, so its own is a fallback for failures nobody below
// stamped.
//
// Mutation: let Stamp replace an existing stamp and the middle is named.
func TestTheDeepestStampWins(t *testing.T) {
	t.Parallel()
	place, ok := origin.Of(middle())
	if !ok || place.Function != here+"leaf" {
		t.Errorf("a chain stamped twice names %+v (%v), want the leaf", place, ok)
	}
}

// A STAMP CHANGES NOTHING A CALLER READS: the message is the error's own, and
// errors.Is and errors.As find what is beneath it.
//
// Mutation: drop Unwrap and every check fails; add the function to Error and
// the message one does.
func TestAStampIsTransparent(t *testing.T) {
	t.Parallel()
	err := middle()
	if err.Error() != "middle: leaf: "+errLeaf.Error() {
		t.Errorf("a stamped message reads %q", err.Error())
	}
	if !errors.Is(err, errLeaf) {
		t.Error("errors.Is does not see through a stamp")
	}
	var want *typed
	if !errors.As(catcher(), &want) || want.code != 7 {
		t.Error("errors.As does not see through a stamp")
	}
	if !errors.Is((&receiver{}).method(), context.Canceled) {
		t.Error("a stamped cancellation is not context.Canceled to errors.Is")
	}
}

// A STAMP DOES NOTHING ON SUCCESS, and nothing to a nil pointer.
func TestAStampIsANoOpOnSuccess(t *testing.T) {
	t.Parallel()
	if err := succeeds(); err != nil {
		t.Errorf("a function that succeeded returned %v", err)
	}
	origin.Stamp(nil)
	if _, ok := origin.Of(nil); ok {
		t.Error("nil carries a stamp")
	}
	if _, ok := origin.Of(errLeaf); ok {
		t.Error("an error nobody stamped carries a stamp")
	}
}

// A STAMP NEVER CARRIES ANYTHING BUT A FUNCTION OF THE ENGINE AND ITS FILE'S
// BASE NAME: nothing of the error's text, which here holds a key and an
// address, ever reaches it.
//
// Mutation: build the place from the message, or keep the frame's whole path,
// and this fails.
func TestAStampCarriesNothingButAnEngineFunction(t *testing.T) {
	t.Parallel()
	place, ok := origin.Of(leaf())
	if !ok {
		t.Fatal("no stamp")
	}
	for _, field := range []string{place.Function, place.File} {
		for _, leak := range []string{"sk-ant", "jane", "example.com", "company", "/"} {
			if field == place.Function && leak == "/" {
				continue
			}
			if strings.Contains(field, leak) {
				t.Errorf("a stamp carries %q: %+v", leak, place)
			}
		}
	}
	if !strings.HasPrefix(place.Function, "github.com/crewlet/crewlet/internal/") {
		t.Errorf("a stamp names %q, outside the engine's internal tree", place.Function)
	}
}
