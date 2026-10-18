package statelog_test

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// AN OPERATION ID CARRIES THE INSTANT IT WAS MINTED, so a retry that reuses the
// id reuses the instant — and nothing a retry does can move it later.
//
// This is the property the pre-adoption arm rests on. It was a field beside the
// id once, filled by every writer from its own clock at the call, so a retry
// after an adoption stated an instant after the adoption for an operation
// minted before it.
func TestAnOperationIDCarriesTheInstantItWasMinted(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 23, 10, 11, 12, 345_678_901, time.UTC)
	truncated := at.Truncate(time.Millisecond)

	t.Run("a fresh id answers its own instant, truncated rather than rounded", func(t *testing.T) {
		t.Parallel()
		id := statelog.NewOpID(at, "update-task-1")
		got, ok := statelog.OpMintedAt(id)
		if !ok {
			t.Fatalf("OpMintedAt(%q) found no instant in an id NewOpID minted", id)
		}
		if !got.Equal(truncated) {
			t.Fatalf("OpMintedAt = %s, want %s — the instant must never read LATER "+
				"than the mint, which rounding up would", got, truncated)
		}
		if again := statelog.NewOpID(at, "update-task-1"); again == id {
			t.Fatal("two fresh ids minted at one instant under one name are the " +
				"same id — a fresh id is one operation, and two operations " +
				"sharing one would have the ledger collapse the second into the " +
				"first")
		}
	})

	t.Run("an id names what it is, for whoever reads the ledger", func(t *testing.T) {
		t.Parallel()
		for name, id := range map[string]string{
			"fresh":   statelog.NewOpID(at, "update-task-1"),
			"derived": statelog.DeriveOpID(at, "update-task-1", "seed"),
		} {
			if !strings.HasSuffix(id, ".update-task-1") {
				t.Errorf("the %s id %q does not name its verb and object — a "+
					"stuck operation in the ledger is found by what it did", name, id)
			}
		}
		if bare := statelog.NewOpID(at, ""); len(bare) != len(uuid.NewString()) {
			t.Errorf("an unnamed id is %q, want a bare uuid — it is the id a "+
				"page comment is addressed by", bare)
		}
	})

	t.Run("a derived id is reproduced by a retry and still carries its instant", func(t *testing.T) {
		t.Parallel()
		one := statelog.DeriveOpID(at, "update-task-1", "work-key", "update", "task-1")
		two := statelog.DeriveOpID(at, "update-task-1", "work-key", "update", "task-1")
		if one != two {
			t.Fatalf("DeriveOpID gave %q then %q for one unit of work — a re-run "+
				"that derives a different id is a second operation", one, two)
		}
		got, ok := statelog.OpMintedAt(one)
		if !ok || !got.Equal(truncated) {
			t.Fatalf("OpMintedAt(derived) = (%s, %v), want %s", got, ok, truncated)
		}
		for name, other := range map[string]string{
			"another object":  statelog.DeriveOpID(at, "update-task-1", "work-key", "update", "task-2"),
			"another verb":    statelog.DeriveOpID(at, "update-task-1", "work-key", "comment", "task-1"),
			"parts regrouped": statelog.DeriveOpID(at, "update-task-1", "work-ke", "yupdate", "task-1"),
			"another name":    statelog.DeriveOpID(at, "update-task-2", "work-key", "update", "task-1"),
			"another instant": statelog.DeriveOpID(at.Add(time.Second), "update-task-1", "work-key", "update", "task-1"),
		} {
			if other == one {
				t.Errorf("%s derived the same id — two operations the ledger would "+
					"collapse into one", name)
			}
		}
	})

	t.Run("a step inherits its gesture's instant", func(t *testing.T) {
		t.Parallel()
		gesture := statelog.NewOpID(at, "create-task-1")
		step := statelog.StepOpID(statelog.StepOpID(gesture, "counter"), "retry")
		if step == gesture {
			t.Fatal("a step shares its gesture's id, so the second append would " +
				"resolve against the first's ledger row")
		}
		got, ok := statelog.OpMintedAt(step)
		if !ok || !got.Equal(truncated) {
			t.Fatalf("OpMintedAt(step) = (%s, %v), want the gesture's %s", got, ok, truncated)
		}
	})

	t.Run("an instant before the epoch reads as the epoch, the conservative end", func(t *testing.T) {
		t.Parallel()
		for name, instant := range map[string]time.Time{
			"the zero time":     {},
			"before the epoch":  time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC),
			"exactly the epoch": time.Unix(0, 0),
		} {
			got, ok := statelog.OpMintedAt(statelog.DeriveOpID(instant, "x"))
			if !ok || !got.Equal(time.Unix(0, 0).UTC()) {
				t.Errorf("%s: OpMintedAt = (%s, %v), want the epoch — an instant "+
					"the layout cannot hold must read as early as it can, never later",
					name, got, ok)
			}
		}
	})

	t.Run("an id the engine did not mint carries no instant", func(t *testing.T) {
		t.Parallel()
		for name, id := range map[string]string{
			"a literal":            "op-1",
			"empty":                "",
			"a random uuid":        uuid.NewString(),
			"a name-based uuid":    uuid.NewSHA1(uuid.NameSpaceURL, []byte("x")).String(),
			"a suffix with no dot": statelog.NewOpID(at, "") + "x",
			"a v7 in braces":       "{" + statelog.NewOpID(at, "") + "}",
		} {
			if got, ok := statelog.OpMintedAt(id); ok {
				t.Errorf("%s (%q) answered an instant %s — an id that carries none "+
					"must say so, or it is read as minted whenever its bytes happen "+
					"to decode to", name, id, got)
			}
		}
	})
}

