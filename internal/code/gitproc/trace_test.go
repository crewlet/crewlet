package gitproc_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// The trace2 event lines below are spelled as git writes them under
// GIT_TRACE2_EVENT_BRIEF (no file, no line; the version event keeps its
// time), each string escaped by json-writer.c's append_quoted_string, which
// jsonString transcribes.

// testSID is a leader's session id as git spells one; a child's is its
// parent's, a "/" and its own.
const testSID = "20261017T171512.837887Z-H0a7c9cdf-P00001c91"

// sidAt is the session id of a process depth levels down the tree (1: the
// leader).
func sidAt(depth int) string {
	sid := testSID
	for i := 1; i < depth; i++ {
		sid += fmt.Sprintf("/20261017T171512.9%05dZ-H0a7c9cdf-P%08x", i, 0x1c91+i)
	}
	return sid
}

// jsonString is s as git's append_quoted_string writes it: a quote and a
// backslash escaped, the five named controls by name, every other byte
// below 0x20 as \u00XX, and every other byte — those past 0x7f included —
// raw.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\f':
			b.WriteString(`\f`)
		case '\b':
			b.WriteString(`\b`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// errorLine is one error event a process depth levels down the tree writes.
func errorLine(depth int, format, msg string) string {
	return `{"event":"error","sid":"` + sidAt(depth) + `","thread":"main","msg":` + jsonString(msg) +
		`,"fmt":` + jsonString(format) + "}\n"
}

// versionLine is the version event a process writes first.
func versionLine(depth int, exe string) string {
	return `{"event":"version","sid":"` + sidAt(depth) +
		`","thread":"main","time":"2026-10-17T17:15:12.837948Z","evt":"4","exe":"` + exe + "\"}\n"
}

// startLine and exitLine are two of the events every process writes beside
// its reports.
func startLine(depth int, argv ...string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = jsonString(a)
	}
	return `{"event":"start","sid":"` + sidAt(depth) + `","thread":"main","t_abs":0.000310,"argv":[` +
		strings.Join(quoted, ",") + "]}\n"
}

func exitLine(depth, code int) string {
	return `{"event":"exit","sid":"` + sidAt(depth) + `","thread":"main","t_abs":0.002893,"code":` +
		fmt.Sprint(code) + "}\n"
}

// controlBytes is n control bytes, in turn every one git escapes six bytes
// to one (\u00XX): all below 0x20 but NUL, which ends a C string, and the
// five append_quoted_string names (\b \t \n \f \r), which it escapes in two.
// What a request can put in one pkt-line, and what makes the 390 KB event.
func controlBytes(n int) string {
	var cycle []byte
	for c := byte(1); c < 0x20; c++ {
		if !strings.ContainsRune("\b\t\n\f\r", rune(c)) {
			cycle = append(cycle, c)
		}
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = cycle[i%len(cycle)]
	}
	return string(b)
}

// requestMark begins every request payload: what a dying upload-pack copied
// into its report is proven by finding it in its stderr. At the START,
// because git formats a report into a buffer of 4 KiB (vreportf) and cuts
// the rest — the trace's msg is the whole of it, stderr its first 4 KiB.
const requestMark = "crewlet-request-payload"

// conversion is one printf conversion, as instantiate fills it.
var conversion = regexp.MustCompile(`%[-#0-9.*]*(?:hh|h|ll|l|j|z|t|L)?[a-zA-Z%]`)

// instantiate is format as git might print it: every conversion given a
// value of its type — an object id, a pack path or a word for a string, a
// number for an integer.
func instantiate(format string) string {
	return conversion.ReplaceAllStringFunc(format, func(c string) string {
		switch c[len(c)-1] {
		case '%':
			return "%"
		case 's':
			return "/c/objects/pack/pack-0123456789abcdef0123456789abcdef01234567.pack"
		case 'x', 'X':
			return "4f494446"
		default:
			return "7"
		}
	})
}

// requestPayloads is the three texts a request carries in each form that
// takes an argument: every row's format, joined by newlines; every row's
// message as git would print it, joined the same way; and 65 000 control
// bytes. Each begins with requestMark.
func requestPayloads() map[string]string {
	formats := slices.Sorted(func(yield func(string) bool) {
		for f := range gitproc.FormatTable() {
			if !yield(f) {
				return
			}
		}
	})
	messages := make([]string, len(formats))
	for i, f := range formats {
		messages[i] = instantiate(f)
	}
	return map[string]string{
		"formats":  requestMark + "\n" + strings.Join(formats, "\n"),
		"messages": requestMark + "\n" + strings.Join(messages, "\n"),
		"controls": requestMark + controlBytes(65000),
	}
}

