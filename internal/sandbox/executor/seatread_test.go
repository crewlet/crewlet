package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/textindex"
)

// A seat's own page of a transcript: held to a seat's bounds, narrowed by kind
// or by words — a match decided on what the redaction shows, its window cut from
// that redaction — every part it could not read handed out, and the seats' one
// slot.

// seatRead is a seat's read of raw, an OpenCode archive, from from on,
// narrowed by filter.
func seatRead(t *testing.T, raw []byte, from int, filter executor.TranscriptFilter) executor.Transcript {
	t.Helper()
	got, err := executor.ReadTranscriptAs(t.Context(), executor.ReadBySeat, filter, executor.OpenCodeName,
		executor.OpenCodeArchiveFormat, opener(bytes.NewReader(raw)), from)
	if err != nil {
		t.Fatalf("a seat's read from %d: %v", from, err)
	}
	return got
}

// called is a step in which the model says said and calls a shell that
// answers answered.
func called(said, answered string) string {
	text, _ := json.Marshal(said)
	result, _ := json.Marshal(answered)
	return openCodeStep(`{"type":"text","text":`+string(text)+`}`, openCodeCall("shell",
		`{"status":"completed","input":{"command":"ls"},"content":[{"type":"text","text":`+string(result)+`}]}`))
}

// A SEAT'S PAGE IS HELD TO A SEAT'S BOUNDS. A seat's read hands out at most
// fifty entries and thirty-two KiB of their text, each entry at most eight KiB
// cut and marked, where a person's page is unchanged — two hundred, a MiB and
// sixteen KiB — and both read the same lines and the same archive bytes.
//
// Mutation: read a seat's page held to a person's bounds, and a check goes
// red.
func TestASeatsPageIsHeldToASeatsBounds(t *testing.T) {
	t.Parallel()
	person := executor.TranscriptReadBounds()
	if want := (executor.TranscriptBounds{Entries: 50, Bytes: 32 << 10, EntryBytes: 8 << 10,
		LineBytes: person.LineBytes, ReadBytes: person.ReadBytes}); executor.SeatTranscriptBounds() != want ||
		executor.ReadBySeat.Bounds() != want {
		t.Errorf("a seat's bounds are %+v, want %+v", executor.ReadBySeat.Bounds(), want)
	}
	if person.Entries != 200 || person.Bytes != 1<<20 || person.EntryBytes != 16<<10 ||
		executor.ReadByPerson.Bounds() != person {
		t.Errorf("a person's bounds are %+v, want them unchanged", executor.ReadByPerson.Bounds())
	}

	page := seatRead(t, gzipped(t, briefs(60)), 0, executor.TranscriptFilter{})
	if len(page.Entries) != executor.SeatTranscriptMaxEntries || page.Cut != executor.CutEntries ||
		page.Next != executor.SeatTranscriptMaxEntries {
		t.Errorf("a seat's page of sixty entries holds %d, cut %q, next %d; want fifty, cut at them",
			len(page.Entries), page.Cut, page.Next)
	}
	long := make([]string, 10)
	for i := range long {
		long[i] = openCodeUser(strings.Repeat(fmt.Sprintf("word%d ", i), 2<<10))
	}
	page = seatRead(t, gzipped(t, openCodeDoc(long...)), 0, executor.TranscriptFilter{})
	text := 0
	for _, e := range page.Entries {
		text += len(e.Text)
		if len(e.Text) > executor.SeatTranscriptEntryMaxBytes || !e.Cut {
			t.Errorf("an entry of %d bytes is handed to a seat as %d, cut %v; want at most %d, cut",
				e.Bytes, len(e.Text), e.Cut, executor.SeatTranscriptEntryMaxBytes)
		}
	}
	if text > executor.SeatTranscriptMaxBytes || page.Cut != executor.CutBytes || len(page.Entries) == 0 {
		t.Errorf("a seat's page of long entries holds %d entries and %d bytes, cut %q; want at most %d "+
			"bytes, cut at them", len(page.Entries), text, page.Cut, executor.SeatTranscriptMaxBytes)
	}
}

