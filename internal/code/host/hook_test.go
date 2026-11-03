package host_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A hook whose engine accepts it and then goes away fails the push and says
// the outcome is unknown; a hook started by hand says it is not to be.
func TestAnEngineThatGoesAwayFailsTheHook(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "h")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
	}()
	env := map[string]string{gitproc.EnvHookSocket: socket, gitproc.EnvHookToken: strings.Repeat("a", 64)}
	var stdout, stderr bytes.Buffer
	status := host.RunHook([]string{"proc-receive"}, strings.NewReader(""), &stdout, &stderr,
		func(k string) string { return env[k] })
	if status != 1 || !strings.Contains(stderr.String(), "the engine went away during this push") {
		t.Errorf("a hook whose engine went away exited %d with %q", status, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	status = host.RunHook([]string{"proc-receive"}, strings.NewReader(""), &stdout, &stderr,
		func(string) string { return "" })
	if status != 2 || !strings.Contains(stderr.String(), "run by the engine's git, never by hand") ||
		stdout.Len() > 0 {
		t.Errorf("a hook run by hand exited %d with %q on stderr and %q on stdout", status, stderr.String(),
			stdout.String())
	}
}

// The argv that STARTS the hook is the argv that is RECOGNISED as it: one
// spelling, so an upgrade cannot rename one and fail every push.
func TestTheHookIsRecognisedByTheArgvThatStartsIt(t *testing.T) {
	argv := host.HookCommand("/bin/crewlet")
	if argv[0] != "/bin/crewlet" {
		t.Fatalf("HookCommand starts %q", argv)
	}
	rest, ok := host.HookArgs(argv[1:])
	if !ok || !slices.Equal(rest, []string{"proc-receive"}) {
		t.Errorf("HookArgs(%q) = (%q, %v), want ([proc-receive], true)", argv[1:], rest, ok)
	}
	for _, args := range [][]string{{"code", "status"}, {"hook", "proc-receive"}, {"code"}, nil} {
		if rest, ok := host.HookArgs(args); ok {
			t.Errorf("HookArgs(%q) = (%q, true): not the hook", args, rest)
		}
	}
}

// §5.16: the hook's budget is its steps — three scans of the largest pack one
// push may store, its put at the pace floor, two write attempts and the
// pending grace — and at the defaults it and the receive-pack deadline it
// sits in are the figures the documentation states.
func TestTheHookBudgetIsItsSteps(t *testing.T) {
	steps := 3*gitproc.PackWorkDeadline(code.MaxPackBytes) + objstore.ReadBase +
		objstore.PaceFor(code.MaxPackBytes) + host.ProposeBudget + host.Production.PendingGrace
	if got := host.HookBudget(host.Production); got != steps {
		t.Errorf("HookBudget = %v, its steps sum to %v", got, steps)
	}
	if got, want := host.HookBudget(host.Production), 42510*time.Second; got != want {
		t.Errorf("HookBudget at the defaults = %v (%d s), the documented %v", got, int64(got.Seconds()), want)
	}
	if got, want := gitproc.ReceivePackDeadline(1<<30, 0, host.HookBudget(host.Production)), 74324*time.Second; got != want {
		t.Errorf("ReceivePackDeadline(1 GiB) = %v (%d s), the documented %v", got, int64(got.Seconds()), want)
	}
}

// A BALANCER IN FRONT OF A CLIENT WITHOUT SIDE-BAND IS SIZED BY THE SESSION'S
// OWN BOUNDS: such a client hears nothing from the end of its upload to its
// report, so docs/guides/deployment.md's idle timeout is what the session may
// take before it gives a push up — the proposal's two write attempts and the
// pending grace whatever the push's size, and for a large push the pack's own
// work: git's indexing and the hook's three scans at gitproc.PackWorkDeadline
// each, and the put at the pace floor; and for a large repository each walk
// of its history (gitproc.HistoryWalk), which a commit dated before its
// parent may make whatever the push's size. A figure stated beside the
// constants rather than from them was 60 s, under the three minutes a
// one-commit push may be silent when its committer fails over or its row
// resolves pending, and a balancer at it cut exactly the pushes whose outcome
// may already be durable.
//
// Mutation: change ProposeBudget, the pending grace, PackWorkPerMiB, the pace
// floor or WalkPerObject, and the page no longer says what the session takes.
func TestTheIdleTimeoutForAClientWithoutSideBandIsTheSessionsBounds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "guides", "deployment.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := strings.Join(strings.Fields(string(raw)), " ")
	words := map[time.Duration]string{time.Minute: "a minute", 2 * time.Minute: "two minutes"}
	fixed := host.ProposeBudget + host.Production.PendingGrace
	if fixed%time.Minute != 0 || words[host.ProposeBudget] == "" || words[host.Production.PendingGrace] == "" {
		t.Fatalf("ProposeBudget (%v) and the pending grace (%v) are no longer the whole minutes the page "+
			"says: say there what they are, and here", host.ProposeBudget, host.Production.PendingGrace)
	}
	// What the session does after the pack is indexed, by its own budget:
	// three scans, the put at the floor, the proposal and the pending wait.
	if got, want := host.HookBudget(host.Production), 3*gitproc.PackWorkDeadline(code.MaxPackBytes)+
		objstore.ReadBase+objstore.PaceFor(code.MaxPackBytes)+fixed; got != want {
		t.Fatalf("HookBudget is %v and the page's steps sum to %v: say there what the hook does", got, want)
	}
	if walk := gitproc.HistoryWalk(3_000_000); walk != 33*time.Second {
		t.Fatalf("a walk of three million objects is given %v, the page's 33 s: say there what it is, and here",
			walk)
	}
	work := gitproc.PackWorkDeadline(1)
	perMiB := 4*(gitproc.PackWorkDeadline(2<<20)-work) + objstore.MiBPace
	if work != gitproc.ReadDeadline+time.Second || gitproc.PackWorkDeadline(2<<20)-work != time.Second ||
		objstore.ReadBase != time.Minute {
		t.Fatalf("a pack's work is %v and a second a mebibyte past the first, and a put %v and %v a "+
			"mebibyte: say on the page what they are, and here", work, objstore.ReadBase, objstore.MiBPace)
	}
	for _, want := range []string{
		fmt.Sprintf("keep the idle timeout at %d minutes or more, whatever the push's size", fixed/time.Minute),
		fmt.Sprintf("the proposal is given %s", words[host.ProposeBudget]),
		fmt.Sprintf("a row that resolved pending %s more", words[host.Production.PendingGrace]),
		fmt.Sprintf("%d s and a second a mebibyte for git's indexing and for each of the engine's three scans",
			gitproc.ReadDeadline/time.Second),
		fmt.Sprintf("a minute and %d s a mebibyte (the floor every transfer is held to) for the object store's put",
			objstore.MiBPace/time.Second),
		fmt.Sprintf("about %d s a mebibyte in all", perMiB/time.Second),
		fmt.Sprintf("each such walk is given %d µs an object of the repository", gitproc.WalkPerObject/time.Microsecond),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/guides/deployment.md does not say %q", want)
		}
	}
}

