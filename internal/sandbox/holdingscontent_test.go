package sandbox

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"testing"
)

// contentCall is an OpenCode call as an ask lists it: keyed by its place in
// its log, its arguments' key whole, its output as the log keeps it.
func contentCall(appended int, output string) LoggedCall {
	return LoggedCall{ID: strconv.Itoa(appended), Asked: true, Output: output, Appended: appended,
		Seq: appended, Name: "read_issue", Args: ArgsKey{Whole: true, Sum: sha256.Sum256([]byte(`{"n":1}`))}}
}

// classOf is c's class, which the case states.
func classOf(t *testing.T, c LoggedCall) CallClass {
	t.Helper()
	class, ok := ClassOf(c)
	if !ok {
		t.Fatalf("the premise: ClassOf(%+v) states no class", c)
	}
	return class
}

// cutOf is output as the log keeps it past its share: its opening and a
// marker, beside the whole output's size.
func cutOf(t *testing.T, output string) (kept string, whole int) {
	t.Helper()
	call := keptCall(BridgeCall{Output: output}, MaxBridgeArgsBytes)
	if call.OutputBytes != len(output) || len(call.Output) >= len(output) {
		t.Fatalf("the premise: the log keeps %d bytes of an output of %d and says it was %d, want it cut",
			len(call.Output), len(output), call.OutputBytes)
	}
	return call.Output, call.OutputBytes
}

// A CALL WHOSE ARGUMENTS THE LOG CUT IS NEVER HELD. A call's class is stated
// only where the log kept its arguments whole and they state them, the call
// was not held for a person's approval, and a cut output's opening reads back
// exactly: arguments cut, withheld, unparsed or unreadable (all ArgsKey
// unwhole), a held call, a cut output whose marker is not its cut, and an
// output whose size is no size each state no class, so no reading holds the
// call.
//
// Mutation: key the class whatever the arguments' key says, key a held call,
// or key a cut output whose opening is not read back, and a case goes red.
func TestACallWhoseArgumentsTheLogCutIsNeverHeld(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a line of the tool's long answer\n", 400)
	kept, whole := cutOf(t, long)
	edited := func(edit func(*LoggedCall)) LoggedCall {
		c := contentCall(3, "ok")
		edit(&c)
		return c
	}
	for name, tc := range map[string]struct {
		call   LoggedCall
		stated bool
	}{
		"arguments kept whole, an output kept whole": {call: edited(func(*LoggedCall) {}), stated: true},
		"arguments kept whole, an output cut": {call: edited(func(c *LoggedCall) {
			c.Output, c.OutputBytes = kept, whole
		}), stated: true},

		"arguments the log did not keep whole": {call: edited(func(c *LoggedCall) { c.Args.Whole = false })},
		"a call held for a person's approval":  {call: edited(func(c *LoggedCall) { c.Held = true })},
		"a cut output whose marker states another size": {call: edited(func(c *LoggedCall) {
			c.Output, c.OutputBytes = kept, 2*whole
		})},
		"an output the log says it cut with no marker": {call: edited(func(c *LoggedCall) {
			c.Output, c.OutputBytes = long[:100], whole
		})},
		"an output whose size is no size": {call: edited(func(c *LoggedCall) { c.OutputBytes = -1 })},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			class, stated := ClassOf(tc.call)
			if stated != tc.stated {
				t.Fatalf("ClassOf = %+v, %v; want stated %v", class, stated, tc.stated)
			}
			if !stated {
				return
			}
			reading := SessionHoldings{Read: true, Kind: HoldingsByContent,
				Members: map[string]CallClass{tc.call.ID: class},
				Classes: map[CallClass]ClassRecords{class: {Calls: 1, Parts: 1, Held: true}}}
			if held, _ := reading.Holds(tc.call); !held {
				t.Errorf("a stated call of a held class is told")
			}
		})
	}
}

// A WHOLE OUTPUT IS MATCHED WHOLE. Two outputs the log kept whole are one
// class only where every byte is alike: two alike for far more than
// HoldingsOpeningBytes and otherwise past it are two classes, and so are two
// of one size and another text.
//
// Mutation: key a whole output by an opening, or by its size alone, and this
// goes red.
func TestAWholeOutputIsMatchedWhole(t *testing.T) {
	t.Parallel()
	opened := strings.Repeat("o", 3*HoldingsOpeningBytes)
	one := classOf(t, contentCall(3, opened+"tail"))
	if again := classOf(t, contentCall(4, opened+"tail")); again != one {
		t.Errorf("one output kept whole keys two classes: %+v and %+v", one, again)
	}
	if !one.Text.Whole || one.Text.Bytes != len(opened)+4 || one.Text.Opening != 0 {
		t.Errorf("an output kept whole keys %+v, want whole, its size, no opening", one.Text)
	}
	for name, other := range map[string]string{
		"alike past the opening a holding by id compares, and otherwise after": opened + "TAIL",
		"of one size and another text":                                         strings.Repeat("x", len(opened)+4),
	} {
		if classOf(t, contentCall(4, other)) == one {
			t.Errorf("%s: keyed the class of the other output", name)
		}
	}
}