// A FILTERED PAGE COUNTS EVERY ENTRY. A read narrowed to some kinds hands out
// only those, each at the ordinal the whole read gives it — so one cursor
// grammar serves both — and its next read starts at the first entry it did not
// examine: past every entry it passed over, never at its first plus how many it
// kept; read on from there, the pages are every entry of the kinds, once, in
// order, and the last read's next is past the transcript's last entry.
//
// Mutation: number the entries a filter kept by how many it kept, or start the
// next read at the first plus how many were kept, and a check goes red.
func TestAFilteredPageCountsEveryEntry(t *testing.T) {
	t.Parallel()
	messages := []string{openCodeUser("look into the flake")}
	for i := range 60 {
		messages = append(messages, called(fmt.Sprintf("step %d", i), fmt.Sprintf("listed %d", i)))
	}
	raw := gzipped(t, openCodeDoc(messages...))
	whole := readOpenCode(t, raw, executor.TranscriptReadBounds())
	if len(whole.Entries) != 181 || len(whole.Places) != 181 || whole.Next != 181 {
		t.Fatalf("the whole transcript reads %d entries, %d places, next %d; want 181", len(whole.Entries),
			len(whole.Places), whole.Next)
	}
	for i, place := range whole.Places {
		if place.Ordinal != i || place.TextFrom != 0 {
			t.Fatalf("the whole read's entry %d is placed at %+v", i, place)
		}
	}

	models := executor.TranscriptFilter{Kinds: []executor.EntryKind{executor.EntryModel}}
	var kept []int
	for from, pages := 0, 0; ; pages++ {
		page := seatRead(t, raw, from, models)
		if len(page.Places) != len(page.Entries) {
			t.Fatalf("a page from %d holds %d entries and %d places", from, len(page.Entries), len(page.Places))
		}
		for i, e := range page.Entries {
			at := page.Places[i].Ordinal
			if e.Kind != executor.EntryModel || at >= len(whole.Entries) || whole.Entries[at].Text != e.Text {
				t.Errorf("the filtered page's %q is placed at %d, where the whole read has %+v", e.Text, at,
					whole.Entries[min(at, len(whole.Entries)-1)])
			}
			kept = append(kept, at)
		}
		if !page.More() {
			if page.Next != len(whole.Entries) {
				t.Errorf("the last filtered page's next is %d, want past the last entry, %d", page.Next,
					len(whole.Entries))
			}
			break
		}
		last := page.Places[len(page.Places)-1].Ordinal
		if page.Next <= last || page.Next == from+len(page.Entries) || pages > 3 {
			t.Fatalf("a filtered page from %d ends at %d and says next %d", from, last, page.Next)
		}
		from = page.Next
	}
	if len(kept) != 60 {
		t.Errorf("the filtered pages hold %d of the model's 60 entries: %v", len(kept), kept)
	}
	for i := 1; i < len(kept); i++ {
		if kept[i] <= kept[i-1] {
			t.Errorf("the filtered pages repeat or reorder an entry: %v", kept)
			break
		}
	}
}

// A MATCH IS FOUND ANYWHERE IN AN ENTRY. A read for words searches what each
// entry's whole text redacts to: a word a hundred and fifty KiB into a tool's
// answer of two hundred makes that answer a hit, handed out as
// the window of its text around the word, a third of the window before it, with
// the whole text's size, the cut, and where in it the window begins; the
// entries that do not hold every word are passed over.
//
// Mutation: search only what an entry's opening hands out, and the answer is
// no hit.
func TestAMatchIsFoundAnywhereInAnEntry(t *testing.T) {
	t.Parallel()
	before := strings.Repeat("lorem ", 25600)
	answer := before + "kubeconfig mounted " + strings.Repeat("ipsum ", 8000)
	raw := gzipped(t, openCodeDoc(openCodeUser("find where the config lives"), called("looking", answer)))
	page := seatRead(t, raw, 0, executor.TranscriptFilter{Terms: textindex.Terms("kubeconfig")})
	if len(page.Entries) != 1 {
		t.Fatalf("the read for a word holds %d entries, want the one answer holding it", len(page.Entries))
	}
	hit, place := page.Entries[0], page.Places[0]
	room := executor.SeatTranscriptEntryMaxBytes - len("…")
	if hit.Kind != executor.EntryToolResult || place.Ordinal != 3 || !strings.Contains(hit.Text, "kubeconfig") ||
		hit.Bytes != len(answer) || !hit.Cut || len(hit.Text) > executor.SeatTranscriptEntryMaxBytes {
		t.Errorf("the hit reads %s at %d, %d of %d bytes, cut %v, holding the word: %v; want the answer, "+
			"its window holding the word", hit.Kind, place.Ordinal, len(hit.Text), hit.Bytes, hit.Cut,
			strings.Contains(hit.Text, "kubeconfig"))
	}
	if want := len(before) - room/3; place.TextFrom != want {
		t.Errorf("the window begins at %d in the answer, want %d: a third of the window before the word",
			place.TextFrom, want)
	}
	if page.Next != 4 || page.Cut != "" {
		t.Errorf("the read ends at %d, cut %q; want the transcript's end, 4", page.Next, page.Cut)
	}
}

