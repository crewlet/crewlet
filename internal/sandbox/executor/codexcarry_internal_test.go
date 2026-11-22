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

// codexGate runs the session gate for session in p's box, from dir, and
// returns what it named: s, k and w.
func codexGate(t *testing.T, p Paths, dir string, session Session) (s, k, w, stderr string) {
	t.Helper()
	out, errText, code := codexShell(t, dir, codexSessionGate(session, p)+`printf '%s\n%s\n%s\n' "$s" "$k" "$w"`)
	if code != 0 {
		t.Fatalf("the gate exited %d: %s", code, errText)
	}
	lines := strings.SplitN(out, "\n", 3)
	if len(lines) != 3 {
		t.Fatalf("the gate printed %q", out)
	}
	return lines[0], lines[1], strings.TrimSuffix(lines[2], "\n"), errText
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
	_, errText, code := codexShell(t, from.Home, "s="+codexThread+" packed=\n"+codexCapture(from)+
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
	s, k, w, _ := codexGate(t, to, t.TempDir(), Session{ID: codexThread, Continues: true})
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
	if _, errText, code := codexShell(t, packed.Home, "s="+codexThread+" packed=\n"+codexCapture(packed)); code != 0 {
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
		s, k, w, _ := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true, Replaces: c.replaces})
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
		s, k, w, stderr := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true})
		if s != "" || k != "no" || w == "" || !strings.Contains(stderr, "starts a new session instead") {
			t.Errorf("%s: the gate named %q continued %q why %q (%q)", name, s, k, w, stderr)
		}
	}
	if s, k, w, _ := codexGate(t, codexBox(t, nil), t.TempDir(), Session{}); s != "" || k != "no" || w != "" {
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
	if s, k, _, _ := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true}); s != codexThread || k != "yes" {
		t.Fatalf("the gate named %q continued %q, want the box's own copy continued", s, k)
	}
	found, _, _ := codexShell(t, p.Home, codexRolloutPath(p, codexThread))
	if got := codexFile(t, strings.TrimSpace(found)); !bytes.Equal(got, whole) {
		t.Errorf("the copy is %d bytes after the gate, want its %d bytes of whole lines", len(got), len(whole))
	}
}

// A CODEX RELEASE PAIR CONTINUES ONLY AT OR PAST THE FLOOR, NEVER BACKWARDS: a
// box below 0.171.0, a session written below it, and a box older than the
// release that last wrote the session are each refused with a reason; a
// version no reader can read is taken for a current one, as every harness's is.
// Only 0.171.0's resume is read, a set and not a range.
//
// Mutation: drop a case of Continues, or answer resumeRead for a range, and a
// row below fails.
func TestACodexReleasePairContinuesOnlyAtOrPastTheFloor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		box, writer string
		ok          bool
	}{
		{"0.171.0", "0.171.0", true},
		{"0.172.3", "0.171.0", true},
		{"0.170.9", "0.170.9", false},
		{"0.171.0", "0.170.0", false},
		{"0.171.0", "0.172.0", false},
		{"codex-cli dev", "0.171.0", true},
	} {
		ok, why := Codex{}.Continues(c.box, c.writer)
		if ok != c.ok || ok == (why != "") {
			t.Errorf("box %s, writer %s: continues %v (%q), want %v", c.box, c.writer, ok, why, c.ok)
		}
	}
	if !(Codex{}).resumeRead("0.171.0") || (Codex{}).resumeRead("0.172.0") || (Codex{}).resumeRead("") {
		t.Error("resumeRead answers outside the one release whose resume was read")
	}
}