// A CUT OUTPUT IS MATCHED BY ALL THE LOG KEPT OF IT. An output the log cut is
// keyed by its whole size and every byte of the opening the log kept: two of
// one size alike for HoldingsOpeningBytes and otherwise before the kept
// opening's end are two classes; two of one size alike through the kept
// opening are one, whatever follows it, which the log never saw; two of
// another size are two.
//
// Mutation: key a cut output by its first HoldingsOpeningBytes alone, or by
// its opening and not its size, and this goes red.
func TestACutOutputIsMatchedByAllTheLogKeptOfIt(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a line of the tool's long answer\n", 400)
	kept, whole := cutOf(t, long)
	opening := strings.Index(kept, "…\n(")
	if opening <= HoldingsOpeningBytes+1 {
		t.Fatalf("the premise: the log keeps %d bytes of the opening, want past %d", opening, HoldingsOpeningBytes)
	}
	cut := func(output string) LoggedCall {
		c := contentCall(3, "")
		c.Output, c.OutputBytes = cutOf(t, output)
		return c
	}
	one := classOf(t, LoggedCall{Name: "read_issue", Args: contentCall(3, "").Args, Output: kept,
		OutputBytes: whole})
	if one.Text.Whole || one.Text.Bytes != whole || one.Text.Opening != opening ||
		one.Text.Sum != sha256.Sum256([]byte(long[:opening])) {
		t.Fatalf("a cut output keys %+v, want its size %d and all %d bytes of the opening kept", one.Text, whole,
			opening)
	}
	edit := func(at int) string { return long[:at] + "X" + long[at+1:] }
	for name, tc := range map[string]struct {
		output string
		same   bool
	}{
		"alike through the kept opening, otherwise past it": {output: edit(opening + 10), same: true},
		"alike for the opening a holding by id compares, otherwise before the kept opening's end": {
			output: edit(opening - 1)},
		"alike for the kept opening, a byte longer": {output: long + "z"},
	} {
		if same := classOf(t, cut(tc.output)) == one; same != tc.same {
			t.Errorf("%s: keyed the class of the logged output %v, want %v", name, same, tc.same)
		}
	}
}

// A CONTENT READING TELLS EVERY CALL IN THE MAIN LIST. A reading of classes
// holds a call only where the ask's key names a member whose class is the
// call's own and that class is held whole: Calls at least one, Parts equal to
// it, not wild. A call keyed by its ID is held whatever its Appended says;
// everything else is told, a call held for a person's approval and one whose
// arguments the log did not keep included, and every call is told in the main
// list, whatever subagents the reading names.
//
// Mutation: read members by Appended rather than the ask's ID, hold a member
// whose class is not its own, hold a class whose counts contradict its
// verdict, or tell a call under a subagent, and a case goes red.
func TestAContentReadingTellsEveryCallInTheMainList(t *testing.T) {
	t.Parallel()
	call := contentCall(3, "the issue's title")
	class := classOf(t, call)
	other := classOf(t, contentCall(5, "another answer"))
	reading := func(records ClassRecords) SessionHoldings {
		return SessionHoldings{Read: true, Kind: HoldingsByContent,
			Members:   map[string]CallClass{"3": class, "5": other, "": class},
			Classes:   map[CallClass]ClassRecords{class: records, other: {Calls: 1, Parts: 1, Held: true}},
			Subagents: map[string]SubagentThread{"a0123": {}},
			Calls:     map[string]CallRecords{"3": {Thread: "a0123", Threads: 1}}}
	}
	held := ClassRecords{Calls: 2, Parts: 2, Held: true}
	edited := func(edit func(*LoggedCall)) LoggedCall {
		c := call
		edit(&c)
		return c
	}
	for name, tc := range map[string]struct {
		reading SessionHoldings
		call    LoggedCall
		held    bool
	}{
		"a member of a held class": {reading: reading(held), call: call, held: true},
		"a member keyed by its ID, whose Appended is another number": {reading: reading(held),
			call: edited(func(c *LoggedCall) { c.Appended = 5 }), held: true},

		"a call whose Appended names a member, keyed otherwise": {reading: reading(held),
			call: edited(func(c *LoggedCall) { c.ID = "7" })},
		"no member under its key": {reading: reading(held), call: edited(func(c *LoggedCall) { c.ID = "9" })},
		"a call with no key":      {reading: reading(held), call: edited(func(c *LoggedCall) { c.ID = "" })},
		"a member whose class is another call's": {reading: reading(held),
			call: edited(func(c *LoggedCall) { c.Output = "another text" })},
		"a member held for a person's approval": {reading: reading(held),
			call: edited(func(c *LoggedCall) { c.Held = true })},
		"a member whose arguments the log did not keep whole": {reading: reading(held),
			call: edited(func(c *LoggedCall) { c.Args.Whole = false })},
		"a class told":                 {reading: reading(ClassRecords{Calls: 2, Parts: 1}), call: call},
		"a class held, one part short": {reading: reading(ClassRecords{Calls: 2, Parts: 1, Held: true}), call: call},
		"a class held, a part over":    {reading: reading(ClassRecords{Calls: 2, Parts: 3, Held: true}), call: call},
		"a class held, no call":        {reading: reading(ClassRecords{Held: true}), call: call},
		"a class held and wild": {reading: reading(ClassRecords{Calls: 2, Parts: 2, Held: true, Wild: true}),
			call: call},
		"a class the reading has no verdict on": {reading: func() SessionHoldings {
			r := reading(held)
			delete(r.Classes, class)
			return r
		}(), call: call},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, under := tc.reading.Holds(tc.call)
			if got != tc.held || under != "" {
				t.Fatalf("Holds = %v, under %q; want held %v, in the main list", got, under, tc.held)
			}
		})
	}
}

