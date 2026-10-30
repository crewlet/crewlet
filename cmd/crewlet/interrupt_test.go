package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"syscall"
	"testing"
	"time"
)

// interruptProbeEnv makes the test binary, re-executed, run
// [runInterruptProbe] instead of its tests; its value is the
// [interruptProbe] to run, as JSON.
const interruptProbeEnv = "CREWLET_INTERRUPT_PROBE"

// interruptProbeLaunchedEnv marks the probe's second image: the one its
// launcher exec'd with the dispositions the case asks for.
const interruptProbeLaunchedEnv = "CREWLET_INTERRUPT_PROBE_LAUNCHED"

// Exit codes the probe ends with when it could not do what the case asks.
// None of them 0, 1 or 2 — a test binary exits 0 or 1 and a panic exits 2 —
// nor [interruptedStatus], which some cases expect.
const (
	interruptProbeLaunchFailed     = 3
	interruptProbeWrongDisposition = 4
	interruptProbeNotCancelled     = 5
	interruptProbeSurvived         = 6
)

// interruptProbe is one case of [TestASecondInterruptKillsTheProcess], as the
// child reads it.
type interruptProbe struct {
	// Ignored is what the process STARTS with ignored, as a background job of
	// a non-interactive shell starts with SIGINT ignored. Every other signal
	// of the two starts at its default.
	Ignored []syscall.Signal
	// Signal is what the operator presses: once to stop the run, then once
	// more.
	Signal syscall.Signal
	// AfterFatal has the run hand the signals back with no signal behind it,
	// as a node that stops itself does, and press only once.
	AfterFatal bool
	// Hangup has the run start a shell that hangs itself up, and then hang
	// the run up: [TestARunStartedIgnoringHangupGivesItsChildrenTheDefault].
	Hangup bool
}

// hangupProbeHeld is what the probe prints when a child it started died of
// the SIGHUP it sent itself and the run survived one of its own.
const hangupProbeHeld = "the child died of its hangup and the run survived its own"

// A RUN STARTED IGNORING A HANGUP GIVES ITS CHILDREN THE DEFAULT, and goes on
// ignoring one itself. Started under nohup, the engine handed the ignore on to
// every process it started, and a non-interactive sh can neither trap nor die
// of a signal it started ignoring: a box's run script could not hear the
// hangup that exports its login, and exited nothing.
//
// Mutation: leave SIGHUP alone in interruptContext, and the child sleeps
// through its own hangup.
func TestARunStartedIgnoringHangupGivesItsChildrenTheDefault(t *testing.T) {
	t.Parallel()
	spec, err := json.Marshal(interruptProbe{Ignored: []syscall.Signal{syscall.SIGHUP},
		Hangup: true})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^TestARunStartedIgnoringHangupGivesItsChildrenTheDefault$")
	child.Env = append(os.Environ(), interruptProbeEnv+"="+string(spec))
	output, err := child.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(hangupProbeHeld)) {
		t.Fatalf("the probe ended with %v; its output:\n%s", err, output)
	}
}

// runHangupProbe is the child's half of
// [TestARunStartedIgnoringHangupGivesItsChildrenTheDefault]: it never returns.
func runHangupProbe() {
	if !signal.Ignored(syscall.SIGHUP) {
		fmt.Println("the probe started with SIGHUP at its default, want it ignored")
		os.Exit(interruptProbeWrongDisposition)
	}
	_, held := interruptContext()
	defer held.end()
	err := exec.Command("sh", "-c", "kill -HUP $$; sleep 5").Run()
	var exit *exec.ExitError
	status, ok := syscall.WaitStatus(0), false
	if errors.As(err, &exit) {
		status, ok = exit.Sys().(syscall.WaitStatus)
	}
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGHUP {
		fmt.Printf("the child ended with %v, want it killed by its own SIGHUP\n", err)
		os.Exit(interruptProbeSurvived)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		fmt.Println("hanging up the run:", err)
		os.Exit(interruptProbeLaunchFailed)
	}
	time.Sleep(200 * time.Millisecond)
	fmt.Println(hangupProbeHeld)
	os.Exit(0)
}