// The index reader hands out exactly the ids git's own show-index reads, in
// the index's order, and refuses an index it cannot read whole.
func TestTheIdxReaderAgreesWithShowIndex(t *testing.T) {
	c := gittest.NewClient(t)
	work := c.Init(t)
	for i := range 20 {
		c.Commit(t, work, map[string]string{"f.txt": strings.Repeat("x", i+1)}, "commit")
	}
	bare := gittest.Empty(t)
	sum := gittest.Pack(t, c, bare, work, "HEAD")
	idx := filepath.Join(bare, "objects", "pack", "pack-"+sum+".idx")

	count, r, err := host.IdxOIDs(idx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(raw))
	f, err := os.Open(idx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// Inside the repository: outside one, git 2.47's show-index never sets
	// up the hash algorithm it reads by and dies of a segmentation fault —
	// measured on 2.47.3, Debian trixie's, where 2.43 and 2.56 answer.
	shown, stderr, err := c.Try(t, bare, f, "show-index")
	if err != nil {
		t.Fatalf("git show-index: %v\n%s", err, stderr)
	}
	var want []string
	for line := range strings.Lines(shown) {
		if fields := strings.Fields(line); len(fields) >= 2 {
			want = append(want, fields[1])
		}
	}
	if !slices.Equal(got, want) || int(count) != len(want) {
		t.Errorf("idxOIDs read %d ids (count %d), show-index %d; equal: %v", len(got), count, len(want),
			slices.Equal(got, want))
	}

	v1 := filepath.Join(t.TempDir(), "v1.idx")
	pack := strings.TrimSuffix(idx, ".idx") + ".pack"
	if _, stderr, err := c.Try(t, "", nil, "index-pack", "--index-version=1", "-o", v1, pack); err != nil {
		t.Fatalf("git index-pack --index-version=1: %v\n%s", err, stderr)
	}
	bad := filepath.Join(t.TempDir(), "bad.idx")
	if err := os.WriteFile(bad, make([]byte, 8+256*4), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{v1, bad} {
		if _, r, err := host.IdxOIDs(path); err == nil || !strings.Contains(err.Error(), path) {
			if r != nil {
				_ = r.Close()
			}
			t.Errorf("IdxOIDs(%s) = %v, want a refusal naming the file", path, err)
		}
	}
}

// The command section a push opens with is bounded twice, each its own
// refusal: by bytes and by the number of commands.
func TestTheCommandSectionIsBounded(t *testing.T) {
	oid := func(c byte) string { return strings.Repeat(string(c), 40) }
	command := func(ref string) string { return pkt(oid('a') + " " + oid('b') + " " + ref + "\n") }
	first := pkt(oid('a') + " " + oid('b') + " refs/heads/main\x00report-status atomic agent=git/2.43.0\n")
	optioned := pkt(oid('a') + " " + oid('b') + " refs/heads/main\x00report-status push-options\n")
	many := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(command("refs/heads/b" + strings.Repeat("x", i%7) + string(rune('a'+i%26))))
		}
		return b.String()
	}
	for _, tc := range []struct {
		name     string
		body     string
		err      error
		commands int
		empty    bool
		// raw is what the section holds, when it ends before the body does.
		raw string
	}{
		{name: "the probe", body: "0000", empty: true},
		{name: "a thousand commands", body: many(code.MaxCommandsPerPush) + "0000", commands: code.MaxCommandsPerPush},
		{name: "a thousand and one", body: many(code.MaxCommandsPerPush+1) + "0000", err: host.ErrTooManyCommands},
		// PAST A MEBIBYTE: a thousand commands naming refs the engine takes
		// — at most code.MaxRefNameBytes — come to about 340 KiB, so a
		// section past it holds lines no count bounds, or refs the engine
		// refuses bad_ref, of which seventeen of the longest a pkt-line
		// carries are enough. Either way it is refused whole, and no push
		// the engine would have taken is.
		{name: "past a mebibyte", body: strings.Repeat(pkt("shallow "+oid('c')+"\n"),
			host.MaxCommandSectionBytes/53+1) + "0000", err: host.ErrSectionTooLarge},
		{name: "refs past the longest, past a mebibyte", body: first +
			strings.Repeat(command("refs/heads/"+strings.Repeat("z", 65_000)), 17) + "0000",
			err: host.ErrSectionTooLarge},
		{name: "a shallow line passes uncounted", body: pkt("shallow "+oid('c')+"\n") + first + "0000", commands: 1},
		{name: "the capabilities of the first", body: first + command("refs/heads/x") + "0000", commands: 2},
		// A REF PAST THE LONGEST IS STILL A COMMAND — the push's to refuse
		// bad_ref — and its line's capabilities are the push's.
		{name: "a ref past the longest, first", body: pkt(oid('a')+" "+oid('b')+" refs/heads/"+
			strings.Repeat("y", code.MaxRefNameBytes)+"\x00report-status atomic agent=git/2.43.0\n") +
			command("refs/heads/x") + "0000", commands: 2},
		// ANY LINE BUT A LONE FLUSH IS A PUSH, not the probe: a line no
		// command parses from is one receive-pack dies on, in its own words.
		{name: "a line no command parses from", body: pkt("not a command\n") + "0000"},
		// A DELIMITER ENDS THE SECTION, as it ends receive-pack's reading of
		// commands: what follows is the pack's, for git to read.
		{name: "a delimiter", body: "0001" + "0000", raw: "0001"},
		{name: "a command after a delimiter is none", body: first + "0001" + command("refs/heads/x") + "0000",
			commands: 1, raw: first + "0001"},
		{name: "ids in capitals", body: pkt(strings.ToUpper(oid('a')+" "+oid('b'))+" refs/heads/x\n") + "0000",
			commands: 1},
		{name: "an empty ref", body: pkt(oid('a')+" "+oid('b')+" \n") + "0000", commands: 1},
		{name: "a push certificate", body: pkt("push-cert\x00report-status\n") + pkt("certificate version 0.1\n") +
			"0000", err: host.ErrPushCertificate},
		// THE OPTIONS AFTER THE COMMANDS, of a client that asked for them:
		// receive-pack reads them before the pack, so they are the section's,
		// to its own bound.
		{name: "options after the commands", body: optioned + "0000" + pkt("change") + pkt("change.title=x") +
			"0000" + "PACK", commands: 1, raw: optioned + "0000" + pkt("change") + pkt("change.title=x") + "0000"},
		{name: "options past a mebibyte with the commands", body: optioned + "0000" +
			strings.Repeat(pkt("ci.variable="+strings.Repeat("v", 60_000)), 18) + "0000", err: host.ErrOptionsTooLarge},
		{name: "options of a client that asked for none are the pack's", body: first + "0000" + pkt("change") +
			"0000", commands: 1, raw: first + "0000"},
		{name: "options of a section that queued no command", body: pkt("not a command\x00push-options\n") +
			"0000" + pkt("change") + "0000", raw: pkt("not a command\x00push-options\n") + "0000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := host.ReadCommands(strings.NewReader(tc.body), host.MaxCommandSectionBytes, code.MaxCommandsPerPush)
			if !errors.Is(err, tc.err) {
				t.Fatalf("ReadCommands = %v, want %v", err, tc.err)
			}
			if tc.err != nil {
				return
			}
			if len(s.Commands) != tc.commands || s.Empty != tc.empty {
				t.Errorf("read %d commands (empty %v), want %d (%v)", len(s.Commands), s.Empty, tc.commands, tc.empty)
			}
			raw := tc.raw
			if raw == "" {
				raw = tc.body
			}
			if string(s.Raw) != raw {
				t.Errorf("the section's bytes are %q, want the client's %q", s.Raw, raw)
			}
			for _, c := range s.Commands {
				if !gitref.ValidOID(c[0]) || !gitref.ValidOID(c[1]) {
					t.Errorf("a command read as %q %q, not two ids in their one spelling", c[0], c[1])
				}
			}
			if strings.Contains(tc.body, "atomic") && !s.Caps.Atomic {
				t.Errorf("the first command's capabilities read as %+v, without atomic", s.Caps)
			}
		})
	}
}

