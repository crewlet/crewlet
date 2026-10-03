package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// TestARefusalCodeOffTheWireIsAValue is the enum rule for a class that
// travels: a newer build may classify a refusal this one has no constant for,
// and it has to arrive as a value a reader can fall back from — never a decode
// error that loses the refusal's sentence along with its class.
func TestARefusalCodeOffTheWireIsAValue(t *testing.T) {
	t.Parallel()
	var got struct {
		Refusal Refusal `json:"refusal"`
	}
	if err := json.Unmarshal([]byte(`{"refusal":"quota_exhausted"}`), &got); err != nil {
		t.Fatalf("an unknown class did not decode: %v", err)
	}
	if got.Refusal != "quota_exhausted" {
		t.Fatalf("an unknown class decoded as %q, want it kept verbatim", got.Refusal)
	}
	if got.Refusal.Valid() {
		t.Fatal("a class this build has no constant for reports Valid")
	}
	// THE EMPTY CLASS IS NOT A CLASS: it is what an MCP server's failure
	// carries, and a reader treating it as valid would read "unclassified"
	// as a known answer.
	if Refusal("").Valid() {
		t.Fatal("the empty class reports Valid")
	}
}

// TestEveryRefusalClassIsKnownOnce holds the closed set a person's surface
// maps to statuses: every class is Valid, spelled as a lowercase wire token,
// and listed once — a duplicate is a table row a reader maps twice.
func TestEveryRefusalClassIsKnownOnce(t *testing.T) {
	t.Parallel()
	seen := map[Refusal]bool{}
	for _, r := range Refusals {
		if !r.Valid() {
			t.Errorf("%q is listed and not Valid", r)
		}
		if seen[r] {
			t.Errorf("%q is listed twice", r)
		}
		seen[r] = true
		for _, c := range r {
			if (c < 'a' || c > 'z') && c != '_' {
				t.Errorf("%q is not a lowercase wire token", r)
				break
			}
		}
	}
	if len(Refusals) != 15 {
		t.Errorf("%d classes, want the 15 the write surface's status table maps", len(Refusals))
	}
}

// A FAULT OF THE NODE IS NOT A CONDITION OF IT. The two classes send a caller
// opposite ways — unavailable is answered "come back", a fault is answered
// "the engine broke" — so a fault classed beneath its error must read back as
// itself, never as unavailable, and never as an outcome that may have landed:
// nothing about it says a write happened.
func TestAFaultIsNotUnavailable(t *testing.T) {
	t.Parallel()
	store := errors.New("open /var/lib/crewlet/replicated.db: disk I/O error")
	r := Result{Output: "the engine could not read it", Failed: true,
		Cause: Classify(RefusalInternalError, store)}
	if got := RefusalOf(r); got != RefusalInternalError {
		t.Errorf("a fault reads as %q, want %q", got, RefusalInternalError)
	}
	if errors.Is(r.Cause, RefusalUnavailable) {
		t.Error("a fault answers errors.Is against unavailable, so a surface " +
			"would tell its caller to come back")
	}
	if UnknownOf(r) {
		t.Error("a fault reads as an unknown outcome")
	}
	if !errors.Is(r.Cause, store) {
		t.Error("classing a fault lost the error the log names")
	}
}