// A READING NEVER ANSWERS FOR THE OTHER KIND. A reading by id answers from
// its records by id alone, and a reading by content from its classes alone:
// each holding the facts of both holds a call only through its own, and a
// reading of no kind this build reads holds nothing.
//
// Mutation: read Classes or Calls whatever the kind, and a case goes red.
func TestAReadingNeverAnswersForTheOtherKind(t *testing.T) {
	t.Parallel()
	call := contentCall(3, "the issue's title")
	class := classOf(t, call)
	byID := map[string]CallRecords{call.ID: recordsOf(call.Output, false)}
	members := map[string]CallClass{call.ID: class}
	classes := map[CallClass]ClassRecords{class: {Calls: 1, Parts: 1, Held: true}}
	for name, tc := range map[string]struct {
		reading SessionHoldings
		held    bool
	}{
		"by id, its records": {reading: SessionHoldings{Read: true, Kind: HoldingsByID, Calls: byID},
			held: true},
		"by content, its classes": {reading: SessionHoldings{Read: true, Kind: HoldingsByContent,
			Members: members, Classes: classes}, held: true},

		"by id, holding classes alone": {reading: SessionHoldings{Read: true, Kind: HoldingsByID,
			Members: members, Classes: classes}},
		"by content, holding records alone": {reading: SessionHoldings{Read: true, Kind: HoldingsByContent,
			Calls: byID}},
		"of no kind this build reads, holding both": {reading: SessionHoldings{Read: true, Kind: "by_name",
			Calls: byID, Members: members, Classes: classes}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if held, under := tc.reading.Holds(call); held != tc.held || under != "" {
				t.Fatalf("Holds = %v, under %q; want held %v, in the main list", held, under, tc.held)
			}
		})
	}
}

// WHERE A RUN BEGAN IS THE CAPTURE'S WORD AND NEVER THE CALLER'S. The reader
// is handed, on the archive it reads, where the run began as the kept capture
// the reading is held to stamped it: never zero where the capture names one,
// and never another capture's, a newer one of its job begun beside it with a
// start of its own included. The caller hands only turn, places and ask, so
// nothing it passes reaches the reader as the start.
//
// Mutation: leave the archive's RunStart zero, or take it from the run's
// newest capture rather than the kept one, and a case goes red.
func TestWhereARunBeganIsTheCapturesWordAndNeverTheCallers(t *testing.T) {
	t.Parallel()
	for name, newer := range map[string]bool{
		"its capture alone":                        false,
		"beside a newer capture of its job, begun": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newHoldingsWorld(t, false)
			if newer {
				begun := w.kept
				begun.CaptureSeq, begun.State, begun.At = w.kept.CaptureSeq+1, CaptureBegun, w.rig.now
				begun.RunAfter = "msg_02newer"
				if took, err := w.rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
					t.Fatalf("stamping the newer capture = %v, %v", took, err)
				}
				if got := w.rig.get("t1"); got.Capture == nil || got.Capture.RunAfter != begun.RunAfter {
					t.Fatalf("the premise: the record's newest capture is %+v, want the begun one", got.Capture)
				}
			}
			want := w.kept.RunStart()
			if !want.Known() {
				t.Fatalf("the premise: the kept capture names no start, %+v", want)
			}
			got, err := w.rig.coordinator.HoldingsOf(t.Context(), "t1", w.kept.LaunchSeq, w.kept.CaptureSeq,
				askingAbout("toolu_01a"), w.reader)
			if err != nil || !got.Read {
				t.Fatalf("HoldingsOf = %+v, %v; want a reading", got, err)
			}
			if asked, _ := w.reader.reads(); len(asked) != 1 || asked[0].RunStart != want {
				t.Fatalf("the reader was handed %+v, want once the kept capture's start %+v", asked, want)
			}
		})
	}
}
