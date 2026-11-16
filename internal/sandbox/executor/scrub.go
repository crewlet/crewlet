package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE SCRUB A KEPT SESSION LEAVES ITS BOX THROUGH.
//
// A session is a harness's whole transcript — every file it read, every
// command's output — and what leaves a box passes the engine's secret
// redaction, as every other text read out of one does (code-sandbox.md). Two
// passes, in this order:
//
//  1. BY VALUE: every secret the engine itself put in the box — its login,
//     its environment's credentials — is taken out wherever it appears, in its
//     raw form and in the form a JSON string holds it in, whatever its shape.
//     A value redact's rules would never recognise (an operator's opaque
//     token) is gone all the same. First, so the second pass sees the marker
//     where the value was rather than a half it could not place.
//  2. BY SHAPE: redact.Secrets, the one denylist every surface uses, which
//     catches what the engine did not put there — a key the run printed.
//
// EACH FILE AS WHAT IT IS, so what is kept is still what its harness restores:
//
//   - A TRANSCRIPT, one JSON value a line (`.jsonl`): each value rewritten
//     string by string — every string scrubbed and redacted as the text it
//     decodes to, which is where a key's lines are lines again; every key of
//     every object scrubbed and redacted the same way, so a secret a tool's
//     structured result names a member by goes too; and a string or a number
//     under a credential's own key taken out whole. A line that is not one
//     JSON value (the last one of a job packed while it wrote, or a line of
//     blanks) is scrubbed and redacted as text, the values in the forms a
//     JSON string escapes them in among those it takes out. A RECORD KEEPS
//     ITS LINE AND ITS KEYS: the transcript keeps every line, and each record
//     every key in the order written, so Claude Code, which reads a session
//     file past 5 MiB by where each line's keys sit, still finds every record
//     where it did; what a record can lose is a loaded tool definition named
//     outside what the provider takes, cut out of its line as written
//     ([loadedToolsRecord]). Every key and string is written again
//     ([jsonStringWriter]) as the text it decodes to, changed only where it held a
//     secret: so an escape can come out spelled otherwise than its harness
//     wrote it, and a lone surrogate's escape or a byte that is no UTF-8, each
//     of which decodes to U+FFFD, comes out as U+FFFD.
//   - A DOCUMENT (`.json`, and OpenCode's whole export): rewritten string by
//     string as it streams, the same way, a token at a time, and one of
//     blanks alone written as nothing ([rewriteDocument]). A `.json` file
//     the rewrite cannot read as one JSON value — a task file its harness was
//     killed while it wrote, which the run's own end packs as it finds it
//     (a pack beside a harness still working refuses one it sees written,
//     claudeHeldTar) — is redacted as text, as a transcript's torn line is,
//     rather than costing the session.
//   - ANYTHING ELSE, as text, scrubbed and redacted as it streams, so a rule
//     that reads across lines sees them all.
//
// AND THE TAR'S OWN WORDS: a member's name is scrubbed by value and by shape
// as the transcript's paths to it are ([memberName]), its header carries
// nothing but its kind, name, mode, size and time — no owner, link target or
// extended record of the box's — and a member that is neither a file nor a
// directory (a link the box's own code made) is left out, since no harness
// keeps its session in one.
//
// # No value held whole, and never more than the archive's bound
//
// A harness keeps no bound on one value: Claude Code writes a whole PDF it
// read on one line of its transcript, its base64 twice, 55,924,056 bytes at
// its 20 MiB file limit, and OpenCode keeps a message part of 27 MiB. So the
// scrub holds NO LINE, STRING OR STRETCH OF TEXT WHOLE, and refuses none for
// its length: JSON is read on internal/jsonscan, which hands a name's or a
// string's decoded text over a piece at a time ([jsonRewrite]); every text is
// taken out by value through a [valueStream], which holds fewer bytes than the
// longest value, and redacted through redact.Stream, which writes exactly
// redact.Secrets of the whole text within its stages' windows ([textScrub]);
// and a transcript's line is never held to learn what it is ([measureLines]).
//
// What a line, or a `.json` file, is, JSON or text, is known only at its end,
// and what a transcript's line loses of its loaded tool definitions only once
// its last key is read; and a tar names each file's size before its bytes. So
// a tar is read TWICE: the first reading reads each line and each `.json` file
// both ways at once, counting what each would write, and keeps a PLAN of a
// bit a line for which it is and of a bit a definition for which is cut
// ([tarPlan]); the second writes what the first counted. An export is read
// twice as well, the first reading dry, so a reading that refuses writes
// nothing ([scrubDocument]). Every rewrite here is a pure function of what it
// reads, so the second reading writes what the first counted.
//
// The archive is the box's own: its code can write a gzip that expands a
// thousandfold, a value nested a million deep, or a session of nothing but
// text the redaction has to read. So the scrub reads at most [scrubExpansion]
// times the archive's bound of plaintext in one reading and rewrites no value
// nested deeper than jsonscan.MaxDepth as JSON (a transcript's line or a
// `.json` file so nested is text, and an export is refused); a session past
// the first, or one the redaction refuses ([errRedaction]: its stream decided
// a window before what it read there was known, redact.ErrDecidedEarly, at a
// write or at its close), is refused, stamped with why, rather than costing
// the engine host its memory; and the
// whole scrub is held to the take's time (internal/sandbox's
// SessionScrubWait).
//
// # What it holds
//
// AT MOST [scrubHeldBytes] of the plaintext bound, derived there from the
// code's own figures: two text scrubs at once (a line's or a `.json` file's
// two readings side by side, each redacting one text), each its redaction's
// stages (redact.Stream's own figure) beside a [valueStream] and a string's
// encoder; two JSON rewrites, each its scanner, its read buffer and a frame a
// container open; the readers and writers of the archive's gzip and tar; and
// the plan, at most a sixteenth of the plaintext ([tarPlan]). About 103 MiB at
// the archive's bound, 16 MiB of it the plan, which a real session leaves
// nearly empty; a test holds the figure at a smaller bound, and
// BenchmarkTheScrubAtThePlaintextBound measures it at the real one.

// archiveLayout is how a harness's archive is laid out.
type archiveLayout int

const (
	// archiveTar is a gzipped tar of files (Claude Code's).
	archiveTar archiveLayout = iota + 1
	// archiveDocument is one gzipped JSON document (OpenCode's export).
	archiveDocument
)

// scrubMarker is what a value the engine put in a box becomes in a kept
// session: redact's own marker, so a reader — and redact's second pass — knows
// it for one, and nothing a JSON string would have to escape.
const scrubMarker = redact.Marker + "engine-secret]"

// credentialMarker is what a string under a credential's own key becomes
// ([credentialKey]).
const credentialMarker = redact.Marker + "credential]"

// minScrubBytes is the shortest value taken out by value.
//
// EIGHT BYTES. A value shorter is as likely to be ordinary text in a transcript
// — a port, a version, a word — as a secret, and taking every occurrence of it
// out would corrupt the very transcript that is kept; nothing the engine hands
// a box from a credential position is that short, and redact's rules stay the
// backstop for one that is.
const minScrubBytes = 8