// A GENERATION RECORD'S ID IS ONE OPERATION PER NODE: the same on every re-run
// of one node's reanchor, and a different one for a peer deriving the same
// generation from the same stream.
//
// A clustered broker checks its duplicate window before the append's
// expectation, so a peer sharing the id was acknowledged as a duplicate of the
// first node's record — as though its own had landed — and opened the
// generation over its own rows too. The transition reads back whose record
// landed whatever the broker answered; this is the id no longer inviting it.
func TestAGenerationRecordsIDIsOneOperationPerNode(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 23, 10, 11, 12, 345_678_901, time.UTC)
	facts := func(writer string) statelog.GenerationFacts {
		return statelog.GenerationFacts{Generation: 2, Writer: writer, By: "ops-1",
			Inputs: statelog.ReanchorInputs{StreamCreatedAt: created}}
	}
	one := facts("node-a").OpID()
	if again := facts("node-a").OpID(); again != one {
		t.Fatalf("a re-run on one node derived %q after %q — the re-run must "+
			"find its own earlier attempt as the same operation", again, one)
	}
	if peer := facts("node-b").OpID(); peer == one {
		t.Fatalf("two nodes deriving generation 2 from one stream share the id %q "+
			"— a clustered broker acknowledges the second as a duplicate of the "+
			"first's record", one)
	}
	next := facts("node-a")
	next.Generation = 3
	if next.OpID() == one {
		t.Fatal("one node's reanchors to two generations derived one id")
	}
	got, ok := statelog.OpMintedAt(one)
	if !ok || !got.Equal(created.Truncate(time.Millisecond)) {
		t.Fatalf("OpMintedAt = (%s, %v), want the adopted stream's creation %s — "+
			"an instant every operator reads identically off the broker",
			got, ok, created.Truncate(time.Millisecond))
	}
}