// requestForm is one request upload-pack reads, against a healthy copy.
// body is the request with p in it where the form carries an argument.
type requestForm struct {
	name     string
	protocol string // GIT_PROTOCOL: "" for v0
	payload  bool   // the form carries p
	body     func(p string) string
	want     error // nil: served; ErrNotOurRef; ErrFailed
	// echoes is whether the report a dying form makes copies p: proved by
	// requestMark in its stderr.
	echoes bool
}

// v2 is a protocol v2 request: command, capabilities, then arguments.
func v2(command string, caps []string, args ...string) string {
	var b strings.Builder
	b.WriteString(pkt("command=" + command + "\n"))
	for _, c := range caps {
		b.WriteString(pkt(c + "\n"))
	}
	b.WriteString("0001")
	for _, a := range args {
		b.WriteString(pkt(a + "\n"))
	}
	b.WriteString("0000")
	return b.String()
}

// fetch is a v2 fetch of want with args, ended by done.
func fetch(args ...string) string { return v2("fetch", nil, append(args, "done")...) }

// v0 is a protocol v0 stateless request: wants (the first carrying caps),
// a flush, then haves and done.
func v0(first string, rest ...string) string {
	var b strings.Builder
	b.WriteString(pkt(first + "\n"))
	b.WriteString("0000")
	for _, r := range rest {
		b.WriteString(pkt(r + "\n"))
	}
	return b.String()
}