// keyBlock is a private key's block, its body lines each a distinct run of
// base64, which the redaction knows.
func keyBlock() (string, []string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var body []string
	for i := range 24 {
		var line strings.Builder
		for j := range 64 {
			line.WriteByte(alphabet[(i*7+j*13+i*j)%len(alphabet)])
		}
		body = append(body, line.String())
	}
	return "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Join(body, "\n") + "\n-----END RSA PRIVATE KEY-----\n", body
}

// A WINDOW NEVER HANDS OUT A CREDENTIAL, OR A MARKER, IN PART. A match's
// window is cut from what the entry's whole text redacts to, so a token that
// runs across where the window would start or end, and a private key whose
// block begins before the window, are markers there, never pieces of
// themselves; and an end that would fall inside a marker moves off it, away
// from the word — a start on past it, an end back to where it begins — so no
// marker is handed out in part, and the window still holds the word. The
// window says where it begins, in what the whole text redacts to.
//
// Mutation: cut the window from the text as the harness recorded it, or leave
// an end inside a marker, and a check goes red.
func TestAWindowNeverHandsOutACredentialOrAMarkerInPart(t *testing.T) {
	t.Parallel()
	bound := executor.SeatTranscriptEntryMaxBytes
	room := bound - len("…")
	key, body := keyBlock()
	if redacted := redact.Secrets(key); strings.Contains(redacted, body[3]) {
		t.Fatalf("the redaction does not know the case's key: %q", redacted)
	}
	filler := func(n int) string { return strings.Repeat("x", n) }
	const tokenMarker, keyMarker = "[REDACTED:github-token]", "[REDACTED:private-key]"
	// INSIDE A MARKER, eleven bytes in, where a window would start or end:
	// each case sizes the filler between the marker and the word from where
	// the redaction puts both.
	startsIn := func(head, marker, before string) string {
		shown := redact.Secrets(head + before + " kubeconfig ")
		at := strings.Index(shown, "kubeconfig")
		return filler(room/3 - (at - strings.Index(shown, marker) - 11))
	}
	for _, c := range []struct {
		name, text, marker string
		secret             []string
		// moved is where the window begins: past the marker its start fell
		// in, or where a third of the room before the word puts it.
		movedPast bool
	}{
		{name: "a token across the window's start",
			text: filler(20<<10) + " " + cutGitHub + " " + startsIn(filler(20<<10)+" "+cutGitHub+" ",
				tokenMarker, "") + " kubeconfig " + filler(30<<10),
			marker: tokenMarker, secret: []string{cutGitHubBody}, movedPast: true},
		{name: "a token across the window's end",
			text: filler(20<<10) + " kubeconfig " + filler(room-room/3-len(tokenMarker)) + " " + cutGitHub +
				" " + filler(30<<10),
			marker: tokenMarker, secret: []string{cutGitHubBody}},
		{name: "a key whose block begins before the window",
			text: filler(20<<10) + "\n" + key + startsIn(filler(20<<10)+"\n"+key, keyMarker, "") +
				" kubeconfig " + filler(30<<10),
			marker: keyMarker, secret: body, movedPast: true},
	} {
		raw := gzipped(t, openCodeDoc(openCodeUser(c.text)))
		page := seatRead(t, raw, 0, executor.TranscriptFilter{Terms: textindex.Terms("kubeconfig")})
		if len(page.Entries) != 1 {
			t.Errorf("%s: the read holds %d entries, want the one holding the word", c.name, len(page.Entries))
			continue
		}
		window, from := page.Entries[0].Text, page.Places[0].TextFrom
		shown := redact.Secrets(c.text)
		at := strings.Index(shown, "kubeconfig")
		if !strings.Contains(window, "kubeconfig") || !page.Entries[0].Cut || len(window) > bound {
			t.Errorf("%s: the window of %d bytes from %d, cut %v, holding the word: %v", c.name, len(window),
				from, page.Entries[0].Cut, strings.Contains(window, "kubeconfig"))
		}
		want := at - room/3
		if c.movedPast {
			want = strings.Index(shown, c.marker) + len(c.marker)
		}
		if from != want || !strings.HasPrefix(shown[from:], strings.TrimSuffix(window, "…")) {
			t.Errorf("%s: the window says it begins at %d, want %d, where what the text redacts to reads "+
				"the window", c.name, from, want)
		}
		if strings.Contains(window, "ACTED:") {
			t.Errorf("%s: the window hands out a marker, or a piece of one: %q", c.name, window)
		}
		for _, secret := range c.secret {
			for i := 0; i+8 <= len(secret); i++ {
				if strings.Contains(window, secret[i:i+8]) {
					t.Errorf("%s: the window hands out %q, a piece of a credential", c.name, secret[i:i+8])
					break
				}
			}
		}
	}
}

