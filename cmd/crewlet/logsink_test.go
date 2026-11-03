package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// TestMain silences the suite and, more to the point, OWNS THE PROCESS SINK.
//
// Every test here calls run(), and run() is handed a writer so its output can
// be asserted on. A command that installed that writer as the process-wide
// log destination would make the global depend on whichever test configured
// it last — see [TestACommandLogsToTheProcessSinkNotItsCallersWriter].
// Installing io.Discard once, here, is what a process legitimately does.
func TestMain(m *testing.M) {
	// main() ITSELF, FIRST, for the case that watches what it does before
	// anything — the hook dispatch, which the next step would answer
	// instead (TestTheHookIsMainsFirstStatement).
	if os.Getenv(mainProbeEnv) != "" {
		runMainProbe()
	}
	// GIT'S CREDENTIAL HELPER, as `crewlet code` configures it for a git that
	// talks to a node — `!'<this binary>' code credential` — and under go test
	// this binary is the test binary. So a git a case starts, through the CLI
	// or by hand, calls it back with the production argv, and it answers as
	// main() does; it must never answer by running the suite inside a git
	// command, which would start that git again.
	if isCredentialHelperCall(os.Args[1:]) {
		runMainProbe()
	}
	// THE CODE HOST'S HOOK: an engine this package boots and pushes to
	// starts this binary as its proc-receive hook through the engine's
	// DEFAULT hook — os.Executable, which is this test binary — so the
	// production argv is the one exercised.
	if status, ok := codetest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	// THE CASES THAT RE-EXECUTE THIS BINARY: as the node they stop, by a
	// signal and by taking its broker connection away, as the process a
	// second interrupt kills, as the engine holding a store's lock
	// ([holdTheStore]), and as an operator's command run beside a node
	// ([offlineCommand]). Before the sink below, which each node probe
	// installs for itself: its log is what the parent reads the order of
	// the shutdown from.
	if os.Getenv(drainProbeEnv) != "" {
		runDrainProbe()
	}
	if os.Getenv(lostBrokerProbeEnv) != "" {
		runLostBrokerProbe()
	}
	if os.Getenv(interruptProbeEnv) != "" {
		runInterruptProbe()
	}
	if path := os.Getenv(holdStoreEnv); path != "" {
		os.Exit(runHoldStore(path))
	}
	if args := os.Getenv(offlineCommandEnv); args != "" {
		os.Exit(runOfflineCommand(args))
	}
	silenceProcessLog()
	// THE PERSON'S GIT ENVIRONMENT, GONE before any case runs — and only now,
	// after every dispatch above: the hook and the credential helper are
	// git's own children and read what git hands them, as in production.
	if err := scrubGitEnv(os.Environ, os.Unsetenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// silenceProcessLog points the process sink at nothing, which is where this
// suite keeps it between the cases that read it.
func silenceProcessLog() {
	logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
}

// keepProcessLog sends the process sink to a buffer of this test's own for the
// rest of it, and prints what the node logged if the test fails.
//
// A FAILURE A NODE EXPLAINS ONLY IN ITS LOG IS OTHERWISE UNREADABLE here: the
// suite discards the sink ([TestMain]), and a route that keeps its reason out
// of the response body (a backup's, say) answers a bare code. So a case that
// drives such a route keeps the log, and a red run carries the reason.
//
// Not for a parallel test, since it reconfigures the process-wide logger; and
// called before the engine is built, so its cleanup runs after the engine has
// stopped and the lines the stop writes are kept too.
func keepProcessLog(t *testing.T) {
	t.Helper()
	sink := &lockedBuffer{}
	logging.Configure(slog.LevelDebug, logging.FormatText, sink)
	t.Cleanup(func() {
		silenceProcessLog()
		if t.Failed() {
			t.Logf("the node's log:\n%s", sink.String())
		}
	})
}

// scrubGitEnv removes from the environment environ lists every variable the
// suite must not read from the person running it: every one whose name
// begins GIT_, and SSH_ASKPASS.
//
// The operator CLI runs the OPERATOR's git, in the operator's environment, by
// design (why cmd/ is outside gitproc's exec gate), so every git a case starts
// through the CLI inherited the environment of whoever ran `go test`, and a
// case's verdict was a fact about their shell rather than about the CLI:
// GIT_CONFIG_COUNT, KEY_n and VALUE_n and GIT_CONFIG_PARAMETERS are
// configuration outranking every file (credential.interactive=false, which a
// git from 2.46 obeys, failed the terminal-prompt case on any git newer than
// the one it was written on); GIT_DIR, GIT_INDEX_FILE and GIT_WORK_TREE are
// exported by git to its hooks, so to a `make check` a pre-commit hook runs;
// GIT_EXEC_PATH chooses the programs git runs; GIT_ASKPASS, SSH_ASKPASS and
// GIT_TERMINAL_PROMPT answer or forbid a prompt in the person's place; and
// GIT_TRACE*, GIT_SSH* and GIT_PROTOCOL change what git does and prints. One
// prefix rather than a list of the variables that bit: git reads a new GIT_
// name in most releases, and a list is a fact about the git it was written
// against. SSH_ASKPASS is the one ASKER outside it, read by the ssh git runs
// and by git itself, as its last asker before the terminal.
//
// That is git's VARIABLES, and only them. What git reads beside them — its
// configuration and attribute FILES, found through HOME and XDG_CONFIG_HOME;
// the locale; curl's proxy variables — the scrub leaves, because the suite
// needs them for everything that is not git (HOME and XDG_CACHE_HOME name the
// store's per-user cache): the operator's git a case starts is given its own
// by [hermeticGitIn], and the sign-off gate's suite pins its own the same way
// (scripts/check-signoff_test.sh).
//
// A function of the environment's two operations rather than of the process,
// so its case can plant an environment ([TestTheSuiteReadsNoGitEnvironment]).
func scrubGitEnv(environ func() []string, unsetenv func(string) error) error {
	for _, kv := range environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "GIT_") && name != "SSH_ASKPASS" {
			continue
		}
		if err := unsetenv(name); err != nil {
			return fmt.Errorf("remove %s from the suite's environment: %w", name, err)
		}
	}
	return nil
}

