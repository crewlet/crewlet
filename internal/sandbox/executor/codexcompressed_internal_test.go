package executor

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// codexMaterializer is a stand-in note helper whose codex-materialize writes
// a thread's `.zst` rollouts plain by renaming them (the box holds plain text
// under the compressed name; the helper's own decode is its own case), a
// plain file beside one winning, and whose other adapters succeed; each argv
// goes on a line of `$0.log`.
const codexMaterializer = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$0.log\"\n" +
	"[ \"$2\" = codex-materialize ] || exit 0\n" +
	"find \"$4\" -type f -name \"rollout-*-$6.jsonl.zst\" | while IFS= read -r z; do " +
	"if [ -e \"${z%.zst}\" ]; then rm -f \"$z\"; else mv \"$z\" \"${z%.zst}\"; fi; done\n"

// codexDated is where [codexBox] writes the thread's rollout in p's box.
func codexDated(p Paths) string {
	return filepath.Join(p.CodexSessions(), "2026", "11", "21", "rollout-2026-11-21T18-33-59-"+codexThread+".jsonl")
}

// codexCompressedBox is a box home whose only copy of codexThread is
// rollout, compressed by the person's Codex, its note helper helper ("" for
// none).
func codexCompressedBox(t *testing.T, rollout []byte, helper string) Paths {
	t.Helper()
	p := codexBox(t, nil)
	if err := os.WriteFile(codexDated(p)+".zst", rollout, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.CodexNoteHelper()); err != nil {
		t.Fatal(err)
	}
	if helper != "" {
		writeHostProgram(t, p.CodexNoteHelper(), []byte(helper), 0o600)
	}
	return p
}

// A CODEX SESSION ITS HOME COMPRESSED CONTINUES: the person's own Codex
// compresses a rollout untouched for 7 days to `.zst` (its
// local_thread_store_compression), and the gate has the note helper write it
// plain before the cut, the renew and the resume, which all read it plain;
// the run then begins past its lines, as for a rollout never compressed. A
// box with no helper, or one whose helper could not write it plain, starts a
// session of its own, saying why, never resuming what it could not renew.
//
// Mutation: find plain rollouts alone (main before this), skip the
// materialize, continue where it failed, or count before it, and a row fails.
func TestACodexSessionItsHomeCompressedContinues(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	for name, c := range map[string]struct {
		helper string
		k, w   string
	}{
		"written plain": {codexMaterializer, "yes", ""},
		"no helper":     {"", "no", codexCompressedNoHelper},
		"not plain":     {"#!/bin/sh\nexit 1\n", "no", codexCompressedUnread},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := codexCompressedBox(t, whole, c.helper)
			out, errText, code := codexShell(t, p.Home, codexPrinted("codex-cli "+codexCarryRelease)+
				codexSessionGate(Session{ID: codexThread, Continues: true}, p, false)+"before=0\n"+
				codexRunStart(p, codexThread)+`printf '%s\n%s\n%s\n' "$k" "$w" "$before"`)
			if code != 0 {
				t.Fatalf("the gate exited %d: %s", code, errText)
			}
			got := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			if len(got) != 3 || got[0] != c.k || got[1] != c.w {
				t.Fatalf("the gate printed %q, want continued %q saying %q", got, c.k, c.w)
			}
			if c.k != "yes" {
				if got[2] != "0" {
					t.Errorf("a new session begins at line %s of another's rollout", got[2])
				}
				return
			}
			if lines := strconv.Itoa(bytes.Count(whole, []byte("\n"))); got[2] != lines {
				t.Errorf("the run begins past line %s, want past the rollout's %s", got[2], lines)
			}
			if raw := codexFile(t, codexDated(p)); !bytes.Equal(raw, whole) {
				t.Error("the resume finds no plain rollout holding the session")
			}
			if _, err := os.Stat(codexDated(p) + ".zst"); !errors.Is(err, fs.ErrNotExist) {
				t.Error("the compressed rollout is left for the resume to take")
			}
			log := strings.Split(strings.TrimSpace(string(codexFile(t, p.CodexNoteHelper()+".log"))), "\n")
			want := "-adapter codex-materialize -sessions " + p.CodexSessions() + " -thread " + codexThread
			if len(log) != 2 || log[0] != want || !strings.HasPrefix(log[1], "-adapter codex-renew ") {
				t.Errorf("the helper ran %q, want %q and then the renew", log, want)
			}
		})
	}
}

