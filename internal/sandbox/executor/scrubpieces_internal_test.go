package executor

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
)

// valueTexts are the pieces [valueCase] builds a scrubber's values and its
// texts from: a few bytes each, sharing their starts and their ends, so a
// value begins inside another, one is another's start, and a text holds a
// value's start where the value does not follow.
var valueTexts = []string{"ab", "abc", "abcd", "b", "bc", "cd", "d", "x", `"`, `\`, "\n", "é", "\xff", " "}

// valueCase is a scrubber of values drawn from r, and a text holding them,
// their starts and the pieces between, drawn from r too.
func valueCase(r *rand.Rand) (scrubber, string) {
	draw := func(pieces int) string {
		var b strings.Builder
		for range pieces {
			b.WriteString(valueTexts[r.IntN(len(valueTexts))])
		}
		return b.String()
	}
	var values []string
	for range 1 + r.IntN(6) {
		values = append(values, draw(3+r.IntN(10)))
	}
	if r.IntN(4) == 0 {
		// A value written across lines, which the scrubber takes out a line
		// at a time.
		values = append(values, values[0]+"\n"+draw(8))
	}
	var text strings.Builder
	for range r.IntN(40) {
		switch v := values[r.IntN(len(values))]; r.IntN(4) {
		case 0:
			text.WriteString(v)
		case 1:
			text.WriteString(v[:r.IntN(len(v)+1)])
		case 2:
			text.WriteString(jsonEscaped(v))
		default:
			text.WriteString(draw(1 + r.IntN(6)))
		}
	}
	return newScrubber(values), text.String()
}

// streamValues is text written to s's [valueStream] in pieces whose ends are
// at, and what it wrote; it fails t where the stream holds as many bytes as
// the longest value once a piece is read.
func streamValues(t *testing.T, s scrubber, text string, at []int) string {
	t.Helper()
	var out bytes.Buffer
	v := s.stream(&out)
	from := 0
	for _, end := range append(at, len(text)) {
		if n, err := v.Write([]byte(text[from:end])); err != nil || n != end-from {
			t.Fatalf("a write of %d bytes answered %d, %v", end-from, n, err)
		}
		if v.values != nil && len(v.held) >= v.values.longest {
			t.Fatalf("the stream holds %d bytes after a piece, its longest value is %d", len(v.held),
				v.values.longest)
		}
		from = end
	}
	if err := v.Close(); err != nil {
		t.Fatalf("closing the stream: %v", err)
	}
	return out.String()
}

// cuttings is ways to cut a text of n bytes, drawn from r: whole, a byte at
// a time, and a few at random, an empty piece among them.
func cuttings(r *rand.Rand, n int) [][]int {
	every := make([]int, 0, n)
	for i := range n {
		every = append(every, i)
	}
	ways := [][]int{nil, every}
	for range 4 {
		var at []int
		for end := 0; end < n; end += r.IntN(9) {
			at = append(at, end)
		}
		ways = append(ways, at)
	}
	return ways
}

// A VALUE STREAM WRITES WHAT THE REPLACER DOES, HOWEVER THE TEXT IS CUT: what a
// [valueStream] writes, put together, is [scrubber.apply] of the whole text
// (today's strings.Replacer), whether the text arrives whole, a byte at a time
// or cut at random, empty pieces among them. Over values that begin inside one
// another, one another's starts, a value written across lines and its JSON
// string's form, and texts holding them, their starts and the bytes between.
// And it holds fewer bytes than its longest value from one piece to the next.
//
// Mutation: decide a place before the longest value's length of text from it
// is read ([valueStream.step]), and a value cut across two pieces is kept;
// order a byte's values shortest first ([newValueTable]), and a value that
// begins with another leaves its tail; let Close write what it holds as it
// is, and a value at the text's end is kept; hold every piece until Close,
// and the stream holds the whole text.
func TestAValueStreamWritesWhatTheReplacerDoesHoweverTheTextIsCut(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 31))
	for range 1500 {
		s, text := valueCase(r)
		want := s.apply(text)
		for _, at := range cuttings(r, len(text)) {
			if got := streamValues(t, s, text, at); got != want {
				t.Fatalf("%q cut at %v: wrote %q, the replacer %q", text, at, got, want)
			}
		}
	}
	// And a stream of no values writes the text as it is.
	if got := streamValues(t, newScrubber(nil), "a text\nof lines", []int{3, 9}); got != "a text\nof lines" {
		t.Errorf("a stream of no values wrote %q", got)
	}
}

// A VALUE STREAM ENDS AT ITS WRITER'S FIRST ERROR, and answers it from every
// call after, Close included, so a scrub never writes on past a write that
// failed.
//
// Mutation: forget the error once answered, and the next piece is written.
func TestAValueStreamEndsAtItsWritersFirstError(t *testing.T) {
	t.Parallel()
	failed := errors.New("the pipe is closed")
	w := &failingWriter{err: failed}
	v := newScrubber([]string{"opaque-engine-value"}).stream(w)
	for i, piece := range []string{"some text long enough ", "to be decided ", "and more to write "} {
		if _, err := v.Write([]byte(piece)); i > 0 && !errors.Is(err, failed) {
			t.Fatalf("write %d answered %v, want the writer's error", i, err)
		}
	}
	if err := v.Close(); !errors.Is(err, failed) {
		t.Errorf("close answered %v, want the writer's error", err)
	}
	if w.writes != 2 {
		t.Errorf("the writer was written %d times, want twice, the second failing", w.writes)
	}
}

// failingWriter takes its first write and fails every one after with err,
// counting them.
type failingWriter struct {
	err    error
	writes int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.writes++; f.writes > 1 {
		return 0, f.err
	}
	return len(p), nil
}

// wholeKeyCredential is [credentialKey]'s reading of a whole key as it was
// before the key was read in pieces, over the same lists of keys and names:
// the key lowered whole, its separators dropped whole, and an ID token's
// `id` found by its byte place. The oracle a [keyReading] is held to.
func wholeKeyCredential(key string) bool {
	if redact.NamesPassword(key) {
		return true
	}
	k := strings.ToLower(key)
	for _, whole := range wholeCredentialKeys {
		if k == whole {
			return true
		}
	}
	joined := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, k)
	for _, name := range credentialNames {
		if strings.Contains(joined, string(name)) {
			return true
		}
	}
	for i := 0; i+len("id") <= len(key); i++ {
		if !strings.EqualFold(key[i:i+len("id")], "id") || !wholeKeyWordStarts(key, i) {
			continue
		}
		rest := strings.TrimLeftFunc(key[i+len("id"):], func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if len(rest) >= len("token") && strings.EqualFold(rest[:len("token")], "token") {
			return true
		}
	}
	return false
}

// wholeKeyWordStarts is [wholeKeyCredential]'s word start at byte i of key.
func wholeKeyWordStarts(key string, i int) bool {
	if i == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(key[:i])
	if !unicode.IsLetter(prev) && !unicode.IsDigit(prev) {
		return true
	}
	cur, _ := utf8.DecodeRuneInString(key[i:])
	return unicode.IsUpper(cur) && (unicode.IsLower(prev) || unicode.IsDigit(prev))
}

// keyWords are the pieces [keyCase] builds a key from: every word a
// credential's key is read by, in more than one case and cut short; its
// separators; runes that lower to a letter of one (the Kelvin sign to `k`,
// the dotted capital I to `i`) and one that folds to `s` without lowering to
// it; and bytes that begin no rune.
var keyWords = []string{"pass", "word", "pwd", "PASSWD", "Password", "id", "ID", "Id", "iD", "i", "d", "_", "-",
	".", " ", "token", "TOKEN", "Token", "toke", "n", "key", "KEY", "auth", "AUTH", "api", "API", "secret", "session",
	"access", "refresh", "bearer", "private", "authorization", "credential", "identity", "registry", "x", "9",
	"paid", "in", "valid", "é", "K", "İ", "ſ", "\xff", "\xe2\x82", "\x82", "\xf0\x9f\x98"}

// keyCase is a key of keyWords drawn from r: a short one most of the time,
// and one long enough to pass every window of a [keyReading] the rest.
func keyCase(r *rand.Rand) string {
	words := 1 + r.IntN(6)
	if r.IntN(3) == 0 {
		words = 6 + r.IntN(40)
	}
	var b strings.Builder
	for range words {
		b.WriteString(keyWords[r.IntN(len(keyWords))])
	}
	return b.String()
}

// readKey is key handed to a [keyReading] in pieces whose ends are at, and
// whether it names a credential.
func readKey(key string, at []int) bool {
	var k keyReading
	from := 0
	for _, end := range append(at, len(key)) {
		_, _ = k.Write([]byte(key[from:end]))
		from = end
	}
	return k.Credential()
}

// A KEY READ IN PIECES IS READ AS THE SAME KEY WHOLE: a [keyReading] handed a
// key in pieces, whole, a byte at a time or cut at random (a rune's bytes
// cut apart among them), names a credential exactly where the whole key's
// reading does ([wholeKeyCredential]); and [credentialKey], a reading handed
// the whole key, does too. Over keys of every word a credential's key is read
// by, in more than one case, cut short and joined every way, runes that
// lower to a letter of one, and bytes that begin no rune; each key short, or
// long enough to pass every window the reading keeps.
//
// Mutation: keep none of a window when it fills, and a password's key or a
// name across the cut is missed; read a piece's last bytes as runes before
// the next piece is read, and a rune cut apart is no letter; read a rune that
// ends an ID token's reading only as its end, and `id_id_token` names none;
// read the whole key's `token`, `key` and `auth` anywhere, and `tokens` names
// one.
func TestAKeyReadInPiecesIsReadAsTheSameKeyWhole(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 32))
	keys := []string{"id_id_token", "xid_Id-token", "aIdToken", "a9IdToken", "OIDC_ID_TOKEN", "paid_tokens",
		"Key", "toKen", "İd_token", "pass\xffword", "pass_word", "tokens", "auth", "Auth\xe2\x82"}
	for range 6000 {
		keys = append(keys, keyCase(r))
	}
	for _, key := range keys {
		want := wholeKeyCredential(key)
		if got := credentialKey(key); got != want {
			t.Fatalf("credentialKey(%q) = %v, the whole key's reading %v", key, got, want)
		}
		for _, at := range cuttings(r, len(key)) {
			if got := readKey(key, at); got != want {
				t.Fatalf("%q cut at %v names a credential: %v, the whole key's reading %v", key, at, got, want)
			}
		}
	}
}

// A KEY READING HOLDS A FIXED STATE, HOWEVER LONG THE KEY: a key of a
// mebibyte, handed over in pieces, leaves the reading holding no more than
// its windows, twice the most a password's key spans in runes and twice the
// longest name in bytes (each with a rune's bytes over), and no more of the
// key lowered than the longest whole key, after every piece.
//
// Mutation: never cut a window back, and it holds the whole key.
func TestAKeyReadingHoldsAFixedStateHoweverLongTheKey(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 33))
	reach := keyReadingReach
	var k keyReading
	for written := 0; written < 1<<20; {
		var piece strings.Builder
		for piece.Len() < 4<<10 {
			// Words that name nothing, so the reading reads to the end.
			piece.WriteString([]string{"pas", "wor", "x_", "K", "é", "\xff", "ses", "sio"}[r.IntN(8)])
		}
		_, _ = k.Write([]byte(piece.String()))
		written += piece.Len()
		if len(k.written) > 2*reach.password*utf8.UTFMax || len(k.joined) >= 2*reach.name+utf8.UTFMax ||
			len(k.lowered) > reach.whole+utf8.UTFMax {
			t.Fatalf("after %d bytes the reading holds %d bytes of runes, %d of letters and %d lowered", written,
				len(k.written), len(k.joined), len(k.lowered))
		}
	}
	if k.Credential() {
		t.Errorf("a key of words that name nothing names a credential")
	}
}