// A MATCH IS DECIDED ON WHAT ITS READER IS SHOWN. An entry is a hit only where
// what the redaction shows of its own words holds every term: a word found
// only inside a credential the redaction takes out is no hit, whether it is
// the one word asked for or one of two whose other the entry shows — so a
// right guess at a redacted password and a wrong one are answered alike — and
// a word the redaction's own marker spells is no word of the entry's. An
// entry that shows the word further on than a credential that held it is a
// hit, its window around the word it shows.
//
// Mutation: decide the hit on the text as the harness recorded it, read a
// marker's words as the entry's, or place the window at the earliest word as
// recorded, and a check goes red.
func TestAMatchIsDecidedOnWhatItsReaderIsShown(t *testing.T) {
	t.Parallel()
	raw := gzipped(t, openCodeDoc(
		openCodeUser("the push used "+cutGitHub+" and failed"),
		openCodeUser("connect with password: hunter2 then run the migration"),
		openCodeUser("passwd: password"),
		openCodeUser("the push used "+cutGitHub+" and failed\n"+strings.Repeat("x ", 6<<10)+
			"\na ghp prefix marks a GitHub token")))
	hits := func(query string) ([]int, executor.Transcript) {
		page := seatRead(t, raw, 0, executor.TranscriptFilter{Terms: textindex.Terms(query)})
		var at []int
		for _, place := range page.Places {
			at = append(at, place.Ordinal)
		}
		if page.Next != 4 || page.Cut != "" {
			t.Errorf("the read for %q ends at %d, cut %q; want the transcript's end, 4", query, page.Next,
				page.Cut)
		}
		return at, page
	}
	right, rightPage := hits("password hunter2")
	wrong, wrongPage := hits("password hunter3")
	if len(right) != 0 || len(wrong) != 0 || len(rightPage.Entries) != len(wrongPage.Entries) {
		t.Errorf("a redacted password guessed right found %v and guessed wrong %v: the two are answered "+
			"alike, with no hit", right, wrong)
	}
	if got, _ := hits("password"); !slices.Equal(got, []int{1}) {
		t.Errorf("the read for a word a marker spells found %v, want the entry showing it alone, 1: the "+
			"marker's words are none of the entry's", got)
	}
	if got, _ := hits("migration password"); !slices.Equal(got, []int{1}) {
		t.Errorf("the read for two words the entry shows found %v, want 1", got)
	}
	got, page := hits("ghp")
	if !slices.Equal(got, []int{3}) {
		t.Fatalf("the read for a word only a credential held, and held in the open further on, found %v, "+
			"want the entry showing it, 3", got)
	}
	if window := page.Entries[0].Text; !strings.Contains(window, "a ghp prefix") || strings.Contains(window, "Q7w") {
		t.Errorf("the hit's window reads %q, want it around the word it shows, holding no credential", window)
	}
}

// unitEscaped is the escape a JSON text writes the UTF-16 code unit unit in: a
// backslash, `u` and its four hex digits.
func unitEscaped(unit rune) string { return fmt.Sprintf(`\u%04x`, unit) }