// scrubExpansion is how many times its archive's bound the plaintext of one
// reading of an archive may be: a session the scrub reads further than that is
// refused.
//
// EIGHT. A harness's transcript is JSON, which gzip shrinks about five to ten
// times, so a session near its bound reads some hundreds of mebibytes; eight
// times the thirty-two mebibyte bound is a quarter of a gibibyte, which the
// scrub's budget (internal/sandbox's SessionScrubWait) reads twice. A session
// more compressible than eight to one at the bound is refused — a judgment the
// two bounds make together, raised by raising this with the budget.
const scrubExpansion = 8

// errScrubBound is a session past what its scrub reads: refused.
var errScrubBound = errors.New("executor: the session is past the bounds a kept session is scrubbed within")

// scrubBounds is how much of an archive one scrub reads.
type scrubBounds struct {
	// plain is the most plaintext one reading of the archive yields.
	plain int64
}

// boundsFor is the scrub of an archive of at most archiveMax bytes.
func boundsFor(archiveMax int64) scrubBounds {
	return scrubBounds{plain: scrubExpansion * archiveMax}
}

// The figures [scrubHeldBytes] adds up, each the most one part of the scrub
// holds.
const (
	// streamHeldBytes is one redact.Stream: its stages' windows, at most
	// about 21 MiB of text and as much again of what they keep beside it,
	// as redact's package doc states and its own test holds, but for the two
	// texts that doc names, which hold a run and which no credential's
	// format writes.
	streamHeldBytes = 42 << 20
	// scannerHeldBytes is one jsonscan.Scanner: its read buffer and its
	// struct, the open containers' bits among it, as jsonscan's package doc
	// states and its own test holds. The scrub never captures a value, so
	// it holds no capture's buffer beside it.
	scannerHeldBytes = 67_072
	// rewriteFrameBytes is the most one of a JSON rewrite's frames takes
	// (rewriteFrame), which a test holds it to.
	rewriteFrameBytes = 40
	// rewriteHeldBytes is one JSON rewrite: its scanner, the buffer it
	// reads a text's pieces into, and a frame for each container open, at
	// most jsonscan.MaxDepth.
	rewriteHeldBytes = scannerHeldBytes + textWholeBytes + jsonscan.MaxDepth*rewriteFrameBytes
	// archiveHeldBytes is the archive's own readers and writers, the gzip
	// writer's compressor the largest: flate's hash chains and window, under
	// a mebibyte at the default level, beside gzip's reader, tar's reader
	// and writer and the buffers of the readers and writers above them.
	archiveHeldBytes = 2 << 20
	// planBlockBytes is the room of the [bitList] blocks being filled beyond
	// their bits: each of the plan's three streams, the three lists a line's
	// cut reads at once, and the block a list's bits move into while the
	// list's last one goes ([bitList.moveTo]) each round up to one.
	planBlockBytes = 7 * bitBlockWords * 8
)

// scrubHeldBytes is the most the scrub of a reading of at most plain bytes
// holds, longest the longest value it takes out ([scrubber]): two text scrubs
// at once, each a redaction's stream, a [valueStream] (fewer bytes than the
// longest value held, and twice that read beside them) and a string's encoder
// (a rune's bytes and the most of a text the cut reads); two JSON rewrites;
// the archive's readers and writers; and the plan, a bit for every two bytes
// of plaintext at most ([tarPlan]), so a sixteenth of it.
//
// TWO OF EACH, because the first reading reads a line, or a `.json` file,
// both ways at once ([measureLines], [measureFile]); the second reads one.
func scrubHeldBytes(plain int64, longest int) int64 {
	text := int64(streamHeldBytes + 3*longest + utf8.UTFMax + providerToolNameMax + 1)
	return 2*text + 2*rewriteHeldBytes + archiveHeldBytes + plain/16 + planBlockBytes
}

// scrubber takes the engine's own values out of text: a whole text through
// strings.Replacer ([scrubber.apply]), and a text that arrives in pieces
// through a [valueStream] of the same values ([scrubber.stream]).
type scrubber struct {
	r      *strings.Replacer
	values *valueTable
}

// newScrubber is the scrubber for values: each line of each value at least
// [minScrubBytes] long — a multi-line value (a key file a setup step wrote) is
// taken out a line at a time, which is how a transcript holds it, and a PEM
// armour line left where it is (redact.Unarmoured) — in its raw form and as a
// JSON string escapes it, the longest first, so a value holding another is
// taken out whole.
func newScrubber(values []string) scrubber {
	seen := map[string]bool{}
	var olds []string
	add := func(v string) {
		if len(v) >= minScrubBytes && !seen[v] {
			seen[v] = true
			olds = append(olds, v)
		}
	}
	for _, value := range values {
		for _, line := range strings.Split(value, "\n") {
			// A KEY'S ARMOUR IS NO SECRET OF IT: every key of its kind
			// carries the same BEGIN and END lines, and the shape pass reads
			// a key's block by them. Taken out by value, every other key of
			// that kind in the session — one the run printed, a fixture in the
			// repository — lost the armour the shape pass looks for, and its
			// body was kept. The rest of the line is taken out as any value is.
			for _, piece := range redact.Unarmoured(line) {
				add(piece)
				add(jsonEscaped(piece))
			}
		}
	}
	if len(olds) == 0 {
		return scrubber{}
	}
	slices.SortStableFunc(olds, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(olds))
	for _, old := range olds {
		pairs = append(pairs, old, scrubMarker)
	}
	return scrubber{r: strings.NewReplacer(pairs...), values: newValueTable(olds)}
}

// apply is s with every value taken out.
func (s scrubber) apply(text string) string {
	if s.r == nil {
		return text
	}
	return s.r.Replace(text)
}

// valueStream is the by-value pass over a text that arrives in pieces, cut
// anywhere: what it writes to w, put together, is [scrubber.apply] of the
// whole text, however the text was cut, once Close has written the last of it.
//
// AS THE REPLACER DECIDES, A PLACE AT A TIME: from the text's start, the
// longest value that begins at a place is taken out and the reading goes on
// after it, and where none begins the place's byte is kept and the reading
// goes on at the next; so a value is taken out leftmost, and longest where
// values begin at the same place, as strings.Replacer takes the first of its
// pairs, which [newScrubber] orders longest first.
//
// IT HOLDS FEWER BYTES THAN THE LONGEST VALUE from one piece to the next, and
// reads a piece where it lies: a place is decided once the longest value's
// length of text from it is read, or the text has ended, so only a place
// nearer a piece's end than that waits for the next piece. A value taken out
// lies whole within the bytes its place is decided on, so the stream carries
// nothing else from piece to piece.
type valueStream struct {
	w      io.Writer
	values *valueTable
	// held is the text past the last place decided, fewer bytes than the
	// longest value.
	held []byte
	// joined is held and the start of the next piece, read together.
	joined []byte
	// err is the first error of w, which ends the stream.
	err error
}

// valueTable is the values a [scrubber] takes out, as a [valueStream] reads
// them: by their first byte, longest first, and the longest one's length.
type valueTable struct {
	byFirst [256][][]byte
	longest int
}

// newValueTable is the table of olds, ordered longest first.
func newValueTable(olds []string) *valueTable {
	t := &valueTable{}
	for _, old := range olds {
		t.byFirst[old[0]] = append(t.byFirst[old[0]], []byte(old))
		t.longest = max(t.longest, len(old))
	}
	return t
}

