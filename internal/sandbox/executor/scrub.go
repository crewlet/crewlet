package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

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
//     under a credential's own key taken out whole. A line that is not JSON —
//     the last one of a job packed while it wrote — is scrubbed and redacted
//     as text, the values in the forms a JSON string escapes them in among
//     those it takes out. A RECORD KEEPS ITS LINE AND ITS KEYS: the transcript
//     keeps every line, and each record every key in the order written, so
//     Claude Code, which reads a session file past 5 MiB by where each line's
//     keys sit, still finds every record where it did; what a record can lose
//     is a loaded tool definition named outside what the provider takes, cut
//     out of its line as written ([withoutBrokenDefinitions]). Every key and
//     string is written again ([jsonRewrite.writeString]) as the text Go
//     decodes it to, changed only where it held a secret: so an escape can
//     come out spelled otherwise than its harness wrote it, and a lone
//     surrogate's escape or a byte that is no UTF-8, each of which Go decodes
//     to U+FFFD, comes out as U+FFFD.
//   - A DOCUMENT (`.json`, and OpenCode's whole export): rewritten string by
//     string as it streams, the same way, a token at a time. A `.json` file
//     the rewrite cannot read as one JSON value — a task file its harness was
//     killed while it wrote, which the run's own end packs as it finds it
//     (a pack beside a harness still working refuses one it sees written,
//     claudeHeldTar) — is redacted as text, as a transcript's torn line is,
//     rather than costing the session.
//   - ANYTHING ELSE, as text: each whole line scrubbed, and redacted as
//     redact settles it (redact.Settled, as a live tail is), so a rule that
//     reads across lines sees them all.
//
// AND THE TAR'S OWN WORDS: a member's name is scrubbed by value and by shape
// as the transcript's paths to it are ([memberName]), its header carries
// nothing but its kind, name, mode, size and time — no owner, link target or
// extended record of the box's — and a member that is neither a file nor a
// directory (a link the box's own code made) is left out, since no harness
// keeps its session in one.
//
// # One piece at a time, and never more than the archive's bound
//
// A line, a string or a settled stretch of text in memory, never a file; and
// a tar is read TWICE — a tar names each file's size before its bytes, and a
// file's size after the scrub is known only once it has been read — so every
// rewrite here is a pure function of what it reads, and the second reading
// writes what the first counted. The archive is the box's own: its code can
// write a gzip that expands a thousandfold, a line with no end, a string a
// gigabyte long or a value nested a million deep. So the scrub reads at most
// [scrubExpansion] times the archive's bound of plaintext in one reading,
// holds no piece longer than [scrubPieceMaxBytes] and rewrites no value
// nested deeper than [scrubDepthMax] as JSON — a session past the first two
// is refused ([errScrubBound]), stamped so, rather than costing the engine
// host its memory.

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

// scrubPieceMaxBytes is the most the scrub holds whole: one line of a
// transcript, one JSON string or literal, one stretch of text held for its
// redaction to settle. Past it the session is refused.
//
// EIGHT MEBIBYTES. A transcript's longest ordinary line is a tool's output it
// echoes — tens of kibibytes — and its longest legitimate one an image the
// model read, a screenshot's few mebibytes encoded; eight is room for that
// twice over. A piece is rewritten in a handful of copies at once (the line, the
// string decoded from it, its scrubbed and redacted forms, what is written), so
// the bound is what one capture can cost a host's memory: a few tens of
// mebibytes, whatever the box wrote.
const scrubPieceMaxBytes = 8 << 20

// scrubSettleStride is the least a stretch held for its redaction grows by
// before the redaction is asked again what of it has settled — and the stretch
// grows by half of itself once that is more ([rewriteText]): asked at every
// line, a stretch of a million one-byte lines that never settles would be read
// a million times over, and asked at a fixed stride, a stretch at the piece's
// bound would be read some hundred times. Grown by half, a stretch is read a
// small multiple of its length in all. The redaction is asked once more before
// a stretch past the piece is refused, so how often it is asked never decides
// whether a session is kept; and every reading of an archive asks at the same
// lengths, so the two readings of a tar write the same bytes.
const scrubSettleStride = 64 << 10