// A SECOND INTERRUPT KILLS THE PROCESS, from the first moment of a run and
// whatever the process inherited.
//
// The first signal cancels the run's context and moves both signals to the
// second tier, so the next one ends the process. That has to hold during a
// BOOT as well as during the drain: a broker recovering a large store cannot
// be interrupted, and an operator at a terminal whose second press was
// swallowed had no way out short of SIGKILL from elsewhere. And it has to
// hold for a process started with SIGINT IGNORED — a background job of a
// non-interactive shell, or anything a parent that ignored it starts: the
// first SIGINT drained it and the second, the one the drain line promises
// "exits at once", was ignored, because handing SIGINT back restored the
// inherited SIG_IGN. Such a process cannot die BY the signal (nothing a Go
// program can call sets SIGINT to its default once it inherited it ignored),
// so it exits with [interruptedStatus], which a shell reports exactly as it
// reports the kill.
//
// A signal's default action cannot be observed from inside the process it
// kills, so each case runs in a CHILD — the test binary re-executed — which
// takes the run's own [interruptContext], interrupts itself once, waits for
// the cancellation and interrupts itself ONCE more: a hand-back that leaves a
// window in which a press is swallowed fails here rather than being pressed
// through.
//
// And the child's dispositions are the CASE'S, never this binary's: the child
// starts as a launcher that sets them and execs itself, since execve keeps an
// ignored signal ignored and resets a caught one to its default. The child
// used to inherit whatever this binary was started with, so started with
// SIGINT ignored — `go test &` from a script — this case failed on a healthy
// build, and setting a disposition here around the child's start instead
// would race every parallel case that forks.
func TestASecondInterruptKillsTheProcess(t *testing.T) {
	t.Parallel()
	signalled := func(s syscall.Signal) func(syscall.WaitStatus) bool {
		return func(w syscall.WaitStatus) bool { return w.Signaled() && w.Signal() == s }
	}
	exitedInterrupted := func(w syscall.WaitStatus) bool {
		return w.Exited() && w.ExitStatus() == interruptedStatus
	}
	for _, c := range []struct {
		name  string
		probe interruptProbe
		want  string
		ended func(syscall.WaitStatus) bool
	}{{
		name:  "a second SIGINT, SIGINT at its default",
		probe: interruptProbe{Signal: syscall.SIGINT},
		want:  "killed by SIGINT",
		ended: signalled(syscall.SIGINT),
	}, {
		name:  "a second SIGINT, SIGINT inherited ignored",
		probe: interruptProbe{Ignored: []syscall.Signal{syscall.SIGINT}, Signal: syscall.SIGINT},
		want:  fmt.Sprintf("exited %d", interruptedStatus),
		ended: exitedInterrupted,
	}, {
		// The runtime installs its own handler for SIGTERM whatever the
		// process inherited, so a SIGTERM ignored at the start is still one
		// the hand-back gives its default — with SIGINT, ignored beside it,
		// kept on the hold's own handler.
		name: "a second SIGTERM, both inherited ignored",
		probe: interruptProbe{Ignored: []syscall.Signal{syscall.SIGINT, syscall.SIGTERM},
			Signal: syscall.SIGTERM},
		want:  "killed by SIGTERM",
		ended: signalled(syscall.SIGTERM),
	}, {
		name:  "a SIGINT after a node stopped itself, SIGINT at its default",
		probe: interruptProbe{Signal: syscall.SIGINT, AfterFatal: true},
		want:  "killed by SIGINT",
		ended: signalled(syscall.SIGINT),
	}, {
		name: "a SIGINT after a node stopped itself, SIGINT inherited ignored",
		probe: interruptProbe{Ignored: []syscall.Signal{syscall.SIGINT},
			Signal: syscall.SIGINT, AfterFatal: true},
		want:  fmt.Sprintf("exited %d", interruptedStatus),
		ended: exitedInterrupted,
	}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			spec, err := json.Marshal(c.probe)
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(t.Context(), os.Args[0],
				"-test.run=^TestASecondInterruptKillsTheProcess$")
			child.Env = append(os.Environ(), interruptProbeEnv+"="+string(spec))
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			err = child.Run()

			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the probe ended with %v, want it %s; its output:\n%s",
					err, c.want, output.String())
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !c.ended(status) {
				t.Fatalf("the probe ended with %v, want it %s; its output:\n%s",
					err, c.want, output.String())
			}
		})
	}
}