// stream is s's by-value pass onto w, for a text written to it a piece at a
// time.
func (s scrubber) stream(w io.Writer) *valueStream {
	return &valueStream{w: w, values: s.values}
}

// Write reads the next piece of the text, and writes what of the text it has
// decided.
func (v *valueStream) Write(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if v.values == nil {
		return v.result(len(p), v.literal(p))
	}
	n := len(p)
	if len(v.held) > 0 {
		// Every place in held is decided on held and the first twice the
		// longest value's length of p: a value read from a place in held
		// ends within the longest value's length past it.
		take := min(len(p), 2*v.values.longest)
		v.joined = append(append(v.joined[:0], v.held...), p[:take]...)
		if take == len(p) {
			at, err := v.step(v.joined, len(v.joined), false)
			v.held = append(v.held[:0], v.joined[at:]...)
			return v.result(n, err)
		}
		at, err := v.step(v.joined, len(v.held), false)
		if err != nil {
			return v.result(n, err)
		}
		p = p[at-len(v.held):]
	}
	at, err := v.step(p, len(p), false)
	v.held = append(v.held[:0], p[at:]...)
	return v.result(n, err)
}

// Close decides every place still held, as the text's end decides them, and
// writes them. It closes nothing it was handed.
func (v *valueStream) Close() error {
	if v.err != nil || v.values == nil {
		return v.err
	}
	_, err := v.step(v.held, len(v.held), true)
	v.held = v.held[:0]
	_, err = v.result(0, err)
	return err
}

// step decides the places of text before stop, from its start, and writes
// them, a value taken out as [scrubMarker]: every place, once the text has
// ended, and otherwise each with the longest value's length of text read from
// it. It answers the first place it did not decide, every byte before which
// is written.
func (v *valueStream) step(text []byte, stop int, ended bool) (int, error) {
	at, last := 0, 0
	for at < stop {
		if !ended && at+v.values.longest > len(text) {
			break
		}
		matched := 0
		for _, value := range v.values.byFirst[text[at]] {
			if bytes.HasPrefix(text[at:], value) {
				matched = len(value)
				break
			}
		}
		if matched == 0 {
			at++
			continue
		}
		if err := v.literal(text[last:at]); err != nil {
			return at, err
		}
		if _, err := io.WriteString(v.w, scrubMarker); err != nil {
			return at, err
		}
		at += matched
		last = at
	}
	return at, v.literal(text[last:at])
}

// literal writes text as it is, and nothing where it is empty.
func (v *valueStream) literal(text []byte) error {
	if len(text) == 0 {
		return nil
	}
	_, err := v.w.Write(text)
	return err
}

// result is n read and the stream's error once err ends it.
func (v *valueStream) result(n int, err error) (int, error) {
	if err != nil {
		v.err = err
		return 0, err
	}
	return n, nil
}

// memberName is a tar member's name as the kept archive holds it: scrubbed by
// value and then by shape, as the transcript's paths to the member are (every
// string of a transcript passes both), so the restored file is where the
// transcript says it is — and a credential in a file's name, which a harness
// writes a tool's output under, does not survive in the archive's own words.
func memberName(s scrubber, name string) string {
	return redact.Secrets(s.apply(name))
}

// jsonEscaped is s as plainjson writes it in a JSON string, without its
// quotes: the form a JSON string a harness writes holds it in, escaped as
// JSON.stringify escapes with `<`, `>` and `&` left as they are, for every s
// but one holding U+2028 or U+2029, which plainjson writes as escapes and
// JSON.stringify as they are.
func jsonEscaped(s string) string {
	out, err := plainjson.Marshal(s, "")
	if err != nil {
		return s
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(out), `"`), `"`)
}

// credentialKey reports whether a JSON object's key names a credential, whose
// string value is taken out whole however it is shaped: a login's tokens, a
// tool call's password or API key, a cloud session's token. Read on the key's
// own words, lowercase: a password's, as redact names them
// (redact.NamesPassword), `token`, `key` and `auth` exactly, or one of the
// names a credential carries — an API key and an API token, the access token a
// login holds and the token that refreshes it, a cloud session's token, which
// AWS names a session token and a security token both, an auth token and a
// bearer token, a private key and a private token (GitLab's header), the
// authorization a request carries, a secret, a credential itself, an OpenID
// Connect ID token ([keyReading.idToken]), and the identity token and registry token
// a Docker config holds beside a registry's `auth`, which is its user and
// password encoded.
//
// THE NAMES ARE READ WITH THE KEY'S SEPARATORS DROPPED, since each format joins
// a name's words its own way: a session's token is `aws_session_token` in AWS's
// credentials file, `SessionToken` in what STS answers and `sessionToken` in
// what Roles Anywhere does, one name once the separators are gone; and a
// request's header joins its words with hyphens, so AWS's
// `X-Amz-Security-Token` is read by its security token, and an API key's
// `x-api-key` and GitLab's `PRIVATE-TOKEN` by theirs. Spelled out per
// separator, the names missed every header's.
//
// NOT `token`, `key` OR `auth` AT LARGE, which only the whole key is read as:
// a transcript counts its tokens and names its sessions in keys of its own
// (`input_tokens`, `sessionId`), an access key's id is no secret
// (`AccessKeyId`), a commit's author and a server's OAuth settings are no
// credential (`author`, `oauth_provider`), and a value under one is none.
//
// ONE READING, IN PIECES OR WHOLE: credentialKey is a [keyReading] handed the
// whole key at once, and a reader that has a key's decoded text a piece at a
// time hands one each piece, so a key cut anywhere is read as the same key
// whole and the rule has one implementation.
func credentialKey(key string) bool {
	var k keyReading
	k.WriteString(key)
	return k.Credential()
}

// wholeCredentialKeys are the keys [credentialKey] reads only as the whole key,
// lowercase: never at large, where a transcript's own keys spell them.
var wholeCredentialKeys = []string{"token", "key", "auth"}

// credentialNames are the names [credentialKey] reads anywhere in a key once
// it is lowercase and its separators are dropped.
var credentialNames = [][]byte{[]byte("apikey"), []byte("apitoken"), []byte("accesstoken"),
	[]byte("refreshtoken"), []byte("sessiontoken"), []byte("securitytoken"), []byte("authtoken"),
	[]byte("bearertoken"), []byte("privatekey"), []byte("privatetoken"), []byte("authorization"),
	[]byte("secret"), []byte("credential"), []byte("identitytoken"), []byte("registrytoken")}

// keyReadingReach is how far back each of [keyReading]'s windows has to reach:
// the longest whole key, the longest name, and the most runes a password's
// key spans ([redact.PasswordKeyMaxBytes]).
var keyReadingReach = func() (r struct{ whole, name, password int }) {
	for _, key := range wholeCredentialKeys {
		r.whole = max(r.whole, len(key))
	}
	for _, name := range credentialNames {
		r.name = max(r.name, len(name))
	}
	r.password = max(redact.PasswordKeyMaxBytes(), 1)
	return r
}()

