package host

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitref"
)

// O14: WHAT THE RELAY AND THE WIRE READ IS BYTES A PUSHER CONTROLS, so each
// reader is fuzzed — never a panic, and what it accepts it re-encodes byte for
// byte — and `make check` runs each seed corpus as an ordinary test.
//
// The seeds are git's own bytes where a test can capture them (an
// upload-pack's advertisement and answers, read from real git through
// gittest), and git's documented protocol where it cannot: proc-receive's
// input reaches only the hook, which the engine itself is.

// oidA and oidB are two object ids, for seeds.
var (
	oidA = strings.Repeat("a", 40)
	oidB = strings.Repeat("b", 40)
)

// FuzzRelayFrames: the hook's handshake and the relay's frames, in both
// directions — readHandshake and readFrame never panic, an accepted frame
// re-encodes to the bytes it was read from, and an accepted handshake names a
// token of the one shape the relay issues.
func FuzzRelayFrames(f *testing.F) {
	var frames bytes.Buffer
	fw := &frameWriter{w: &frames}
	_ = fw.write(frameStdout, append(pktString("version=1\x00"), flushPkt...))
	_ = fw.write(frameStderr, []byte("crewlet: committing the push (3s)\r"))
	_ = fw.write(frameExit, []byte{0})
	f.Add(frames.Bytes())
	f.Add([]byte(hookHandshake + strings.Repeat("0f", 32) + "\n"))
	f.Add([]byte{frameStdout, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{frameExit, 0, 0, 0, 2, 0, 0})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if token, err := readHandshake(readerOf(raw)); err == nil &&
			(len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "") {
			t.Fatalf("a handshake was accepted naming %q", token)
		}
		r := bytes.NewReader(raw)
		for {
			before := r.Len()
			ch, payload, err := readFrame(r)
			if err != nil {
				return
			}
			consumed := raw[len(raw)-before : len(raw)-r.Len()]
			var again bytes.Buffer
			if err := (&frameWriter{w: &again}).write(ch, payload); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again.Bytes(), consumed) {
				t.Fatalf("frame %d of %d bytes re-encodes as %x, read from %x", ch, len(payload), again.Bytes(),
					consumed)
			}
		}
	})
}

// FuzzProcReceive: git's proc-receive input as the engine reads it — the
// version block, the commands and, where the version block offered them, the
// push's options. Never a panic; an accepted command list holds only commands
// of two object ids and a ref, at most a push's; the answer to the version
// block offers push-options exactly where git did; and every accepted option
// is one line of what was sent.
func FuzzProcReceive(f *testing.F) {
	version := string(pktString("version=1\x00atomic push-options\n")) + flushPkt
	f.Add([]byte(version + string(pktString(oidA+" "+oidB+" refs/heads/main\n")) + flushPkt +
		string(pktString("change")) + string(pktString("change.title=Retry")) + flushPkt))
	f.Add([]byte(string(pktString("version=1\n")) + flushPkt +
		string(pktString(gitref.ZeroOID+" "+oidB+" refs/heads/new\n")) +
		string(pktString(oidA+" "+gitref.ZeroOID+" refs/tags/v1\n")) + flushPkt))
	f.Add([]byte(string(pktString("repo=other\n")) + flushPkt))
	f.Add([]byte(version + delimPkt + flushPkt))
	f.Add([]byte(version + flushPkt + delimPkt))
	f.Fuzz(func(t *testing.T, raw []byte) {
		br := readerOf(raw)
		s := &Server{}
		var answer bytes.Buffer
		features, err := s.versionBlock(br, &frameWriter{w: &answer})
		if err != nil {
			return
		}
		if offered := bytes.Contains(answer.Bytes(), []byte("push-options")); offered != features.pushOptions {
			t.Fatalf("the relay answered %q to a version block whose push-options it read as %v", answer.Bytes(),
				features.pushOptions)
		}
		cmds, err := readHookCommands(br)
		if err != nil {
			return
		}
		for _, c := range cmds {
			if !gitref.ValidOID(c.Old) || !gitref.ValidOID(c.New) || c.Ref == "" {
				t.Fatalf("an accepted command %+v", c)
			}
		}
		if !features.pushOptions {
			return
		}
		options, err := readHookPushOptions(br)
		if err != nil {
			return
		}
		for _, o := range options {
			if !bytes.Contains(raw, []byte(o)) {
				t.Fatalf("an accepted option %q is none of what was sent", o)
			}
		}
	})
}