// A VALUE WRITTEN INTO AN OPERATION'S NAME IS ONE STEP OF IT, AND NO TWO
// VALUES ARE WRITTEN ALIKE.
//
// A dot in a name begins a step, so a path written as itself made a file
// write's id read as a name and a step; a value past the bound or carrying a
// space put into every ledger row and message-id header an id the engine's
// own rule for a caller's id refuses. [OpNamePart] writes an identifier as
// itself and everything else escaped — and escaped injectively, so a value
// that spells another's escape is escaped too rather than kept.
//
// Mutation: return the value unchanged (the escaped rows fail), drop the
// bound (the 65-byte and 1024-byte rows are kept), or drop the escape
// marker's guard (the row spelling another's escape is kept, and two values
// share a part).
func TestAValueIsWrittenIntoAnOperationNameAsOneStep(t *testing.T) {
	t.Parallel()
	asItself := []string{"ENG-1", uuid.NewString(), "ci:release", "workspace",
		"n" + strings.Repeat("x", 62) + "9"}
	escaped := []string{"ENG/docs/a.md", "a.b", "docs/My Notes.md", "\u00fc.md",
		"~abc", "", "tab\tseparated", "n" + strings.Repeat("x", 63) + "9",
		"docs/" + strings.Repeat("a", 1016) + ".md",
		statelog.OpNamePart("a.b")}
	for _, value := range asItself {
		if got := statelog.OpNamePart(value); got != value {
			t.Errorf("OpNamePart(%q) = %q — an identifier the engine writes "+
				"into a name should read in the ledger as itself", value, got)
		}
	}
	segment := regexp.MustCompile(`^[!-~]{1,64}$`)
	spelled := map[string]string{}
	for _, value := range append(append([]string{}, asItself...), escaped...) {
		part := statelog.OpNamePart(value)
		if !segment.MatchString(part) || strings.Contains(part, ".") {
			t.Errorf("OpNamePart(%q) = %q, which is not one segment of at most %d "+
				"bytes of visible ASCII with no dot", value, part,
				statelog.MaxOpNamePartBytes)
		}
		if other, dup := spelled[part]; dup {
			t.Errorf("%q and %q are both written %q — two objects would name "+
				"one operation", value, other, part)
		}
		spelled[part] = value
	}
	for _, value := range escaped {
		if got := statelog.OpNamePart(value); got == value {
			t.Errorf("OpNamePart(%q) kept the value as itself, which is not a "+
				"segment the grammar may hold", value)
		}
	}
}

// EVERY SEGMENT AN ID IS MINTED WITH IS ONE STEP OF IT — its name, and each
// step a gesture appends.
//
// Nothing downstream holds the grammar, so the mint does: a name or a step
// that holds a dot is written as one escaped segment rather than spelling
// steps of its own, a name at the bound is kept and one past it escaped, and
// [OpNamed] compares through the same rule, so a name the mint had to escape
// still names its own id. A step that held a dot made two gestures one id:
// `x` with the step `y.z`, and `x.y` with the step `z`.
//
// Mutation: drop the rule from the name's writer, the step's or [OpNamed],
// and the matching assertion fails.
func TestEverySegmentAnIDIsMintedWithIsOneStepOfIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	atBound := "update-" + strings.Repeat("x", statelog.MaxOpSegmentBytes-len("update-"))
	for _, name := range []string{"file-ENG/docs/a.md", "x.y", "docs/My Notes",
		"~update-ENG-1", atBound + "x", atBound, "update-ENG-1"} {
		for shape, id := range map[string]string{
			"minted":  statelog.NewOpID(at, name),
			"derived": statelog.DeriveOpID(at, name, "identity"),
		} {
			segment := id[37:]
			if strings.Contains(segment, ".") || len(segment) > statelog.MaxOpSegmentBytes {
				t.Errorf("the %s id %q names %q in %q — more than one segment",
					shape, id, name, segment)
			}
			if err := statelog.CheckCallerOpID(id); err != nil {
				t.Errorf("the %s id named %q is one the engine refuses back: %v",
					shape, name, err)
			}
			if !statelog.OpNamed(id, name) {
				t.Errorf("the %s id %q is not named %q — a surface holding an id "+
					"to its call would refuse its own", shape, id, name)
			}
			readable := name == atBound || name == "update-ENG-1"
			if readable != (segment == name) {
				t.Errorf("the %s id named %q writes %q: a segment within the bound "+
					"is written as itself, and only one", shape, name, segment)
			}
		}
	}
	gesture := statelog.NewOpID(at, "evict-node-4")
	for _, step := range []string{"y.z", "", "a b", "~c"} {
		id := statelog.StepOpID(gesture, step)
		if tail := strings.TrimPrefix(id, gesture+"."); strings.Contains(tail, ".") {
			t.Errorf("StepOpID(%q, %q) = %q, a step that spells more than one", gesture,
				step, id)
		}
	}
	if statelog.StepOpID("x", "y.z") == statelog.StepOpID("x.y", "z") {
		t.Error("the gesture `x` with the step `y.z` and the gesture `x.y` with " +
			"the step `z` are one id — one ledger row answering two gestures")
	}
	if got := statelog.StepOpID(gesture, "evict", "tracker:node-4"); got !=
		gesture+".evict.tracker:node-4" {
		t.Errorf("StepOpID wrote %q, want steps that are already segments as "+
			"themselves", got)
	}
}