// keyReading is a JSON object's key read a piece at a time, as its decoded
// text arrives, cut anywhere, a rune's bytes included: Write hands it the next
// piece, and Credential ends the key and reports whether it names a
// credential, [credentialKey]'s reading of the whole key.
//
// ITS STATE DOES NOT GROW WITH THE KEY, since a box's code can write a key a
// gibibyte long. It holds the bytes of a rune a piece ended inside (three at
// most); the key lowered for as long as it is no longer than the longest whole
// key; the key's last runes, as written, in a window twice the most a
// password's key spans, asked of [redact.NamesPassword] each time it fills and
// cut back to one rune fewer than that span, so a key the window has not yet
// seen whole is still in it; the lowered key's last letters and digits, in a
// window twice the longest name, read for every name and cut back the same
// way; and how far an ID token has read ([keyReading.idToken]), a few words.
//
// EVERY RUNE AS THE WHOLE KEY'S READING DECODES IT: a byte that begins no rune
// is U+FFFD wherever the key is cut, since a piece's last bytes wait for the
// next piece until they are a whole rune or none ([utf8.FullRune]).
type keyReading struct {
	// cut is the bytes of a rune the last piece ended inside.
	cut  [utf8.UTFMax - 1]byte
	ncut int
	// lowered is the key lowered while it is no longer than the longest
	// whole key; long once it is.
	lowered []byte
	long    bool
	// written is the key's last runes as written, U+FFFD for a byte that
	// begins none, and runes how many it holds.
	written []byte
	runes   int
	// joined is the lowered key's last letters and digits.
	joined []byte
	// prev is the key's last rune; any whether it has one.
	prev rune
	any  bool
	// id is how far an ID token has read, and token how much of `token`.
	id    idTokenRead
	token int
	// named is whether a credential is named already: nothing read after
	// changes it.
	named bool
}

// idTokenRead is how far [keyReading.idToken] has read an ID token's words.
type idTokenRead int

const (
	// idNone: no `id` that starts a word is being read.
	idNone idTokenRead = iota
	// idI: an `i` that starts a word, its `d` next.
	idI
	// idWord: `id`, its separators being read, `token` next.
	idWord
	// idToken: `id` and its separators, and keyReading.token bytes of
	// `token`.
	idToken
)

// reset readies k for another key, keeping its windows' room.
func (k *keyReading) reset() {
	*k = keyReading{lowered: k.lowered[:0], written: k.written[:0], joined: k.joined[:0]}
}

// WriteString is Write of s's bytes.
func (k *keyReading) WriteString(s string) {
	for len(s) > 0 {
		var chunk [256]byte
		n := copy(chunk[:], s)
		_, _ = k.Write(chunk[:n])
		s = s[n:]
	}
}

// Write reads the next piece of the key. It never fails.
func (k *keyReading) Write(p []byte) (int, error) {
	n := len(p)
	if k.named {
		return n, nil
	}
	if k.ncut > 0 {
		// The rune a piece ended inside, finished from this one: at most
		// three of its bytes are needed, and a run that is no rune is read
		// a byte at a time, as the whole key's decode reads it.
		var joined [2*utf8.UTFMax - 2]byte
		s := append(joined[:0], k.cut[:k.ncut]...)
		s = append(s, p[:min(len(p), utf8.UTFMax-1)]...)
		i := 0
		for i < k.ncut {
			if !utf8.FullRune(s[i:]) {
				// The whole of p is in s, and still no rune.
				k.ncut = copy(k.cut[:], s[i:])
				return n, nil
			}
			r, w := utf8.DecodeRune(s[i:])
			k.rune(r)
			i += w
		}
		p = p[i-k.ncut:]
		k.ncut = 0
	}
	for i := 0; i < len(p); {
		if c := p[i]; c < utf8.RuneSelf {
			k.rune(rune(c))
			i++
			continue
		}
		if !utf8.FullRune(p[i:]) {
			k.ncut = copy(k.cut[:], p[i:])
			break
		}
		r, w := utf8.DecodeRune(p[i:])
		k.rune(r)
		i += w
	}
	return n, nil
}

// Credential ends the key, a rune a piece ended inside read as the whole
// key's decode reads it, and reports whether the key names a credential.
func (k *keyReading) Credential() bool {
	for i := 0; i < k.ncut; {
		r, w := utf8.DecodeRune(k.cut[i:k.ncut])
		k.rune(r)
		i += w
	}
	k.ncut = 0
	if k.named {
		return true
	}
	if k.runes > 0 && redact.NamesPassword(string(k.written)) {
		return true
	}
	if !k.long && slices.Contains(wholeCredentialKeys, string(k.lowered)) {
		return true
	}
	return k.joinedNames()
}

// rune reads the key's next rune.
func (k *keyReading) rune(r rune) {
	if k.named {
		return
	}
	reach := keyReadingReach
	k.written = utf8.AppendRune(k.written, r)
	if k.runes++; k.runes == 2*reach.password {
		if redact.NamesPassword(string(k.written)) {
			k.named = true
			return
		}
		k.written, k.runes = lastRunes(k.written, reach.password-1), reach.password-1
	}
	lower := unicode.ToLower(r)
	if !k.long {
		if k.lowered = utf8.AppendRune(k.lowered, lower); len(k.lowered) > reach.whole {
			k.long, k.lowered = true, nil
		}
	}
	if unicode.IsLetter(lower) || unicode.IsDigit(lower) {
		if k.joined = utf8.AppendRune(k.joined, lower); len(k.joined) >= 2*reach.name {
			if k.joinedNames() {
				k.named = true
				return
			}
			// A name still to be read whole begins in the last bytes
			// but one of its length: bytes, not runes, since the names
			// are matched as bytes.
			k.joined = append(k.joined[:0], k.joined[len(k.joined)-(reach.name-1):]...)
		}
	}
	k.idToken(r)
	k.prev, k.any = r, true
}

// joinedNames reports whether the joined window holds one of
// [credentialNames].
func (k *keyReading) joinedNames() bool {
	for _, name := range credentialNames {
		if bytes.Contains(k.joined, name) {
			return true
		}
	}
	return false
}

// idToken reads r for an OpenID Connect ID token: `id` a word of its own and
// `token` after it, however a format joins the two (`id_token`, `idToken`,
// `OIDC_ID_TOKEN`), each of its letters in either case.
//
// A WORD OF ITS OWN, where every other name is read anywhere in the key,
// because `idtoken` is what a count spells by chance (`paid_tokens`,
// `invalid_tokens`), and a number under such a key is no credential's.
//
// ONE `id` AT A TIME: no letter of `d`, the separators or `token` is an `i`,
// so an `id` read on is never beside another that could start, and a rune that
// ends one reading is read again as the start of the next.
func (k *keyReading) idToken(r rune) {
	switch k.id {
	case idI:
		if r == 'd' || r == 'D' {
			k.id = idWord
			return
		}
	case idWord:
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return
		}
		if r == 't' || r == 'T' {
			k.id, k.token = idToken, 1
			return
		}
	case idToken:
		if r|0x20 == rune("token"[k.token]) {
			if k.token++; k.token == len("token") {
				k.named = true
			}
			return
		}
	}
	k.id = idNone
	if (r == 'i' || r == 'I') && k.wordStarts(r) {
		k.id = idI
	}
}

// wordStarts reports whether one of the key's words begins at r, read after
// the key's last rune: at the key's start, after a character that is neither
// a letter nor a digit, or at an uppercase letter after a lowercase one or a
// digit (`oidcIdToken`).
func (k *keyReading) wordStarts(r rune) bool {
	if !k.any || !unicode.IsLetter(k.prev) && !unicode.IsDigit(k.prev) {
		return true
	}
	return unicode.IsUpper(r) && (unicode.IsLower(k.prev) || unicode.IsDigit(k.prev))
}