// THE COMMAND SECTION IS READ AS RECEIVE-PACK READS IT, measured against real
// git: each section, followed by an empty pack, is handed to `git receive-pack
// --stateless-rpc` in a bare repository holding one commit and NO HOOK, under
// the receive configuration's receive.advertisePushOptions, so git creates
// every ref it queued that its own rules take. What git created is held to
// what the reader read: every ref git creates is a command the reader read —
// so no check the receiver makes before receive-pack runs is skipped for a
// command git acts on — the reader's capabilities are the ones git answered
// with (its report, and its side-band), and the bytes the reader takes for the
// section are the section's whole, the push's options after its commands
// among them, so what receive-pack reads before the pack is what the
// receiver bounded. Each row pins git's side as well as the reader's, so a
// git that changes its reading fails here rather than in a push. A section
// the reader refuses is one git acts on regardless, which is why it is
// refused.
func TestTheCommandSectionIsReadAsReceivePackReadsIt(t *testing.T) {
	c := gittest.NewClient(t)
	work := c.Init(t)
	head := c.Commit(t, work, map[string]string{"README.md": "one\n"}, "one")
	upper, mixed := strings.ToUpper(head), strings.ToUpper(head[:20])+head[20:]
	create := func(id, ref string) string { return gitref.ZeroOID + " " + id + " " + ref }
	pack := emptyPack(t, c, work)
	for _, tc := range []struct {
		name    string
		section string   // the command section, the packet that ends it included
		created []string // the refs git creates, measured
		read    []string // the refs of the commands the reader reads, in order
		err     error    // the reader's refusal
		dies    bool     // git dies on the section, before it acts on any command
	}{
		{name: "ids in capitals", section: pkt(create(upper, "refs/heads/upper")+"\x00report-status\n") + "0000",
			created: []string{"refs/heads/upper"}, read: []string{"refs/heads/upper"}},
		{name: "ids in mixed case", section: pkt(create(mixed, "refs/heads/mixed")+"\x00report-status\n") + "0000",
			created: []string{"refs/heads/mixed"}, read: []string{"refs/heads/mixed"}},
		{name: "the capabilities on a later line", section: pkt(create(head, "refs/heads/one")+"\n") +
			pkt(create(head, "refs/heads/two")+"\x00report-status side-band-64k\n") + "0000",
			created: []string{"refs/heads/one", "refs/heads/two"}, read: []string{"refs/heads/one", "refs/heads/two"}},
		{name: "capabilities past a second NUL", section: pkt(create(head, "refs/heads/nul")+
			"\x00report-status\x00side-band-64k\n") + "0000",
			created: []string{"refs/heads/nul"}, read: []string{"refs/heads/nul"}},
		{name: "an empty ref, which git refuses itself", section: pkt(create(head, "")+"\x00report-status\n") +
			pkt(create(head, "refs/heads/ok")+"\n") + "0000",
			created: []string{"refs/heads/ok"}, read: []string{"", "refs/heads/ok"}},
		{name: "a ref holding a line feed before its capabilities", section: pkt(create(head, "refs/heads/lf\n")+
			"\x00report-status\n") + pkt(create(head, "refs/heads/cr\r")+"\n") + "0000",
			read: []string{"refs/heads/lf\n", "refs/heads/cr\r"}},
		{name: "shallow lines: in capitals, with more after the id, with capabilities git does not read",
			section: pkt("shallow "+upper+"\n") + pkt("shallow "+head+" "+create(head, "refs/heads/hidden")+"\n") +
				pkt("shallow "+head+"\x00side-band-64k\n") +
				pkt(create(head, "refs/heads/shallow")+"\x00report-status\n") + "0000",
			created: []string{"refs/heads/shallow"}, read: []string{"refs/heads/shallow"}},
		{name: "a delimiter ends the commands", section: pkt(create(head, "refs/heads/delim")+"\x00report-status\n") +
			"0001", created: []string{"refs/heads/delim"}, read: []string{"refs/heads/delim"}},
		{name: "and so does a response end", section: pkt(create(head, "refs/heads/end")+"\x00report-status\n") +
			"0002", created: []string{"refs/heads/end"}, read: []string{"refs/heads/end"}},
		{name: "a line git dies on", section: pkt(create(head, "refs/heads/before")+"\x00report-status\n") +
			pkt("not a command\n") + pkt(create(head, "refs/heads/after")+"\n") + "0000",
			read: []string{"refs/heads/before", "refs/heads/after"}, dies: true},
		{name: "an id that is not hex", section: pkt(create(strings.Repeat("g", 40), "refs/heads/g")+
			"\x00report-status\n") + "0000", dies: true},
		{name: "a push certificate, whose commands no line of their own carries", section: pkt("push-cert\x00"+
			"report-status\n") + pkt("certificate version 0.1\n") + pkt("pusher p <p@example.com> 0 +0000\n") +
			pkt("pushee crewlet\n") + pkt("nonce n\n") + pkt("\n") +
			pkt(create(head, "refs/heads/cert1")+"\n"+create(head, "refs/heads/cert2")+"\n") +
			pkt(create(head, "refs/heads/ce")) + pkt("rt3\n") + pkt("push-cert-end\n") + "0000",
			created: []string{"refs/heads/cert1", "refs/heads/cert2", "refs/heads/cert3"},
			err:     host.ErrPushCertificate},
		// THE OPTIONS AFTER THE COMMANDS, which receive-pack reads before the
		// pack once a line asked for push-options — the first or a later one.
		{name: "options after the commands", section: pkt(create(head, "refs/heads/opt")+
			"\x00report-status push-options\n") + "0000" + pkt("change") + pkt("change.title=Retry") + "0000",
			created: []string{"refs/heads/opt"}, read: []string{"refs/heads/opt"}},
		{name: "options asked for on a later line", section: pkt(create(head, "refs/heads/o1")+"\n") +
			pkt(create(head, "refs/heads/o2")+"\x00report-status push-options\n") + "0000" + pkt("ci.skip") + "0000",
			created: []string{"refs/heads/o1", "refs/heads/o2"}, read: []string{"refs/heads/o1", "refs/heads/o2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := host.ReadCommands(strings.NewReader(tc.section+pack), host.MaxCommandSectionBytes,
				code.MaxCommandsPerPush)
			if !errors.Is(err, tc.err) {
				t.Fatalf("ReadCommands = %v, want %v", err, tc.err)
			}
			var read []string
			for _, cmd := range s.Commands {
				read = append(read, cmd[2])
			}
			if tc.err == nil && !slices.Equal(read, tc.read) {
				t.Errorf("the reader read %q, want %q", read, tc.read)
			}
			if tc.err == nil && string(s.Raw) != tc.section {
				t.Errorf("the reader took %q for the section, want the section whole: %q", s.Raw, tc.section)
			}

			bare := t.TempDir()
			c.Run(t, bare, "init", "--bare", "-q")
			c.Run(t, work, "push", "-q", bare, "main")
			stdout, stderr, gitErr := c.Try(t, bare, strings.NewReader(tc.section+pack), "-c",
				"receive.advertisePushOptions=true", "receive-pack", "--stateless-rpc", bare)
			if (gitErr != nil) != tc.dies {
				t.Fatalf("git receive-pack exited %v, want it to die: %v\n%s", gitErr, tc.dies, stderr)
			}
			var created []string
			for line := range strings.Lines(c.Run(t, bare, "for-each-ref", "--format=%(refname)")) {
				if ref := strings.TrimSuffix(line, "\n"); ref != "refs/heads/main" {
					created = append(created, ref)
				}
			}
			if !slices.Equal(created, tc.created) {
				t.Fatalf("git created %q, want %q: the row no longer says what git does\n%s", created, tc.created,
					stderr)
			}
			if tc.err != nil {
				return
			}
			for _, ref := range created {
				if !slices.Contains(read, ref) {
					t.Errorf("git created %q, which the reader read no command for: no check before "+
						"receive-pack ever saw it", ref)
				}
			}
			if tc.dies {
				return
			}
			banded := len(stdout) > 4 && (stdout[4] == 1 || stdout[4] == 2)
			if banded != s.Caps.SideBand64k {
				t.Errorf("git answered in side-band: %v; the reader read side-band-64k: %v", banded,
					s.Caps.SideBand64k)
			}
			if reported := strings.Contains(stdout, "unpack ok"); reported != (s.Caps.ReportStatus ||
				s.Caps.ReportStatusV2) {
				t.Errorf("git wrote a report: %v; the reader read %+v", reported, s.Caps)
			}
		})
	}
}