// requestForms is every request form a client can send upload-pack that
// carries text of its choosing, and the controls beside them: the planning
// pass's forms (§2.5 of the unit's specification), against the fixture's
// healthy copy. tip, c3, tree and blob are the fixture's.
func requestForms(tip, c3, tree, blob string) []requestForm {
	absent := strings.Repeat("0123456789", 4)
	wantTip := "want " + tip
	served := func(name, protocol, body string) requestForm {
		return requestForm{name: name, protocol: protocol, body: func(string) string { return body }}
	}
	dies := func(name, protocol, body string, want error) requestForm {
		return requestForm{name: name, protocol: protocol, body: func(string) string { return body }, want: want}
	}
	echo := func(name, protocol string, body func(p string) string) requestForm {
		return requestForm{name: name, protocol: protocol, payload: true, body: body, want: gitproc.ErrFailed,
			echoes: true}
	}
	const v2p = "version=2"
	return []requestForm{
		// v2 fetch arguments carrying the request's text.
		echo("v2 an unknown line", v2p, func(p string) string { return fetch(wantTip, p) }),
		echo("v2 an unknown line holding a newline", v2p, func(p string) string { return fetch(wantTip, "x\n"+p) }),
		echo("v2 an unknown line with no newline", v2p, func(p string) string {
			return pkt("command=fetch\n") + "0001" + pkt(wantTip+"\n") + pkt(p) + "0000"
		}),
		echo("v2 filter", v2p, func(p string) string { return fetch(wantTip, "filter "+p) }),
		echo("v2 filter blob:limit", v2p, func(p string) string { return fetch(wantTip, "filter blob:limit="+p) }),
		// The tree and combine filters name what they expected, or the one
		// character they refused, and copy nothing else.
		{name: "v2 filter tree", protocol: v2p, payload: true, want: gitproc.ErrFailed,
			body: func(p string) string { return fetch(wantTip, "filter tree:"+p) }},
		{name: "v2 filter sparse:oid", protocol: v2p, payload: true, want: gitproc.ErrFailed,
			body: func(p string) string { return fetch(wantTip, "filter sparse:oid="+p) }},
		echo("v2 filter object:type", v2p, func(p string) string { return fetch(wantTip, "filter object:type="+p) }),
		{name: "v2 filter combine", protocol: v2p, payload: true, want: gitproc.ErrFailed,
			body: func(p string) string { return fetch(wantTip, "filter combine:"+p) }},
		echo("v2 want", v2p, func(p string) string { return fetch("want " + p) }),
		echo("v2 have", v2p, func(p string) string { return fetch(wantTip, "have "+p) }),
		echo("v2 shallow", v2p, func(p string) string { return fetch(wantTip, "shallow "+p) }),
		echo("v2 deepen", v2p, func(p string) string { return fetch(wantTip, "deepen "+p) }),
		echo("v2 deepen-since", v2p, func(p string) string { return fetch(wantTip, "deepen-since "+p) }),
		echo("v2 deepen-not", v2p, func(p string) string { return fetch(wantTip, "deepen-not "+p) }),
		echo("v2 want-ref", v2p, func(p string) string { return fetch("want-ref " + p) }),
		{name: "v2 packfile-uris", protocol: v2p, payload: true, want: gitproc.ErrFailed,
			body: func(p string) string { return fetch(wantTip, "packfile-uris "+p) }},
		{name: "v2 packfile-uris twice", protocol: v2p, payload: true, want: gitproc.ErrFailed,
			body: func(p string) string { return fetch(wantTip, "packfile-uris "+p, "packfile-uris "+p) }},
		// v2 ls-refs and commands.
		echo("v2 ls-refs an unknown argument", v2p, func(p string) string { return v2("ls-refs", nil, p) }),
		{name: "v2 ls-refs ref-prefix", protocol: v2p, payload: true,
			body: func(p string) string { return v2("ls-refs", nil, "ref-prefix "+p) }},
		echo("v2 an unknown command", v2p, func(p string) string { return v2(p, nil) }),
		// v2 capabilities.
		echo("v2 object-format", v2p, func(p string) string { return v2("fetch", []string{"object-format=" + p}, wantTip, "done") }),
		{name: "v2 agent", protocol: v2p, payload: true,
			body: func(p string) string { return v2("fetch", []string{"agent=" + p}, wantTip, "done") }},
		echo("v2 session-id", v2p, func(p string) string { return v2("fetch", []string{"session-id=" + p}, wantTip, "done") }),
		echo("v2 an unknown capability", v2p, func(p string) string { return v2("fetch", []string{p}, wantTip, "done") }),
		// v0.
		echo("v0 want", "", func(p string) string { return v0("want "+p, "done") }),
		echo("v0 shallow", "", func(p string) string { return v0(wantTip) + pkt("shallow "+p+"\n") + "0000" + pkt("done\n") }),
		echo("v0 deepen", "", func(p string) string { return v0(wantTip) + pkt("deepen "+p+"\n") + "0000" + pkt("done\n") }),
		echo("v0 deepen-since", "", func(p string) string {
			return v0(wantTip) + pkt("deepen-since "+p+"\n") + "0000" + pkt("done\n")
		}),
		echo("v0 deepen-not", "", func(p string) string {
			return v0(wantTip) + pkt("deepen-not "+p+"\n") + "0000" + pkt("done\n")
		}),
		echo("v0 filter", "", func(p string) string {
			return v0(wantTip+" filter") + pkt("filter "+p+"\n") + "0000" + pkt("done\n")
		}),
		echo("v0 have", "", func(p string) string { return v0(wantTip, "have "+p, "done") }),
		echo("v0 garbage", "", func(p string) string { return v0(p, "done") }),
		echo("v0 garbage holding a newline", "", func(p string) string { return v0("x\n"+p, "done") }),
		echo("v0 garbage among the haves", "", func(p string) string { return v0(wantTip, p, "done") }),
		{name: "v0 first-line capabilities", protocol: "", payload: true,
			body: func(p string) string { return v0(wantTip+" "+p, "done") }},
		echo("v1 want", "version=1", func(p string) string { return v0("want "+p, "done") }),

		// The forms that carry no text of the request's.
		served("v2 a well-formed filter", v2p, fetch(wantTip, "filter combine:blob:none+tree:0")),
		dies("v2 want of an absent id", v2p, fetch("want "+absent), gitproc.ErrNotOurRef),
		served("v2 want of a tree", v2p, fetch("want "+tree)),
		served("v2 want of a blob", v2p, fetch("want "+blob)),
		served("v2 have of an absent id", v2p, fetch(wantTip, "have "+absent)),
		served("v2 have of a blob and a tree", v2p, fetch(wantTip, "have "+blob, "have "+tree)),
		served("v2 have with no done", v2p, v2("fetch", nil, wantTip, "have "+c3)),
		served("v2 shallow of an absent id", v2p, fetch(wantTip, "shallow "+absent)),
		dies("v2 shallow of a blob", v2p, fetch(wantTip, "shallow "+blob), gitproc.ErrFailed),
		served("v2 shallow of a commit under deepen 1", v2p, fetch(wantTip, "shallow "+c3, "deepen 1")),
		served("v2 the deepest deepen, relative", v2p, fetch(wantTip, "deepen 2147483647", "deepen-relative")),
		served("v2 deepen-since 0", v2p, fetch(wantTip, "deepen-since 0")),
		dies("v2 deepen-since the far future", v2p, fetch(wantTip, "deepen-since 99999999999"), gitproc.ErrFailed),
		dies("v2 deepen-not the tip's branch", v2p, fetch(wantTip, "deepen-not refs/heads/main"), gitproc.ErrFailed),
		dies("v2 deepen-not of an id", v2p, fetch(wantTip, "deepen-not "+tip), gitproc.ErrFailed),
		dies("v2 deepen-not of a name a branch and a tag both hold", v2p, fetch(wantTip, "deepen-not twice"), gitproc.ErrFailed),
		served("v2 the flags", v2p, fetch(wantTip, "thin-pack", "ofs-delta", "no-progress", "include-tag")),
		dies("v2 sideband-all", v2p, fetch(wantTip, "sideband-all"), gitproc.ErrFailed),
		served("v2 wait-for-done", v2p, fetch(wantTip, "wait-for-done")),
		served("v2 no arguments", v2p, v2("fetch", nil)),
		dies("v2 a sha256 client", v2p, v2("fetch", []string{"object-format=sha256"}, wantTip, "done"), gitproc.ErrFailed),
		served("v2 an empty server option", v2p, v2("fetch", []string{"server-option="}, wantTip, "done")),
		dies("v2 bundle-uri", v2p, v2("bundle-uri", nil), gitproc.ErrFailed),
		dies("v2 object-info", v2p, v2("object-info", nil, "size", "oid "+tip), gitproc.ErrFailed),
		dies("v2 a second command", v2p, pkt("command=ls-refs\n")+pkt("command=fetch\n")+"0001"+"0000", gitproc.ErrFailed),
		served("v2 ls-refs with peel, symrefs and unborn", v2p, v2("ls-refs", nil, "peel", "symrefs", "unborn")),
		dies("v2 a length header of letters", v2p, "zzzz", gitproc.ErrFailed),
		// Stateless: a request that ends part way through its first
		// packet is a request that ended.
		served("v2 a truncated packet", v2p, pkt("command=fetch\n")[:10]),
		dies("v2 a response end", v2p, "0002", gitproc.ErrFailed),
		dies("v2 arguments ended by a delimiter", v2p, pkt("command=fetch\n")+"0001"+pkt(wantTip+"\n")+"0001", gitproc.ErrFailed),
		served("version=9 read as v0", "version=9", v0(wantTip, "done")),
		// Not tips, so upload-pack starts its own rev-list to ask whether
		// the tip reaches them: it does.
		served("v0 want of a blob", "", v0("want "+blob, "done")),
		served("v0 want of a tree", "", v0("want "+tree, "done")),
		served("v0 want of a commit below the tip", "", v0("want "+c3, "done")),
		served("v0 a second want", "", pkt(wantTip+"\n")+pkt("want "+c3+"\n")+"0000"+pkt("done\n")),
		served("v0 a have after the flush", "", v0(wantTip, "have "+c3, "done")),
		served("v0 no done", "", v0(wantTip)),
		dies("v0 an empty body", "", "", gitproc.ErrFailed),
	}
}