// lastRunes is b's last n runes, moved to b's start.
func lastRunes(b []byte, n int) []byte {
	at := len(b)
	for ; n > 0 && at > 0; n-- {
		_, w := utf8.DecodeLastRune(b[:at])
		at -= w
	}
	return append(b[:0], b[at:]...)
}

// scrubArchive streams the archive first holds, of layout, scrubbed onto dst,
// within bounds. reopen opens the same archive again from its start, for the
// second of the two readings every layout is read in. It closes nothing it was
// handed; the caller owns every reader.
func scrubArchive(layout archiveLayout, first io.Reader, reopen func() (io.ReadCloser, error),
	dst io.Writer, s scrubber, b scrubBounds) error {

	switch layout {
	case archiveTar:
		return scrubTar(first, reopen, dst, s, b)
	case archiveDocument:
		return scrubDocument(first, reopen, dst, s, b)
	}
	return fmt.Errorf("executor: no scrub for an archive of layout %d", layout)
}

// scrubDocument streams a gzipped JSON document, rewritten string by string,
// read TWICE: once dry, writing nothing, and again to write it.
//
// DRY FIRST, because a reading refuses a document only once it has read it: a
// document that is no one JSON value, one past the plaintext a reading reads,
// or text the redaction could not redact. Read once, the bytes rewritten
// before the refusal would reach the take's pipe, its uploader, its checksum
// and its context meter before the pipe ended in the error, a published key
// among them where the redaction is what refused. So no byte of a reading that
// refuses reaches any of them. The second reading writes what the first read,
// and one that comes out another length, or that the scanner refuses where
// the first read one value whole, is an export that changed between the two,
// which fails as a tar's member that changed does; a second reading refused
// for another reason (past the plaintext's bound, or by the redaction) says
// that reason, as a first reading's refusal does.
func scrubDocument(first io.Reader, reopen func() (io.ReadCloser, error), dst io.Writer, s scrubber,
	b scrubBounds) error {

	dry := &counted{}
	if err := rewriteExport(first, dry, s, b); err != nil {
		return fmt.Errorf("executor: scrubbing the session's export: %w", err)
	}
	again, err := reopen()
	if err != nil {
		return fmt.Errorf("executor: opening the session's export again to write it: %w", err)
	}
	zw := gzip.NewWriter(dst)
	written := &counted{w: zw}
	err = rewriteExport(again, written, s, b)
	var fault *jsonscan.Error
	switch {
	case errors.As(err, &fault) || err == nil && written.n != dry.n:
		return errors.New("executor: the session's export changed while it was read")
	case err != nil:
		return fmt.Errorf("executor: scrubbing the session's export: %w", err)
	}
	return zw.Close()
}

// rewriteExport is one reading of a gzipped export, rewritten onto dst.
func rewriteExport(src io.Reader, dst io.Writer, s scrubber, b scrubBounds) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("executor: the session's export is not gzip: %w", err)
	}
	return rewriteDocument(dst, boundedPlain(zr, b.plain), s)
}

