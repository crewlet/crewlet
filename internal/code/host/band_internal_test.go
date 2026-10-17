package host

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// The lines a damaged clone's answer carries, as git 2.47 and later write
// them: pack-objects' last words in band 2, upload-pack's own in band 3.
const (
	midxLine  = "fatal: invalid MIDX object position, MIDX is likely corrupt"
	abortLine = "aborting due to possible repository corruption on the remote side."
)

// sideband is data on band ch as one pkt-line.
func sideband(ch byte, data string) string { return string(pktLine(append([]byte{ch}, data...))) }

// line is a negotiation line as one pkt-line.
func line(s string) string { return string(pktString(s)) }

// THE ANSWER BEGINS WITH ITS PACK — its first band-1 packet, a keepalive's
// included, or a progress line that names no damage — and never with what
// upload-pack writes before it (the negotiation's lines, a v2 section header,
// an ERR) nor with a fatal word in band 3; a damage pack-objects names in
// band 2 holds it, whichever packets its words were cut across. Read whole or a
// byte at a time, the reading is the same.
func TestTheAnswerBeginsWithItsPack(t *testing.T) {
	t.Parallel()
	notOurs := "ERR upload-pack: not our ref " + strings.Repeat("a", 40)
	for _, tc := range []struct {
		name   string
		answer string
		begun  bool
		kind   error // the words' kind
		named  string
	}{
		{"a v2 section header alone", line("packfile\n"), false, gitproc.ErrFailed, ""},
		{"progress", line("packfile\n") + sideband(bandProgress, "Enumerating objects: 3, done.\n"),
			true, gitproc.ErrFailed, ""},
		{"progress rewritten in place", line("packfile\n") +
			sideband(bandProgress, "Counting objects:  33% (1/3)\rCounting objects:  66% (2/3)\r"),
			true, gitproc.ErrFailed, ""},
		{"the pack", line("packfile\n") + sideband(bandData, "PACK\x00\x00\x00\x02"), true, gitproc.ErrFailed, ""},
		{"a keepalive", line("packfile\n") + "0005\x01", true, gitproc.ErrFailed, ""},
		{"pack-objects naming damage", line("packfile\n") + sideband(bandProgress, midxLine+"\n") +
			sideband(bandError, abortLine+"\n") + flushPkt, false, gitproc.ErrCorrupt, midxLine},
		{"damage cut across two packets", line("packfile\n") + sideband(bandProgress, midxLine[:17]) +
			sideband(bandProgress, midxLine[17:]+"\n") + sideband(bandError, abortLine+"\n"),
			false, gitproc.ErrCorrupt, midxLine},
		{"damage with no newline", line("packfile\n") + sideband(bandProgress, midxLine), false,
			gitproc.ErrCorrupt, midxLine},
		{"a fatal word in band 3", line("packfile\n") + sideband(bandError, abortLine+"\n"), false,
			gitproc.ErrFailed, ""},
		{"a v2 negotiation round", line("acknowledgments\n") + line("NAK\n") + flushPkt, false,
			gitproc.ErrFailed, ""},
		{"a v0 negotiation and its progress", line("NAK\n") + sideband(bandProgress, "Total 3 (delta 0)\n"),
			true, gitproc.ErrFailed, ""},
		{"a v0 pack with no side band", line("NAK\n") + "PACK\x00\x00\x00\x02", true, gitproc.ErrFailed, ""},
		{"a want refused", line(notOurs) + flushPkt, false, gitproc.ErrFailed, ""},
		{"an empty packet", "0004" + line("packfile\n"), false, gitproc.ErrFailed, ""},
		{"a length no packet has", "0003", true, gitproc.ErrFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, whole := range []bool{true, false} {
				var s answerScan
				if whole {
					s.scan([]byte(tc.answer))
				} else {
					for i := range len(tc.answer) {
						s.scan([]byte{tc.answer[i]})
					}
				}
				s.finish()
				if s.begun != tc.begun || !errors.Is(s.words.Kind(), tc.kind) || s.words.Named() != tc.named {
					t.Errorf("read whole %v: begun %v, words %v named %q; want begun %v, %v named %q",
						whole, s.begun, s.words.Kind(), s.words.Named(), tc.begun, tc.kind, tc.named)
				}
			}
		})
	}
}