// A REQUEST CANNOT NAME THE DAMAGE. Every form a client can send upload-pack
// that carries text of its own — the arguments, capabilities and commands of
// protocol v2 and the lines of v0, each holding every row's format, every
// row's message and 65 000 control bytes in turn — and the forms beside them
// that carry none, against a HEALTHY copy under the cache's serve
// configuration, on the node pin and on the node floor's own build
// (onNodeGits): no Damage, no hint, and the kind each form is (nil for a
// form that is served, ErrNotOurRef for a want of something the copy does
// not hold, ErrFailed for every other refusal). The formats every run met are
// EXACTLY the request formats this git prints (requestFormats, and the one
// row a request may reach, "not our ref"), so a form git starts answering
// with a new format is noticed and judged. And each form that dies copying
// the request's text into its report is proven to have copied it — its
// stderr carries the payload's end — so the case cannot pass on a form git
// stopped reading.
//
// Mutations, each red: classify any event's message by the rows' formats as
// substrings; classify stderr; take "%s" as a row; find the hint's sentence
// anywhere in a message, BOTH anchors dropped (the "packed object …" payload
// inside a filter form's "%s" message, "invalid filter-spec '…'", then
// hints). Dropping the END anchor alone is not this test's to see: a hint is
// read only from a bare-"%s" event, and no form's "%s" message begins with
// the sentence, so the start anchor keeps it clean —
// TestAHintIsOnlyTheOneAnchoredMessage holds each anchor on its own.
func TestARequestCannotNameTheDamage(t *testing.T) {
	t.Parallel()
	onNodeGits(t, requestCannotNameTheDamageOn)
}