// A STALE COMPRESSED CODEX COPY DOES NOT DEFEAT A CARRIED ARCHIVE: a resume
// takes a dated `.zst` of the thread over the restored rollout at the
// sessions root (measured on 0.171.0), so the restore removes compressed
// copies with plain ones, and the run resumes what was carried.
//
// Mutation: remove only `.jsonl` copies, and the stale `.zst` is left.
func TestAStaleCompressedCodexCopyDoesNotDefeatACarriedArchive(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	packed := codexBox(t, whole)
	if _, errText, code := codexShell(t, packed.Home, "sid="+codexThread+" packed=\n"+codexCapture(packed)); code != 0 {
		t.Fatalf("the capture exited %d: %s", code, errText)
	}
	p := codexCompressedBox(t, []byte(`{"type":"session_meta","payload":{"stale":true}}`+"\n"), codexMaterializer)
	if err := os.WriteFile(p.SessionCarried(), codexFile(t, packed.SessionCapture()), 0o600); err != nil {
		t.Fatal(err)
	}
	s, k, w, _ := codexGate(t, p, p.Home, Session{ID: codexThread, Continues: true}, false)
	if s != codexThread || k != "yes" || w != "" {
		t.Fatalf("the gate named %q continued %q why %q, want the carried thread continued", s, k, w)
	}
	if _, err := os.Stat(codexDated(p) + ".zst"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the stale compressed copy is left, and a resume would take it over the carried one")
	}
	if got := codexFile(t, codexRestoredRollout(p, codexThread)); !bytes.Equal(got, whole) {
		t.Error("the restored rollout is not the carried one")
	}
}

// A CODEX PACK OF A SESSION ITS HOME COMPRESSED is the rollout written plain
// where the box has the note helper; where it has none, the pack fails
// saying there was nothing to pack because the person's Codex compressed it,
// never the silent nothing to pack of a session the box does not hold.
//
// Mutation: find plain rollouts alone (main before this), or exit
// packNothing where it is compressed, and a row fails.
func TestACodexPackOfASessionItsHomeCompressed(t *testing.T) {
	t.Parallel()
	whole := codexRolloutFixture(t)
	p := codexCompressedBox(t, whole, codexMaterializer)
	pack := Codex{}.Pack(p, codexThread, p.SessionPackStaging("p"), p.SessionPackHeld("p"), true)
	if _, errText, code := codexShell(t, p.Home, pack); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	if members := codexMembers(t, codexFile(t, p.SessionPackHeld("p"))); members[codexArchiveMember(codexThread)] != string(whole) {
		t.Error("the pack is not the compressed rollout's lines")
	}

	bare := codexCompressedBox(t, whole, "")
	save, err := Codex{}.PackSave(bare, sessionRecord{id: codexThread}, bare.SessionSaveStaging("t", "1"),
		bare.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	_, errText, code := codexShell(t, bare.Home, save)
	if code != packFailed || !strings.Contains(errText, "nothing to pack: "+codexCompressedNoHelper) {
		t.Errorf("a save of a compressed session with no helper exited %d saying %q, want %d saying it was "+
			"compressed", code, errText, packFailed)
	}
}

// A COMPRESSED CODEX ROLLOUT ALONE HOLDS NO LINE OF THE RUN'S: only a resume
// writes it plain and appends, so where it is the thread's only copy the
// run's `aborted` and `finish` read nothing of it, never its bytes as the
// lines past where the run began.
//
// Mutation: find `rollout-*-<id>.jsonl*` in either, and the compressed
// file's lines are read as the run's.
func TestACompressedCodexRolloutAloneHoldsNoLineOfTheRuns(t *testing.T) {
	t.Parallel()
	rollout := `{"type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":null}}` + "\n"
	p := codexCompressedBox(t, []byte(rollout), "")
	out, errText, code := codexShell(t, p.Home, "sid="+codexThread+" tid= before=0 packed=\n"+
		codexAborted(p)+codexFinish(p)+"if aborted; then echo ABORTED; fi\nfinish\n")
	if code != 0 {
		t.Fatalf("the script exited %d: %s", code, errText)
	}
	if strings.Contains(out, "ABORTED") || strings.Contains(out, codexRolloutEvent) {
		t.Errorf("the run read the compressed rollout as its own lines: %q", out)
	}
}
