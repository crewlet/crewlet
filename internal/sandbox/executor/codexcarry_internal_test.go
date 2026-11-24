package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/shellword"
)

// codexThread is the thread of the rollout fixture ([codexRolloutFixture]).
const codexThread = "01a1ff62-fbda-7aa2-af57-6c0abeb8e50b"

// codexBox is a box home with Codex's rollout of codexThread written where
// Codex dates it, holding rollout.
func codexBox(t *testing.T, rollout []byte) Paths {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	dir := filepath.Join(p.CodexSessions(), "2026", "11", "21")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.WorkDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, config := range map[string]string{p.CodexConfig(): "continued\n", p.CodexConfigFresh(): "fresh\n"} {
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A STAND-IN NOTE HELPER whose codex-renew renews the rollout as it is,
	// recording its argv beside itself: the gate's renew is its own case.
	if err := os.WriteFile(p.CodexNoteHelper(), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.argv\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rollout != nil {
		name := filepath.Join(dir, "rollout-2026-11-21T18-33-59-"+codexThread+".jsonl")
		if err := os.WriteFile(name, rollout, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// codexShell runs script with sh in dir, and returns its stdout, stderr and
// exit status.
func codexShell(t *testing.T, dir, script string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script) //nolint:gosec // the script under test
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return stdout.String(), stderr.String(), exit.ExitCode()
	case err != nil:
		t.Fatalf("run the script: %v", err)
	}
	return stdout.String(), stderr.String(), 0
}

// codexGate runs the session gate for session in p's box, from dir, wired
// for a fresh session where rewired, on a box whose Codex is the supported
// release, and returns what it named: sid, k and w.
func codexGate(t *testing.T, p Paths, dir string, session Session, rewired bool) (s, k, w, stderr string) {
	t.Helper()
	return codexGateOn(t, p, dir, session, rewired, "codex-cli "+codexCarryRelease)
}

// codexGateOn is [codexGate] on a box whose `codex --version` printed version.
func codexGateOn(t *testing.T, p Paths, dir string, session Session, rewired bool,
	version string) (s, k, w, stderr string) {
	t.Helper()
	out, errText, code := codexShell(t, dir, codexPrinted(version)+
		codexSessionGate(session, p, rewired)+`printf '%s\n%s\n%s\n' "$sid" "$k" "$w"`)
	if code != 0 {
		t.Fatalf("the gate exited %d: %s", code, errText)
	}
	lines := strings.SplitN(out, "\n", 3)
	if len(lines) != 3 {
		t.Fatalf("the gate printed %q", out)
	}
	return lines[0], lines[1], strings.TrimSuffix(lines[2], "\n"), errText
}

// codexPrinted is the script's reading of a box whose `codex --version`
// printed version ([codexReleaseParse]).
func codexPrinted(version string) string {
	return "v=" + shellword.Quote(version) + "\n" + codexReleaseParse()
}

// codexMembers is every member of a gzipped tar and its content, by name.
func codexMembers(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("the archive is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading the archive's members: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		out[h.Name] = string(body)
	}
}

// codexFile is path's content, failing the test where it cannot be read.
func codexFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A CODEX SESSION PACKED IN ONE BOX IS RESUMED FROM A FRESH CODEX_HOME IN
// ANOTHER CWD: the run's capture packs the rollout's whole lines as the one
// member, its digest kept; the gate of a box that holds nothing restores it
// under the name a resume looks for, at the sessions root, records its digest
// and names the thread to resume. A resume needs nothing else and no matching
// cwd (measured on 0.171.0), so the restored file alone is the session.
//
// Mutation: pack the torn tail, pack under another member name, restore under
// a name without the thread id or outside sessions/, or skip the digest, and a
// case below fails.
func TestACodexSessionPackedInOneBoxIsResumedFromAFreshCodexHome(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	from := codexBox(t, append(append([]byte(nil), whole...), `{"type":"response_item","pay`...))
	_, errText, code := codexShell(t, from.Home, "sid="+codexThread+" packed=\n"+codexCapture(from)+
		`[ -n "$packed" ] || exit 9`)
	if code != 0 {
		t.Fatalf("the capture exited %d: %s", code, errText)
	}
	archive := codexFile(t, from.SessionCapture())
	members := codexMembers(t, archive)
	if len(members) != 1 || members[codexArchiveMember(codexThread)] != string(whole) {
		t.Fatalf("the capture holds %d members, want the rollout's whole lines as %s", len(members),
			codexArchiveMember(codexThread))
	}
	if !strings.Contains(string(codexFile(t, from.SessionDigests())), codexThread+" ") {
		t.Error("the capture kept no digest of its archive")
	}

	to := codexBox(t, nil)
	if err := os.WriteFile(to.SessionCarried(), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	s, k, w, _ := codexGate(t, to, t.TempDir(), Session{ID: codexThread, Continues: true}, false)
	if s != codexThread || k != "yes" || w != "" {
		t.Fatalf("the gate named %q continued %q why %q, want the carried thread continued", s, k, w)
	}
	if got := codexFile(t, codexRestoredRollout(to, codexThread)); !bytes.Equal(got, whole) {
		t.Error("the restored rollout is not the packed one")
	}
	found, _, _ := codexShell(t, t.TempDir(), codexRolloutPath(to, codexThread))
	if strings.TrimSpace(found) != codexRestoredRollout(to, codexThread) {
		t.Errorf("the locator found %q, not the restored rollout", found)
	}
	if _, err := os.Stat(to.SessionCarried()); !errors.Is(err, os.ErrNotExist) {
		t.Error("the carried archive is left in the box")
	}
	if !strings.Contains(string(codexFile(t, to.SessionDigests())), codexThread+" ") {
		t.Error("the restore recorded no digest of what it restored")
	}
}

// A CODEX SAVE IS THE ROLLOUT'S WHOLE LINES AS THEY STAND: Codex appends, so a
// pack beside a working Codex copies a prefix of the session and never the
// line being written, and a later save holds what the earlier one did and
// more. A box with no rollout of the thread, or none with a whole line, has
// nothing to pack, which is no failure.
//
// Mutation: copy the file whole, or exit packFailed where there is nothing,
// and a case below fails.
func TestACodexSaveIsTheRolloutsWholeLinesAsTheyStand(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	p := codexBox(t, append(append([]byte(nil), whole...), `{"type":"event_msg","pa`...))
	save, err := Codex{}.PackSave(p, sessionRecord{id: codexThread}, p.SessionSaveStaging("t", "1"),
		p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, errText, code := codexShell(t, p.Home, save); code != 0 {
		t.Fatalf("the save exited %d: %s", code, errText)
	}
	members := codexMembers(t, codexFile(t, p.SessionSave("t", "1")))
	if members[codexArchiveMember(codexThread)] != string(whole) {
		t.Error("the save is not the rollout's whole lines")
	}

	for name, rollout := range map[string][]byte{"none": nil, "no whole line": []byte(`{"type":"sess`)} {
		empty := codexBox(t, rollout)
		pack := Codex{}.Pack(empty, codexThread, empty.SessionPackStaging("p"), empty.SessionPackHeld("p"), true)
		if _, errText, code := codexShell(t, empty.Home, pack); code != packNothing {
			t.Errorf("%s: the pack exited %d (%s), want packNothing", name, code, errText)
		}
	}
	if _, err := (Codex{}).PackSave(p, sessionRecord{}, "x", "y"); err == nil {
		t.Error("a save of a record naming no session was made")
	}
}

// A BOX'S OWN CODEX COPY IS KEPT ONLY WHERE IT HOLDS THE CARRIED ARCHIVE: a
// copy that went on past the archive (its digest among the box's) is resumed
// as it stands; one that does not hold it is replaced, every other rollout of
// the thread removed so the resume finds the restored one; a copy Codex was
// cut off in is replaced whatever it holds.
//
// Mutation: restore over a copy that holds the archive, leave the dated copy
// beside the restored one, or ignore Replaces, and a case below fails.
func TestABoxsOwnCodexCopyIsKeptOnlyWhereItHoldsTheCarriedArchive(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	packed := codexBox(t, whole)
	if _, errText, code := codexShell(t, packed.Home, "sid="+codexThread+" packed=\n"+codexCapture(packed)); code != 0 {
		t.Fatalf("the capture exited %d: %s", code, errText)
	}
	archive := codexFile(t, packed.SessionCapture())
	digests := codexFile(t, packed.SessionDigests())
	wentOn := append(append([]byte(nil), whole...), `{"type":"event_msg","payload":{"type":"task_started"}}`+"\n"...)
	dated := func(p Paths) string {
		return filepath.Join(p.CodexSessions(), "2026", "11", "21", "rollout-2026-11-21T18-33-59-"+codexThread+".jsonl")
	}
	for _, c := range []struct {
		name     string
		held     bool
		replaces bool
		want     []byte
		restored bool
	}{
		{name: "held", held: true, want: wentOn},
		{name: "not held", want: whole, restored: true},
		{name: "cut off", held: true, replaces: true, want: whole, restored: true},
	} {
		p := codexBox(t, wentOn)
		if c.held {
			if err := os.WriteFile(p.SessionDigests(), digests, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(p.SessionCarried(), archive, 0o600); err != nil {
			t.Fatal(err)
		}
		s, k, w, _ := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true, Replaces: c.replaces}, false)
		if s != codexThread || k != "yes" || w != "" {
			t.Fatalf("%s: the gate named %q continued %q why %q", c.name, s, k, w)
		}
		found, _, _ := codexShell(t, p.Home, codexRolloutPath(p, codexThread))
		if got := codexFile(t, strings.TrimSpace(found)); !bytes.Equal(got, c.want) {
			t.Errorf("%s: the resume finds %d bytes, want %d", c.name, len(got), len(c.want))
		}
		if _, err := os.Stat(dated(p)); c.restored != errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the dated copy is there: %v, want it removed: %v", c.name, err == nil, c.restored)
		}
	}
}

// A CARRIED CODEX SESSION THE BOX CANNOT RESTORE STARTS A NEW ONE, saying why:
// never a resume of a thread the box does not hold, which Codex refuses, nor
// one the run's caller believes is the old one. The new thread is the one
// Codex mints, so the gate names none.
//
// Mutation: continue where the archive did not restore, or name the asked id
// for the new thread, and the run goes on a session it does not have.
func TestACarriedCodexSessionTheBoxCannotRestoreStartsANewOne(t *testing.T) {
	t.Parallel()
	for name, carried := range map[string][]byte{"none carried": nil, "no tar": []byte("not a tar\n")} {
		p := codexBox(t, nil)
		if carried != nil {
			if err := os.WriteFile(p.SessionCarried(), carried, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		s, k, w, stderr := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true}, false)
		if s != "" || k != "no" || w == "" || !strings.Contains(stderr, "starts a new session instead") {
			t.Errorf("%s: the gate named %q continued %q why %q (%q)", name, s, k, w, stderr)
		}
	}
	if s, k, w, _ := codexGate(t, codexBox(t, nil), t.TempDir(), Session{}, false); s != "" || k != "no" || w != "" {
		t.Errorf("a fresh run's gate named %q continued %q why %q", s, k, w)
	}
}

// A CODEX ROLLOUT CUT OFF MID-LINE IS CUT BACK BEFORE IT IS RESUMED: Codex
// appends in place, so the first line the resume writes would join the torn
// one and be lost with it.
//
// Mutation: drop the cut, and the box's copy keeps its torn tail.
func TestACodexRolloutCutOffMidLineIsCutBackBeforeItIsResumed(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	p := codexBox(t, append(append([]byte(nil), whole...), `{"type":"response_item"`...))
	if s, k, _, _ := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true}, false); s != codexThread || k != "yes" {
		t.Fatalf("the gate named %q continued %q, want the box's own copy continued", s, k)
	}
	found, _, _ := codexShell(t, p.Home, codexRolloutPath(p, codexThread))
	if got := codexFile(t, strings.TrimSpace(found)); !bytes.Equal(got, whole) {
		t.Errorf("the copy is %d bytes after the gate, want its %d bytes of whole lines", len(got), len(whole))
	}
}

// A CODEX SESSION IS RENEWED TO THE RUN'S STANDING INSTRUCTIONS BEFORE IT IS
// RESUMED, by the note helper's codex-renew on the rollout the resume takes
// up and the run's renew config; a box with no helper, or one whose helper
// could not renew the rollout, starts a session of its own, saying why, on
// the configuration wired for one. Resumed as it stands, the session would
// hand its model an earlier launch's standing instructions as current.
//
// Mutation: drop the renew, resume where it failed, or renew another file,
// and a row goes red.
func TestACodexSessionIsRenewedToTheRunsInstructionsBeforeItIsResumed(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		helper string // "" for none
		k, w   string
	}{
		"renewed":           {"#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.argv\"\n", "yes", ""},
		"no helper":         {"", "no", codexRenewNoHelper},
		"the renew refused": {"#!/bin/sh\nexit 1\n", "no", codexRenewFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := codexBox(t, codexRolloutFixture(t))
			if err := os.Remove(p.CodexNoteHelper()); err != nil {
				t.Fatal(err)
			}
			if c.helper != "" {
				if err := os.WriteFile(p.CodexNoteHelper(), []byte(c.helper), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, k, w, _ := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true, Fresh: "fresh"}, true)
			if k != c.k || w != c.w {
				t.Fatalf("the gate continued %q saying %q, want %q saying %q", k, w, c.k, c.w)
			}
			if want := map[string]string{"yes": "continued\n", "no": "fresh\n"}[k]; string(codexFile(t, p.CodexLiveConfig())) != want {
				t.Errorf("the run starts on %q, want %q", codexFile(t, p.CodexLiveConfig()), want)
			}
			if k != "yes" {
				return
			}
			found, _, _ := codexShell(t, p.Home, codexRolloutPath(p, codexThread))
			argv := strings.Split(strings.TrimSpace(string(codexFile(t, p.CodexNoteHelper()+".argv"))), "\n")
			if want := []string{"-adapter", "codex-renew", "-config", p.CodexRenew(), "-rollout",
				strings.TrimSpace(found)}; strings.Join(argv, " ") != strings.Join(want, " ") {
				t.Errorf("the helper ran %q, want %q", argv, want)
			}
		})
	}
}