// requestCannotNameTheDamageOn is TestARequestCannotNameTheDamage on the git
// at git.
func requestCannotNameTheDamageOn(t *testing.T, git string) {
	f := newFixture(t)
	p := poolOn(t, git, gitproc.Options{})
	dir, _ := f.bare(t)
	// A branch and a tag of one name beside the copy's main, so a deepen-not
	// can name a ref ambiguously: a healthy copy may hold both.
	gittest.SetRefs(t, dir, map[string]string{"refs/heads/main": f.tip(), "refs/heads/twice": f.Commits[2],
		"refs/tags/twice": f.Commits[2]})
	tree := strings.TrimSpace(f.Client.Run(t, f.Work, "rev-parse", f.tip()+"^{tree}"))
	blob := strings.TrimSpace(f.Client.Run(t, f.Work, "rev-parse", f.tip()+":big.txt"))
	payloads := requestPayloads()
	// A packed object's own sentence, which 2.56 dies with through "%s":
	// inside a request it is text, and hints nothing.
	payloads["hint"] = "packed object " + f.tip() + " (stored in ./objects/pack/pack-" + strings.Repeat("ab", 20) +
		".pack) is corrupt\n" + requestMark

	met := map[string]bool{}
	runs := 0
	for _, form := range requestForms(f.tip(), f.Commits[2], tree, blob) {
		instances := map[string]string{"": ""}
		if form.payload {
			instances = payloads
		}
		for name, payload := range instances {
			runs++
			t.Run(form.name+"/"+name, func(t *testing.T) {
				res, err := p.Run(t.Context(), gitproc.Cmd{
					Dir: dir, Args: []string{"upload-pack", "--stateless-rpc", dir}, Protocol: form.protocol,
					Config: serveConfig, Stdin: strings.NewReader(form.body(payload)), Stdout: io.Discard,
					Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: gitproc.ReadDeadline,
				})
				for _, c := range res.Trace.Formats {
					met[c] = true
				}
				if res.Damage != nil {
					t.Errorf("a healthy copy's answer to a request named damage: %v (formats %q)", res.Damage,
						res.Trace.Formats)
				}
				if len(res.Trace.Hints) != 0 {
					t.Errorf("a request hinted %v", res.Trace.Hints)
				}
				switch {
				case form.want == nil && err != nil:
					t.Errorf("%v (formats %q), want the form served", err, res.Trace.Formats)
				case form.want != nil:
					var exit *gitproc.ExitError
					if !errors.As(err, &exit) || !errors.Is(exit.Kind, form.want) {
						t.Errorf("%v (formats %q), want an ExitError of kind %v", err, res.Trace.Formats, form.want)
					}
				}
				if form.echoes && name != "" && !strings.Contains(res.Stderr, requestMark) {
					t.Errorf("the form died without copying the request's text: stderr %q",
						res.Stderr[max(0, len(res.Stderr)-400):])
				}
			})
		}
	}
	want := map[string]bool{gitproc.CanonicalFormat(`git upload-pack: not our ref %s`): true}
	for format := range requestFormats {
		want[gitproc.CanonicalFormat(format)] = true
	}
	got := slices.Sorted(func(yield func(string) bool) {
		for c := range met {
			if !yield(c) {
				return
			}
		}
	})
	wantList := slices.Sorted(func(yield func(string) bool) {
		for c := range want {
			if !yield(c) {
				return
			}
		}
	})
	if !slices.Equal(got, wantList) {
		t.Errorf("%d runs on git %s met formats\n  %q\nwant exactly\n  %q", runs, p.Version(), got, wantList)
	}
}

