package code

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A ROW'S COMPUTED SIZE IS ITS ENCODING'S LENGTH, EXACTLY. A page is held to
// its byte budget by these sizes, so a size short of the encoding lets a page
// past the reply ceiling and one past it cuts pages short. Each row type is
// filled AT RANDOM, EVERY FIELD by reflection — so a field added to a row
// without its term in rowbytes.go is filled here and the sizes part — and
// compared with json.Marshal, hundreds of times over strings that take every
// escape encoding/json writes.
func TestARowsComputedSizeIsItsEncodingsLength(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	check := func(name string, row any, got int) {
		t.Helper()
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("encode a %s: %v", name, err)
		}
		if got != len(b) {
			t.Fatalf("a %s computes to %d bytes and encodes to %d:\n%s", name, got, len(b), b)
		}
	}
	for range 400 {
		var ref RefRow
		fill(rng, reflect.ValueOf(&ref).Elem())
		check("ref", ref, ref.encodedBytes())

		var pack PackRow
		fill(rng, reflect.ValueOf(&pack).Elem())
		check("pack", pack, pack.encodedBytes())

		var summary RepoSummary
		fill(rng, reflect.ValueOf(&summary).Elem())
		check("listing row", summary, summary.encodedBytes())

		var push PushRow
		fill(rng, reflect.ValueOf(&push).Elem())
		commands, err := json.Marshal(push.Commands)
		if err != nil {
			t.Fatal(err)
		}
		check("push", push, push.encodedBytes(len(commands)))
		brief := push.brief()
		check("push in brief", brief, brief.encodedBytes(brief.commandsBytes()))
		// AND ONE CUT, its omitted commands counted: fill draws at most
		// five commands, which a brief row carries whole.
		push.Omitted = CommandCounts{}
		push.Commands = make([]Command, BriefCommands+1+rng.IntN(3*BriefCommands))
		for i := range push.Commands {
			fill(rng, reflect.ValueOf(&push.Commands[i]).Elem())
			push.Commands[i].Result = CommandResults()[rng.IntN(len(CommandResults()))]
			if rng.IntN(3) == 0 {
				push.Commands[i].Old = ZeroOID
			}
		}
		cut := push.brief()
		if cut.Omitted == (CommandCounts{}) {
			t.Fatalf("a push of %d commands read in brief omits none", len(push.Commands))
		}
		check("push in brief, cut", cut, cut.encodedBytes(cut.commandsBytes()))

		var introduced IntroducedRow
		fill(rng, reflect.ValueOf(&introduced).Elem())
		check("introducing push", introduced, introduced.encodedBytes())

		var retired RetiredRow
		fill(rng, reflect.ValueOf(&retired).Elem())
		check("retirement", retired, retired.encodedBytes())

		var candidate CompactionCandidate
		fill(rng, reflect.ValueOf(&candidate).Elem())
		check("compaction candidate", candidate, candidate.encodedBytes())

		var publishing PublicationCandidate
		fill(rng, reflect.ValueOf(&publishing).Elem())
		check("publication candidate", publishing, publishing.encodedBytes())

		var due ChangeDue
		fill(rng, reflect.ValueOf(&due).Elem())
		check("due change", due, due.encodedBytes())

		var attempt CheckRow
		fill(rng, reflect.ValueOf(&attempt).Elem())
		check("check row", attempt, attempt.encodedBytes())

		var layer LayerRow
		fill(rng, reflect.ValueOf(&layer).Elem())
		check("layer row", layer, layer.encodedBytes())

		var change ChangeSummary
		fill(rng, reflect.ValueOf(&change).Elem())
		check("change listing row", change, change.encodedBytes())

		var revision RevisionRow
		fill(rng, reflect.ValueOf(&revision).Elem())
		check("revision", revision, revision.encodedBytes())

		var reviewer ReviewerRow
		fill(rng, reflect.ValueOf(&reviewer).Elem())
		check("reviewer", reviewer, reviewer.encodedBytes())

		var comment CommentRow
		fill(rng, reflect.ValueOf(&comment).Elem())
		check("comment", comment, comment.encodedBytes())

		var review ReviewRow
		fill(rng, reflect.ValueOf(&review).Elem())
		check("review", review, review.encodedBytes())

		var finding FindingRow
		fill(rng, reflect.ValueOf(&finding).Elem())
		check("finding", finding, finding.encodedBytes())

		// A REPOSITORY'S PATH OWNERS, which MaxPathOwnersBytes bounds by this
		// size: a list nil, empty or of owners whose globs may be either too.
		var owners []PathOwner
		fill(rng, reflect.ValueOf(&owners).Elem())
		check("path owners", owners, pathOwnersBytes(owners))
	}
}

