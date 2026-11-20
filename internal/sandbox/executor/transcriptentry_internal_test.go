package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// streamIn writes text to w in pieces of about piece bytes, each whole
// characters, as the scanner hands a string's.
func streamIn(w io.Writer, text string, piece int) {
	for len(text) > 0 {
		k := min(len(text), piece)
		for k < len(text) && !utf8.RuneStart(text[k]) {
			k++
		}
		_, _ = w.Write([]byte(text[:k]))
		text = text[k:]
	}
}

// shaperTexts are texts an entry is shaped from, built to meet every edge the
// streamed shaping has: credentials across the entry's bound, a slice's and a
// piece's; markers written as the text's own words, alone and in runs, where a
// window's start and end fall; a marker's opening that runs on as words, short
// and long; characters of every width across the edges; JSON a call's text is
// read through; and the redact package's hold shapes.
func shaperTexts() map[string]string {
	filler := func(n int) string {
		line := "compiling a package of the build, step by step\n"
		return strings.Repeat(line, n/len(line)+1)[:n]
	}
	texts := map[string]string{
		"empty":           "",
		"words":           "a target and a build",
		"blank":           " \t\n\u3000 ",
		"the marker word": "redacted [REDACTED: target",
	}
	for _, at := range []int{0, 1000, 4090, 5000, 16380, 20000} {
		texts[fmt.Sprintf("a target at %d", at)] = filler(at) + "target" + filler(18000) + " build"
	}
	for _, shape := range redacttest.Shapes() {
		for _, at := range []int{4090, 8180, 16384 - len(shape.Sample)/2} {
			texts[fmt.Sprintf("%s at %d", shape.Name, at)] = filler(at) + shape.Sample + " target " + filler(17000)
		}
	}
	marker := "[REDACTED:github-token]"
	for _, n := range []int{1, 3, 40, 800} {
		run := strings.Repeat(marker, n)
		for _, at := range []int{0, 2000, 5300, 5460, 16000, 21000} {
			texts[fmt.Sprintf("a run of %d markers at %d", n, at)] = filler(at) + run + "target" + run +
				filler(17000) + run
			texts[fmt.Sprintf("a run of %d markers at %d after the target", n, at)] = filler(5460) + "target" +
				filler(at) + run + filler(17000)
		}
	}
	for _, name := range []string{"abc def", strings.Repeat("a", 5000) + " x", strings.Repeat("a", 5000) + "]",
		"api-key] target", "x-y-z"} {
		texts["a marker's opening running on as "+name[:min(len(name), 8)]+fmt.Sprint(len(name))] =
			filler(5400) + "target [REDACTED:" + name + " target " + filler(17000)
	}
	wide := strings.Repeat("é€😀 ", 2000)
	texts["characters of every width"] = wide + " target " + wide
	texts["a call's JSON"] = `{"command":"echo \u0074arget \\new","dir":"` + filler(20000) + `"}`
	for name, text := range holdShapes(24 << 10) {
		texts["hold shape "+name] = text + " target"
	}
	return texts
}