// A CALL'S WORDS ARE READ THROUGH ITS INPUT'S ESCAPES. A tool call's text is
// its input as JSON, where a string's line break or tab is written `\n` or
// `\t`, glued as written to the word after it: a word that opens a line of a
// multi-line command, or of a file a call writes, or follows a tab, is the
// call's all the same — a hit, shown as the call's JSON — where the call's
// result does not echo it; and so is a word with a letter the input names by
// `\u` and its digits. The escape's own letter glued to the word is no word of
// the call's.
//
// Mutation: read a call's text as written, for the hit or for the test before
// its redaction, and a case goes red.
func TestACallsWordsAreReadThroughItsInputsEscapes(t *testing.T) {
	t.Parallel()
	command := `{"command":"cd repo\ngit push origin main"}`
	file := `{"path":"main.go","content":"package main\n\nfunc main() {\n\tfmt.Println(\"caf` +
		unitEscaped('é') + `\")\n}"}`
	raw := gzipped(t, openCodeDoc(openCodeStep(
		openCodeCall("bash", `{"status":"completed","input":`+command+
			`,"content":[{"type":"text","text":"Everything up-to-date"}]}`),
		openCodeCall("write", `{"status":"completed","input":`+file+
			`,"content":[{"type":"text","text":"wrote the file"}]}`))))
	for _, c := range []struct {
		query, call string
		hits        []int
	}{
		{"git push", command, []int{0}},
		{"func", file, []int{2}},
		{"fmt", file, []int{2}},
		{"café", file, []int{2}},
		{"ngit", "", nil},
	} {
		page := seatRead(t, raw, 0, executor.TranscriptFilter{Terms: textindex.Terms(c.query)})
		var hits []int
		for i, place := range page.Places {
			hits = append(hits, place.Ordinal)
			if e := page.Entries[i]; e.Kind != executor.EntryToolCall || e.Text != c.call || place.TextFrom != 0 {
				t.Errorf("the read for %q hands out %s %q from %d, want the call %q whole", c.query, e.Kind,
					e.Text, place.TextFrom, c.call)
			}
		}
		if !slices.Equal(hits, c.hits) || page.Next != 4 || page.Cut != "" {
			t.Errorf("the read for %q answers %s; want the entries at %v, to the transcript's end, 4", c.query,
				readOf(page), c.hits)
		}
	}
}