// scrubDepthMax is how deep the containers of one JSON value the scrub
// rewrites may nest: a decoder keeps a frame for every container open, and so
// does the rewrite, so a value a box wrote with a million brackets would cost
// a host a million frames. A value nested deeper is not rewritten as JSON — a
// transcript's line or a `.json` file is redacted as the text it is, and an
// export is refused.
//
// TEN THOUSAND, encoding/json's own bound on a value it decodes whole. A
// harness's transcript line nests some tens deep, and a value at the bound costs
// the rewrite well under a mebibyte of frames.
const scrubDepthMax = 10000

// errScrubBound is a session past what its scrub reads or holds: refused.
var errScrubBound = errors.New("executor: the session is past the bounds a kept session is scrubbed within")

// errNotOneValue is JSON holding more than one value where the rewrite reads
// one: a transcript's line, a `.json` file, an export.
var errNotOneValue = errors.New("executor: more than one JSON value where one was expected")

// errTooDeep is a JSON value nested past [scrubDepthMax].
var errTooDeep = fmt.Errorf("executor: a JSON value nested deeper than %d", scrubDepthMax)

// scrubBounds is how much of an archive one scrub reads and holds.
type scrubBounds struct {
	// plain is the most plaintext one reading of the archive yields.
	plain int64
	// piece is the most it holds whole.
	piece int
}

// boundsFor is the scrub of an archive of at most archiveMax bytes.
func boundsFor(archiveMax int64) scrubBounds {
	return scrubBounds{plain: scrubExpansion * archiveMax, piece: scrubPieceMaxBytes}
}

// scrubber takes the engine's own values out of text.
type scrubber struct{ r *strings.Replacer }

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
	return scrubber{r: strings.NewReplacer(pairs...)}
}

// apply is s with every value taken out.
func (s scrubber) apply(text string) string {
	if s.r == nil {
		return text
	}
	return s.r.Replace(text)
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
// Connect ID token ([namesIDToken]), and the identity token and registry token
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
func credentialKey(key string) bool {
	if redact.NamesPassword(key) {
		return true
	}
	k := strings.ToLower(key)
	if k == "token" || k == "key" || k == "auth" {
		return true
	}
	joined := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, k)
	for _, name := range []string{"apikey", "apitoken", "accesstoken", "refreshtoken", "sessiontoken",
		"securitytoken", "authtoken", "bearertoken", "privatekey", "privatetoken", "authorization", "secret",
		"credential", "identitytoken", "registrytoken"} {
		if strings.Contains(joined, name) {
			return true
		}
	}
	return namesIDToken(key)
}