// AN ID IS THE CALL IT WAS MINTED FOR, OR IT IS NOTHING.
//
// A surface that names the call in an id's name holds a brought-back id to
// exactly that name: the same name with a step after it is a step's id — a NEW
// call's operation — and a name that merely starts or ends the same way is
// another call's. Each of those passing would answer a second gesture as the
// first.
func TestAnOperationIDIsNamedForExactlyOneCall(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	const name = "work-purge-task-1-0123456789abcdef"
	minted := statelog.NewOpID(at, name)
	for id, want := range map[string]bool{
		minted: true,
		statelog.DeriveOpID(at, name, "identity"): true,
		statelog.StepOpID(minted, "evict"):        false,
		statelog.NewOpID(at, name+"0"):            false,
		statelog.NewOpID(at, "work-purge-task-1"): false,
		statelog.NewOpID(at, "x"+name[1:]):        false,
		statelog.NewOpID(at, ""):                  false,
		minted[:36] + "-" + name:                  false,
		"0" + minted:                              false,
	} {
		if got := statelog.OpNamed(id, name); got != want {
			t.Errorf("OpNamed(%q, %q) = %v, want %v", id, name, got, want)
		}
	}
	if statelog.OpNamed(statelog.NewOpID(at, ""), "") {
		t.Error("an unnamed id was read as named for the empty call — a surface " +
			"that names its calls has no call without a name")
	}
}

// A KEYED WRITE IS ONE OPERATION FOR ONE ASK, and another for anything else.
//
// The ledger answers an operation it already holds before the write is
// decided, so two writes under one id are one write: the second is answered
// with the first's result and nothing of it lands. Each part of the
// derivation is what keeps two different writes under one key apart — the
// verb (a save and the rename after it), the object (two pages), and what
// the write says (a second save with another body) — while a retry, which
// resends all of it, has to land on the first attempt's id. And the content
// is digested through its JSON encoding, so a map built in another order is
// the same ask.
//
// Mutation: drop any part from the identity and its row collapses two writes.
func TestAKeyedWriteIsOneOperationForOneAsk(t *testing.T) {
	t.Parallel()
	since := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	key := statelog.NewOpID(since, "")
	type ask struct{ Body, Title string }
	for shape, keyed := range keyedShapes {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			derive := func(key, verb, object string, content any) string {
				t.Helper()
				id, err := keyed(key, verb, object, content)
				if err != nil {
					t.Fatalf("derive (%q, %q, %q): %v", key, verb, object, err)
				}
				return id
			}
			first := derive(key, "save", "page-1", ask{Body: "v2"})
			if again := derive(key, "save", "page-1", ask{Body: "v2"}); again != first {
				t.Errorf("the same write derived %q and then %q — a retry would "+
					"land twice", first, again)
			}
			ordered := derive(key, "save", "page-1", map[string]any{"a": 1, "b": 2})
			reordered := derive(key, "save", "page-1", map[string]any{"b": 2, "a": 1})
			if ordered != reordered {
				t.Errorf("one map built in two orders derived %q and %q — a re-run "+
					"decoding the same call would be a second write", ordered, reordered)
			}
			for part, other := range map[string]string{
				"another key": derive(statelog.NewOpID(since, ""), "save", "page-1",
					ask{Body: "v2"}),
				"another verb":    derive(key, "rename", "page-1", ask{Body: "v2"}),
				"another object":  derive(key, "save", "page-2", ask{Body: "v2"}),
				"another content": derive(key, "save", "page-1", ask{Body: "v3"}),
			} {
				if other == first {
					t.Errorf("a write under %s derived the first write's id %q — "+
						"the ledger would answer it as that write and drop it",
						part, first)
				}
			}
		})
	}
}