// TWO RECORDS THAT REDACT ALIKE ARE READ ALIKE. Two transcripts that differ
// only in what the redaction takes out — a password the guess names and one it
// does not, of another length — are shown alike, so a seat's read, for any
// words or for none, answers both alike: the same entries, windows, places
// and sizes, and the same stop. A word a redaction's marker ends, `deploy`
// before a token, is a word its reader is shown, a hit in both, though as
// recorded it is a piece of a longer word and only one of the passwords held
// it whole; a page full of hits stops at the next hit in both, whether or not
// the entry before it held the guessed word where the redaction took it out;
// and an entry states the size of what its whole text redacts to — or, for
// the opening of a text longer than the read redacts, its size as recorded
// rounded up to a KiB — never its size as recorded to the byte, which beside a
// text shown whole told the password's length.
//
// Mutation: ask the recorded text whether it holds every term whole before
// redacting it, ask the page's bound before the hit is decided, or state an
// entry's size as recorded, and a check goes red.
func TestTwoRecordsThatRedactAlikeAreReadAlike(t *testing.T) {
	t.Parallel()
	glued := func(password string) []byte {
		return gzipped(t, openCodeDoc(openCodeUser("deploy"+cutGitHub+" failed, password: "+password)))
	}
	full := func(password string) []byte {
		var messages []string
		for range executor.SeatTranscriptMaxEntries {
			messages = append(messages, openCodeUser("deploy hunter2 hunter345"))
		}
		messages = append(messages, openCodeUser("deploy as before, password: "+password),
			openCodeUser("deploy hunter2 hunter345"))
		return gzipped(t, openCodeDoc(messages...))
	}
	// LONGER THAN A SEAT'S READ REDACTS, the password in its opening: both
	// records' sizes as recorded lie inside one KiB.
	opening := "connect with password: hunter2 then run the migration "
	longer := func(password string) []byte {
		return gzipped(t, openCodeDoc(openCodeUser(strings.Replace(opening, "hunter2", password, 1)+
			strings.Repeat("and wait ", 20<<10))))
	}
	page := make([]int, executor.SeatTranscriptMaxEntries)
	for i := range page {
		page[i] = i
	}
	for _, c := range []struct {
		name, query string
		one, other  []byte
		hits        []int
		next        int
		cut         executor.TranscriptCut
	}{
		{"a word a marker ends", "deploy", glued("deploy"), glued("ship"), []int{0}, 1, ""},
		{"no words", "", glued("deploy"), glued("ship"), []int{0}, 1, ""},
		{"a full page, one guess", "deploy hunter2", full("hunter2"), full("hunter345"), page,
			executor.SeatTranscriptMaxEntries + 1, executor.CutEntries},
		{"a full page, the other guess", "deploy hunter345", full("hunter2"), full("hunter345"), page,
			executor.SeatTranscriptMaxEntries + 1, executor.CutEntries},
		{"a longer text's opening", "", longer("hunter2"), longer("hunter345"), []int{0}, 1, ""},
		{"a word of a longer text", "migration", longer("hunter2"), longer("hunter345"), []int{0}, 1, ""},
	} {
		filter := executor.TranscriptFilter{Terms: textindex.Terms(c.query)}
		one, other := seatRead(t, c.one, 0, filter), seatRead(t, c.other, 0, filter)
		if !reflect.DeepEqual(one, other) {
			t.Errorf("%s: the read for %q answers two records that redact alike apart: %s, and %s", c.name,
				c.query, sizedReadOf(one), sizedReadOf(other))
			continue
		}
		var hits []int
		for i, place := range one.Places {
			hits = append(hits, place.Ordinal)
			if e := one.Entries[i]; !e.Cut && e.Bytes != len(e.Text) {
				t.Errorf("%s: an entry shown whole states %d bytes, want its own %d", c.name, e.Bytes,
					len(e.Text))
			}
		}
		if !slices.Equal(hits, c.hits) || one.Next != c.next || one.Cut != c.cut {
			t.Errorf("%s: the read for %q answers %s; want the entries at %v, next %d, cut %q", c.name, c.query,
				readOf(one), c.hits, c.next, c.cut)
		}
	}
	read := seatRead(t, longer("hunter2"), 0, executor.TranscriptFilter{})
	recorded := len(opening) + len(strings.Repeat("and wait ", 20<<10))
	if e := read.Entries[0]; !e.Cut || e.Bytes%executor.StatedSizeGrain != 0 || e.Bytes < recorded ||
		e.Bytes >= recorded+executor.StatedSizeGrain {
		t.Errorf("the opening of a text of %d bytes states %d, cut %v; want its size rounded up to a KiB",
			recorded, e.Bytes, e.Cut)
	}
}

// sizedReadOf is what a read answered, as a failure says it, with the size
// each entry states.
func sizedReadOf(t executor.Transcript) string {
	sizes := make([]int, len(t.Entries))
	for i, e := range t.Entries {
		sizes[i] = e.Bytes
	}
	return fmt.Sprintf("%s, sizes %v", readOf(t), sizes)
}

// readOf is what a read answered, as a failure says it.
func readOf(t executor.Transcript) string {
	at := make([]int, len(t.Places))
	for i, place := range t.Places {
		at[i] = place.Ordinal
	}
	return fmt.Sprintf("entries at %v, next %d, cut %q", at, t.Next, t.Cut)
}

