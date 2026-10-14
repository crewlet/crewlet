package host_test

import (
	"bufio"
	"bytes"
	"errors"
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
	"github.com/crewlet/crewlet/internal/objstore"
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
	if got, want := gitproc.ReceivePackDeadline(1<<30, host.HookBudget(host.Production)), 74324*time.Second; got != want {
		t.Errorf("ReceivePackDeadline(1 GiB) = %v (%d s), the documented %v", got, int64(got.Seconds()), want)
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
	shown, stderr, err := c.Try(t, "", f, "show-index")
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
		// ANY LINE BUT A LONE FLUSH IS A PUSH, not the probe: what the
		// receiver cannot parse is receive-pack's to refuse.
		{name: "a line no command parses from", body: pkt("not a command\n") + "0000"},
		{name: "a delimiter", body: "0001" + "0000"},
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
			if string(s.Raw) != tc.body {
				t.Errorf("the section's bytes are not the client's")
			}
			if strings.Contains(tc.body, "atomic") && (!s.Atomic || s.Agent != "git/2.43.0") {
				t.Errorf("the first command's capabilities read as atomic %v, agent %q", s.Atomic, s.Agent)
			}
		})
	}
}