// keyedShapes are the two shapes a keyed write's id takes, which one rule
// derives: an operation's, named by its verb, and an object's own.
var keyedShapes = map[string]func(key, verb, object string, content any) (string, error){
	"operation": statelog.KeyedOpID, "object": statelog.KeyedObjectOpID,
}

// A KEYED ID CARRIES ITS KEY'S INSTANT, NAMED BY ITS VERB — or unnamed where
// it is also the id of the object it makes.
//
// The instant is what the ledger vouches for a retry by, so an id that did
// not carry the key's own would be read as minted at some other moment: at
// the epoch, and answered `unknown` on every node whose ledger ever lost a
// row; or at the call, and decided again after the loss. The name is how a
// reader of the ledger finds the operation; an object's own id carries none,
// because a reader addresses the object by it — and the two shapes of one
// ask are two ids, so a comment and an operation under one key never collide.
//
// Mutation: derive at the zero instant, name the object's id, or derive the
// two shapes alike, and the matching assertion fails.
func TestAKeyedIDCarriesItsKeysInstant(t *testing.T) {
	t.Parallel()
	since := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	key := statelog.NewOpID(since, "")
	op, err := statelog.KeyedOpID(key, "comment-edit", "c-1", "words")
	if err != nil {
		t.Fatalf("KeyedOpID: %v", err)
	}
	object, err := statelog.KeyedObjectOpID(key, "comment-edit", "c-1", "words")
	if err != nil {
		t.Fatalf("KeyedObjectOpID: %v", err)
	}
	for shape, id := range map[string]string{"operation": op, "object": object} {
		minted, ok := statelog.OpMintedAt(id)
		if !ok || !minted.Equal(since) {
			t.Errorf("the keyed %s id %q carries (%s, %v), want the key's own %s",
				shape, id, minted, ok, since)
		}
	}
	if !statelog.OpNamed(op, "comment-edit") {
		t.Errorf("the operation id %q is not named for its verb — a reader of "+
			"the ledger could not find what it did", op)
	}
	if len(object) != len(key) {
		t.Errorf("the object's id %q carries a name — a reader addresses the "+
			"object by it", object)
	}
	if object == op[:len(object)] {
		t.Errorf("the object's id and the operation's share the uuid %q — the "+
			"two shapes of one ask must be two ids", object)
	}
}

// A KEYED WRITE REFUSES WHAT IT CANNOT DERIVE FROM, rather than deriving an id
// that reads as something else.
//
// A key that carries no instant would derive an id read as minted before
// every loss — every such write `unknown` once the ledger had swept anything,
// and never published. A verb that is not a name would put a step in the
// grammar, or no name at all. Content that will not encode has no digest to
// bind. Each is refused, with no id.
func TestAKeyedWriteRefusesWhatItCannotDeriveFrom(t *testing.T) {
	t.Parallel()
	key := statelog.NewOpID(time.Now(), "")
	for name, c := range map[string]struct {
		key, verb string
		content   any
	}{
		"no key":                           {key: "", verb: "save"},
		"a key with no instant":            {key: uuid.NewString(), verb: "save"},
		"a key that is not an id":          {key: "my-retry-key", verb: "save"},
		"a key with a space before it":     {key: " " + key, verb: "save"},
		"no verb":                          {key: key, verb: ""},
		"a verb that reads as a step":      {key: key, verb: "comment.edit"},
		"a verb that reads as an escape":   {key: key, verb: "~save"},
		"content that will not encode":     {key: key, verb: "save", content: make(chan int)},
		"a number JSON has no spelling of": {key: key, verb: "save", content: math.NaN()},
	} {
		for shape, derive := range keyedShapes {
			id, err := derive(c.key, c.verb, "page-1", c.content)
			if err == nil || id != "" {
				t.Errorf("%s: the %s derivation answered (%q, %v), want a refusal "+
					"and no id", name, shape, id, err)
			}
		}
	}
}