// A FILTER HANDS OUT EVERY PART IT COULD NOT READ. A part of the record the read
// could not read is an entry of its own, and a filter cannot say what kind it
// was or which words it held: so a read narrowed by kind, or by words, hands
// it out as it is — its account of the gap, never searched — where it is, so
// a filtered page that reaches the transcript's end never passes over what it
// never read.
//
// Mutation: let a filter pass over an unread part, or search its account of the
// gap for the words, and a check goes red.
func TestAFilterHandsOutEveryPartItCouldNotRead(t *testing.T) {
	t.Parallel()
	raw := gzipped(t, openCodeDoc(openCodeUser("look into the flake"),
		`{"type":"todo-updated","text":"the kubeconfig is in vault"}`,
		openCodeUser("the kubeconfig moved")))
	for name, c := range map[string]struct {
		filter executor.TranscriptFilter
		want   []int
	}{
		"by kind":                  {executor.TranscriptFilter{Kinds: []executor.EntryKind{executor.EntryModel}}, []int{1}},
		"by words the record held": {executor.TranscriptFilter{Terms: textindex.Terms("kubeconfig")}, []int{1, 2}},
		"by words nothing shows":   {executor.TranscriptFilter{Terms: textindex.Terms("vault")}, []int{1}},
	} {
		page := seatRead(t, raw, 0, c.filter)
		var got []int
		for i, place := range page.Places {
			got = append(got, place.Ordinal)
			if place.Ordinal == 1 && (page.Entries[i].Kind != executor.EntryUnread || place.TextFrom != 0 ||
				!strings.Contains(page.Entries[i].Text, "was not read")) {
				t.Errorf("%s: the part not read is handed out as %+v at %+v, want its account, whole",
					name, page.Entries[i], place)
			}
		}
		if !slices.Equal(got, c.want) || page.Next != 3 || page.Cut != "" {
			t.Errorf("%s: the read holds entries %v, next %d, cut %q; want %v, to the transcript's end",
				name, got, page.Next, page.Cut, c.want)
		}
	}
}

// A MATCH READ PAST ITS BUDGET SAYS WHERE IT STOPPED. A transcript whose archive
// inflates past what one read takes stops the search there: the hits before it
// are handed out, the cut says no later read gets further, and the next
// ordinal is the first entry the read did not examine — what a reader is told
// the search stopped at.
//
// Mutation: answer a read past its budget as no cut, and a check goes red.
func TestAMatchReadPastItsBudgetSaysWhereItStopped(t *testing.T) {
	t.Parallel()
	raw := gzipped(t, openCodeDoc(openCodeUser("the kubeconfig is mounted"), openCodeUser("nothing here"),
		openCodeUser("the kubeconfig moved"), openCodeUser(strings.Repeat("padding ", 32<<10)),
		openCodeUser("the kubeconfig again, past the budget")))
	bounds := executor.SeatTranscriptBounds()
	bounds.ReadBytes = 64 << 10
	page, err := executor.ReadTranscriptFiltered(t.Context(), executor.OpenCodeName,
		executor.OpenCodeArchiveFormat, opener(bytes.NewReader(raw)), 0, bounds,
		executor.TranscriptFilter{Terms: textindex.Terms("kubeconfig")})
	if err != nil {
		t.Fatalf("a read past its budget: %v", err)
	}
	if page.Cut != executor.CutRead || len(page.Entries) != 2 || page.Places[0].Ordinal != 0 ||
		page.Places[1].Ordinal != 2 || page.Next != 3 {
		t.Errorf("a read past its budget holds %d hits at %+v, cut %q, next %d; want the two before the "+
			"budget, cut where no later read gets further, next 3", len(page.Entries), page.Places, page.Cut,
			page.Next)
	}
}

// ---- the seats' slot --------------------------------------------------- //
//
// NOT PARALLEL, for the reason the process's slots' cases are not: the slots
// are the process's own.

// countedWait is a read's context that says when the read asks for its Done
// channel the nth time: a seat's read asks once as it waits for the seats'
// slot and once more as it waits for one of the process's.
type countedWait struct {
	context.Context
	nth   int32
	calls atomic.Int32
	asked chan struct{}
	once  sync.Once
}

func (c *countedWait) Done() <-chan struct{} {
	if c.calls.Add(1) == c.nth {
		c.once.Do(func() { close(c.asked) })
	}
	return c.Context.Done()
}

// seatReadOf starts a seat's read of raw under ctx, its opener held until gate
// closes (nil: not held); opened closes as the read opens its archive, and done
// once the read has ended, with what it ended with in *err.
func seatReadOf(ctx context.Context, raw []byte, gate <-chan struct{}, err *error) (opened, done chan struct{}) {
	opened, done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, *err = executor.ReadTranscriptAs(ctx, executor.ReadBySeat, executor.TranscriptFilter{},
			executor.OpenCodeName, executor.OpenCodeArchiveFormat, func(context.Context) (io.ReadCloser, error) {
				close(opened)
				if gate != nil {
					<-gate
				}
				return io.NopCloser(bytes.NewReader(raw)), nil
			}, 0)
	}()
	return opened, done
}

