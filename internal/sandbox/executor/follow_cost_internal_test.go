package executor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// presetEntries is a decoder that hands out one entry made in advance for each
// line it is fed, so what a read allocates is the reading's own cost alone.
type presetEntries struct {
	entries     []string
	given, seen int
}

func (d *presetEntries) Line([]byte)            { d.seen++ }
func (d *presetEntries) Skipped(int64, int)     {}
func (d *presetEntries) Result() sandbox.Result { return sandbox.Result{} }
func (d *presetEntries) Ended() bool            { return false }
func (d *presetEntries) Requests() int          { return 0 }
func (d *presetEntries) Entries(bool) []string {
	out := d.entries[d.given:d.seen]
	d.given = d.seen
	return out
}

// A LIVE READ ADDS ITS ENTRIES ONCE: the pending text a read's entries join is
// built once, so a read of tens of thousands of short events costs no more
// allocations than a read of one, and no more bytes than a few copies of the
// stream and its text, and the transcript holds every entry, in order.
//
// Not parallel: the counts are the process's ([allocationsOf]).
//
// Mutation: add each entry to the pending text on its own, and the read costs
// an allocation and a copy of the text so far per entry.
func TestALiveReadAddsItsEntriesOnce(t *testing.T) {
	read := func(n int) (objects, bytes uint64, text string) {
		b := sandbox.NewFakeSandbox("box-many-entries")
		p := PathsFor(b)
		b.Put(p.Stream(), strings.Repeat("{}\n", n))
		entries := make([]string, n)
		for i := range entries {
			entries[i] = "a step " + string(rune('a'+i%26))
		}
		var f follower
		objects, bytes = allocationsOf(func() {
			f = follower{lineBound: maxLineBytes, dec: &presetEntries{entries: entries}}
			if err := f.readEvents(t.Context(), b, p.Stream(), false); err != nil {
				t.Fatalf("readEvents: %v", err)
			}
		})
		return objects, bytes, f.transcript.text
	}
	const many = 20000
	oneObjects, oneBytes, _ := read(1)
	manyObjects, manyBytes, text := read(many)
	if lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n"); len(lines) != many ||
		lines[0] != "a step a" || lines[many-1] != "a step "+string(rune('a'+(many-1)%26)) {
		t.Fatalf("the transcript holds %d lines, %.20q first; want all %d entries in order",
			len(lines), lines[0], many)
	}
	if manyObjects > oneObjects+listSlack {
		t.Errorf("a read of %d entries costs %d allocations, and a read of one %d", many, manyObjects, oneObjects)
	}
	if limit := oneBytes + listCopies*uint64(3*many+len(text)); manyBytes > limit {
		t.Errorf("a read of %d entries, %d bytes of text, costs %d bytes allocated, more than %d copies of "+
			"its stream and text", many, len(text), manyBytes, listCopies)
	}
}
