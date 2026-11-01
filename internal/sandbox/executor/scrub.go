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
//     decodes to, which is where a key's lines are lines again, and a string
//     under a credential's own key taken out whole — and then the rewritten
//     line redacted as text, which is where a password's key and its value,
//     two strings apart, meet; that second reading is kept only where it
//     leaves the line JSON. A line that is not JSON — the last one of a job
//     packed while it wrote — is redacted as text.
//   - A DOCUMENT (`.json`, and OpenCode's whole export): rewritten string by
//     string as it streams, the same way, a token at a time.
//   - ANYTHING ELSE, as text: each whole line scrubbed, and redacted as
//     redact settles it (redact.Settled, as a live tail is), so a rule that
//     reads across lines sees them all.
//
// ONE PIECE AT A TIME, as everything a session crosses is: a line, a string or
// a settled stretch of text in memory, never a file. Which is why a tar is read
// TWICE — a tar names each file's size before its bytes, and a file's size
// after the scrub is known only once it has been read — and why every rewrite
// here is a pure function of what it reads, so the second reading writes what
// the first counted.

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
// out would corrupt the very transcript the next turn continues; nothing the
// engine hands a box as a credential is that short, and redact's rules stay the
// backstop for one that is.
const minScrubBytes = 8

// scrubber takes the engine's own values out of text.
type scrubber struct{ r *strings.Replacer }

// newScrubber is the scrubber for values: each line of each value at least
// [minScrubBytes] long — a multi-line value (a key file a setup step wrote) is
// taken out a line at a time, which is how a transcript holds it — in its raw
// form and as a JSON string escapes it, the longest first, so a value holding
// another is taken out whole.
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
			line = strings.TrimSpace(line)
			add(line)
			add(jsonEscaped(line))
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

// jsonEscaped is how a JSON string a harness writes holds s, without its
// quotes: escaped as JSON.stringify escapes, which leaves `<`, `>` and `&` as
// they are (plainjson).
func jsonEscaped(s string) string {
	out, err := plainjson.Marshal(s, "")
	if err != nil {
		return s
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(out), `"`), `"`)
}

// credentialKey reports whether a JSON object's key names a credential, whose
// string value is taken out whole however it is shaped: a login's tokens, a
// tool call's password or API key. Read on the key's own words, lowercase:
// `token` and `key` exactly, or one of the words a credential's name carries.
func credentialKey(key string) bool {
	k := strings.ToLower(key)
	if k == "token" || k == "key" {
		return true
	}
	for _, word := range []string{"password", "passwd", "secret", "apikey", "api_key", "accesstoken",
		"access_token", "refreshtoken", "refresh_token", "authorization", "private_key", "privatekey",
		"credential"} {
		if strings.Contains(k, word) {
			return true
		}
	}
	return false
}

// LoginValues is every string a login document holds — a harness's credentials
// file, the document its login export printed — long enough to be taken out by
// value ([minScrubBytes]): its tokens and its keys, for the scrub a session a
// box ran under that login is kept through. Every string, not only those under
// a credential's key, since a login names its tokens however its harness
// spells them; what else such a document holds is a scope or a plan's name,
// whose occurrences a transcript loses nothing by. A document that is not JSON
// holds none.
func LoginValues(doc []byte) []string {
	var tree any
	if err := json.Unmarshal(doc, &tree); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if len(t) >= minScrubBytes {
				out = append(out, t)
			}
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(tree)
	return out
}

// scrubArchive streams the archive first holds, of layout, scrubbed onto dst.
// reopen opens the same archive again from its start, for a layout read
// twice. It closes nothing it was handed; the caller owns both readers.
func scrubArchive(layout archiveLayout, first io.Reader, reopen func() (io.ReadCloser, error),
	dst io.Writer, s scrubber) error {

	switch layout {
	case archiveTar:
		return scrubTar(first, reopen, dst, s)
	case archiveDocument:
		return scrubDocument(first, dst, s)
	}
	return fmt.Errorf("executor: no scrub for an archive of layout %d", layout)
}

// scrubDocument streams a gzipped JSON document, rewritten string by string.
func scrubDocument(src io.Reader, dst io.Writer, s scrubber) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("executor: the session's export is not gzip: %w", err)
	}
	zw := gzip.NewWriter(dst)
	if err = rewriteDocument(zw, zr, s); err != nil {
		return fmt.Errorf("executor: scrubbing the session's export: %w", err)
	}
	return zw.Close()
}