// namesIDToken reports whether key names an OpenID Connect ID token: `id` a
// word of its own and `token` after it, however a format joins the two
// (`id_token`, `idToken`, `OIDC_ID_TOKEN`).
//
// A WORD OF ITS OWN, where every other name is read anywhere in the key,
// because `idtoken` is what a count spells by chance — `paid_tokens`,
// `invalid_tokens` — and a number under such a key is no credential's.
func namesIDToken(key string) bool {
	for i := 0; i+len("id") <= len(key); i++ {
		if !strings.EqualFold(key[i:i+len("id")], "id") || !wordStarts(key, i) {
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

// wordStarts reports whether one of key's words begins at byte i: at the key's
// start, after a character that is neither a letter nor a digit, or at an
// uppercase letter after a lowercase one or a digit (`oidcIdToken`).
func wordStarts(key string, i int) bool {
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

// scrubArchive streams the archive first holds, of layout, scrubbed onto dst,
// within bounds. reopen opens the same archive again from its start, for a
// layout read twice. It closes nothing it was handed; the caller owns both
// readers.
func scrubArchive(layout archiveLayout, first io.Reader, reopen func() (io.ReadCloser, error),
	dst io.Writer, s scrubber, b scrubBounds) error {

	switch layout {
	case archiveTar:
		return scrubTar(first, reopen, dst, s, b)
	case archiveDocument:
		return scrubDocument(first, dst, s, b)
	}
	return fmt.Errorf("executor: no scrub for an archive of layout %d", layout)
}

// scrubDocument streams a gzipped JSON document, rewritten string by string.
func scrubDocument(src io.Reader, dst io.Writer, s scrubber, b scrubBounds) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("executor: the session's export is not gzip: %w", err)
	}
	zw := gzip.NewWriter(dst)
	if err = rewriteDocument(zw, boundedPlain(zr, b.plain), s, b); err != nil {
		return fmt.Errorf("executor: scrubbing the session's export: %w", err)
	}
	return zw.Close()
}

// memberPlan is how the first reading of a tar decided to write one member:
// its kind as the archive gave it, which the second reading must find again;
// whether it is kept; and, for a file, its size as rewritten and whether it is
// kept as text.
type memberPlan struct {
	kind byte
	keep bool
	size int64
	text bool
}

// scrubTar streams a gzipped tar with every regular file rewritten
// ([rewriteFile]): read once to learn each file's size as rewritten — and, for
// a `.json` file, whether it is JSON — and again to write it under a header
// that says so.
//
// THE SECOND READING MUST BE THE FIRST'S ARCHIVE: a file whose rewrite comes
// out another size, or a member the first reading did not have, is an archive
// that changed between the two, and the scrub fails rather than write a tar
// whose headers lie. The failure names the member SHORT ([memberNamed]).
func scrubTar(first io.Reader, reopen func() (io.ReadCloser, error), dst io.Writer, s scrubber,
	b scrubBounds) error {

	var plans []memberPlan
	err := eachMember(first, b, func(h *tar.Header, body io.Reader) error {
		switch h.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir:
			plans = append(plans, memberPlan{kind: h.Typeflag, keep: true})
			return nil
		default:
			plans = append(plans, memberPlan{kind: h.Typeflag})
			return nil
		}
		size, text, err := measureFile(h.Name, body, s, b)
		if err != nil {
			return err
		}
		plans = append(plans, memberPlan{kind: h.Typeflag, keep: true, size: size, text: text})
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
	i := 0
	err = eachMember(again, b, func(h *tar.Header, body io.Reader) error {
		if i >= len(plans) {
			return errors.New("executor: the session's archive gained a member while it was read")
		}
		plan := plans[i]
		i++
		if h.Typeflag != plan.kind {
			return fmt.Errorf("executor: the session's archive changed its member %s while it was read",
				memberNamed(h.Name))
		}
		if !plan.keep {
			return nil
		}
		file := h.Typeflag == tar.TypeReg
		header := tar.Header{Typeflag: h.Typeflag, Name: memberName(s, h.Name), Mode: h.Mode & 0o777,
			ModTime: h.ModTime}
		if file {
			header.Size = plan.size
		}
		if headerErr := tw.WriteHeader(&header); headerErr != nil {
			return headerErr
		}
		if !file {
			return nil
		}
		w := &counted{w: tw}
		if memberErr := rewriteMember(h.Name, plan.text, w, body, s, b); memberErr != nil {
			return memberErr
		}
		if w.n != plan.size {
			return fmt.Errorf("executor: the session's archive changed its member %s while it was read",
				memberNamed(h.Name))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if i != len(plans) {
		return errors.New("executor: the session's archive lost a member while it was read")
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return zw.Close()
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

// measureFile is how many bytes one file of an archive comes to as it is
// kept, and — for a `.json` file — whether it is kept as text because it is
// not JSON.
//
// A `.json` FILE IS READ BOTH WAYS AT ONCE, its bytes handed to a rewrite as a
// document and to one as text: which it takes is known only once it has been
// read through, and a tar member is read once in each reading.
func measureFile(name string, body io.Reader, s scrubber, b scrubBounds) (int64, bool, error) {
	if path.Ext(name) != ".json" {
		n := &counted{}
		err := rewriteFile(name, n, body, s, b)
		return n.n, false, err
	}
	asDocument := func(r io.Reader) (int64, error) {
		n := &counted{}
		err := rewriteDocument(n, r, s, b)
		return n.n, err
	}
	asText := func(r io.Reader) (int64, error) {
		n := &counted{}
		err := rewriteText(n, r, s, b)
		return n.n, err
	}
	// A DOCUMENT THE REWRITE CANNOT READ, FOR WHATEVER REASON, IS TEXT: torn,
	// more than one value, nested past the rewrite's depth, or holding a
	// string past its piece — which the text's own bounds refuse as surely,
	// a JSON string holding no line break of its own.
	document, documentErr, text, textErr := both(body, asDocument, asText)
	switch {
	case documentErr == nil:
		return document, false, nil
	case textErr != nil:
		return 0, false, textErr
	}
	return text, true, nil
}

// rewriteMember writes one file of an archive as it is kept: as text where the
// first reading found a `.json` file that is not JSON, and as what its name
// says it is otherwise ([rewriteFile]).
func rewriteMember(name string, text bool, dst io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
	if text {
		return rewriteText(dst, src, s, b)
	}
	return rewriteFile(name, dst, src, s, b)
}

// rewriteFile writes one file of an archive scrubbed, read as what its name
// says it is (see the file comment).
func rewriteFile(name string, dst io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
	switch path.Ext(name) {
	case ".jsonl":
		return rewriteLines(dst, src, s, b)
	case ".json":
		return rewriteDocument(dst, src, s, b)
	}
	return rewriteText(dst, src, s, b)
}

// both reads src once into two rewrites at once, each counting what it would
// write, and answers both counts and both errors. A rewrite that stops early —
// it failed — is fed no further, and the other is fed to the end.
func both(src io.Reader, first, second func(io.Reader) (int64, error)) (int64, error, int64, error) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	var (
		wg         sync.WaitGroup
		n1, n2     int64
		err1, err2 error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		n1, err1 = first(r1)
		_ = r1.CloseWithError(errRewriteDone)
	}()
	go func() {
		defer wg.Done()
		n2, err2 = second(r2)
		_ = r2.CloseWithError(errRewriteDone)
	}()
	_, copyErr := io.Copy(&fanOut{to: []*io.PipeWriter{w1, w2}, open: []bool{true, true}}, src)
	_ = w1.CloseWithError(copyErr)
	_ = w2.CloseWithError(copyErr)
	wg.Wait()
	if copyErr != nil && !errors.Is(copyErr, errRewriteDone) {
		return 0, copyErr, 0, copyErr
	}
	return n1, err1, n2, err2
}

// errRewriteDone is a rewrite that stopped reading: what [both] closes its pipe
// with, so writes to it fail and it is fed no further.
var errRewriteDone = errors.New("executor: the rewrite stopped reading")

// fanOut writes to every pipe still read, and fails only once none is.
type fanOut struct {
	to   []*io.PipeWriter
	open []bool
}

func (f *fanOut) Write(p []byte) (int, error) {
	fed := false
	for i, w := range f.to {
		if !f.open[i] {
			continue
		}
		if _, err := w.Write(p); err != nil {
			f.open[i] = false
			continue
		}
		fed = true
	}
	if !fed {
		return 0, errRewriteDone
	}
	return len(p), nil
}

// readLine is the next line of r, its break included, refused past bound: a
// line is held whole to be scrubbed.
func readLine(r *bufio.Reader, bound int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > bound {
			return nil, fmt.Errorf("%w: a line of the session runs past %d bytes", errScrubBound, bound)
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// rewriteLines rewrites a file of one JSON value a line ([rewriteLine]).
func rewriteLines(dst io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
	r := bufio.NewReaderSize(src, 64<<10)
	w := bufio.NewWriter(dst)
	for {
		line, err := readLine(r, b.piece)
		if len(line) > 0 {
			body, ended := strings.CutSuffix(string(line), "\n")
			if _, werr := w.WriteString(rewriteLine(body, s, b)); werr != nil {
				return werr
			}
			if ended {
				if werr := w.WriteByte('\n'); werr != nil {
					return werr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return w.Flush()
		}
		if err != nil {
			return err
		}
	}
}

// rewriteLine is one line of a transcript as it is kept: its JSON value
// rewritten string by string; or, a line that is not JSON, scrubbed and
// redacted as text. The line is within the scrub's piece already ([readLine]),
// and so is every string in it.
//
// NOT REDACTED AGAIN AS A WHOLE LINE once rewritten. Every pair a rule reads a
// secret by sits in one JSON value — a string's text, or a key and the value
// under it, which the rewrite takes out by the key ([credentialKey]) — and a
// rule that read across two of them replaced JSON syntax with its marker,
// leaving no line JSON: a reading that could only keep what the rewrite had
// already taken.
func rewriteLine(line string, s scrubber, b scrubBounds) string {
	var out bytes.Buffer
	if err := rewriteDocument(&out, strings.NewReader(line), s, b); err != nil {
		return redact.Secrets(s.apply(line))
	}
	return withoutBrokenDefinitions(out.String())
}

// loadedToolsRecord is the `type` of the attachment Claude Code keeps in its
// transcript for the tool definitions a session loaded through tool search.
// A session resumed from the transcript sends each one again as a tool of
// every request it makes (measured on 2.1.322).
const loadedToolsRecord = "deferred_tools_record"

// providerToolName reports whether name is one the provider takes for a tool
// (`^[a-zA-Z0-9_-]{1,128}$`); a request carrying a definition named outside
// that is refused whole. Spelled out, not a compiled pattern: a package-level
// one is allocated at every start of the binary, the git hooks' included.
func providerToolName(name string) bool {
	if len(name) < 1 || len(name) > 128 {
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

// withoutBrokenDefinitions is a rewritten transcript line less every loaded
// tool definition ([loadedToolsRecord]) whose name the rewrite left outside
// what [providerToolName] takes: a marker stands where part of the name was,
// and resent under that name the definition has every request of the restored
// session refused. Left out, the session loads the tool again by search when
// it needs it. A line that holds no marker is returned as it is, since the
// rewrite renamed nothing in it, a definition its writer named outside the
// pattern included; one that holds a marker loses every definition named
// outside the pattern, whoever named it so. A line that is not one JSON object
// [threadDecoding] reads is returned as it is too. The record is read as
// Claude Code's JSON.parse reads it, its attachment, the attachment's type and
// its list each the last value written under its key.
//
// CUT OUT OF THE LINE AS WRITTEN, each definition with the comma that joined
// it to its list, every other byte of the line left where it was: never the
// record decoded and written again, which wrote its keys sorted, the
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
func withoutBrokenDefinitions(line string) string {
	if !strings.Contains(line, loadedToolsRecord) || !strings.Contains(line, redact.Marker) {
		return line
	}
	raw := []byte(line)
	attachment, ok := lastValue(raw, lineSpan{end: len(raw)}, "attachment")
	if !ok {
		return line
	}
	kind, ok := lastValue(raw, attachment, "type")
	if !ok || looseString(raw[kind.start:kind.end]) != loadedToolsRecord {
		return line
	}
	entries, ok := lastValue(raw, attachment, "entries")
	if !ok {
		return line
	}
	if kept, cut := withoutDefinitions(raw, entries); cut {
		return kept
	}
	return line
}

// lineSpan is where one JSON value sits in a transcript line:
// line[start:end], its own bytes and no blank around them.
type lineSpan struct{ start, end int }

// lastValue is where in line the value under the LAST key named key of the
// object at object sits, as JSON.parse reads a key written twice, and whether
// the object holds such a key: it holds none where it is no object
// [threadDecoding] reads, or is followed by more than blanks. Read in the one
// walk of an object ([eachKey]), which hands each value with where it sits.
func lastValue(line []byte, object lineSpan, key string) (lineSpan, bool) {
	var (
		found lineSpan
		ok    bool
	)
	if eachKey(line[object.start:object.end], func(name []byte, value json.RawMessage, at int) error {
		if string(name) == key {
			start := object.start + at
			found, ok = lineSpan{start: start, end: start + len(value)}, true
		}
		return nil
	}) != nil {
		return lineSpan{}, false
	}
	return found, ok
}

// withoutDefinitions is line less every definition of the list at list that
// [brokenDefinition] refuses, each cut out with the comma that joined it to
// the list (the one before it, or the one after it where every definition
// before it goes as well), every other byte kept where it was, and whether
// any went: none goes where none is refused, or where the list is none
// [threadDecoding] reads.
//
// ONE DEFINITION AT A TIME, in the one walk of a list ([eachElement], which
// says what a list a box can write a million elements long costs read whole):
// the line is copied once the first definition is cut, and nothing is
// collected of the list.
func withoutDefinitions(line []byte, list lineSpan) (string, bool) {
	var (
		// cut is the line as it is kept, written from the first definition
		// cut on: every byte before it is kept.
		cut     strings.Builder
		cutting bool
		// kept says a definition before this one is kept, and end is where
		// in line the one just before this one ends.
		kept bool
		end  int
	)
	err := eachElement(line[list.start:list.end], func(definition json.RawMessage, at int) error {
		start := list.start + at
		next := start + len(definition)
		// ITS STRETCH OF THE LIST, kept or cut with it: from the end of the
		// definition before it, the comma between them included, where one
		// before it is kept; else from its own first byte, so that the comma
		// after a definition cut before it goes with that one.
		from := start
		if kept {
			from = end
		}
		if brokenDefinition(definition) {
			if !cutting {
				cutting = true
				cut.Grow(len(line))
				cut.Write(line[:from])
			}
		} else {
			if cutting {
				cut.Write(line[from:next])
			}
			kept = true
		}
		end = next
		return nil
	})
	if err != nil || !cutting {
		return "", false
	}
	cut.Write(line[end:])
	return cut.String(), true
}

// brokenDefinition reports whether a loaded tool definition is one no request
// may carry: not an object, or named by nothing [providerToolName] takes. The
// name is the one Claude Code sends the definition under, read as Claude
// Code's JSON.parse reads it: the value of the key `name` exactly, the last
// written where it is written twice, whatever a `name` before it holds; a
// value that is no string names nothing. Read in the one walk of an object
// ([eachKey]).
//
// NOT A DECODE OF THE DEFINITION into a name. encoding/json's fills a field
// from a key that differs from its name only in case, the last such key
// winning: a definition holding a renamed `name` and then a sound `NAME` was
// read as sound and kept, and sent under the renamed name (2.1.310 and 2.1.322
// key every definition they send again by its `name`), which had every
// request of the restored session refused. And a decode into a string fails
// at the first `name` that is none, whichever `name` JSON.parse reads: a
// definition holding a number and then a sound `name` was cut, though Claude
// Code sends it under the sound one.
func brokenDefinition(definition []byte) bool {
	// name is the last `name`'s value, a copy of its own: what the walk hands
	// over is valid only while it does.
	var name []byte
	if eachKey(definition, func(key []byte, value json.RawMessage, _ int) error {
		if string(key) == "name" {
			name = append(name[:0], value...)
		}
		return nil
	}) != nil {
		return true
	}
	return !providerToolName(looseString(name))
}

// rewriteDocument streams one JSON value from src to dst a token at a time,
// every string value and every key scrubbed and redacted, and a value under a
// credential's key taken out whole. A second value is refused
// ([errNotOneValue]); so is a string or a literal past the scrub's piece, and
// a value nested past [scrubDepthMax], before the decoder holds either
// ([tokenBound]).
func rewriteDocument(dst io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
	dec := json.NewDecoder(&tokenBound{r: src, bound: b.piece})
	dec.UseNumber()
	j := &jsonRewrite{out: bufio.NewWriter(dst), scrub: s}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err = j.token(tok); err != nil {
			return err
		}
	}
	if len(j.stack) > 0 {
		return io.ErrUnexpectedEOF
	}
	return j.out.Flush()
}

// tokenBound passes a JSON text through to its decoder, refusing one the
// decoder would hold more than bound bytes of at once, or nest deeper than
// [scrubDepthMax]: the decoder holds a string or a bare literal whole before it
// hands it over — and the blanks before the next token with it, which it skips
// only once it sees that token — and keeps a frame for every container open,
// and a document a box's own code wrote can hold a token of any length, or open
// a container at every byte.
type tokenBound struct {
	r        io.Reader
	bound    int
	inString bool
	escaped  bool
	// run is how much the decoder holds whole so far: the string being read,
	// or the literal and the blanks around it.
	run int
	// depth is how many containers are open.
	depth int
}

func (t *tokenBound) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	for _, c := range p[:n] {
		switch {
		case t.inString:
			t.run++
			switch {
			case t.escaped:
				t.escaped = false
			case c == '\\':
				t.escaped = true
			case c == '"':
				t.inString, t.run = false, 0
			}
		case c == '"':
			t.inString, t.run = true, 1
		case c == '[' || c == '{':
			t.depth++
			t.run = 0
			if t.depth > scrubDepthMax {
				return 0, errTooDeep
			}
		case c == ']' || c == '}':
			t.depth--
			t.run = 0
		case c == ',' || c == ':':
			t.run = 0
		default:
			t.run++
		}
		if t.run > t.bound {
			return 0, fmt.Errorf("%w: a JSON value of the session runs past %d bytes", errScrubBound, t.bound)
		}
	}
	return n, err
}

// jsonRewrite writes the tokens a decoder reads back as JSON.
type jsonRewrite struct {
	out   *bufio.Writer
	scrub scrubber
	stack []jsonFrame
	// done says the one value outside any container has been written.
	done bool
}

// jsonFrame is one open object or array.
type jsonFrame struct {
	object bool
	// n is how many members (an object's pairs, an array's values) are
	// written; key is the object's key whose value comes next, and wantKey
	// says a key comes next.
	n       int
	key     string
	wantKey bool
}

func (j *jsonRewrite) frame() *jsonFrame {
	if len(j.stack) == 0 {
		return nil
	}
	return &j.stack[len(j.stack)-1]
}

// beginValue writes what comes before a value — a comma between an array's
// values; an object's value follows its key's colon — refusing a second value
// outside any container.
func (j *jsonRewrite) beginValue() error {
	f := j.frame()
	switch {
	case f == nil && j.done:
		return errNotOneValue
	case f != nil && !f.object && f.n > 0:
		_ = j.out.WriteByte(',')
	}
	return nil
}

// endValue counts a value written.
func (j *jsonRewrite) endValue() {
	f := j.frame()
	if f == nil {
		j.done = true
		return
	}
	f.n++
	if f.object {
		f.wantKey = true
	}
}

func (j *jsonRewrite) token(tok json.Token) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{', '[':
			if err := j.beginValue(); err != nil {
				return err
			}
			_ = j.out.WriteByte(byte(t))
			j.stack = append(j.stack, jsonFrame{object: t == '{', wantKey: t == '{'})
		default:
			j.stack = j.stack[:len(j.stack)-1]
			_ = j.out.WriteByte(byte(t))
			j.endValue()
		}
	case string:
		if f := j.frame(); f != nil && f.object && f.wantKey {
			if f.n > 0 {
				_ = j.out.WriteByte(',')
			}
			// THE KEY AS IT IS KEPT: a key is a string a tool's structured
			// result can name anything by, a secret included. Judged as a
			// credential's by what it said, and written as what is kept.
			if err := j.writeString(redact.Secrets(j.scrub.apply(t))); err != nil {
				return err
			}
			_ = j.out.WriteByte(':')
			f.key, f.wantKey = t, false
			return nil
		}
		if err := j.beginValue(); err != nil {
			return err
		}
		if err := j.writeString(j.value(t)); err != nil {
			return err
		}
		j.endValue()
	case json.Number:
		if err := j.beginValue(); err != nil {
			return err
		}
		if j.credential() {
			// A NUMBER UNDER A CREDENTIAL'S KEY goes as a string under one
			// does — a PIN, a numeric password, an account's key — written
			// as the marker, a string where the number stood: no shape
			// places a bare number, and kept as it came it survived every
			// pass.
			if err := j.writeString(credentialMarker); err != nil {
				return err
			}
		} else {
			_, _ = j.out.WriteString(t.String())
		}
		j.endValue()
	case bool:
		if err := j.beginValue(); err != nil {
			return err
		}
		_, _ = j.out.WriteString(strconv.FormatBool(t))
		j.endValue()
	case nil:
		if err := j.beginValue(); err != nil {
			return err
		}
		_, _ = j.out.WriteString("null")
		j.endValue()
	default:
		return fmt.Errorf("executor: a JSON token of type %T", tok)
	}
	return nil
}

// value is a string value as it is kept.
func (j *jsonRewrite) value(s string) string {
	if s != "" && j.credential() {
		return credentialMarker
	}
	return keptString(j.scrub, s)
}

// keptString is a string value under a key that is no credential's, as a kept
// document holds it: every value the take names taken out, then every shape of
// a secret redacted. The ONE rule for it, so what the take keeps beside an
// archive of a document — where its run began in it ([keptRunStart]) — is
// spelled as the document spells the same text.
func keptString(scrub scrubber, s string) string {
	return redact.Secrets(scrub.apply(s))
}

// credential reports whether the value being written is a member of an object
// under a credential's own key ([credentialKey]).
func (j *jsonRewrite) credential() bool {
	f := j.frame()
	return f != nil && f.object && credentialKey(f.key)
}

// writeString writes s as a JSON string, escaped as plainjson escapes: as
// JSON.stringify does, but for U+2028 and U+2029, which it writes as escapes
// where JSON.stringify writes them as they are. JSON.parse reads either back
// as the same text.
func (j *jsonRewrite) writeString(s string) error {
	out, err := plainjson.Marshal(s, "")
	if err != nil {
		return err
	}
	_, err = j.out.Write(out)
	return err
}

// rewriteText streams text, every whole line scrubbed, and redacted as redact
// settles it ([redact.Settled]): a line a rule may yet read with the lines after
// it is held until they come, and the rest once the text ends. Held, never
// past the scrub's piece: a stretch still unsettled past it is refused.
func rewriteText(dst io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
	r := bufio.NewReaderSize(src, 64<<10)
	w := bufio.NewWriter(dst)
	var held []byte
	// asked is how long the stretch was when the redaction was last asked.
	asked := 0
	for {
		line, err := readLine(r, b.piece)
		if len(line) > 0 {
			held = append(held, s.apply(string(line))...)
		}
		if errors.Is(err, io.EOF) {
			if _, werr := w.WriteString(redact.Secrets(string(held))); werr != nil {
				return werr
			}
			return w.Flush()
		}
		if err != nil {
			return err
		}
		if len(held) <= b.piece && len(held)-asked < max(scrubSettleStride, asked/2) {
			continue
		}
		pending := string(held)
		if n := redact.Settled(pending); n > 0 {
			if _, werr := w.WriteString(redact.Secrets(pending[:n])); werr != nil {
				return werr
			}
			held = append(held[:0], held[n:]...)
		}
		if len(held) > b.piece {
			return fmt.Errorf("%w: a stretch of the session's text a redaction holds runs past %d bytes",
				errScrubBound, b.piece)
		}
		asked = len(held)
	}
}