// A CODEX SESSION CONTINUES ONLY ON THE ONE SUPPORTED RELEASE: box and writer
// both exactly 0.171.0. Any other, older or newer, or a writer that named no
// release, starts fresh with a reason naming the release found; no ordering
// of releases is read, and a box that named no release is refused.
// resumeRead answers for that one release alone.
//
// Mutation: let a newer release continue, compare releases by order, or read
// an unnamed writer as the supported one, and a row below fails.
func TestACodexSessionContinuesOnlyOnTheOneSupportedRelease(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		box, writer string
		ok          bool
		names       string
	}{
		{"0.171.0", "0.171.0", true, ""},
		{"", "0.171.0", false, "named no release"},
		{"0.172.0", "0.171.0", false, "Codex 0.172.0"},
		{"0.170.9", "0.171.0", false, "Codex 0.170.9"},
		{"0.171.0", "0.170.0", false, "Codex 0.170.0"},
		{"0.171.0", "0.172.3", false, "Codex 0.172.3"},
		{"0.171.0", "", false, "named no release"},
	} {
		ok, why := Codex{}.Continues(c.box, c.writer)
		if ok != c.ok || ok == (why != "") || !strings.Contains(why, c.names) {
			t.Errorf("box %q, writer %q: continues %v (%q), want %v naming %q", c.box, c.writer, ok, why, c.ok, c.names)
		}
	}
	if !(Codex{}).resumeRead("0.171.0") || (Codex{}).resumeRead("0.172.0") || (Codex{}).resumeRead("0.170.0") ||
		(Codex{}).resumeRead("") {
		t.Error("resumeRead answers for a release other than the one supported")
	}
}

