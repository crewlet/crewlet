package host

import (
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

// scanned is answer read by an answerScan whole, or a byte at a time.
func scanned(answer string, whole bool) *answerScan {
	var s answerScan
	if whole {
		s.scan([]byte(answer))
		return &s
	}
	for i := range len(answer) {
		s.scan([]byte{answer[i]})
	}
	return &s
}

// THE ANSWER BEGINS WITH ITS PACK — its first band-1 packet, a keepalive's
// included, or a band-2 line that is no report of git's — and never with what
// upload-pack writes before it (the negotiation's lines, a v2 section header,
// an ERR) nor with anything in band 3; the line pack-objects dies with in
// band 2 holds it, whichever packets it was cut across. Read whole or a byte
// at a time, the reading is the same.
func TestTheAnswerBeginsWithItsPack(t *testing.T) {
	t.Parallel()
	notOurs := "ERR upload-pack: not our ref " + strings.Repeat("a", 40)
	for _, tc := range []struct {
		name   string
		answer string
		begun  bool
	}{
		{"a v2 section header alone", line("packfile\n"), false},
		{"progress", line("packfile\n") + sideband(bandProgress, "Enumerating objects: 3, done.\n"), true},
		{"progress rewritten in place", line("packfile\n") +
			sideband(bandProgress, "Counting objects:  33% (1/3)\rCounting objects:  66% (2/3)\r"), true},
		{"the pack", line("packfile\n") + sideband(bandData, "PACK\x00\x00\x00\x02"), true},
		{"a keepalive", line("packfile\n") + "0005\x01", true},
		{"pack-objects dying", line("packfile\n") + sideband(bandProgress, midxLine+"\n") +
			sideband(bandError, abortLine+"\n") + flushPkt, false},
		{"its death cut across two packets", line("packfile\n") + sideband(bandProgress, midxLine[:3]) +
			sideband(bandProgress, midxLine[3:]+"\n") + sideband(bandError, abortLine+"\n"), false},
		{"its death with no newline", line("packfile\n") + sideband(bandProgress, midxLine), false},
		{"a line in band 3", line("packfile\n") + sideband(bandError, abortLine+"\n"), false},
		{"a v2 negotiation round", line("acknowledgments\n") + line("NAK\n") + flushPkt, false},
		{"a v0 negotiation and its progress", line("NAK\n") + sideband(bandProgress, "Total 3 (delta 0)\n"), true},
		{"a v0 pack with no side band", line("NAK\n") + "PACK\x00\x00\x00\x02", true},
		{"a want refused", line(notOurs) + flushPkt, false},
		{"an empty packet", "0004" + line("packfile\n"), false},
		{"a length no packet has", "0003", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, whole := range []bool{true, false} {
				if s := scanned(tc.answer, whole); s.begun != tc.begun {
					t.Errorf("read whole %v: begun %v, want %v", whole, s.begun, tc.begun)
				}
			}
		})
	}
}

// A REPORT DOES NOT BEGIN THE PACK. A band-2 line beginning with one of
// git's report prefixes — "fatal: ", "error: ", "warning: ", "BUG: " — keeps
// the hold, whatever it goes on to say: the rule judges whether a line is a
// report, and nothing about what it names (the run's trace decides that).
// Progress begins the pack, a line rewritten in place included, and so does a
// band-1 keepalive; nothing in band 3 does.
//
// Mutation: drop "fatal: " from the prefixes — red here, and in
// TestADamagedMidxBitmapIsRepaired (E's retry is lost: the answer starts at
// pack-objects' dying line).
func TestAReportDoesNotBeginThePack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		line  string
		begun bool
	}{
		{"fatal", "fatal: x\n", false},
		{"error", "error: x\n", false},
		{"warning", "warning: x\n", false},
		{"BUG", "BUG: x\n", false},
		{"a report naming nothing git classifies", "fatal: the remote end hung up unexpectedly\n", false},
		{"enumerating", "Enumerating objects: 3, done.\n", true},
		{"counting, rewritten in place", "Counting objects:  50% (1/2)\r", true},
		{"a word that is no prefix", "fatality\n", true},
		{"a prefix's word without its colon", "fatal\n", true},
	} {
		for _, whole := range []bool{true, false} {
			s := scanned(line("packfile\n")+sideband(bandProgress, tc.line), whole)
			if s.begun != tc.begun {
				t.Errorf("%s (read whole %v): begun %v, want %v", tc.name, whole, s.begun, tc.begun)
			}
		}
	}
	if s := scanned(line("packfile\n")+"0005\x01", true); !s.begun {
		t.Error("a band-1 keepalive did not begin the pack")
	}
	if s := scanned(line("packfile\n")+sideband(bandError, "Enumerating objects: 3, done.\n"), true); s.begun {
		t.Error("a line in band 3 began the pack")
	}
}

// A REPORT SPLIT ACROSS PACKETS KEEPS THE HOLD: upload-pack relays
// pack-objects' writes as they come, so a report's prefix may arrive across
// two packets of its band, and it is judged whole. And the scan keeps no more
// of a band-2 line than the longest prefix, however long the line grows.
//
// Mutation: judge a line at the end of each packet rather than at reportLead
// bytes or its end, and the split arms begin the pack.
func TestAReportSplitAcrossPacketsKeepsTheHold(t *testing.T) {
	t.Parallel()
	for _, report := range []string{"fatal: x\n", "error: x\n", "warning: x\n", "BUG: x\n", midxLine + "\n"} {
		for cut := 1; cut < len(report)-1; cut++ {
			answer := line("packfile\n") + sideband(bandProgress, report[:cut]) +
				sideband(bandProgress, report[cut:])
			if s := scanned(answer, true); s.begun {
				t.Errorf("%q cut after %d bytes began the pack", report, cut)
			}
		}
	}
	var s answerScan
	s.scan([]byte(line("packfile\n")))
	long := strings.Repeat("x", maxPktPayload-1)
	for range 8 {
		s.scan([]byte(sideband(bandProgress, long)))
		if len(s.lead) > reportLead {
			t.Fatalf("the scan holds %d bytes of a line, past the %d its rule reads", len(s.lead), reportLead)
		}
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

// A FETCH'S HELD ANSWER REACHES THE CLIENT ONLY WHEN IT IS GIT'S ANSWER: a
// run that succeeded or failed by itself, but never one the host found still
// damaged after its repair, nor a run the node ended, nor the repair's own
// failure — each answered with a status that nothing held may precede.
func TestOnlyGitsOwnAnswerIsReleased(t *testing.T) {
	t.Parallel()
	exit := &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrNotOurRef}
	damaged := &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrCorrupt, Result: gitproc.Result{
		Exit: 128, Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartMultiPackIndex}}}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, true},
		{"git's own failure", exit, true},
		{"git's own failure naming damage", damaged, true},
		{"still damaged after its repair", fmt.Errorf("%w: %w", errStillDamaged, exit), false},
		{"a rebuild refused", cache.ErrRebuildRefused, false},
		{"ended by the node", fmt.Errorf("gitproc: ended: %w", gitproc.ErrStalled), false},
	} {
		if got := gitsAnswer(tc.err); got != tc.want {
			t.Errorf("%s: gitsAnswer = %v, want %v", tc.name, got, tc.want)
		}
	}
}