// gitEnvProbeEnv starts [TestTheSuiteReadsNoGitEnvironment]'s child: this
// test binary again, under a planted git environment, reporting what its
// case sees. Not a GIT_ name, so the scrub keeps it.
const gitEnvProbeEnv = "CREWLET_GIT_ENV_PROBE"

// plantedGitEnv is a git environment of the kind a person's shell holds —
// configuration on the command line, a repository, git's programs, two
// askers — every entry of which the scrub removes.
var plantedGitEnv = []string{
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=credential.interactive",
	"GIT_CONFIG_VALUE_0=false",
	"GIT_CONFIG_PARAMETERS='credential.helper'='!false'",
	"GIT_DIR=/nonexistent",
	"GIT_EXEC_PATH=/nonexistent",
	"GIT_ASKPASS=/bin/false",
	"SSH_ASKPASS=/bin/false",
}

// THE SUITE READS NO GIT VARIABLE OF THE PERSON RUNNING IT ([scrubGitEnv]).
// The other half of what git reads from them — its configuration and
// attribute files, the locale, curl's proxies — is pinned on the operator's
// git itself, and held by [TestTheOperatorsGitInTheSuiteReadsNothingOfTheMachine].
//
// Two halves. The scrub over a planted environment removes exactly the git
// variables and keeps everything else, in order — the near misses included:
// GITHUB_ACTIONS is not GIT_, and SSH_AUTH_SOCK is not the asker. And this
// suite started again with that environment planted: its case sees none of
// it, and sees the rest — which is what fails if TestMain stops applying the
// scrub, or applies it to something other than the process.
//
// Mutation: scrub only GIT_CONFIG_* → GIT_DIR is kept, both halves red.
// Mutation: drop the scrub from TestMain → the second half red.
func TestTheSuiteReadsNoGitEnvironment(t *testing.T) {
	if os.Getenv(gitEnvProbeEnv) != "" {
		// THE CHILD: what reached a case once TestMain ran. Judged by the
		// rule written out again rather than by the scrub's own, which is
		// what is under test.
		for _, kv := range os.Environ() {
			if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") || name == "SSH_ASKPASS" {
				t.Errorf("a case of the suite reads %s", kv)
			}
		}
		if os.Getenv(gitEnvKeptEnv) != "kept" {
			t.Errorf("the environment the parent planted did not reach the case: %q", os.Environ())
		}
		return
	}
	t.Parallel()

	t.Run("the scrub", func(t *testing.T) {
		kept := []string{"PATH=/usr/bin", "HOME=/home/someone", "GITHUB_ACTIONS=true", "SSH_AUTH_SOCK=/tmp/agent",
			"LANG=C.UTF-8", "EMPTY="}
		// The planted variables between the kept ones, so an order the scrub
		// disturbed shows.
		env := slices.Concat(kept[:2], plantedGitEnv[:4], kept[2:4], plantedGitEnv[4:], kept[4:])
		var removed []string
		unset := func(name string) error {
			removed = append(removed, name)
			env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") })
			return nil
		}
		if err := scrubGitEnv(func() []string { return slices.Clone(env) }, unset); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(env, kept) {
			t.Errorf("after the scrub the environment is\n%q\nwant\n%q", env, kept)
		}
		var planted []string
		for _, kv := range plantedGitEnv {
			name, _, _ := strings.Cut(kv, "=")
			planted = append(planted, name)
		}
		if !slices.Equal(removed, planted) {
			t.Errorf("the scrub removed %q, want %q", removed, planted)
		}
	})

	t.Run("the suite", func(t *testing.T) {
		t.Parallel()
		child := exec.CommandContext(t.Context(), os.Args[0],
			"-test.run=^TestTheSuiteReadsNoGitEnvironment$", "-test.count=1", "-test.v")
		child.Env = slices.Concat(os.Environ(), plantedGitEnv, []string{
			gitEnvProbeEnv + "=1",
			gitEnvKeptEnv + "=kept",
			// A race-built child otherwise sleeps a second as it exits.
			procgrouptest.GORACE + "=" + procgrouptest.StandInRaceOptions(),
		})
		out, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("the suite started under a planted git environment = %v:\n%s", err, out)
		}
		// A pattern that matched nothing passes too, having judged nothing.
		if !strings.Contains(string(out), "--- PASS: TestTheSuiteReadsNoGitEnvironment ") {
			t.Errorf("the child ran no case:\n%s", out)
		}
	})
}