// A SIDE BAND'S LINE IS READ AS A STDERR LINE IS, AT MOST gitproc.MaxLine OF
// IT AT ONCE: a line of git's longer than any git writes is read in MaxLine
// pieces however long it grows, and damage after it is still named.
func TestASideBandLineIsReadToMaxLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxPktPayload-1)
	var s answerScan
	s.scan([]byte(line("packfile\n")))
	for chunk := range 8 {
		s.scan([]byte(sideband(bandError, long)))
		if got := len(s.text[bandError-bandProgress]); got > gitproc.MaxLine {
			t.Fatalf("after %d packets a band-3 line holds %d bytes, past MaxLine", chunk+1, got)
		}
	}
	s.scan([]byte(sideband(bandError, midxLine+"\n")))
	s.finish()
	if !errors.Is(s.words.Damage(), gitproc.ErrCorrupt) {
		t.Errorf("damage after a long line is %v, want ErrCorrupt", s.words.Damage())
	}
}

// A FETCH'S SINK HOLDS ITS ANSWER UNTIL THE PACK BEGINS: nothing reaches the
// client — no status, no byte — before it, and everything held reaches it, in
// order, once it does; a preamble outgrowing preambleHoldBytes starts the
// answer as before; a held answer whose run's words stand is released whole,
// and one whose run wrote nothing is not started, so it can still be answered
// with a status; another run over the same request starts from nothing.
func TestASinkHoldsAFetchsAnswerUntilItsPackBegins(t *testing.T) {
	t.Parallel()
	newHeld := func() (*sink, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		s := newSink(w, "application/x-git-upload-pack-result", time.Now)
		s.holdPack()
		return s, w
	}
	write := func(t *testing.T, s *sink, b string) {
		t.Helper()
		if n, err := s.Write([]byte(b)); n != len(b) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	t.Run("until the pack", func(t *testing.T) {
		s, w := newHeld()
		head := line("packfile\n")
		write(t, s, head)
		if s.Started() || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
			t.Fatalf("the answer started before its pack: %q", w.Body.String())
		}
		pack := sideband(bandData, "PACK")
		write(t, s, pack)
		if !s.Started() || w.Body.String() != head+pack {
			t.Fatalf("the answer at its pack is %q, want %q", w.Body.String(), head+pack)
		}
	})
	t.Run("past the hold", func(t *testing.T) {
		s, w := newHeld()
		acks := strings.Repeat(line("ACK "+strings.Repeat("a", 40)+" common\n"), preambleHoldBytes/50)
		write(t, s, line("acknowledgments\n"))
		write(t, s, acks)
		if !s.Started() || w.Body.Len() != len(line("acknowledgments\n"))+len(acks) {
			t.Fatalf("a %d-byte preamble was held, past the %d-byte hold", w.Body.Len(), preambleHoldBytes)
		}
	})
	t.Run("released", func(t *testing.T) {
		s, w := newHeld()
		round := line("acknowledgments\n") + line("NAK\n") + flushPkt
		write(t, s, round)
		if err := s.release(); err != nil || !s.Started() || w.Body.String() != round {
			t.Fatalf("release: %v, the answer %q, want %q", err, w.Body.String(), round)
		}
	})
	t.Run("nothing to release", func(t *testing.T) {
		s, _ := newHeld()
		if err := s.release(); err != nil || s.Started() {
			t.Fatalf("a release of nothing started the answer (%v)", err)
		}
	})
	t.Run("again", func(t *testing.T) {
		s, w := newHeld()
		write(t, s, line("packfile\n")+sideband(bandProgress, midxLine+"\n"))
		s.again()
		pack := line("packfile\n") + sideband(bandData, "PACK")
		write(t, s, pack)
		if w.Body.String() != pack {
			t.Fatalf("the answer after another run is %q, want only that run's %q", w.Body.String(), pack)
		}
	})
	// onStart — which frees a fetch's request body and its spool allowance
	// (fetch.go sets it to body.started) — fires with the answer's first byte,
	// which for a held fetch is its PACK's, not upload-pack's opening line. So a
	// spooled request's room is held through pack-objects' silence, to its first
	// band-1 packet (a keepalive's included, at most one upload-pack keepalive
	// for a client that asked for no progress), and no later.
	t.Run("onStart at the pack", func(t *testing.T) {
		s, _ := newHeld()
		var started int
		s.onStart = func() { started++ }
		write(t, s, line("packfile\n"))
		if started != 0 {
			t.Fatalf("the body was given back at upload-pack's opening line, before its pack (started %d)", started)
		}
		write(t, s, "0005\x01") // a band-1 keepalive: pack-objects has begun its pack
		if started != 1 {
			t.Fatalf("the body was given back %d times at the pack's first byte, want once", started)
		}
	})
}

