package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// interruptProbeEnv makes the test binary, re-executed, run
// [runInterruptProbe] instead of its tests.
const interruptProbeEnv = "CREWLET_INTERRUPT_PROBE"

// Exit codes the probe ends with when the second interrupt did NOT kill it.
// Neither 0, 1 nor 2: a test binary exits 0 or 1 and a panic exits 2.
const (
	interruptProbeNotCancelled = 5
	interruptProbeSurvived     = 6
)

// A SECOND INTERRUPT KILLS THE PROCESS, from the first moment of a run.
//
// The first SIGINT cancels the run's context and hands both signals back to
// the operating system, so the second does what it always does. That has to
// hold during a BOOT as well as during the drain: a broker recovering a large
// store cannot be interrupted, and an operator at a terminal whose second
// press was swallowed had no way out short of SIGKILL from elsewhere.
//
// A signal's default action cannot be observed from inside the process it
// kills, so the probe runs in a CHILD — the test binary re-executed — which
// takes the run's own [interruptContext], interrupts itself once, waits for
// the cancellation and interrupts itself again. The parent asserts the child
// died of that SIGINT rather than exiting.
func TestASecondInterruptKillsTheProcess(t *testing.T) {
	t.Parallel()
	child := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^TestASecondInterruptKillsTheProcess$")
	child.Env = append(os.Environ(), interruptProbeEnv+"=1")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	err := child.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the probe ended with %v rather than a signal; its output:\n%s",
			err, output.String())
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("the second interrupt did not kill the process (%v); its output:\n%s",
			err, output.String())
	}
}

// runInterruptProbe is the child's half of
// [TestASecondInterruptKillsTheProcess]. It never returns: the second
// interrupt kills it, or it exits with a code naming what went wrong.
func runInterruptProbe() {
	ctx, stop := interruptContext()
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		fmt.Println("first interrupt:", err)
		os.Exit(interruptProbeNotCancelled)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		fmt.Println("the first interrupt did not cancel the run")
		os.Exit(interruptProbeNotCancelled)
	}

	// THE HAND-BACK RUNS JUST AFTER THE CANCELLATION, on a goroutine of its
	// own, so a press in that instant can still be swallowed. A person
	// presses again; so does this, for ten seconds — far longer than the
	// hand-back ever takes, and exactly as long as a swallowed press lasts.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
			fmt.Println("second interrupt:", err)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println("every second interrupt was swallowed")
	os.Exit(interruptProbeSurvived)
}