// A CODEX RUN STARTS ON THE CONFIGURATION OF THE SESSION IT STARTS: a run that
// continues its thread on the continued one's, the fresh wiring removed; one
// that falls back on the fresh wiring, where it was handed one, and the brief
// written for a new session; a fresh run on its one configuration. A run whose
// configuration cannot be installed never starts.
//
// Mutation: install the continued configuration on a fall-back, leave the
// other wiring beside it, skip the fresh brief, append to the configuration
// a box already holds rather than replace it, or go on past a failed move,
// and a row below fails.
func TestACodexRunStartsOnTheConfigurationOfTheSessionItStarts(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	for _, c := range []struct {
		name     string
		rollout  []byte
		session  Session
		rewired  bool
		live     string
		k, stdin string
	}{
		{"continued", whole, Session{ID: codexThread, Continues: true}, true, "continued\n", "yes", ""},
		{"fallen back, rewired", nil, Session{ID: codexThread, Continues: true}, true, "fresh\n", "no", "the fresh brief"},
		{"fallen back", nil, Session{ID: codexThread, Continues: true}, false, "continued\n", "no", "the fresh brief"},
		{"fresh", nil, Session{}, false, "continued\n", "no", ""},
	} {
		p := codexBox(t, c.rollout)
		if err := os.WriteFile(p.FreshBrief(), []byte("the fresh brief"), 0o600); err != nil {
			t.Fatal(err)
		}
		// A CONFIGURATION ALREADY THERE, which the install replaces whole.
		if err := os.MkdirAll(p.CodexHome(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.CodexLiveConfig(), []byte("stale\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, errText, code := codexShell(t, p.Home, codexPrinted("codex-cli "+codexCarryRelease)+
			codexSessionGate(c.session, p, c.rewired)+`printf '%s\n' "$k"; if [ "$k" = no ]; then cat; fi`)
		if code != 0 {
			t.Fatalf("%s: the gate exited %d: %s", c.name, code, errText)
		}
		k, stdin, _ := strings.Cut(out, "\n")
		if k != c.k || (c.session.Continues && stdin != c.stdin) {
			t.Errorf("%s: continued %q reading %q, want %q reading %q", c.name, k, stdin, c.k, c.stdin)
		}
		if got := string(codexFile(t, p.CodexLiveConfig())); got != c.live {
			t.Errorf("%s: Codex reads %q, want %q", c.name, got, c.live)
		}
		for _, left := range []string{p.CodexConfig(), p.CodexConfigFresh()} {
			if _, err := os.Stat(left); c.rewired && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: %s is left beside the installed configuration", c.name, filepath.Base(left))
			}
		}
	}
	p := codexBox(t, nil)
	if err := os.Remove(p.CodexConfig()); err != nil {
		t.Fatal(err)
	}
	if _, errText, code := codexShell(t, p.Home, codexSessionGate(Session{}, p, false)+"echo started"); code != 1 ||
		!strings.Contains(errText, "does not start") {
		t.Errorf("a run with no configuration exited %d (%s), want 1 and why", code, errText)
	}
}

// A CODEX BOX RESTORES A CARRIED SESSION ONLY ON THE ONE SUPPORTED RELEASE, as
// the engine's check judges it: a box whose `codex --version` holds no
// release, or one other than 0.171.0, older or newer, restores nothing and
// starts a new session, saying why in the engine's own words.
//
// Mutation: drop either check, read the release otherwise than releaseOf
// does (a pre-release as its numbers), or word the reason otherwise than
// Codex.Continues, and a row fails.
func TestACodexBoxRestoresACarriedSessionOnlyOnTheOneSupportedRelease(t *testing.T) {
	t.Parallel()
	packed := codexBox(t, codexRolloutFixture(t))
	if _, errText, code := codexShell(t, packed.Home, "sid="+codexThread+" packed=\n"+codexCapture(packed)); code != 0 {
		t.Fatalf("the capture exited %d: %s", code, errText)
	}
	archive := codexFile(t, packed.SessionCapture())
	_, engine := Codex{}.Continues("0.172.0", codexCarryRelease)
	for _, c := range []struct {
		version, k, why string
	}{
		{"codex-cli 0.171.0", "yes", ""},
		{"codex-cli 0.172.0", "no", engine},
		{"codex-cli 0.170.9", "no", "this box runs Codex 0.170.9, and only Codex 0.171.0"},
		{"codex-cli dev", "no", codexNoRelease},
		{"codex-cli 0.171.0-alpha.1", "no", codexNoRelease},
		{"", "no", codexNoRelease},
	} {
		p := codexBox(t, nil)
		if err := os.WriteFile(p.SessionCarried(), archive, 0o600); err != nil {
			t.Fatal(err)
		}
		_, k, w, _ := codexGateOn(t, p, p.Home, Session{ID: codexThread, Continues: true}, false, c.version)
		if k != c.k || (c.why == "" && w != "") || !strings.Contains(w, c.why) {
			t.Errorf("%q: continued %q because %q, want %q because %q", c.version, k, w, c.k, c.why)
		}
		if _, err := os.Stat(codexRestoredRollout(p, codexThread)); (c.k == "yes") != (err == nil) {
			t.Errorf("%q: restored %v, want %v", c.version, err == nil, c.k == "yes")
		}
	}
}

// A CODEX BOX CONTINUES EVEN ITS OWN COPY ONLY ON THE ONE SUPPORTED RELEASE:
// a box that holds the session's rollout, with nothing carried, resumes it
// only where its Codex is exactly 0.171.0; a newer, older, pre-release or
// unreadable one starts a new session, saying why in the engine's own words
// ([Codex.continuesInPlace]), and leaves the copy as it was.
//
// Mutation: drop the release check on the path where the box holds its copy,
// or read a pre-release as its numbers, and a row fails.
func TestACodexBoxContinuesItsOwnCopyOnlyOnTheOneSupportedRelease(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	_, newer := Codex{}.continuesInPlace("0.172.0")
	for _, c := range []struct {
		version, k, why string
	}{
		{"codex-cli 0.171.0", "yes", ""},
		{"codex-cli 0.172.0", "no", newer},
		{"codex-cli 0.170.9", "no", "this box runs Codex 0.170.9, and only Codex 0.171.0"},
		{"codex-cli 0.171.0-alpha.2", "no", codexNoRelease},
		{"codex-cli dev", "no", codexNoRelease},
	} {
		p := codexBox(t, whole)
		s, k, w, stderr := codexGateOn(t, p, p.Home, Session{ID: codexThread, Continues: true}, false, c.version)
		if k != c.k || (c.why == "" && w != "") || !strings.Contains(w, c.why) {
			t.Errorf("%q: continued %q because %q, want %q because %q", c.version, k, w, c.k, c.why)
		}
		if c.k == "no" && (s != "" || !strings.Contains(stderr, "starts a new session instead")) {
			t.Errorf("%q: the gate names %q and says %q, want a new session and why", c.version, s, stderr)
		}
		found, _, _ := codexShell(t, p.Home, codexRolloutPath(p, codexThread))
		if got := codexFile(t, strings.TrimSpace(found)); !bytes.Equal(got, whole) {
			t.Errorf("%q: the box's copy is %d bytes after the gate, want its %d untouched", c.version, len(got), len(whole))
		}
	}
}

// A CODEX RELEASE IS READ AS A BARE MAJOR.MINOR.PATCH ALONE, by the run's
// script and by releaseOf alike: the first word starting with a digit must be
// one, so a pre-release, garbage or nothing names no release, which the script
// refuses at start, and words around one are passed over. A release below the
// floor is refused, one at or past it starts.
//
// Mutation: read the last run of digits and dots, take a pre-release for its
// numbers, accept three parts that are not all digits, start on an unreadable
// release, or let releaseOf and the script disagree, and a row fails.
func TestACodexReleaseIsReadAsABareMajorMinorPatch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		printed, release string
		starts           bool
	}{
		{"codex-cli 0.171.0", "0.171.0", true},
		{"codex-cli 0.172.3\n", "0.172.3", true},
		{"codex-cli 0.171.0 (extra words)", "0.171.0", true},
		{"codex-cli 0.171.0.", "0.171.0", true},
		{"codex-cli 0.170.9", "0.170.9", false},
		{"codex-cli 0.150.0-alpha.1", "", false},
		{"codex-cli 0.171.0-alpha.1", "", false},
		{"codex-cli 0.172.0-rc.2", "", false},
		{"codex-cli 0.172.0-rc", "", false},
		{"codex-cli 0.172.0+build", "", false},
		{"codex-cli 0.172.x", "", false},
		{"codex-cli 0.171.0+build.7", "", false},
		{"codex-cli 0.171", "", false},
		{"codex-cli 0.171.0.1", "", false},
		{"codex-cli 0.171.0..", "", false},
		{"codex-cli dev", "", false},
		{"Usage: codex [OPTIONS] *", "", false},
		{"", "", false},
	} {
		if got := releaseOf(c.printed); got != c.release {
			t.Errorf("releaseOf(%q) = %q, want %q", c.printed, got, c.release)
		}
		out, errText, code := codexShell(t, t.TempDir(), codexPrinted(c.printed)+codexReleaseRefusal()+`printf '%s' "$n"`)
		if starts := code == 0; starts != c.starts || (c.starts && out != c.release) {
			t.Errorf("%q: the script exited %d reading %q (%s), want it starting %v on %q", c.printed, code, out,
				errText, c.starts, c.release)
		}
		if !c.starts && !strings.Contains(errText, codexFloor()+" or later") {
			t.Errorf("%q: the refusal %q names no floor", c.printed, errText)
		}
	}
}