// runInterruptProbe is the child's half of
// [TestASecondInterruptKillsTheProcess]. It never returns: the case's
// interrupt ends it, or it exits with a code naming what went wrong.
func runInterruptProbe() {
	var probe interruptProbe
	if err := json.Unmarshal([]byte(os.Getenv(interruptProbeEnv)), &probe); err != nil {
		fmt.Println("reading the probe:", err)
		os.Exit(interruptProbeLaunchFailed)
	}
	if os.Getenv(interruptProbeLaunchedEnv) == "" {
		launchInterruptProbe(probe)
	}
	if probe.Hangup {
		runHangupProbe()
	}
	// THE DISPOSITION THE CASE ASKED FOR, before the hold's Notify clears
	// what signal.Ignored reports. SIGINT's only: the runtime replaces an
	// inherited SIGTERM ignore with its own handler before main, which is
	// what one of the cases rests on.
	if want := slices.Contains(probe.Ignored, syscall.SIGINT); signal.Ignored(syscall.SIGINT) != want {
		fmt.Printf("the probe started with SIGINT ignored %t, want %t\n", !want, want)
		os.Exit(interruptProbeWrongDisposition)
	}

	ctx, held := interruptContext()
	defer held.end()

	if probe.AfterFatal {
		held.handBack()
	} else if err := syscall.Kill(syscall.Getpid(), probe.Signal); err != nil {
		fmt.Println("first interrupt:", err)
		os.Exit(interruptProbeNotCancelled)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		fmt.Println("the first interrupt did not cancel the run")
		os.Exit(interruptProbeNotCancelled)
	}

	// ONCE. The hand-back is done before the context is cancelled, so the
	// next press reaches the second tier with no window to land in; a person
	// pressing again would hide one, and so would this pressing in a loop.
	if err := syscall.Kill(syscall.Getpid(), probe.Signal); err != nil {
		fmt.Println("second interrupt:", err)
		os.Exit(interruptProbeSurvived)
	}
	time.Sleep(10 * time.Second)
	fmt.Printf("the interrupt after the run was cancelled was swallowed (%v)\n", probe.Signal)
	os.Exit(interruptProbeSurvived)
}

// launchInterruptProbe sets the dispositions the case starts with and execs
// this binary again with them: execve keeps an ignored signal ignored and
// resets one this process catches to its default, so the image it starts
// holds exactly the case's, whatever this one inherited.
func launchInterruptProbe(probe interruptProbe) {
	for _, s := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if slices.Contains(probe.Ignored, s) {
			signal.Ignore(s)
		} else {
			signal.Notify(make(chan os.Signal, 1), s)
		}
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Println("finding the probe binary:", err)
		os.Exit(interruptProbeLaunchFailed)
	}
	err = syscall.Exec(self, os.Args, append(os.Environ(), interruptProbeLaunchedEnv+"=1"))
	fmt.Println("exec of the probe:", err)
	os.Exit(interruptProbeLaunchFailed)
}

// A PRESS THAT LANDED BEFORE THE HAND-BACK IS RAISED AGAIN after it, so the
// default action answers it. The hand-back runs on the hold's goroutine after
// it read the first signal, and a second that reached the channel in between
// would sit there unread once the channel stopped — swallowed by a handler
// that no longer listened. signal.Stop guarantees a signal either reaches the
// channel before it returns or takes the default action, so draining the
// channel after it loses no press.
//
// SIGUSR1 rather than either of the run's signals, and a recorded raise
// rather than a real one, so the case runs inside this binary: a SIGUSR1 no
// channel wants is one the runtime drops.
func TestAPressThatLandsBeforeTheHandBackIsRaisedAgain(t *testing.T) {
	held := make(chan os.Signal, 1)
	signal.Notify(held, syscall.SIGUSR1)
	t.Cleanup(func() { signal.Stop(held) })
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(held) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the signal never reached the channel")
		}
		time.Sleep(time.Millisecond)
	}

	var raised []os.Signal
	release(held, func(s os.Signal) { raised = append(raised, s) })
	if !slices.Equal(raised, []os.Signal{syscall.SIGUSR1}) {
		t.Fatalf("release raised %v, want the press it held raised again", raised)
	}

	// And a channel holding nothing raises nothing: a hand-back with no
	// second press must not invent one.
	empty := make(chan os.Signal, 1)
	signal.Notify(empty, syscall.SIGUSR1)
	t.Cleanup(func() { signal.Stop(empty) })
	raised = nil
	release(empty, func(s os.Signal) { raised = append(raised, s) })
	if len(raised) != 0 {
		t.Fatalf("a release with nothing held raised %v", raised)
	}
}
