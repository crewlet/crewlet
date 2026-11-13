package hostboxtest

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// writerEnv makes this test binary the package's own writer rather than the
// suite: see [runWriter]. Its value is the shared credential directory.
const writerEnv = "CREWLET_HOSTBOXTEST_TEST_WRITER"

// childEnv makes this test binary a process the writer started: see
// [runChild].
const childEnv = "CREWLET_HOSTBOXTEST_TEST_CHILD"

// TestMain runs the writer or its child when this binary was re-executed as
// one, and the suite otherwise: dispatched here rather than from a test
// function, so neither is reported as a test that skipped.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		runChild()
		os.Exit(0)
	}
	if dir := os.Getenv(writerEnv); dir != "" {
		runWriter(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// ownScript is what the package's own writer writes after its setup: one
// sign-in's login of two files, then the other's over it.
var ownScript = Script{Names: []string{"auth.json", "config.toml"}, Logins: []map[string]string{first, second}}

// runWriter writes ownScript's logins into dir, each in one write under the
// lock every writer takes, the first as its setup. Before anything, it starts a
// process of its own and waits for it, as a writer starts a harness's command
// or a box's runner.
func runWriter(dir string) {
	w := NewWriter()
	child := exec.Command(os.Args[0]) //nolint:gosec // this test binary
	child.Env = append(os.Environ(), childEnv+"=1")
	if out, err := child.CombinedOutput(); err != nil {
		refuse(fmt.Sprintf("the writer's own process: %v: %s", err, out))
	}
	ctx := context.Background()
	write := func(login map[string]string) {
		release, err := hostbox.LockCredentials(ctx, dir)
		if err != nil {
			refuse(err.Error())
		}
		files := map[string][]byte{}
		for name, content := range login {
			files[name] = []byte(content)
		}
		err = hostbox.WriteCredentials(dir, ownScript.Names, files, w.Steps())
		release()
		if err != nil {
			refuse(err.Error())
		}
	}
	write(ownScript.Logins[0])
	w.Ready()
	write(ownScript.Logins[1])
	w.Done()
}

// runChild is a process the writer started. It announces on descriptor 3,
// written to directly, whatever it holds there: the matrix's pipe, were the
// writer to hand it on, and otherwise nothing that takes the line.
func runChild() {
	_, _ = syscall.Write(announceFD, []byte(`{"kind":"child"}`+"\n"))
}

// first and second are two sign-ins' logins of two files, every file
// different, so any mix of the two is told apart from both.
var (
	first  = map[string]string{"auth.json": "first token", "config.toml": "first config"}
	second = map[string]string{"auth.json": "second token", "config.toml": "second config"}
)

// threeWrites is hostbox's own script: a write that drops a file, one that
// puts both back, and one that changes nothing.
var threeWrites = Script{
	Names:  []string{"auth.json", "config.toml"},
	Logins: []map[string]string{first, {"auth.json": second["auth.json"]}, first, first},
}

// step is one step's announcement in a run to the end.
func step(index int, s hostbox.CredentialStep, name string) announcement {
	return announcement{Kind: kindStep, Index: index, Step: s, Name: name}
}

// framed is steps as a whole run announces them: ready first, done last.
func framed(steps ...announcement) []announcement {
	return append(append([]announcement{{Kind: kindReady}}, steps...), announcement{Kind: kindDone})
}

// threeWritesRun is what a writer of threeWrites announces on its way to its
// end.
var threeWritesRun = framed(
	step(1, hostbox.StepCommitted, ""),
	step(2, hostbox.StepWritten, "auth.json"),
	step(3, hostbox.StepRemoved, "config.toml"),
	step(4, hostbox.StepClosed, ""),
	step(5, hostbox.StepCommitted, ""),
	step(6, hostbox.StepWritten, "auth.json"),
	step(7, hostbox.StepWritten, "config.toml"),
	step(8, hostbox.StepClosed, ""),
	step(9, hostbox.StepCommitted, ""),
	step(10, hostbox.StepClosed, ""),
)

// EVERY STEP OF A RUN IS PLACED EXACTLY: its write, and what the steps so far
// leave of the login files and the journal, which is what a kill there must
// leave. The positive control of the judge below, and the replay it rests on:
// judged against what its own replay says, every stop of hostbox's script
// passes.
//
// Mutation: replay a removal as nothing, and step 3 leaves config.toml.
func TestEveryStepOfARunIsPlacedExactly(t *testing.T) {
	t.Parallel()
	stops, err := number(threeWrites, threeWritesRun)
	if err != nil {
		t.Fatalf("number: %v", err)
	}
	dropped := map[string]string{"auth.json": second["auth.json"]}
	mixed := map[string]string{"auth.json": second["auth.json"], "config.toml": first["config.toml"]}
	want := []struct {
		write    int
		files    map[string]string
		standing bool
	}{
		{1, first, true}, {1, mixed, true}, {1, dropped, true}, {1, dropped, false},
		{2, dropped, true}, {2, map[string]string{"auth.json": first["auth.json"]}, true}, {2, first, true},
		{2, first, false},
		{3, first, true}, {3, first, false},
	}
	if len(stops) != len(want) {
		t.Fatalf("the run placed %d stops, want %d", len(stops), len(want))
	}
	for i, stop := range stops {
		if stop.Index != i+1 || stop.Write != want[i].write || !maps.Equal(stop.files, want[i].files) ||
			stop.standing != want[i].standing {
			t.Errorf("%v placed with files %v and a journal standing %t, want write %d, %v and %t",
				stop, stop.files, stop.standing, want[i].write, want[i].files, want[i].standing)
		}
		if problems := judge(threeWrites, stop, stop.files, stop.standing,
			threeWrites.Logins[stop.Write], false); len(problems) != 0 {
			t.Errorf("a kill that left exactly what %v leaves was judged: %v", stop, problems)
		}
	}
}

// A RUN THAT DOES NOT FIT ITS SCRIPT IS REFUSED BEFORE ANY KILL, naming what
// does not fit. Each refusal is a way a writer can pass a matrix it does not
// meet: a writer that writes the login a file at a time makes two writes of
// one login, and one that writes it outside the journal makes none, which the
// same sentence refuses; and a step that does not fit the replay would place
// every kill after it wrongly.
//
// Mutation: drop any one check from number, and its case is accepted.
func TestARunThatDoesNotFitItsScriptIsRefused(t *testing.T) {
	t.Parallel()
	one := Script{Names: threeWrites.Names, Logins: []map[string]string{first, second}}
	whole := []announcement{
		step(1, hostbox.StepCommitted, ""), step(2, hostbox.StepWritten, "auth.json"),
		step(3, hostbox.StepWritten, "config.toml"), step(4, hostbox.StepClosed, ""),
	}
	stopped := step(2, hostbox.StepWritten, "auth.json")
	stopped.Stopped = true
	for name, c := range map[string]struct {
		run  []announcement
		want string
	}{
		"a login split over two writes": {framed(
			step(1, hostbox.StepCommitted, ""), step(2, hostbox.StepWritten, "auth.json"),
			step(3, hostbox.StepClosed, ""), step(4, hostbox.StepCommitted, ""),
			step(5, hostbox.StepWritten, "config.toml"), step(6, hostbox.StepClosed, ""),
		), "has split one login over two"},
		"a login written outside the journal": {framed(), "outside the journal"},
		"no ready":                            {append(whole[:4:4], announcement{Kind: kindDone}), "not ready"},
		"no done":                             {append([]announcement{{Kind: kindReady}}, whole...), "not ready"},
		"a second ready": {framed(append([]announcement{{Kind: kindReady}}, whole...)...),
			`"ready" among its steps`},
		"an index out of order": {framed(
			step(1, hostbox.StepCommitted, ""), step(3, hostbox.StepWritten, "auth.json"),
			step(4, hostbox.StepWritten, "config.toml"), step(5, hostbox.StepClosed, ""),
		), "came as step 2"},
		"an unknown step": {framed(
			step(1, hostbox.StepCommitted, ""), step(2, "renamed", "auth.json"),
			step(3, hostbox.StepClosed, ""),
		), "no step a login write reports"},
		"a stop in a run told to stop nowhere": {framed(
			step(1, hostbox.StepCommitted, ""), stopped,
			step(3, hostbox.StepWritten, "config.toml"), step(4, hostbox.StepClosed, ""),
		), "stopped in a run told to stop nowhere"},
		"a step before the first commit": {framed(
			step(1, hostbox.StepWritten, "auth.json"), step(2, hostbox.StepCommitted, ""),
			step(3, hostbox.StepClosed, ""),
		), "before the script's first commit"},
		"a step after a close": {framed(append(whole[:4:4], step(5, hostbox.StepWritten, "config.toml"))...),
			"outside a write"},
		"a run that ends inside a write": {framed(
			step(1, hostbox.StepCommitted, ""), step(2, hostbox.StepWritten, "auth.json"),
			step(3, hostbox.StepWritten, "config.toml"),
		), "ended inside write 1"},
		"a file written that the login does not hold": {framed(
			step(1, hostbox.StepCommitted, ""), step(2, hostbox.StepWritten, "auth.json"),
			step(3, hostbox.StepClosed, ""),
		), "closed a write whose steps leave"},
		"a file removed that the login holds": {framed(
			step(1, hostbox.StepCommitted, ""), step(2, hostbox.StepWritten, "auth.json"),
			step(3, hostbox.StepRemoved, "config.toml"), step(4, hostbox.StepClosed, ""),
		), "removed a file that write 1's login holds"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := number(one, c.run); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("number = %v, want a refusal saying %q", err, c.want)
			}
		})
	}
	// Each write finishes before the next begins: a script of two writes,
	// the second committed while the first's journal stands.
	two := Script{Names: threeWrites.Names, Logins: []map[string]string{first, second, first}}
	if _, err := number(two, framed(
		step(1, hostbox.StepCommitted, ""), step(2, hostbox.StepCommitted, ""),
		step(3, hostbox.StepClosed, ""), step(4, hostbox.StepClosed, ""),
	)); err == nil || !strings.Contains(err.Error(), "while write 1's journal still stood") {
		t.Errorf("number = %v, want a refusal of a commit while a journal stands", err)
	}
	// A file written that the write's login does not hold, refused at the
	// step: the first write of threeWrites drops config.toml.
	dropping := Script{Names: threeWrites.Names, Logins: threeWrites.Logins[:2]}
	if _, err := number(dropping, framed(
		step(1, hostbox.StepCommitted, ""), step(2, hostbox.StepWritten, "config.toml"),
		step(3, hostbox.StepClosed, ""),
	)); err == nil || !strings.Contains(err.Error(), "wrote a file that write 1's login does not hold") {
		t.Errorf("number = %v, want a refusal of a file the login does not hold", err)
	}
}