// FuzzCommandSection: a push's command section as the receiver reads it,
// before anything checks it. Never a panic; what was read is a prefix of what
// was sent and reads again to the same section — the bytes receive-pack is
// handed are the bytes the commands were read from — and every command holds
// two ids in their one spelling and a ref with no NUL, whatever case the
// client wrote them in. The seeds are git's own shapes (its first command's
// capabilities, a shallow line) and the ones receive-pack reads beyond them.
func FuzzCommandSection(f *testing.F) {
	upper := strings.ToUpper(oidB)
	f.Add([]byte(string(pktString(gitref.ZeroOID+" "+oidB+" refs/heads/main\x00report-status-v2 side-band-64k "+
		"quiet object-format=sha1 agent=git/2.43.0\n")) + flushPkt))
	f.Add([]byte(string(pktString("shallow "+oidA+"\n")) + string(pktString(oidA+" "+oidB+" refs/heads/x\n")) +
		flushPkt))
	f.Add([]byte(string(pktString(gitref.ZeroOID+" "+upper+" refs/heads/x\n")) +
		string(pktString(gitref.ZeroOID+" "+oidB+" \x00report-status atomic\n")) + delimPkt))
	f.Add([]byte(string(pktString("push-cert\x00report-status\n")) + string(pktString("certificate version 0.1\n")) +
		flushPkt))
	f.Add([]byte(flushPkt))
	f.Add([]byte(string(pktString(gitref.ZeroOID+" "+oidB+" refs/heads/x\x00report-status push-options\n")) +
		flushPkt + string(pktString("change")) + string(pktString("change.title=Retry")) + flushPkt + "PACK"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := readCommands(readerOf(raw), MaxCommandSectionBytes, code.MaxCommandsPerPush)
		if !bytes.HasPrefix(raw, s.Raw) {
			t.Fatalf("the section read %q, not a prefix of %q", s.Raw, raw)
		}
		for _, c := range s.Commands {
			if !gitref.ValidOID(c.Old) || !gitref.ValidOID(c.New) || strings.ContainsRune(c.Ref, 0) {
				t.Fatalf("a command read as %+v", c)
			}
		}
		if err != nil {
			return
		}
		again, err := readCommands(readerOf(s.Raw), MaxCommandSectionBytes, code.MaxCommandsPerPush)
		if err != nil || !bytes.Equal(again.Raw, s.Raw) || !slices.Equal(again.Commands, s.Commands) ||
			again.Caps != s.Caps {
			t.Fatalf("the section's own bytes read again as %+v (%v), not %+v", again, err, s)
		}
		if s.Empty() != (string(s.Raw) == flushPkt) {
			t.Fatalf("a section of %q read as empty: %v", s.Raw, s.Empty())
		}
	})
}

// FuzzPktLines: pkt-lines and side-band. readPkt never panics and every packet
// it accepts re-encodes (encodePkt) to the bytes it was read from — what lets
// the receive path hand git exactly what the client sent — and the side-band
// encoder's packets decode back to its channel and its data.
func FuzzPktLines(f *testing.F) {
	for _, seed := range gitTranscripts(f) {
		f.Add(seed, bandData)
	}
	f.Add([]byte("0000000100020004"), bandProgress)
	f.Add([]byte("fff0"+strings.Repeat("x", 65516)), bandData)
	f.Add([]byte("FFFF"), bandData)
	f.Fuzz(func(t *testing.T, raw []byte, band byte) {
		r := readerOf(raw)
		var read []byte
		for {
			p, err := readPkt(r)
			if err != nil {
				break
			}
			read = append(read, encodePkt(p)...)
		}
		if !bytes.HasPrefix(raw, read) {
			t.Fatalf("the packets read re-encode as %q, not a prefix of %q", read, raw)
		}

		ch := bandData + band%2 // the two channels the engine writes
		out := readerOf(banded(ch, raw))
		var data []byte
		for {
			p, err := readPkt(out)
			if err != nil {
				break
			}
			if len(p.payload) == 0 || p.payload[0] != ch || len(p.payload) > maxPktPayload {
				t.Fatalf("a side-band packet of %d bytes on channel %d", len(p.payload), p.payload[0])
			}
			data = append(data, p.payload[1:]...)
		}
		if !bytes.Equal(data, raw) {
			t.Fatalf("side band %d carried %d bytes back of %d", ch, len(data), len(raw))
		}
	})
}