// AN ENTRY STREAMED PAST THE VALUE BOUND IS THE ENTRY HELD: shaped from a text
// written to it in pieces, an entry is what the text held whole shapes
// ([transcriptBuilder.held]): its opening, size and cut, or, under terms,
// whether its own words hold them and the window around the first; and where
// the stream refuses the redaction, an [EntryUnread] in its place. At a
// person's bound, a seat's and one a window's edges cross markers at, as words
// and as a call's JSON, in pieces of every size.
//
// Mutation: cut the window without moving its edges off markers, keep a hit a
// marker's run took back, or show what a refused stream wrote, and a case goes
// red.
func TestAnEntryStreamedPastTheValueBoundIsTheEntryHeld(t *testing.T) {
	t.Parallel()
	person, seat, narrow := TranscriptReadBounds(), SeatTranscriptBounds(), TranscriptReadBounds()
	narrow.EntryBytes = 160
	filters := []TranscriptFilter{{}, {Terms: []string{"target"}}, {Terms: []string{"target", "build"}},
		{Terms: []string{"redacted"}}, {Terms: []string{"github"}}, {Terms: []string{"zzzabsent"}}}
	pieces := []int{17, windowSlice + 3, 64 << 10}
	type read struct {
		bounds TranscriptBounds
		filter TranscriptFilter
	}
	var reads []read
	for _, bounds := range []TranscriptBounds{person, seat, narrow} {
		for _, filter := range filters {
			reads = append(reads, read{bounds, filter})
		}
	}
	i := 0
	for name, text := range shaperTexts() {
		refused := refusesToStream(text)
		i++
		for j, r := range reads {
			// EVERY READ OF A MARKER'S TEXT, where a window's edges move; a few of
			// every other text's, a different few each.
			if !strings.Contains(name, "marker") && (i+j)%6 != 0 {
				continue
			}
			for _, kind := range []EntryKind{EntryModel, EntryToolCall} {
				if kind == EntryToolCall && name != "a call's JSON" && (i+j)%5 != 0 {
					continue
				}
				b := newTranscriptBuilder(0, r.bounds, r.filter)
				e := TranscriptEntry{Kind: kind, Tool: "Bash", Thread: "a1"}
				want := b.held(e, text, 0)
				if refused {
					want = b.held(TranscriptEntry{Kind: EntryUnread, Thread: e.Thread}, unredactedText, 0)
				}
				s := b.streamed(e)
				piece := pieces[(i+j)%len(pieces)]
				streamIn(s, text, piece)
				if got := s.end(0); !reflect.DeepEqual(got, want) {
					t.Errorf("%s at %d bytes, terms %q, as %s, in pieces of %d:\nstreamed %.300s\nheld     %.300s",
						name, r.bounds.EntryBytes, r.filter.Terms, kind, piece, fmt.Sprintf("%+v", got),
						fmt.Sprintf("%+v", want))
				}
			}
		}
	}
}

// A MATCH IS FOUND ANYWHERE IN A VALUE OF ANY LENGTH, and only in its own
// words: a hit the finder took inside a `[REDACTED:` run is taken back when the
// run closes as a marker, the window's hit with it (its state snapshotted at the
// finder's Mark), so the window opens around the first hit in the text's own
// words wherever it falls, as [windowAround] of the whole redaction does: the
// streamed entry is the held one, in pieces of every size.
//
// Mutation: keep a window's hit a marker's run took back, and the window opens
// around the marker.
func TestAMatchIsFoundAnywhereInAValueOfAnyLength(t *testing.T) {
	t.Parallel()
	filler := func(n int) string {
		line := "compiling a package of the build, step by step\n"
		return strings.Repeat(line, n/len(line)+1)[:n]
	}
	b := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{Terms: []string{"github"}})
	for _, at := range []int{0, 100, 5000, 30000} {
		for _, name := range []string{"github-token", "github", "a-github-b"} {
			text := filler(at) + "[REDACTED:" + name + "]" + filler(20000) + " github " + filler(20000)
			want := b.held(TranscriptEntry{Kind: EntryModel}, text, 0)
			if want.passed || want.entry.Text == "" {
				t.Fatalf("the held entry of %q at %d holds no hit; the case moves nothing", name, at)
			}
			for _, piece := range []int{1, 17, windowSlice + 3, 64 << 10} {
				s := b.streamed(TranscriptEntry{Kind: EntryModel})
				streamIn(s, text, piece)
				if got := s.end(0); !reflect.DeepEqual(got, want) {
					t.Errorf("a hit inside [REDACTED:%s] at %d, in pieces of %d:\nstreamed %.200s\nheld     %.200s",
						name, at, piece, fmt.Sprintf("%+v", got), fmt.Sprintf("%+v", want))
				}
			}
		}
	}
}

// refusesToStream reports whether a stream refuses text's redaction.
func refusesToStream(text string) bool {
	s := redact.NewStream(io.Discard)
	streamIn(s, text, 32<<10)
	return errors.Is(s.Close(), redact.ErrDecidedEarly)
}

// refusingRedaction is a stream that refuses every text once its first write
// is past.
type refusingRedaction struct{ wrote bool }

func (r *refusingRedaction) Write(p []byte) (int, error) {
	if r.wrote {
		return 0, redact.ErrDecidedEarly
	}
	r.wrote = true
	return len(p), nil
}