// fill sets every field v holds to a random value — or, a quarter of the time
// for a field that may be omitted, its zero, so both spellings of every
// omitempty field are measured; a slice and a pointer are nil a quarter of the
// time too.
func fill(rng *rand.Rand, v reflect.Value) {
	switch v.Type() {
	case reflect.TypeFor[time.Time]():
		if rng.IntN(4) > 0 {
			nanos := []int{0, 1, 120_000_000, 999_999_999, 500_000}[rng.IntN(5)]
			v.Set(reflect.ValueOf(time.Unix(rng.Int64N(4_000_000_000), int64(nanos)).UTC()))
		}
		return
	case reflect.TypeFor[objstore.Key]():
		v.Set(reflect.ValueOf(objstore.KeyAt(time.Unix(rng.Int64N(4_000_000_000), 0))))
		return
	}
	switch v.Kind() {
	case reflect.String:
		if rng.IntN(4) > 0 {
			v.SetString(randomString(rng))
		}
	case reflect.Bool:
		v.SetBool(rng.IntN(2) == 0)
	case reflect.Int, reflect.Int64, reflect.Int32:
		if rng.IntN(4) > 0 {
			n := rng.Int64N(1 << 40)
			if rng.IntN(5) == 0 {
				n = -n
			}
			v.SetInt(n)
		}
	case reflect.Uint32:
		v.SetUint(uint64(rng.Uint32()))
	case reflect.Uint64:
		v.SetUint(rng.Uint64() >> uint(rng.IntN(64)))
	case reflect.Struct:
		for i := range v.NumField() {
			fill(rng, v.Field(i))
		}
	case reflect.Slice:
		if rng.IntN(4) == 0 {
			return
		}
		n := rng.IntN(6)
		s := reflect.MakeSlice(v.Type(), n, n)
		for i := range n {
			fill(rng, s.Index(i))
		}
		v.Set(s)
	case reflect.Pointer:
		// A POINTER IS AN OPTIONAL VALUE — a change's work item — so nil a
		// quarter of the time, and otherwise a value filled like any other.
		if rng.IntN(4) == 0 {
			return
		}
		p := reflect.New(v.Type().Elem())
		fill(rng, p.Elem())
		v.Set(p)
	default:
		panic("fill: a row field of kind " + v.Kind().String() + " — teach fill and rowbytes.go its encoding")
	}
}

// randomString draws from what a row's text can hold: plain ASCII, every
// character encoding/json escapes, multi-byte runes, the two line separators
// it escapes, and bytes that are not UTF-8.
func randomString(rng *rand.Rand) string {
	pieces := []string{"refs/heads/main", "a", "x", "é", "日本", "😀", "\u2028", "\u2029",
		"\"", "\\", "\b", "\f", "\n", "\r", "\t", "\x00", "\x1f", "\x7f", "<", ">", "&",
		"\xff", "\xc3", "\xe2\x80", "/", " "}
	var b strings.Builder
	for range rng.IntN(12) {
		b.WriteString(pieces[rng.IntN(len(pieces))])
	}
	return b.String()
}

// FuzzStringBytes holds stringBytes to encoding/json on any string at all.
func FuzzStringBytes(f *testing.F) {
	for _, seed := range []string{"", "plain", "\"\\\b\f\n\r\t", "\x00\x01\x1f\x7f", "<>&",
		"\u2028\u2029", "é日本😀", "\xff\xfe", "\xe2\x80", "a\xc3"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := stringBytes(s); got != len(b) {
			t.Fatalf("stringBytes(%q) = %d, and it encodes to %d bytes: %s", s, got, len(b), b)
		}
	})
}