// A RUN'S FAILURE CARRIES THE DAMAGE ITS ANSWER'S SIDE BAND NAMED — still the
// process's failure to errors.As, and damage to errors.Is and so to
// cache.DamageReason — quoting git's words; a process the engine ended is no
// verdict about what it read, and an answer that started is past judging.
func TestAFailureCarriesTheDamageItsAnswerNamed(t *testing.T) {
	t.Parallel()
	exit := func() error {
		return &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrFailed,
			Result: gitproc.Result{Exit: 128, Stderr: "fatal: git upload-pack: " + abortLine + "\n"}}
	}
	damaged := line("packfile\n") + sideband(bandProgress, midxLine+"\n") + sideband(bandError, abortLine+"\n")
	held := func(answer string) *sink {
		s := newSink(httptest.NewRecorder(), "", time.Now)
		s.holdPack()
		_, _ = s.Write([]byte(answer))
		return s
	}

	err := held(damaged).judged(exit())
	var e *gitproc.ExitError
	if !errors.As(err, &e) || !errors.Is(err, gitproc.ErrCorrupt) || !strings.Contains(err.Error(), midxLine) {
		t.Errorf("a failure whose answer named damage is %v", err)
	}
	if reason, ok := cache.DamageReason(err); !ok || reason != cache.RebuildCorrupt {
		t.Errorf("the cache reads it as %q, %v; want a rebuild for corrupt", reason, ok)
	}
	if err := held(line("packfile\n") + sideband(bandError, abortLine+"\n")).judged(exit()); errors.Is(err,
		gitproc.ErrCorrupt) {
		t.Errorf("a failure whose answer named no damage is %v", err)
	}
	ended := fmt.Errorf("gitproc: git upload-pack was ended: %w", gitproc.ErrStalled)
	var folded *bandDamage
	if err := held(damaged).judged(ended); errors.As(err, &folded) || !errors.Is(err, gitproc.ErrStalled) {
		t.Errorf("a process the engine ended is judged %v", err)
	}
	started := held(damaged + sideband(bandData, "PACK"))
	if err := started.judged(exit()); errors.Is(err, gitproc.ErrCorrupt) {
		t.Errorf("an answer already started is judged %v", err)
	}
}