// A KILL THAT LEFT WHAT NO STEP LEAVES IS REPORTED, whichever way it falls
// short: half of each login read as one, a file holding neither login's
// version, a file moved ahead of its step, a journal missing inside its write
// or standing past it, and a reader that did not finish the write.
//
// Mutation: drop any one check from judge, and its case reports nothing.
func TestAKillThatLeftWhatNoStepLeavesIsReported(t *testing.T) {
	t.Parallel()
	stops, err := number(threeWrites, threeWritesRun)
	if err != nil {
		t.Fatalf("number: %v", err)
	}
	committed, writtenAuth, closed := stops[4], stops[5], stops[7]
	half := map[string]string{"auth.json": second["auth.json"], "config.toml": first["config.toml"]}
	for name, c := range map[string]struct {
		stop          Stop
		files         map[string]string
		standing      bool
		read          map[string]string
		standingAfter bool
		want          string
	}{
		"half of each login, read as one": {closed, first, false, half, false, "the next reader read"},
		"a file holding neither login's version": {closed,
			map[string]string{"auth.json": "third token", "config.toml": first["config.toml"]}, false, first,
			false, "the writer left the login files"},
		"a file moved ahead of its step": {writtenAuth, first, true, first, false,
			"the writer left the login files"},
		"no journal at the commit": {committed, committed.files, false, first, false, "a journal stands: false"},
		"no journal at a file's step": {writtenAuth, writtenAuth.files, false, first, false,
			"a journal stands: false"},
		"a journal at the close": {closed, first, true, first, false, "a journal stands: true"},
		"a journal after the reader": {writtenAuth, writtenAuth.files, true, first, true,
			"did not finish the write"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			problems := judge(threeWrites, c.stop, c.files, c.standing, c.read, c.standingAfter)
			if len(problems) != 1 || !strings.Contains(problems[0], c.want) ||
				!strings.Contains(problems[0], c.stop.String()) {
				t.Errorf("judge = %q, want one problem naming %v and saying %q", problems, c.stop, c.want)
			}
		})
	}
}