func (r *refusingRedaction) Close() error { return redact.ErrDecidedEarly }

// A READ SHAPES EVERY TEXT PAST THE VALUE BOUND AS IT SHAPES ONE HELD: every
// session of the corpus, both harnesses', read with a text past a kibibyte
// streamed reads to the page and the error it reads to with every text held,
// from its start and from a later entry, whole and under a filter's terms;
// and with a redaction refused (through the seam a test's stream comes in by),
// each streamed text is an [EntryUnread] in its place and the read goes on.
//
// Mutation: let a refusal end the read, or stream a text the page passes over,
// and a case goes red.
func TestAReadShapesEveryTextPastTheValueBoundAsItShapesOneHeld(t *testing.T) {
	t.Parallel()
	for _, s := range shaperSessions(t) {
		c, why := carried(s.harness, s.format)
		if c == nil {
			t.Fatal(why)
		}
		for _, from := range []int{0, 3} {
			for _, filter := range []TranscriptFilter{{}, {Terms: []string{"the"}}} {
				read := func(valueMax int, redactor func(io.Writer) entryRedaction) (string, error) {
					b := newTranscriptBuilder(from, TranscriptReadBounds(), filter)
					b.valueMax, b.redactor = valueMax, redactor
					page, err := b.decode(t.Context(), c, bytes.NewReader(s.archive))
					return fmt.Sprintf("%+v", page), err
				}
				held, heldErr := read(maxLineBytes, nil)
				if streamed, err := read(1<<10, nil); streamed != held || fmt.Sprint(err) != fmt.Sprint(heldErr) {
					t.Errorf("%s from %d, terms %q:\nstreamed %.600s %v\nheld     %.600s %v", s.name, from,
						filter.Terms, streamed, err, held, heldErr)
				}
				refusing := func(io.Writer) entryRedaction { return &refusingRedaction{} }
				if _, err := read(1<<10, refusing); fmt.Sprint(err) != fmt.Sprint(heldErr) {
					t.Errorf("%s from %d: a refused redaction ended the read %v, want %v", s.name, from, err,
						heldErr)
				}
			}
		}
	}
	b := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{})
	b.valueMax = 1 << 10
	b.redactor = func(io.Writer) entryRedaction { return &refusingRedaction{} }
	long := strings.Repeat("the model's words ", 200)
	for _, text := range []string{"before", long, "after"} {
		if err := b.add(TranscriptEntry{Kind: EntryModel}, text); err != nil {
			t.Fatal(err)
		}
	}
	if len(b.entries) != 3 || b.entries[1].Kind != EntryUnread || b.entries[1].Text != unredactedText ||
		b.entries[0].Text != "before" || b.entries[2].Text != "after" {
		t.Errorf("a refused redaction between two entries: %+v; want an unread entry in its place, the two read",
			b.entries)
	}
}

// shaperSession is one session archive of the corpus a read is shaped over.
type shaperSession struct {
	name, harness string
	format        int
	archive       []byte
}

// shaperSessions is every Claude Code session tree and OpenCode export of the
// package's testdata, each as its runner packs it.
func shaperSessions(t *testing.T) []shaperSession {
	t.Helper()
	var sessions []shaperSession
	for _, release := range []string{"2.1.310", "2.1.322"} {
		trees, err := filepath.Glob(filepath.Join("testdata", "claude", release, "holdings", "*"))
		if err != nil {
			t.Fatal(err)
		}
		for _, tree := range trees {
			sessions = append(sessions, shaperSession{tree, ClaudeCode{}.Name(), claudeArchiveFormat,
				tarOfTree(t, tree)})
		}
	}
	for _, path := range []string{"2.0.32/export.json", "2.2.5/export.json", "2.3.0/text.json", "2.3.0/tool.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "opencode", path))
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, shaperSession{path, OpenCode{}.Name(), openCodeArchiveFormat,
			gzipOf(t, string(raw))})
	}
	return sessions
}

// tarOfTree is a gzipped tar of every file under root, named from root.
func tarOfTree(t *testing.T, root string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if err = tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