// A SEAT TAKES AT MOST ONE OF THE NODE'S TRANSCRIPT SLOTS. With one seat's
// read in flight — the seats' slot and one of the process's held — a second
// seat's read waits for the seats' slot, opening nothing, while a person's
// read takes the process's other slot and reads; the waiting seat's read
// leaves with its caller, holding nothing. And a seat's read that holds the
// seats' slot and waits for one of the process's gives the seats' slot back
// when its caller leaves.
//
// Mutation: take a seat's read straight to the process's slots, and the second
// seat's read opens its archive beside the first; give the seats' slot back
// only once a read ends, and the read that left still holds it.
func TestASeatTakesAtMostOneOfTheNodesTranscriptSlots(t *testing.T) {
	if executor.SeatTranscriptReadsAtOnce != 1 || executor.TranscriptReadsAtOnce < 2 {
		t.Fatalf("the seats take %d of the process's %d slots, want one, with a person's beside it",
			executor.SeatTranscriptReadsAtOnce, executor.TranscriptReadsAtOnce)
	}
	if executor.TranscriptReadsHeld() != 0 || executor.SeatTranscriptReadsHeld() != 0 {
		t.Fatal("slots are held before the case reads anything")
	}
	raw := gzipped(t, briefs(1))
	gate := make(chan struct{})
	letFirst := sync.OnceFunc(func() { close(gate) })
	var firstErr error
	firstOpened, firstDone := seatReadOf(t.Context(), raw, gate, &firstErr)
	t.Cleanup(func() {
		letFirst()
		<-firstDone
	})
	awaited(t, firstOpened, "a seat's read given free slots opens its archive")
	if executor.SeatTranscriptReadsHeld() != 1 || executor.TranscriptReadsHeld() != 1 {
		t.Fatalf("a seat's read holds %d of the seats' slots and %d of the process's, want one of each",
			executor.SeatTranscriptReadsHeld(), executor.TranscriptReadsHeld())
	}

	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	second := &countedWait{Context: ctx, nth: 1, asked: make(chan struct{})}
	var secondErr error
	secondOpened, secondDone := seatReadOf(second, raw, nil, &secondErr)
	awaited(t, second.asked, "a second seat's read waits for the seats' slot")
	person, err := executor.ReadTranscript(t.Context(), executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		opener(bytes.NewReader(raw)), 0)
	if err != nil || len(person.Entries) != 1 {
		t.Errorf("a person's read beside a seat's: (%d entries, %v), want it read", len(person.Entries), err)
	}
	leave()
	awaited(t, secondDone, "a seat's read waiting for the seats' slot ends once its caller leaves")
	select {
	case <-secondOpened:
		t.Error("a second seat's read opened its archive beside the first")
	default:
	}
	if !errors.Is(secondErr, context.Canceled) {
		t.Errorf("the second seat's read ended %v, want its caller's own error", secondErr)
	}
	letFirst()
	awaited(t, firstDone, "the first seat's read ends once its archive opens")
	if firstErr != nil {
		t.Fatalf("the first seat's read: %v", firstErr)
	}

	for range executor.TranscriptReadsAtOnce {
		holdSlot(t, raw)
	}
	ctx, leave = context.WithCancel(t.Context())
	defer leave()
	waiting := &countedWait{Context: ctx, nth: 2, asked: make(chan struct{})}
	var waitingErr error
	_, waitingDone := seatReadOf(waiting, raw, nil, &waitingErr)
	awaited(t, waiting.asked, "a seat's read waits for one of the process's slots")
	if executor.SeatTranscriptReadsHeld() != 1 {
		t.Fatalf("a seat's read waiting for one of the process's slots holds %d of the seats'",
			executor.SeatTranscriptReadsHeld())
	}
	leave()
	awaited(t, waitingDone, "a seat's read waiting for one of the process's slots ends once its caller leaves")
	if !errors.Is(waitingErr, context.Canceled) || executor.SeatTranscriptReadsHeld() != 0 {
		t.Errorf("a seat's read that left while it waited ended %v holding %d of the seats' slots, want "+
			"its caller's own error and none", waitingErr, executor.SeatTranscriptReadsHeld())
	}
}