// scrubTar streams a gzipped tar with every regular file rewritten
// ([rewriteFile]): read once to learn each file's size as rewritten — and, for
// a `.json` file, whether it is JSON, and for a transcript, which of its lines
// are and which loaded tool definitions it loses; and again to write it under
// a header that says so.
//
// THE SECOND READING MUST BE THE FIRST'S ARCHIVE: a file whose rewrite comes
// out another size, or a member the first reading did not have, is an archive
// that changed between the two, and the scrub fails rather than write a tar
// whose headers lie. The failure names the member SHORT ([memberNamed]).
func scrubTar(first io.Reader, reopen func() (io.ReadCloser, error), dst io.Writer, s scrubber,
	b scrubBounds) error {

	plan := &tarPlan{}
	err := eachMember(first, b, func(h *tar.Header, body io.Reader) error {
		plan.members.putByte(h.Typeflag)
		if h.Typeflag != tar.TypeReg {
			return nil
		}
		from := plan.lines.count()
		size, text, err := measureFile(h.Name, body, s, plan)
		if err != nil {
			return err
		}
		plan.members.putNumber(uint64(size))
		plan.members.put(text)
		plan.members.putNumber(uint64(plan.lines.count() - from))
		return nil
	})
	if err != nil {
		return err
	}
	again, err := reopen()
	if err != nil {
		return fmt.Errorf("executor: opening the session's archive again to write it: %w", err)
	}
	zw := gzip.NewWriter(dst)
	tw := tar.NewWriter(zw)
	err = eachMember(again, b, func(h *tar.Header, body io.Reader) error {
		kind, ok := plan.members.takeByte()
		if !ok {
			return errors.New("executor: the session's archive gained a member while it was read")
		}
		if h.Typeflag != kind {
			return errMemberChanged(h.Name)
		}
		header := tar.Header{Typeflag: h.Typeflag, Name: memberName(s, h.Name), Mode: h.Mode & 0o777,
			ModTime: h.ModTime}
		switch h.Typeflag {
		case tar.TypeDir:
			return tw.WriteHeader(&header)
		case tar.TypeReg:
		default:
			return nil
		}
		size, sized := plan.members.takeNumber()
		text, read := plan.members.take()
		lines, listed := plan.members.takeNumber()
		if !sized || !read || !listed {
			return errMemberChanged(h.Name)
		}
		header.Size = int64(size)
		if headerErr := tw.WriteHeader(&header); headerErr != nil {
			return headerErr
		}
		w := &counted{w: tw}
		from := plan.lines.read
		end := from + int(lines)
		if memberErr := rewriteMember(h.Name, text, plan, end, w, body, s); memberErr != nil {
			if errors.Is(memberErr, errLinesChanged) {
				return errMemberChanged(h.Name)
			}
			return memberErr
		}
		if w.n != header.Size || plan.lines.read != end {
			return errMemberChanged(h.Name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !plan.members.done() {
		return errors.New("executor: the session's archive lost a member while it was read")
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// errMemberChanged is the member named name, as a failure quotes it, found
// changed by the second reading.
func errMemberChanged(name string) error {
	return fmt.Errorf("executor: the session's archive changed its member %s while it was read",
		memberNamed(name))
}

// memberNamed is an archive member's name as a failure quotes it: its first
// line, redacted and held to what a transcript line quotes of a path
// ([firstLine], [transcriptDetailLimit]).
//
// THE NAME IS THE ARCHIVE'S, which the box wrote, and a tar can name a member
// in up to a MiB (a PAX record); the failure becomes the reason the run's
// session was not kept, which the run's row, the launch's record and a box
// host's reply to the node that asked for the read all carry.
func memberNamed(name string) string { return firstLine(name, transcriptDetailLimit) }

// eachMember reads a gzipped tar member by member, its plaintext bounded.
func eachMember(src io.Reader, b scrubBounds, member func(*tar.Header, io.Reader) error) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("executor: the session's archive is not gzip: %w", err)
	}
	tr := tar.NewReader(boundedPlain(zr, b.plain))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("executor: reading the session's archive: %w", err)
		}
		if err = member(h, tr); err != nil {
			return err
		}
	}
}

// boundedPlain is a session's decompressed stream, refused past bound bytes:
// an archive a box's own code wrote can expand without end.
func boundedPlain(r io.Reader, bound int64) io.Reader {
	return &plainBound{r: r, left: bound, bound: bound}
}

// plainBound is [boundedPlain]'s reader.
type plainBound struct {
	r           io.Reader
	left, bound int64
}

func (p *plainBound) Read(buf []byte) (int, error) {
	if p.left <= 0 {
		// ONE BYTE MORE says whether the stream ends at the bound or goes
		// past it.
		var one [1]byte
		n, err := p.r.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: it expands past %d bytes", errScrubBound, p.bound)
		}
		return 0, err
	}
	if int64(len(buf)) > p.left {
		buf = buf[:p.left]
	}
	n, err := p.r.Read(buf)
	p.left -= int64(n)
	return n, err
}

// counted counts what passes through it to w, or counts alone where w is nil.
type counted struct {
	w io.Writer
	n int64
}

func (c *counted) Write(p []byte) (int, error) {
	if c.w == nil {
		c.n += int64(len(p))
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// measureFile is how one file of an archive is to be kept, read through by the
// tar's first reading: how many bytes it comes to, and, for a `.json` file,
// whether it is kept as text because it is not JSON; a transcript's lines are
// planned on plan ([measureLines]).
//
// A `.json` FILE IS READ BOTH WAYS AT ONCE, its bytes handed to a rewrite as a
// document and to one as text: which it takes is known only once it has been
// read through, and a tar member is read once in each reading.
//
// ON ONE GOROUTINE, THE SCRUB'S OWN: the document's rewrite reads the file
// through a reader that hands every byte it reads to the text's scrub as well
// ([teeRead]), as a transcript's line is read ([lineReader]), so every byte of
// the file is rewritten and redacted where the take's containment of a panic
// sees it (scrubContained). A goroutine for each reading, fed through pipes,
// as the two were read until then, put the rewrite and the redaction of every
// `.json` file a box wrote outside it, and a panic there ended the engine; and
// a panic in the copy beside them left both blocked on their pipes for good.
func measureFile(name string, body io.Reader, s scrubber, plan *tarPlan) (size int64, text bool, err error) {
	switch path.Ext(name) {
	case ".jsonl":
		size, err = measureLines(body, s, plan)
		return size, false, err
	case ".json":
	default:
		n := &counted{}
		err = rewriteText(n, body, s)
		return n.n, false, err
	}
	asText := &counted{}
	textual := s.text(asText)
	tee := &teeRead{r: body, w: textual}
	asDocument := &counted{}
	documentErr := rewriteDocument(asDocument, tee, s)
	// THE REST IS THE TEXT'S ALONE: a document the rewrite stopped reading
	// is read on to its end, for its text.
	if _, err = io.Copy(io.Discard, tee); err == nil {
		err = tee.readErr
	}
	if err != nil {
		return 0, false, err
	}
	textErr := tee.writeErr
	if closeErr := textual.Close(); textErr == nil {
		textErr = closeErr
	}
	// A DOCUMENT THE REWRITE CANNOT READ, FOR WHATEVER REASON, IS TEXT: torn,
	// more than one value, nested past the scanner's depth, or holding text
	// its redaction could not redact; kept as text only where its text's own
	// redaction did not fail. The archive's own failure is neither's, and
	// refuses the session above.
	switch {
	case documentErr == nil:
		return asDocument.n, false, nil
	case textErr != nil:
		return 0, false, textErr
	}
	return asText.n, true, nil
}

// teeRead reads r and hands every byte it reads to w as well: a `.json`
// file's text scrub, fed what the document's rewrite reads of the file
// ([measureFile]).
type teeRead struct {
	r io.Reader
	w io.Writer
	// readErr is r's failure, other than its end, which no reading of the
	// file survives; writeErr is w's first, after which w is handed nothing
	// more and the document is read on.
	readErr, writeErr error
}

func (t *teeRead) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.writeErr == nil {
		_, t.writeErr = t.w.Write(p[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) && t.readErr == nil {
		t.readErr = err
	}
	return n, err
}

// rewriteMember writes one file of an archive as the first reading planned it:
// as text where it found a `.json` file that is not JSON, and as what its name
// says it is otherwise ([rewriteFile]), a transcript's lines up to the bit end
// of plan's.
func rewriteMember(name string, text bool, plan *tarPlan, end int, dst io.Writer, src io.Reader,
	s scrubber) error {

	if text {
		return rewriteText(dst, src, s)
	}
	return rewriteFile(name, plan, end, dst, src, s)
}

// rewriteFile writes one file of an archive scrubbed, read as what its name
// says it is (see the file comment), a transcript's lines as plan plans them.
func rewriteFile(name string, plan *tarPlan, end int, dst io.Writer, src io.Reader, s scrubber) error {
	switch path.Ext(name) {
	case ".jsonl":
		return writeLines(dst, src, s, plan, end)
	case ".json":
		return rewriteDocument(dst, src, s)
	}
	return rewriteText(dst, src, s)
}

// tarPlan is how the first reading of a tar decided to write it, which the
// second reads back as it writes: three streams of bits ([planBits]), each
// written by the first reading in the order it decides them and read by the
// second in that same order, and nothing else, whatever the tar holds.
//
//   - members: for each member, its kind as the archive gave it, which the
//     second reading must find again, and for a regular file its size as
//     rewritten, whether it is kept as text (a `.json` file that is no JSON),
//     and how many of lines' bits its transcript's lines took.
//   - lines: for each line of two bytes or more before its break, of every
//     transcript in turn, whether it is kept as text, since it is no JSON
//     value ([measureLines]).
//   - cuts: for each line that loses a loaded tool definition
//     ([definitionsReading]), how many of lines' bits lie between it and the
//     line of the cut before it, the list it loses them from as its line's
//     containers count it, how many elements that list holds, and a bit for
//     each, set for one cut.
//
// A LINE SHORTER THAN TWO BYTES HAS NO BIT: it is one byte or none, which no
// pass by value or by shape changes, so it is the same whether it is read as
// JSON (a digit) or as text, and both readings write it as text.
//
// SO THE PLAN IS AT MOST A SIXTEENTH OF THE PLAINTEXT, a bit for every two of
// its bytes, beside a block of each stream's being filled: a member's header
// is 512 bytes and its record a few dozen bits; a line with a bit is two bytes
// or more; a list's element, a byte and its comma, takes one bit, and its cut
// a few dozen bits more, which the loaded-tools record it is cut out of, some
// seventy bytes, pays for. Nothing is kept for a member, a line or a cut but
// its bits: a list or a record of its own for each, as the plan was first
// written, cost a block or some dozen bytes a line, and let a box's archive of
// a mebibyte, a transcript of short lines each losing a definition, hold the
// engine host to gibibytes between the two readings.
type tarPlan struct {
	members, lines, cuts planBits
	// planned is the line after the last one the first reading cut, as
	// lines' bits count them, where the next cut's distance counts from;
	// reached is the same for the second reading.
	planned, reached int
	// next is the second reading's next cut, read off cuts ahead of its line
	// (pending), at line nextAt.
	next    definitionCut
	nextAt  int
	pending bool
}

// addCut plans the cut of the line at index, as lines' bits count it, of list:
// its bits move to the plan, and list holds none after it.
func (p *tarPlan) addCut(index int, list *listRead) {
	p.cuts.putNumber(uint64(index - p.planned))
	p.planned = index + 1
	p.cuts.putNumber(uint64(list.container))
	p.cuts.putNumber(uint64(list.n))
	list.bits.moveTo(&p.cuts)
}

// cutAt is the cut the second reading applies to the line at index, nil where
// the first reading cut nothing from it. A cut planned for a line the second
// reading has passed is a transcript that changed between the two.
func (p *tarPlan) cutAt(index int) (*definitionCut, error) {
	if err := p.readCut(); err != nil {
		return nil, err
	}
	switch {
	case !p.pending || p.nextAt > index:
		return nil, nil
	case p.nextAt < index:
		return nil, errLinesChanged
	}
	p.pending = false
	p.reached = index + 1
	return &p.next, nil
}

// passed says the second reading has written every line before end, as lines'
// bits count them: a cut planned for one it did not apply is a transcript that
// changed between the two.
func (p *tarPlan) passed(end int) error {
	if err := p.readCut(); err != nil {
		return err
	}
	if p.pending && p.nextAt < end {
		return errLinesChanged
	}
	return nil
}

// readCut reads the next cut's line, list and length, where none is pending
// and one is left. Its bits are read as its list is written ([definitionCut]).
func (p *tarPlan) readCut() error {
	if p.pending || p.cuts.done() {
		return nil
	}
	gap, ok := p.cuts.takeNumber()
	list, listed := p.cuts.takeNumber()
	n, counted := p.cuts.takeNumber()
	if !ok || !listed || !counted {
		return errLinesChanged
	}
	p.next = definitionCut{list: int(list), n: int(n), bits: &p.cuts}
	p.nextAt = p.reached + int(gap)
	p.pending = true
	return nil
}

// errLinesChanged is a transcript the second reading finds other than the
// first planned it: a line JSON where it was text or text where it was JSON,
// or more lines than were planned. The tar's failure names the member.
var errLinesChanged = errors.New("executor: a transcript changed while it was read")

// lineReader is a transcript's lines, read one at a time ([lineReader.next])
// as the scanner's text or the text scrub's: a line's bytes up to its break,
// never the break, with io.EOF at its end. tee is handed every byte of the line
// read, where it is set: the first reading's text candidate.
type lineReader struct {
	r   *bufio.Reader
	tee io.Writer
	// done says the line is read to its end; ended that its break was read.
	done, ended bool
	// err is an error of the transcript's reader, which ends the reading.
	err error
	// sc and j rewrite a line as JSON, each allocated once for every line
	// of a reading; drain is what a line's rest is read into.
	sc    *jsonscan.Scanner
	j     *jsonRewrite
	drain []byte
}

// newLineReader reads src's lines, rewriting them with s's values.
func newLineReader(src io.Reader, s scrubber) *lineReader {
	l := &lineReader{r: bufio.NewReaderSize(src, 64<<10), j: newJSONRewrite(s), drain: make([]byte, 32<<10)}
	l.sc = jsonscan.New(l)
	return l
}

// next readies the next line, tee handed its bytes: short says it is shorter
// than two bytes before its break, and more is false at the transcript's end,
// where no line is left.
func (l *lineReader) next(tee io.Writer) (short, more bool, err error) {
	head, err := l.r.Peek(2)
	if len(head) == 0 {
		if errors.Is(err, io.EOF) {
			return false, false, nil
		}
		return false, false, err
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return false, false, err
	}
	l.tee, l.done, l.ended, l.err = tee, false, false, nil
	return head[0] == '\n' || len(head) == 1 || head[1] == '\n', true, nil
}

// Read reads the line's next bytes, never past its break.
func (l *lineReader) Read(p []byte) (int, error) {
	if l.done {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if l.r.Buffered() == 0 {
		if _, err := l.r.Peek(1); err != nil {
			l.done = true
			if !errors.Is(err, io.EOF) {
				l.err = err
				return 0, err
			}
			return 0, io.EOF
		}
	}
	chunk, _ := l.r.Peek(min(l.r.Buffered(), len(p)))
	took := len(chunk)
	if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
		chunk, took = chunk[:i], i+1
		l.done, l.ended = true, true
	}
	n := copy(p, chunk)
	if l.tee != nil && n > 0 {
		if _, err := l.tee.Write(p[:n]); err != nil {
			l.err, l.done = err, true
			return 0, err
		}
	}
	_, _ = l.r.Discard(took)
	if n == 0 && l.done {
		return 0, io.EOF
	}
	return n, nil
}

// rest reads the line to its end, its bytes handed to tee.
func (l *lineReader) rest() error {
	for {
		_, err := l.Read(l.drain)
		if errors.Is(err, io.EOF) {
			return l.err
		}
		if err != nil {
			return err
		}
	}
}

// measureLines is the first reading of a transcript: how many bytes it comes
// to as kept, each of its lines planned on plan.
//
// EACH LINE IS READ BOTH WAYS AT ONCE, as JSON for as long as its grammar
// holds, and as text: which it is is known only at its end, and a line is read
// once in each reading and never held. So two scrubs of it run side by side,
// each counting what it would write, and the plan keeps a bit for which the
// line is: what the second reading writes, a line of each written exactly as
// the scrub of that line whole writes it.
func measureLines(src io.Reader, s scrubber, plan *tarPlan) (int64, error) {
	l := newLineReader(src, s)
	defs := &definitionsReading{}
	var asJSON rewriteOut
	var size int64
	for {
		asText := &counted{}
		text := s.text(asText)
		short, more, err := l.next(text)
		if err != nil || !more {
			return size, err
		}
		var jsonErr error
		if !short {
			asJSON.reset(nil, true)
			l.sc.Reset(l)
			l.j.reset(l.sc, &asJSON, defs, nil)
			jsonErr = l.j.run()
		}
		if err = l.rest(); err != nil {
			return 0, err
		}
		keptAsText, err := lineKeptAs(short, jsonErr, text.Close())
		switch {
		case err != nil:
			return 0, err
		case short:
			size += asText.n
		case keptAsText:
			plan.lines.put(true)
			size += asText.n
		default:
			index := plan.lines.count()
			plan.lines.put(false)
			n := asJSON.n
			if list, taken, ok := defs.decide(asJSON.marked); ok {
				plan.addCut(index, list)
				n -= taken
			}
			size += n
		}
		if l.ended {
			size++
		}
	}
}

// lineKeptAs is which of a line's two readings the first reading keeps, short
// saying the line has no bit, jsonErr what ended its JSON reading and textErr
// what its text's scrub answered at its close: the text where the line is
// short or no JSON value (a *jsonscan.Error), and its JSON otherwise. err is
// the kept reading's own failure, which refuses the session.
//
// NEVER THE OTHER'S: a reading that is not written is no miss, so a text the
// redaction could not finish deciding refuses a line kept as text and not one
// kept as JSON, whose strings the redaction reads on their own; as a `.json`
// file's document read whole leaves its text's failure unread
// ([measureFile]).
func lineKeptAs(short bool, jsonErr, textErr error) (text bool, err error) {
	var fault *jsonscan.Error
	switch {
	case short || errors.As(jsonErr, &fault):
		return true, textErr
	case jsonErr != nil:
		return false, jsonErr
	}
	return false, nil
}

// writeLines is the second reading of a transcript, writing each line as the
// first reading planned it ([measureLines]), its lines' bits those of plan's
// up to end.
func writeLines(dst io.Writer, src io.Reader, s scrubber, plan *tarPlan, end int) error {
	l := newLineReader(src, s)
	w := bufio.NewWriterSize(dst, 64<<10)
	var out rewriteOut
	for {
		short, more, err := l.next(nil)
		if err != nil {
			return err
		}
		if !more {
			if plan.lines.read != end {
				return errLinesChanged
			}
			if err = plan.passed(end); err != nil {
				return err
			}
			return w.Flush()
		}
		asText := short
		index := plan.lines.read
		if !short {
			var ok bool
			if asText, ok = plan.lines.take(); !ok || index >= end {
				return errLinesChanged
			}
		}
		if asText {
			text := s.text(w)
			l.tee = text
			if err = l.rest(); err != nil {
				return err
			}
			if err = text.Close(); err != nil {
				return err
			}
		} else {
			cut, cutErr := plan.cutAt(index)
			if cutErr != nil {
				return cutErr
			}
			out.reset(w, false)
			l.sc.Reset(l)
			l.j.reset(l.sc, &out, nil, cut)
			err = l.j.run()
			var fault *jsonscan.Error
			switch {
			case l.err != nil:
				return l.err
			case errors.As(err, &fault):
				return errLinesChanged
			case err != nil:
				return err
			case cut != nil && !cut.whole():
				return errLinesChanged
			}
		}
		if l.ended {
			if err = w.WriteByte('\n'); err != nil {
				return err
			}
		}
	}
}

// loadedToolsRecord is the `type` of the attachment Claude Code keeps in its
// transcript for the tool definitions a session loaded through tool search.
// A session resumed from the transcript sends each one again as a tool of
// every request it makes (measured on 2.1.322).
//
// A DEFINITION THE SCRUB RENAMED IS CUT OUT OF ITS LINE ([definitionsReading],
// [definitionCut]): every loaded tool definition whose name the rewrite left
// outside what [providerToolName] takes, where a marker stands where part of
// the name was, and resent under that name the definition has every request of
// the restored session refused. Left out, the session loads the tool again by
// search when it needs it. A line that holds no marker loses nothing, since
// the rewrite renamed nothing in it, a definition its writer named outside the
// pattern included; one that holds a marker loses every definition named
// outside the pattern, whoever named it so. A line that is not one JSON object
// loses nothing either. The record is read as Claude Code's JSON.parse reads
// it, its attachment, the attachment's type and its list each the last value
// written under its key, each key by its name exactly; and a definition is
// judged by the name Claude Code sends it under, its `name` exactly and the
// last written, a value that is no string naming nothing, so a definition that
// is no object, or names nothing, is cut where its line holds a marker.
//
// CUT OUT OF THE LINE AS THE REWRITE WROTE IT, each definition with the comma
// that joined it to its list, every other byte of the line left where it was:
// never the record decoded and written again, which wrote its keys sorted, the
// attachment (a subagent's agentId before it) first and its list inside it.
// Claude Code reads a session file past 5 MiB by the bytes of its lines, and
// finds a record by where its keys sit, in two readings (read in 2.1.310 and
// 2.1.322):
//
//   - 2.1.310, reading such a file as it does by default, takes a record for
//     a subagent's only where `"isSidechain":true` sits in its line's first
//     256 bytes, and walks the thread back from the last record it takes for
//     the main thread's. A subagent's record written sorted, its list before
//     that key, was that record: the walk began there, and records the
//     subagent wrote after it were left out of the thread a relaunch resumed.
//     2.1.322 looks for the key past those bytes as well.
//   - Under CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP (a switch a managed settings
//     file can set whatever a launch unsets), both prune such a file, taking a
//     line for a record only where it begins `{"parentUuid":` and walking the
//     main thread back from its last record through such lines alone. A
//     record's line that begins otherwise ends that walk, and where the
//     records the walk reached held half the file or less, every record above
//     that line was pruned from the session a relaunch resumed.
//
// DECIDED IN THE FIRST READING, APPLIED IN THE SECOND: whether a line loses a
// definition is known only at its end (the attachment's last `type`, its last
// `entries`, a marker anywhere), and the second reading writes the list before
// it reads that end.
const loadedToolsRecord = "deferred_tools_record"

// providerToolNameMax is the longest name the provider takes for a tool.
const providerToolNameMax = 128

// providerToolName reports whether name is one the provider takes for a tool
// (`^[a-zA-Z0-9_-]{1,128}$`); a request carrying a definition named outside
// that is refused whole. Spelled out, not a compiled pattern: a package-level
// one is allocated at every start of the binary, the git hooks' included.
func providerToolName(name string) bool {
	if len(name) < 1 || len(name) > providerToolNameMax {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// rewriteDocument streams one JSON value from src to dst a token at a time,
// every string value and every key scrubbed and redacted, and a value under a
// credential's key taken out whole ([jsonRewrite]). A text that is no one JSON
// value ends it on the scanner's *jsonscan.Error: more than one value, one
// cut short, or one nested past jsonscan.MaxDepth.
//
// BUT A DOCUMENT OF BLANKS ALONE, or of nothing, holds no value, and is
// written as nothing, as the scrub wrote one when it read documents on
// encoding/json, whose decoder answers such a text with its end: blanks hold
// nothing a harness reads, so a `.json` file of them is kept empty and an
// export of them kept, empty, rather than refused as no JSON. A transcript's
// line of blanks is read on its own and kept as its blanks, as every line
// that is no JSON value is kept as its text ([measureLines]).
func rewriteDocument(dst io.Writer, src io.Reader, s scrubber) error {
	w := bufio.NewWriterSize(dst, 64<<10)
	var out rewriteOut
	out.reset(w, false)
	j := newJSONRewrite(s)
	j.reset(jsonscan.New(src), &out, nil, nil)
	var fault *jsonscan.Error
	if err := j.run(); err != nil && (!errors.As(err, &fault) || fault.Fault != jsonscan.FaultNoValue) {
		return err
	}
	return w.Flush()
}

// keptString is a string value under a key that is no credential's, as a kept
// document holds it: every value the take names taken out, then every shape of
// a secret redacted. The ONE rule for it, so what the take keeps beside an
// archive of a document — where its run began in it ([keptRunStart]) — is
// spelled as the document spells the same text, whose rewrite scrubs a string
// by this rule whole or, past [textWholeBytes], through the streams that write
// exactly what it writes.
func keptString(scrub scrubber, s string) string {
	return keptText(scrub, s)
}

// rewriteText streams text scrubbed by value and redacted by shape as it
// arrives ([scrubber.text]): what it writes is exactly redact.Secrets of the
// whole text with every value taken out, held to the redaction's own windows
// rather than to a line or a stretch of it.
func rewriteText(dst io.Writer, src io.Reader, s scrubber) error {
	w := bufio.NewWriterSize(dst, 64<<10)
	text := s.text(w)
	if _, err := io.Copy(text, src); err != nil {
		return err
	}
	if err := text.Close(); err != nil {
		return err
	}
	return w.Flush()
}