// A HINT IS ONLY THE ONE ANCHORED MESSAGE: git's sentence for a corrupt
// packed object, under a bare "%s", whole, naming a pack of the run's own
// repository — as upload-pack spells it from inside the repository and as
// rev-list spells it from outside. Anything else — another format, another
// directory, a path that climbs out, text on either side, an id that is not
// git's, the sentence inside a longer message — hints nothing; and a run
// holds at most sixteen hints, each once.
//
// Mutations, each red: drop the start or the end anchor, or accept a pack of
// any directory.
func TestAHintIsOnlyTheOneAnchoredMessage(t *testing.T) {
	t.Parallel()
	const dir = "/srv/crewlet/git/repos/0190c3a2.3.git"
	oid := strings.Repeat("0123456789abcdef0123", 2)
	pack := strings.Repeat("ab", 20)
	sentence := func(path string) string { return "packed object " + oid + " (stored in " + path + ") is corrupt" }
	relative := sentence("./objects/pack/pack-" + pack + ".pack")
	absolute := sentence(dir + "/objects/pack/pack-" + pack + ".pack")
	stream := func(lines ...string) []byte {
		return []byte(versionLine(1, "2.56.0") + startLine(1, "git", "upload-pack") + strings.Join(lines, "") +
			exitLine(1, 128))
	}
	want := gitproc.Hint{Object: oid, Pack: pack}
	for _, tc := range []struct {
		name  string
		lines []string
		hints int
	}{
		{"upload-pack's relative path", []string{errorLine(2, "%s", relative)}, 1},
		{"rev-list's absolute path", []string{errorLine(1, "%s", absolute)}, 1},
		{"the same pack twice", []string{errorLine(2, "%s", relative), errorLine(1, "%s", absolute)}, 1},
		{"another format", []string{errorLine(1, "fatal: %s", relative), errorLine(1, "%s: %s", relative)}, 0},
		{"another repository's pack", []string{errorLine(1, "%s",
			sentence("/srv/crewlet/git/repos/0190c3a2.2.git/objects/pack/pack-"+pack+".pack"))}, 0},
		{"a path that climbs out", []string{errorLine(1, "%s",
			sentence(dir+"/objects/pack/../../../other.git/objects/pack/pack-"+pack+".pack"))}, 0},
		{"a file that is no pack", []string{errorLine(1, "%s", sentence("./objects/pack/pack-"+pack+".idx"))}, 0},
		{"text before", []string{errorLine(1, "%s", "x "+relative)}, 0},
		{"text after", []string{errorLine(1, "%s", relative+" x"), errorLine(1, "%s", relative+"\n")}, 0},
		{"an id that is not git's", []string{errorLine(1, "%s", strings.Replace(relative, oid,
			strings.ToUpper(oid), 1)), errorLine(1, "%s", strings.Replace(relative, oid, oid[:39], 1))}, 0},
		{"a pack checksum that is not git's", []string{errorLine(1, "%s", sentence("./objects/pack/pack-zz"+
			pack[2:]+".pack"))}, 0},
		{"inside a longer message", []string{errorLine(1, "unexpected line: '%s'", "unexpected line: '"+relative+"'"),
			errorLine(1, "%s", "unexpected line: '"+relative+"'")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, kind, damage := gitproc.ReadTrace(dir, stream(tc.lines...), 0)
			if len(tr.Hints) != tc.hints || (tc.hints == 1 && tr.Hints[0] != want) {
				t.Errorf("hints %v, want %d naming %v", tr.Hints, tc.hints, want)
			}
			if damage != nil || !errors.Is(kind, gitproc.ErrFailed) {
				t.Errorf("a hint's run is kind %v, damage %v; want ErrFailed and none", kind, damage)
			}
		})
	}
	// THE SAME SENTENCE AS THE ROW (2.55) names the same pack: a hint
	// beside the row's damage, under the same anchoring — so the repair can
	// name the pack the row says is damaged. Mutation: read hints only from
	// "%s" events.
	const row = `packed object %s (stored in %s) is corrupt`
	for _, tc := range []struct {
		name    string
		message string
		hints   int
	}{
		{"upload-pack's relative path", relative, 1},
		{"rev-list's absolute path", absolute, 1},
		{"another repository's pack", sentence("/srv/crewlet/git/repos/0190c3a2.2.git/objects/pack/pack-" + pack +
			".pack"), 0},
		{"text after", relative + " x", 0},
	} {
		t.Run("the row/"+tc.name, func(t *testing.T) {
			tr, kind, damage := gitproc.ReadTrace(dir, stream(errorLine(2, row, tc.message)), 0)
			if len(tr.Hints) != tc.hints || (tc.hints == 1 && tr.Hints[0] != want) {
				t.Errorf("hints %v, want %d naming %v", tr.Hints, tc.hints, want)
			}
			var dmg *gitproc.Damage
			if !errors.Is(kind, gitproc.ErrCorrupt) || !errors.As(damage, &dmg) || dmg.Parts&gitproc.PartPack == 0 {
				t.Errorf("the row's run is kind %v, damage %v; want ErrCorrupt naming a pack", kind, damage)
			}
		})
	}
	// A run that names a pack with no repository of its own hints nothing.
	if tr, _, _ := gitproc.ReadTrace("", stream(errorLine(2, "%s", relative)), 0); len(tr.Hints) != 0 {
		t.Errorf("a run with no Cmd.Dir hinted %v", tr.Hints)
	}
	var many []string
	for i := range 17 {
		many = append(many, errorLine(2, "%s", sentence(fmt.Sprintf("./objects/pack/pack-%040x.pack", i))))
	}
	if tr, _, _ := gitproc.ReadTrace(dir, stream(many...), 0); len(tr.Hints) != 16 {
		t.Errorf("seventeen packs hinted %d times, want the sixteen a run holds", len(tr.Hints))
	}
}