// FuzzAnswerScan: an upload-pack answer is read alike however its writes cut
// it — the pool hands a fetch's sink whatever pieces git's pipe gave it — and
// the reading never panics nor holds more than reportLead of a side band's
// line. Seeded with git's own answers and a damaged clone's.
func FuzzAnswerScan(f *testing.F) {
	for _, seed := range gitTranscripts(f) {
		f.Add(seed, []byte{3, 7, 250})
	}
	damaged := string(pktString("packfile\n")) +
		string(pktLine(append([]byte{bandProgress}, "fatal: invalid MIDX object position, MIDX is likely corrupt\n"...))) +
		string(pktLine(append([]byte{bandError}, "aborting due to possible repository corruption\n"...))) + flushPkt
	f.Add([]byte(damaged), []byte{1, 2, 3, 4, 5})
	f.Add([]byte(damaged), []byte{20, 9})
	f.Add([]byte(string(pktString("NAK\n"))+"PACK\x00\x00\x00\x02"), []byte{2})
	f.Fuzz(func(t *testing.T, answer, cuts []byte) {
		var whole, pieces answerScan
		whole.scan(answer)
		rest := answer
		for _, c := range cuts {
			n := int(c) % (len(rest) + 1)
			pieces.scan(rest[:n])
			rest = rest[n:]
			if len(pieces.lead) > reportLead {
				t.Fatalf("band 2's line in progress holds %d bytes", len(pieces.lead))
			}
		}
		pieces.scan(rest)
		if whole.begun != pieces.begun || whole.raw != pieces.raw {
			t.Fatalf("read whole: begun %v raw %v; in pieces: begun %v raw %v", whole.begun, whole.raw,
				pieces.begun, pieces.raw)
		}
		if len(whole.lead) > reportLead || len(pieces.lead) > reportLead {
			t.Fatalf("band 2's line in progress holds %d bytes whole, %d in pieces", len(whole.lead),
				len(pieces.lead))
		}
	})
}

// gitTranscripts is what real git writes on the wire for a small repository:
// upload-pack's v0 advertisement, its v2 capability advertisement, and its
// answers to an ls-refs and a fetch — the seeds of FuzzPktLines.
func gitTranscripts(f *testing.F) [][]byte {
	f.Helper()
	c := gittest.NewClient(f)
	work := c.Init(f)
	head := c.Commit(f, work, map[string]string{"README.md": "seed\n"}, "seed")
	c.Run(f, work, "tag", "-a", "v1", "-m", "v1")
	var out [][]byte
	run := func(protocol string, stdin string, args ...string) {
		cl := c
		if protocol != "" {
			cl = c.Env("GIT_PROTOCOL=" + protocol)
		}
		stdout, stderr, err := cl.Try(f, work, strings.NewReader(stdin), args...)
		if err != nil {
			f.Fatalf("git %v: %v\n%s", args, err, stderr)
		}
		out = append(out, []byte(stdout))
	}
	run("", "", "upload-pack", "--stateless-rpc", "--advertise-refs", ".git")
	run("version=2", "", "upload-pack", "--stateless-rpc", "--advertise-refs", ".git")
	run("version=2", string(pktString("command=ls-refs\n"))+delimPkt+string(pktString("peel\n"))+flushPkt,
		"upload-pack", "--stateless-rpc", ".git")
	run("version=2", string(pktString("command=fetch\n"))+delimPkt+string(pktString("want "+head+"\n"))+
		string(pktString("done\n"))+flushPkt, "upload-pack", "--stateless-rpc", ".git")
	return out
}

