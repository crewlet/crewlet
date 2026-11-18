package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateWalk = flag.Bool("update", false, "rewrite testdata/opencode/walk.golden.json from the person reader's pages")

// walkGolden is where the person reader's pages of every walk case are kept.
var walkGolden = filepath.Join("testdata", "opencode", "walk.golden.json")

// walkCase is one read of an export by a person's transcript: the document,
// the bounds and page it is read at, and whether its caller left first.
type walkCase struct {
	name      string
	doc       []byte
	bounds    TranscriptBounds
	from      int
	filter    TranscriptFilter
	cancelled bool
}

// walkPage is what one walk case read: the page, and the error it ended on.
type walkPage struct {
	Transcript Transcript `json:"transcript"`
	Error      string     `json:"error,omitempty"`
}

// walkCases are every driven release's export, read whole, paged, narrowed and
// held to a line bound it passes, and the documents the walk treats apart: a
// key before and after the list, an element no message, a kind no release
// wrote, a key written twice, two lists, a document cut short, one with more
// after it, one that is no export, a message that does not decode.
func walkCases(t *testing.T) []walkCase {
	t.Helper()
	person, seat := TranscriptReadBounds(), SeatTranscriptBounds()
	narrow := person
	narrow.LineBytes = 4 << 10
	var cases []walkCase
	for _, path := range []string{"2.0.32/export.json", "2.2.5/export.json", "2.3.0/text.json", "2.3.0/tool.json"} {
		doc, err := os.ReadFile(filepath.Join("testdata", "opencode", path))
		if err != nil {
			t.Fatalf("read the fixture: %v", err)
		}
		cases = append(cases,
			walkCase{name: path + " whole", doc: doc, bounds: person},
			walkCase{name: path + " a seat's second page", doc: doc, bounds: seat, from: 3},
			walkCase{name: path + " its calls", doc: doc, bounds: person,
				filter: TranscriptFilter{Kinds: []EntryKind{EntryToolCall, EntryToolResult}}},
			walkCase{name: path + " past a 4 KiB line", doc: doc, bounds: narrow},
		)
	}
	const (
		user      = `{"type":"user","text":"Fix the flake.","time":{"created":1794632160771}}`
		assistant = `{"type":"assistant","time":{"created":1794632160800,"completed":1794632160900},` +
			`"content":[{"type":"text","text":"Running it."},7,{"type":"reasoning","text":"hm"},` +
			`{"type":"tool","name":"bash","time":{"created":1794632160810,"ran":1794632160820,` +
			`"completed":1794632160830},"state":{"status":"completed","input":{"command":"go test ./..."},` +
			`"content":[{"type":"text","text":"ok"}]}},{"type":"tool","name":"crewlet_read","state":` +
			`{"status":"error","input":{"id":1},"error":"no such item"}}]}`
		compaction = `{"type":"compaction","status":"completed","summary":"The flake is fixed.","time":{"created":5}}`
	)
	messages := `[` + user + `,1,"x",null,{"type":"later-kind","time":{"created":2}},` + assistant + `,` +
		`{"type":"user","text":"first","text":"second"},{"type":"assistant","content":"words"},` + compaction + `]`
	whole := `{"info":{"id":"ses_x"},"extra":[1,{"a":2}],"messages":` + messages + `,"after":true}`
	for name, doc := range map[string]string{
		"keys around the list and odd elements": whole,
		"two lists":                             `{"messages":[` + user + `],"messages":[` + compaction + `]}`,
		"cut short":                             whole[:len(whole)/2],
		"more after the document":               whole + ` {"messages":[` + user + `]}`,
		"no object":                             `[1,2]`,
		"a list that is no list":                `{"messages":5}`,
		"a message that does not decode":        `{"messages":[` + user + `,{"type":"user","text":]}`,
	} {
		cases = append(cases, walkCase{name: name, doc: []byte(doc), bounds: person})
	}
	cases = append(cases, walkCase{name: "abandoned", doc: []byte(whole), bounds: person, cancelled: true})
	return cases
}

// LIFTING THE EXPORT WALK CHANGES NO PERSON'S TRANSCRIPT: every case reads to
// the page and the error main's walk read before the lift, kept in
// walk.golden.json (written with -update by that walk, and never since).
//
// Mutation: drop notMessage's account of an element that is no message, or
// stop at the first list, and a case reads another page.
func TestLiftingTheExportWalkChangesNoPersonsTranscript(t *testing.T) {
	c, why := carried(OpenCode{}.Name(), openCodeArchiveFormat)
	if c == nil {
		t.Fatal(why)
	}
	got := map[string]walkPage{}
	for _, tc := range walkCases(t) {
		var zipped bytes.Buffer
		zw := gzip.NewWriter(&zipped)
		if _, err := zw.Write(tc.doc); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		if tc.cancelled {
			cancel()
		}
		page, err := decodeTranscript(ctx, c, &zipped, tc.from, tc.bounds, tc.filter)
		cancel()
		read := walkPage{Transcript: page}
		if err != nil {
			read.Error = err.Error()
		}
		got[tc.name] = read
	}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if *updateWalk {
		if err = os.WriteFile(walkGolden, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(walkGolden)
	if err != nil {
		t.Fatalf("%v: run with -update on the walk before the lift", err)
	}
	var kept map[string]walkPage
	if err = json.Unmarshal(want, &kept); err != nil {
		t.Fatal(err)
	}
	for name, read := range got {
		wantRead, err := json.Marshal(kept[name])
		if err != nil {
			t.Fatal(err)
		}
		gotRead, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := kept[name]; !ok || !bytes.Equal(gotRead, wantRead) {
			t.Errorf("%s: read\n%s\nwant\n%s", name, gotRead, wantRead)
		}
	}
	if len(kept) != len(got) {
		t.Errorf("%d cases read, %d kept: run with -update on the walk before the lift", len(got), len(kept))
	}
}
