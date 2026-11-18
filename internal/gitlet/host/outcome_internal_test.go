package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REFUSAL LINE FITS ONE PKT-LINE, newline included, whatever its ref: the
// ref whole (git matches a status to its command by the ref's name) and the
// sentence cut to the room left, marked as cut. A line past git's largest,
// as the longest ref a command line carries makes one, loses the push's whole
// report to a protocol error.
func TestARefusalLineFitsOnePktLine(t *testing.T) {
	longest := "refs/heads/" + strings.Repeat("r", maxPktPayload-len(gitlet.ZeroOID+" "+gitlet.ZeroOID+" ")-
		len("refs/heads/"))
	// The longest sentence a line carries whole: maxSentence with its code.
	detail := strings.Repeat("d", maxSentence-len(gitlet.RefusalStale+": "))
	for _, tc := range []struct {
		name string
		a    answered
		cut  bool
	}{
		{name: "a short ref keeps its whole sentence", a: answered{Ref: "refs/heads/main", Reason: gitlet.RefusalStale,
			Detail: detail}},
		{name: "the longest ref, a code's own sentence", a: answered{Ref: longest, Reason: gitlet.RefusalBadRef},
			cut: true},
		{name: "the longest ref, a receiver's sentence", a: answered{Ref: longest, Detail: detail}, cut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := tc.a.line()
			if n := len(line + "\n"); n > maxPktPayload {
				t.Errorf("the line and its newline are %d bytes, past the %d one pkt-line carries", n,
					maxPktPayload)
			}
			if !strings.HasPrefix(line, "ng "+tc.a.Ref+" ") {
				t.Errorf("the line does not name its ref whole")
			}
			words := strings.TrimPrefix(line, "ng "+tc.a.Ref+" ")
			if cut := strings.HasSuffix(words, "…"); cut != tc.cut {
				t.Errorf("the sentence %q was cut %v, want %v", words, cut, tc.cut)
			}
		})
	}
}

// outcomeReads is a host's estate whose every read of a push's outcome is
// answered by nobody — no data node answered — noting the instant of each.
type outcomeReads struct {
	Estate
	at []time.Time
}

func (e *outcomeReads) PushOutcome(context.Context, gitlet.OutcomeQuery) (gitlet.PushOutcome, error) {
	e.at = append(e.at, time.Now())
	return gitlet.PushOutcome{}, errors.New("refused: no data node answered")
}

// A PENDING PUSH IS READ AGAIN UNTIL ITS GRACE IS SPENT, AND NO LONGER: at
// once, then after waits doubling from PendingFirst to PendingMax, the last
// cut to what is left so it lands at the grace's very end; then it is said
// pending. A synctest bubble lands every read at its step to the nanosecond,
// however slow the runner.
//
// Mutation: read again at PendingFirst for ever — the spacing never doubles,
// red; wait a whole step whatever is left of the grace — the last read comes
// past it, red; wait out half the grace — the reads stop short, red.
func TestAPendingPushIsReadAgainUntilItsGraceIsSpent(t *testing.T) {
	timing := Timing{PendingGrace: 300 * time.Millisecond, PendingFirst: 40 * time.Millisecond,
		PendingMax: 80 * time.Millisecond}
	// At once; then 40 ms, doubled to the ceiling's 80 ms, and 80 ms again
	// twice; and the 20 ms the grace has left.
	want := []time.Duration{0, 40 * time.Millisecond, 120 * time.Millisecond, 200 * time.Millisecond,
		280 * time.Millisecond, 300 * time.Millisecond}
	synctest.Test(t, func(t *testing.T) {
		e := &outcomeReads{}
		s := &Server{o: Options{Timing: timing, Estate: e}, now: time.Now}
		began := time.Now()
		v := s.awaitPending(t.Context(), &landing{Progress: func(string) {}}, nil, false, nil,
			statelog.Position{Stream: gitlet.Domain{}.Stream().Name, Seq: 1 << 20})
		if v.outcome != types.AuditPending {
			t.Errorf("a push whose outcome never resolved came to %q, want it pending", v.outcome)
		}
		got := make([]time.Duration, len(e.at))
		for i, at := range e.at {
			got[i] = at.Sub(began)
		}
		if !slices.Equal(got, want) {
			t.Errorf("the outcome was read %v after the wait began, want %v", got, want)
		}
		if ended := time.Since(began); ended != timing.PendingGrace {
			t.Errorf("the push was said pending %v after the wait began, want at its grace's end, %v", ended,
				timing.PendingGrace)
		}
	})
}

// A COLLAPSED ROW READS AS ITS COMMANDS SAY: a retried push id that meets the
// row an earlier record applied reads applied where a command moved a ref or
// every one was a noop, and refused under its first code where it was an
// attempt, every command refused.
//
// Mutation: read every collapsed row applied (the attempt's reads applied, red).
func TestACollapsedAttemptReadsRefused(t *testing.T) {
	at := statelog.Position{Stream: "s", Generation: 1, Seq: 7}
	refused := gitlet.Command{Ref: "refs/heads/locked", Result: gitlet.ResultNG, Reason: gitlet.RefusalProtectedDirect}
	moved := gitlet.Command{Ref: "refs/heads/topic", Result: gitlet.ResultOK}
	if v := settled([]gitlet.Command{refused}, at, true, true); v.outcome != types.AuditRefused ||
		v.refusal != AuditRefusal(gitlet.RefusalProtectedDirect) || !v.position.IsZero() {
		t.Errorf("a collapsed attempt's row read %s %s at %s, want refused protected_direct", v.outcome, v.refusal,
			v.position)
	}
	if v := settled([]gitlet.Command{refused, moved}, at, true, true); v.outcome != types.AuditApplied ||
		v.position != at {
		t.Errorf("a collapsed row with a ref moved read %s at %s, want applied at %s", v.outcome, v.position, at)
	}
}