// A FETCH'S HELD ANSWER REACHES THE CLIENT ONLY WHEN IT IS GIT'S ANSWER: a
// run that succeeded or failed by itself, but never one the host found still
// damaged after its repair, nor a run the node ended, nor the repair's own
// failure — each answered with a status that nothing held may precede.
func TestOnlyGitsOwnAnswerIsReleased(t *testing.T) {
	t.Parallel()
	exit := &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrNotOurRef}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, true},
		{"git's own failure", exit, true},
		{"damage in band", &bandDamage{err: exit, kind: gitproc.ErrCorrupt, named: midxLine}, true},
		{"still damaged after its repair", fmt.Errorf("%w: %w", errStillDamaged, exit), false},
		{"a rebuild refused", cache.ErrRebuildRefused, false},
		{"ended by the node", fmt.Errorf("gitproc: ended: %w", gitproc.ErrStalled), false},
	} {
		if got := gitsAnswer(tc.err); got != tc.want {
			t.Errorf("%s: gitsAnswer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// AN UPLOAD-PACK RUN IS REPAIRED ONLY FOR WHAT ITS REQUEST CANNOT WRITE: the
// damage its answer's side band named (pack-objects' words), and the pool's
// refusal of the incarnation's shape before git starts — and never for damage
// read off upload-pack's own stderr, which echoes the request, so a request
// quoting " is corrupt" in an argument git refuses forces no rebuild. A run the
// node ended names no damage.
//
// Mutation: read every failure that is no bandDamage as nothing (the shape
// refusal row fails, and TestACacheGitRefusesForItsShapeIsRepairedUnderTheRequest's
// ls-refs arm with it); read the run's stderr Kind (the echo rows fail, and
// TestACraftedRequestCannotForceARebuild with them).
func TestAnUploadPackRunIsRepairedOnlyForWhatItsRequestCannotWrite(t *testing.T) {
	t.Parallel()
	exit := func(kind error, stderr string) *gitproc.ExitError {
		return &gitproc.ExitError{Command: "upload-pack", Kind: kind,
			Result: gitproc.Result{Exit: 128, Stderr: stderr}}
	}
	aborted := exit(gitproc.ErrFailed, "fatal: git upload-pack: "+abortLine+"\n")
	for _, tc := range []struct {
		name   string
		err    error
		reason cache.RebuildReason // "" for no repair
	}{
		{"the side band names the multi-pack-index",
			&bandDamage{err: aborted, kind: gitproc.ErrCorrupt, named: midxLine}, cache.RebuildMidx},
		{"the side band names a damaged pack",
			&bandDamage{err: aborted, kind: gitproc.ErrCorrupt,
				named: "fatal: packed object 0123 (stored in pack-1.pack) is corrupt"}, cache.RebuildCorrupt},
		{"the side band names a missing object",
			&bandDamage{err: aborted, kind: gitproc.ErrMissingObject,
				named: "fatal: bad object " + strings.Repeat("ab", 20)}, cache.RebuildMissing},
		{"the pool refuses the incarnation's shape",
			fmt.Errorf("git upload-pack: %w", &gitproc.ShapeError{Rule: gitproc.ShapeConfig, Dir: "/c/repos/r.1.git"}),
			cache.RebuildShape},
		{"upload-pack's stderr echoes an argument naming corruption",
			exit(gitproc.ErrCorrupt, "fatal: unexpected line: 'x is corrupt'\n"), ""},
		{"upload-pack's stderr echoes a line naming the multi-pack-index",
			exit(gitproc.ErrCorrupt, "fatal: unexpected line: 'x\n"+midxLine+"\n'\n"), ""},
		{"upload-pack's stderr names a missing object",
			exit(gitproc.ErrMissingObject, "fatal: unexpected line: 'bad object "+strings.Repeat("ab", 20)+"'\n"), ""},
		{"upload-pack refuses a want", exit(gitproc.ErrNotOurRef, "fatal: git upload-pack: not our ref\n"), ""},
		{"the node ended git", fmt.Errorf("gitproc: ended: %w", gitproc.ErrStalled), ""},
		{"no slot was free", gitproc.ErrBusy, ""},
	} {
		reason, ok := fetchDamage(tc.err)
		if ok != (tc.reason != "") || reason != tc.reason {
			t.Errorf("%s: fetchDamage = %q, %v; want %q", tc.name, reason, ok, tc.reason)
		}
	}
}