// FuzzServiceFor: the advertisement's reading of its service never runs a
// process the one classifier did not allow — a request auth.GitAccess calls a
// fetch never runs receive-pack, whatever its query holds.
func FuzzServiceFor(f *testing.F) {
	for _, q := range []string{"service=git-upload-pack", "service=git-receive-pack", "",
		"service=git-receive-pack&service=git-upload-pack", "service=git-upload-pack;service=git-receive-pack",
		"service=%67it-receive-pack", "service=", "service=git-upload-pack&x=%zz", "Service=git-receive-pack"} {
		f.Add("/git/acme/info/refs", q)
	}
	f.Add("/git/acme.git/info/refs", "service=git-upload-pack")
	f.Add("/git/a/b/info/refs", "service=git-upload-pack")
	f.Fuzz(func(t *testing.T, path, query string) {
		r := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: path, RawQuery: query}, Header: http.Header{}}
		op := auth.GitAccess(r)
		svc, dumb, ok := serviceFor(r)
		switch {
		case dumb && op != auth.GitFetch:
			t.Fatalf("%s?%s: the dumb protocol's request, classified %q", path, query, op)
		case ok && svc == receivePackService && op != auth.GitPush:
			t.Fatalf("%s?%s: runs receive-pack, classified %q", path, query, op)
		case ok && svc == uploadPackService && op != auth.GitFetch:
			t.Fatalf("%s?%s: runs upload-pack, classified %q", path, query, op)
		case ok && svc != receivePackService && svc != uploadPackService:
			t.Fatalf("%s?%s: a service %q", path, query, svc)
		}
	})
}

// FuzzNegotiation: the two readings of a fetch request — the level it is
// answered at (parseNegotiation) and the wants this node checks for
// (wantsOf) — never panic and agree on whether it names an object.
func FuzzNegotiation(f *testing.F) {
	v2 := string(pktString("command=fetch\n")) + string(pktString("agent=git/2.43.0\n")) + delimPkt
	f.Add([]byte(v2+string(pktString("want "+oidA+"\n"))+string(pktString("done\n"))+flushPkt), true)
	f.Add([]byte(v2+string(pktString("want-ref refs/heads/main\n"))+flushPkt), true)
	f.Add([]byte(string(pktString("command=ls-refs\n"))+delimPkt+string(pktString("peel\n"))+flushPkt), true)
	f.Add([]byte(string(pktString("want "+oidA+" multi_ack_detailed side-band-64k\n"))+flushPkt+
		string(pktString("done\n"))), false)
	f.Add([]byte(v2+string(pktString("want "+oidA+"\n"))+string(pktString("have "+oidB+"\n"))+flushPkt), true)
	f.Add([]byte(string(pktString("want "+oidA+"\n"))+flushPkt+string(pktString("have "+oidB+"\n"))+
		string(pktString("want "+oidB+"\n"))+string(pktString("done\n"))), false)
	f.Add([]byte(string(pktString("want "+gitref.ZeroOID+"\n"))+flushPkt), false)
	f.Add([]byte(v2+string(pktString("want "+strings.ToUpper(oidA)+"x\n"))+flushPkt), true)
	f.Fuzz(func(t *testing.T, raw []byte, v2 bool) {
		n, err := parseNegotiation(bytes.NewReader(raw), int64(len(raw)), v2)
		if err != nil {
			t.Fatalf("a request in memory failed to read: %v", err)
		}
		var wants []string
		for oid, err := range wantsOf(func() io.Reader { return bytes.NewReader(raw) }, int64(len(raw)), v2) {
			if err != nil {
				t.Fatalf("wants of a request in memory: %v", err)
			}
			if !gitref.ValidOID(oid) || oid == gitref.ZeroOID {
				t.Fatalf("a want %q", oid)
			}
			wants = append(wants, oid)
		}
		if n.wants != (len(wants) > 0) {
			t.Fatalf("the level's reading says wants %v, the want reader found %d", n.wants, len(wants))
		}
	})
}