// TestEveryClassIsReadOutOfTheCause holds the derivation for every class this
// build knows, in the two shapes a tool states one: the class AS the cause (a
// refusal with nothing underneath it), and the class BENEATH the error that
// decided it ([Classify]) — where the domain's own error must still answer
// [errors.Is], because a write surface branches on it. A class that read back
// as anything else, or a domain error lost under its class, is a status the
// write surface answers wrongly and an audit row that disagrees with it.
func TestEveryClassIsReadOutOfTheCause(t *testing.T) {
	t.Parallel()
	underneath := errors.New("tracker: no such task")
	for _, class := range Refusals {
		t.Run(string(class), func(t *testing.T) {
			t.Parallel()
			outright := Result{Output: "no", Failed: true, Cause: class}
			if got := RefusalOf(outright); got != class {
				t.Errorf("a cause that IS %q reads as %q", class, got)
			}
			beneath := Result{Output: "no", Failed: true, Cause: Classify(class, underneath)}
			if got := RefusalOf(beneath); got != class {
				t.Errorf("a cause classified %q reads as %q", class, got)
			}
			if !errors.Is(beneath.Cause, underneath) {
				t.Error("classifying a cause lost the error that decided it")
			}
			if beneath.Cause.Error() != underneath.Error() {
				t.Errorf("a classified cause reads %q, want the reason underneath it %q",
					beneath.Cause.Error(), underneath.Error())
			}
			// ONLY AN UNKNOWN OUTCOME IS UNKNOWN: a class — unavailable
			// among them — says the call was not served, and a reader
			// that took it for "may have landed" would ask somebody to
			// check on a write nobody made.
			if UnknownOf(outright) || UnknownOf(beneath) {
				t.Errorf("a %q refusal reads as an unknown outcome", class)
			}
		})
	}
}

// TestTheClassAToolStatesWins is the rule for a cause that carries two: the
// tool's own statement about the call, made with what it knows about the
// gesture, outranks a class somebody stated about a part of it underneath.
func TestTheClassAToolStatesWins(t *testing.T) {
	t.Parallel()
	inner := Classify(RefusalConflict, errors.New("lost the race"))
	got := RefusalOf(Result{Failed: true, Cause: Classify(RefusalStaleVersion, inner)})
	if got != RefusalStaleVersion {
		t.Errorf("the outer class is %q, want %q", got, RefusalStaleVersion)
	}
	// THE CLASS ITSELF, not something that wraps it: errors.Is would pass a
	// wrapped class, which is exactly the failure this asks about.
	if alone, ok := Classify(RefusalInvalid, nil).(Refusal); !ok || alone != RefusalInvalid { //nolint:errorlint // the unwrapped class is the assertion
		t.Error("classifying nothing is not the class alone")
	}
}

// TestAnUnknownOutcomeIsUnavailableAndUnknown holds the pairing a person's
// surface depends on: a write that may have landed is NOT served now
// (unavailable) and IS the caller's to find out about (unknown) — and stays
// both when a tool wraps it in its own error with its own facts.
func TestAnUnknownOutcomeIsUnavailableAndUnknown(t *testing.T) {
	t.Parallel()
	toolsOwn := fmt.Errorf("operation 0190: %w", ErrOutcomeUnknown)
	for name, cause := range map[string]error{
		"the sentinel":         ErrOutcomeUnknown,
		"a tool's own wrapper": toolsOwn,
		"beneath a step's error": errors.Join(ErrOutcomeUnknown,
			Classify(RefusalConflict, errors.New("the step lost its race"))),
	} {
		r := Result{Output: "may have landed", Failed: true, Cause: cause}
		if !UnknownOf(r) {
			t.Errorf("%s: an unknown outcome does not read as unknown", name)
		}
		if got := RefusalOf(r); got != RefusalUnavailable {
			t.Errorf("%s: an unknown outcome is classed %q, want %q", name, got, RefusalUnavailable)
		}
	}
}

// TestNothingClassifiesWhatNobodyClassified holds the two empty answers: an
// MCP server's failure carries no cause and reads as unclassified rather than
// a guess, and a cause with no class in its chain — an error a tool forgot to
// classify — reads the same way rather than as some default. And a SUCCESS is
// neither refused nor unknown, whatever it carries.
func TestNothingClassifiesWhatNobodyClassified(t *testing.T) {
	t.Parallel()
	cases := map[string]Result{
		"an MCP server's failure":   {Output: "upstream said no", Failed: true},
		"an unclassified cause":     {Output: "no", Failed: true, Cause: errors.New("bare")},
		"a success":                 {Output: "done"},
		"a success with a class":    {Output: "done", Cause: RefusalForbidden},
		"a success with an unknown": {Output: "done", Cause: ErrOutcomeUnknown},
	}
	for name, r := range cases {
		if got := RefusalOf(r); got != "" {
			t.Errorf("%s reads as %q, want unclassified", name, got)
		}
		if UnknownOf(r) {
			t.Errorf("%s reads as an unknown outcome", name)
		}
	}
}