// gitEnvKeptEnv is a variable the child's case must still see: the planted
// environment reached it, so what is missing was removed.
const gitEnvKeptEnv = "CREWLET_GIT_ENV_KEPT"

// unresolvedBootstrap is a Tier A document whose ${VAR} nothing answers,
// which is what makes config.LogUnresolved emit a warning during validate.
// The log line is the whole point: it is the one thing a command writes that
// does not go through the writers it was handed.
func unresolvedBootstrap(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boot.yaml")
	body := "node:\n  id: n1\nstore:\n  path: \"${NOT_SET_ANYWHERE}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A COMMAND LOGS TO THE PROCESS'S SINK, NEVER TO THE WRITER IT WAS HANDED.
//
// `run` takes stdout and stderr so a test can read what a command printed.
// It used to also install that stderr as the process-wide slog destination,
// which is a different thing entirely: log lines are the PROCESS's
// telemetry, and pointing the global at a caller's buffer makes every other
// caller's lines land there too.
//
// Asserted on where the bytes go, not with the race detector, so it fails on
// a plain `go test` as well as under -race.
func TestACommandLogsToTheProcessSinkNotItsCallersWriter(t *testing.T) {
	var sink bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &sink)
	t.Cleanup(func() {
		silenceProcessLog()
	})

	var out, errOut bytes.Buffer
	_ = run([]string{"validate", unresolvedBootstrap(t)}, &out, &errOut)

	if !strings.Contains(sink.String(), "NOT_SET_ANYWHERE") {
		t.Fatalf("the warning did not reach the process sink, so this test "+
			"is asserting nothing: sink=%q", sink.String())
	}
	if strings.Contains(errOut.String(), "NOT_SET_ANYWHERE") {
		t.Errorf("the command wrote a log line into the writer it was handed; "+
			"under `go test` that writer belongs to ONE test and every other "+
			"test's lines would land in it too: %q", errOut.String())
	}
}

// AND TWO COMMANDS AT ONCE DO NOT WRITE THE SAME BUFFER.
//
// The shape CI actually hit: 29 parallel tests in this package each call
// run() with their own bytes.Buffer, one of them writes usage() into its
// buffer directly while another's slog line is routed into the same one.
// This forces the overlap so -race adjudicates it rather than leaving it to
// how a runner happens to schedule.
func TestConcurrentCommandsDoNotShareABuffer(t *testing.T) {
	t.Parallel()
	cfg := unresolvedBootstrap(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			var out bytes.Buffer
			if i%2 == 0 {
				_ = run([]string{"help"}, &out, &out)
				return
			}
			_ = run([]string{"validate", cfg}, &out, &out)
		})
	}
	wg.Wait()
}