// scrubTar streams a gzipped tar with every regular file rewritten
// ([rewriteFile]): read once to learn each file's size as rewritten, and again
// to write it under a header that says so.
//
// THE SECOND READING MUST BE THE FIRST'S ARCHIVE: a file whose rewrite comes
// out another size, or a member the first reading did not have, is an archive
// that changed between the two, and the scrub fails rather than write a tar
// whose headers lie.
func scrubTar(first io.Reader, reopen func() (io.ReadCloser, error), dst io.Writer, s scrubber) error {
	var sizes []int64
	err := eachMember(first, func(h *tar.Header, body io.Reader) error {
		if h.Typeflag != tar.TypeReg {
			sizes = append(sizes, -1)
			return nil
		}
		n := &counted{}
		if err := rewriteFile(h.Name, n, body, s); err != nil {
			return err
		}
		sizes = append(sizes, n.n)
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
	err = eachMember(again, func(h *tar.Header, body io.Reader) error {
		if i >= len(sizes) {
			return errors.New("executor: the session's archive gained a member while it was read")
		}
		header := *h
		size := sizes[i]
		i++
		if (header.Typeflag == tar.TypeReg) != (size >= 0) {
			return fmt.Errorf("executor: the session's archive changed its member %s while it was read", h.Name)
		}
		if size >= 0 {
			header.Size = size
		}
		if err := tw.WriteHeader(&header); err != nil {
			return err
		}
		if size < 0 {
			return nil
		}
		w := &counted{w: tw}
		if err := rewriteFile(h.Name, w, body, s); err != nil {
			return err
		}
		if w.n != size {
			return fmt.Errorf("executor: the session's archive changed its member %s while it was read", h.Name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if i != len(sizes) {
		return errors.New("executor: the session's archive lost a member while it was read")
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// eachMember reads a gzipped tar member by member.
func eachMember(src io.Reader, member func(*tar.Header, io.Reader) error) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("executor: the session's archive is not gzip: %w", err)
	}
	tr := tar.NewReader(zr)
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

// rewriteFile writes one file of an archive scrubbed, read as what its name
// says it is (see the file comment).
func rewriteFile(name string, dst io.Writer, src io.Reader, s scrubber) error {
	switch path.Ext(name) {
	case ".jsonl":
		return rewriteLines(dst, src, s)
	case ".json":
		return rewriteDocument(dst, src, s)
	}
	return rewriteText(dst, src, s)
}

// rewriteLines rewrites a file of one JSON value a line ([rewriteLine]).
func rewriteLines(dst io.Writer, src io.Reader, s scrubber) error {
	r := bufio.NewReader(src)
	w := bufio.NewWriter(dst)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			body, ended := strings.CutSuffix(line, "\n")
			if _, werr := w.WriteString(rewriteLine(body, s)); werr != nil {
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
// rewritten string by string, then redacted as a line where that leaves it
// JSON; or, a line that is not JSON, scrubbed and redacted as text.
func rewriteLine(line string, s scrubber) string {
	var b bytes.Buffer
	if err := rewriteDocument(&b, strings.NewReader(line), s); err != nil {
		return redact.Secrets(s.apply(line))
	}
	rewritten := b.String()
	if whole := redact.Secrets(rewritten); whole != rewritten && json.Valid([]byte(whole)) {
		return whole
	}
	return rewritten
}

// rewriteDocument streams JSON from src to dst a token at a time, every string
// value scrubbed and redacted, and one under a credential's key taken out
// whole. Several values one after another (a transcript's lines) are written a
// line each.
func rewriteDocument(dst io.Writer, src io.Reader, s scrubber) error {
	dec := json.NewDecoder(src)
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

// jsonRewrite writes the tokens a decoder reads back as JSON.
type jsonRewrite struct {
	out   *bufio.Writer
	scrub scrubber
	stack []jsonFrame
	// top is how many values have been written outside any container.
	top int
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

// beginValue writes what comes before a value: a comma between an array's
// values, a line between top-level ones. An object's value follows its key's
// colon.
func (j *jsonRewrite) beginValue() {
	f := j.frame()
	switch {
	case f == nil && j.top > 0:
		_ = j.out.WriteByte('\n')
	case f != nil && !f.object && f.n > 0:
		_ = j.out.WriteByte(',')
	}
}

// endValue counts a value written.
func (j *jsonRewrite) endValue() {
	f := j.frame()
	if f == nil {
		j.top++
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
			j.beginValue()
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
			if err := j.writeString(t); err != nil {
				return err
			}
			_ = j.out.WriteByte(':')
			f.key, f.wantKey = t, false
			return nil
		}
		j.beginValue()
		if err := j.writeString(j.value(t)); err != nil {
			return err
		}
		j.endValue()
	case json.Number:
		j.beginValue()
		_, _ = j.out.WriteString(t.String())
		j.endValue()
	case bool:
		j.beginValue()
		_, _ = j.out.WriteString(strconv.FormatBool(t))
		j.endValue()
	case nil:
		j.beginValue()
		_, _ = j.out.WriteString("null")
		j.endValue()
	default:
		return fmt.Errorf("executor: a JSON token of type %T", tok)
	}
	return nil
}

// value is a string value as it is kept.
func (j *jsonRewrite) value(s string) string {
	if f := j.frame(); f != nil && f.object && s != "" && credentialKey(f.key) {
		return credentialMarker
	}
	return redact.Secrets(j.scrub.apply(s))
}

// writeString writes s as a JSON string, escaped as JSON.stringify escapes
// (plainjson).
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
// it is held until they come, and the rest once the text ends.
func rewriteText(dst io.Writer, src io.Reader, s scrubber) error {
	r := bufio.NewReader(src)
	w := bufio.NewWriter(dst)
	var pending string
	for {
		line, err := r.ReadString('\n')
		pending += s.apply(line)
		if errors.Is(err, io.EOF) {
			if _, werr := w.WriteString(redact.Secrets(pending)); werr != nil {
				return werr
			}
			return w.Flush()
		}
		if err != nil {
			return err
		}
		if n := redact.Settled(pending); n > 0 {
			if _, werr := w.WriteString(redact.Secrets(pending[:n])); werr != nil {
				return werr
			}
			pending = pending[n:]
		}
	}
}