// rowCanon is every row of the table by its canonical format.
func rowCanon() map[string]bool {
	out := map[string]bool{}
	for f := range gitproc.FormatTable() {
		out[gitproc.CanonicalFormat(f)] = true
	}
	return out
}

// namesARow reports whether stream holds, as one whole line (the last one
// with or without its newline), an error event whose format is a row: the
// only thing a Damage may be read from.
func namesARow(stream []byte, rows map[string]bool) bool {
	for _, line := range bytes.Split(stream, []byte("\n")) {
		if len(line) > gitproc.MaxLine {
			continue
		}
		var ev struct{ Event, Fmt string }
		if json.Unmarshal(line, &ev) == nil && ev.Event == "error" && rows[gitproc.CanonicalFormat(ev.Fmt)] {
			return true
		}
	}
	return false
}

// FUZZ: A DAMAGE IS READ ONLY FROM A WHOLE LINE THAT IS AN EVENT NAMING A ROW.
// Streams of git-shaped event lines — request text escaped as git escapes it
// under formats that are no row, lines cut at any offset and interleaved with
// whole lines of another process, two values on one line, a line past
// MaxLine — and arbitrary bytes, read in pieces of any size: the sink never
// names damage unless the stream holds, as one complete line, an error event
// whose format is a row, and never holds more than MaxLine of a line.
//
// Mutations, each red: decode a line with a streaming json.Decoder (a line
// holding a whole damage event and more then decodes); decode a fragment
// leniently.
func FuzzTraceSink(f *testing.F) {
	damage := errorLine(2, "index file %s is too small", "index file ./objects/pack/pack-1.idx is too small")
	echo := errorLine(1, "unexpected line: '%s'", "unexpected line: '"+damage+"'")
	f.Add([]byte(versionLine(1, "2.47.3")+startLine(1, "git", "upload-pack")+damage+exitLine(1, 128)), uint16(0))
	f.Add([]byte(versionLine(1, "2.47.3")+echo+exitLine(1, 128)), uint16(7))
	// A long write cut by a child's whole line.
	f.Add([]byte(echo[:40]+damage+echo[40:]), uint16(3))
	// Two values on one line, and a fragment that begins as an event.
	f.Add([]byte(strings.TrimSuffix(damage, "\n")+`{}`+"\n"), uint16(0))
	f.Add([]byte(`{"event":"error",}`+strings.TrimSuffix(damage, "\n")+"\n"), uint16(5))
	f.Add([]byte(`{"event":"error","fmt":"index file %s is too small"`), uint16(0))
	// Past MaxLine: a whole damage event padded with what JSON calls space.
	f.Add([]byte(strings.TrimSuffix(damage, "\n")+strings.Repeat(" ", gitproc.MaxLine)+"\n"), uint16(4096))
	f.Add([]byte(errorLine(1, "unexpected line: '%s'", controlBytes(12000))+damage), uint16(1000))
	f.Add([]byte{0x7b, 0x22, 0x0a, 0xff, 0x00}, uint16(1))
	rows := rowCanon()
	f.Fuzz(func(t *testing.T, stream []byte, piece uint16) {
		_, _, damage := gitproc.ReadTrace("/srv/r.git", stream, int(piece))
		if damage != nil && !namesARow(stream, rows) {
			t.Fatalf("the sink read %v from a stream holding no whole line naming a row: %q", damage, stream)
		}
		if held := gitproc.TraceHeld(stream, max(1, int(piece))); held > gitproc.MaxLine {
			t.Fatalf("the sink held %d bytes of a line, past MaxLine", held)
		}
	})
}