// A WRITER IS KILLED AT EVERY STEP, EACH IN A PROCESS OF ITS OWN, AND NOTHING
// IT STARTS ANNOUNCES. The package's own writer, the test binary re-executed,
// runs the whole protocol: the run to its end, then a process per step, each
// parked, killed by SIGKILL and judged. Before its setup the writer starts a
// process of its own, as a harness's command or a box's runner is started,
// which writes to descriptor 3 whatever it holds there: inherited, the
// matrix's pipe would carry a line no writer announced, and a process that
// outlived the writer would hold the pipe open.
//
// Mutation: leave the matrix's pipe open across exec in the writer, and the
// run announces its child's line before it is ready, which is refused.
func TestAWriterIsKilledAtEveryStepAndNothingItStartsAnnounces(t *testing.T) {
	t.Parallel()
	stops := KillAtEveryStep(t, ownScript, func(t *testing.T) Run {
		dir := filepath.Join(t.TempDir(), "credentials")
		cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), writerEnv+"="+dir)
		return Run{Cmd: cmd, Dir: dir, Read: func(t *testing.T) map[string]string {
			t.Helper()
			release, err := hostbox.LockCredentials(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			login, err := hostbox.ReadCredentials(dir, ownScript.Names)
			release()
			if err != nil {
				t.Fatalf("reading what the killed writer left: %v", err)
			}
			out := map[string]string{}
			for name, content := range login {
				out[name] = string(content)
			}
			return out
		}}
	})
	var got []string
	for _, stop := range stops {
		got = append(got, subtest(stop))
	}
	want := []string{"1-write1-committed", "2-write1-written-auth.json", "3-write1-written-config.toml",
		"4-write1-closed"}
	if !slices.Equal(got, want) {
		t.Errorf("the writer's steps were %q, want %q", got, want)
	}
}