// A LINE LONGER THAN A LINE GIT WRITES IS SKIPPED WHOLE. The 390 KB event a
// request makes upload-pack write — one pkt-line of control bytes, escaped
// six bytes to one — is skipped and counted, and the damage event after it is
// read; a damage event padded past MaxLine is skipped too, though its first
// MaxLine bytes decode, while one padded to exactly MaxLine is read. Through
// all of it the sink never holds more than MaxLine of a line.
//
// Mutations, each red: classify the first MaxLine bytes of a long line (the
// padded event then names damage); buffer the whole line (the holding
// assertion).
func TestALineLongerThanALineGitWritesIsSkippedWhole(t *testing.T) {
	t.Parallel()
	damage := errorLine(2, "index file %s is too small", "index file ./objects/pack/pack-1.idx is too small")
	echo := errorLine(1, "unexpected line: '%s'", "unexpected line: '"+controlBytes(65000)+"'")
	if len(echo) < 390_000 {
		t.Fatalf("the echo line is %d bytes, want the 390 KB git writes", len(echo))
	}
	stream := []byte(versionLine(1, "2.47.3") + echo + damage + exitLine(1, 128))
	for _, piece := range []int{0, 1 << 10, 4 << 10, 64 << 10} {
		tr, kind, dmg := gitproc.ReadTrace("/srv/r.git", stream, piece)
		if !errors.Is(kind, gitproc.ErrCorrupt) || dmg == nil || tr.Unread != 1 || tr.Events != 4 {
			t.Errorf("in pieces of %d: kind %v, damage %v, %d unread of %d events; want the damage read past one "+
				"unread line of four", piece, kind, dmg, tr.Unread, tr.Events)
		}
		if held := gitproc.TraceHeld(stream, max(piece, 1)); held > gitproc.MaxLine {
			t.Errorf("in pieces of %d the sink held %d bytes of a line, past MaxLine", piece, held)
		}
	}
	event := strings.TrimSuffix(damage, "\n")
	for _, tc := range []struct {
		name   string
		line   string
		damage bool
	}{
		{"padded to MaxLine", event + strings.Repeat(" ", gitproc.MaxLine-len(event)), true},
		{"padded one byte past MaxLine", event + strings.Repeat(" ", gitproc.MaxLine-len(event)+1), false},
		{"padded far past MaxLine", event + strings.Repeat(" ", 4*gitproc.MaxLine), false},
	} {
		tr, _, dmg := gitproc.ReadTrace("/srv/r.git", []byte(tc.line+"\n"), 4<<10)
		if (dmg != nil) != tc.damage || (tr.Unread == 1) == tc.damage {
			t.Errorf("%s: damage %v, %d unread; want damage %v", tc.name, dmg, tr.Unread, tc.damage)
		}
	}
}

// A REFUSAL IS READ BY ITS FORMAT, never by its words: Reported names a format
// an error event printed, as git's source spells it — whatever conversions it
// spells, %lu or %d — and nothing a message says, so a request whose path
// spells another refusal's words is not that refusal.
//
// Mutation: Reported matching the event's message rather than its format —
// the path that spells the refusal reads as it → red.
func TestATraceReportsARefusalByItsFormat(t *testing.T) {
	t.Parallel()
	const pastEnd = "file %s has only %lu lines"
	tr, _, _ := gitproc.ReadTrace("/srv/r.git", []byte(versionLine(1, "2.56.0")+
		errorLine(1, pastEnd, "file a.txt has only 3 lines")+exitLine(1, 128)), 0)
	switch {
	case !tr.Reported(pastEnd):
		t.Errorf("a trace holding %q does not report it: %q", pastEnd, tr.Formats)
	case !tr.Reported("file %s has only %d lines"):
		t.Error("a format read another conversion of is not reported as the one it is")
	case !tr.Reported("file %s has only %lu line", pastEnd):
		t.Error("one of several formats reported is not reported")
	case tr.Reported("file %s has only %lu line"):
		t.Error("the singular is reported for the plural")
	}
	spelled, _, _ := gitproc.ReadTrace("/srv/r.git", []byte(versionLine(1, "2.56.0")+
		errorLine(1, "no such path %s in %s", "no such path file a.txt has only 3 lines in HEAD")+
		exitLine(1, 128)), 0)
	if spelled.Reported(pastEnd) {
		t.Error("a message whose path spells the refusal is reported as the refusal")
	}
	if spelled.Reported() {
		t.Error("no format asked about is reported")
	}
}
