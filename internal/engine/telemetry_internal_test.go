package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// A failed turn's events carry their failure texts cut to what one event can
// hold, and THIS NODE'S LOG CARRIES THE WHOLE: one turn_failure_cut line, at
// WARN, naming the run, holding every text the cut shortened and holding each
// once. These cases are the log half of that; events.ClipDiagnostic's own
// tests are the event half.

// logged is a turn telemetry that logs into a buffer, and a reader of the
// turn_failure_cut lines it wrote.
func logged(t *testing.T) (*Engine, *pub, turnTelemetry, func() []map[string]any) {
	t.Helper()
	e, p, tel := failing(t)
	tel.startedAt = time.Now().UTC().Add(-time.Second)
	var buf bytes.Buffer
	tel.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return e, p, tel, func() []map[string]any {
		var out []map[string]any
		lines := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
		lines.Buffer(nil, buf.Len()+1)
		for lines.Scan() {
			var record map[string]any
			if err := json.Unmarshal(lines.Bytes(), &record); err != nil {
				t.Fatalf("a log line is not a JSON record: %v", err)
			}
			if record["msg"] == "turn_failure_cut" {
				out = append(out, record)
			}
		}
		return out
	}
}

// pastTheBound is a failure text past what one event's diagnostic carries,
// with an end of its own so a test can tell a head from the whole.
func pastTheBound(head string) string {
	return head + strings.Repeat("ошибка ", events.MaxDiagnosticBytes/6) + " :the end"
}

// headOf asserts a text on an event is the marked head of whole.
func headOf(t *testing.T, what, got, whole string) {
	t.Helper()
	if got != events.ClipDiagnostic(whole) || got == whole {
		t.Errorf("%s is %d bytes of a %d-byte text; want its marked head", what, len(got), len(whole))
	}
}

// oneCut is the one turn_failure_cut line, failing unless there is exactly one.
func oneCut(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("%d turn_failure_cut lines; want exactly one for the turn", len(lines))
	}
	line := lines[0]
	if line["level"] != "WARN" || line["turn_id"] != "t-1" || line["work_key"] != "wk-1" {
		t.Errorf("the line is %v for run %v, key %v; want WARN, naming run t-1 and key wk-1",
			line["level"], line["turn_id"], line["work_key"])
	}
	return line
}

func TestATurnErrorPastTheBoundIsWholeInTheNodesLog(t *testing.T) {
	t.Parallel()
	e, p, tel, lines := logged(t)
	failure := pastTheBound("runner: execute round 1: ")

	e.publishTurnCompleted(context.Background(), tel, runner.Spend{},
		turn.Result{Decision: phase.Failed}, errors.New(failure))

	summary := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	headOf(t, "the summary's error", summary.Error, failure)
	line := oneCut(t, lines())
	if line["error"] != failure {
		t.Errorf("the line's error is not the turn's whole error (%d bytes)", len(failure))
	}
}

func TestAGuardBreachPastTheBoundIsWholeInTheNodesLogOnce(t *testing.T) {
	t.Parallel()
	e, p, tel, lines := logged(t)
	detail := pastTheBound("the guard fired: ")

	// NO ERROR, so two events carry the one detail — the summary and the
	// breach — and the line carries it once.
	e.publishTurnCompleted(context.Background(), tel, runner.Spend{}, turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: types.GuardStall, Detail: detail},
	}, nil)

	summary := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	headOf(t, "the summary's error", summary.Error, detail)
	breach := only[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	headOf(t, "the breach's detail", breach.Detail, detail)
	line := oneCut(t, lines())
	if line["breach_detail"] != detail {
		t.Error("the line does not carry the breach's whole detail")
	}
}

// A panic's detail is quoted by the error that wraps it, so the one line holds
// it once — inside the error — rather than the same text twice.
func TestAPanicPastTheBoundIsWholeInTheNodesLogOnce(t *testing.T) {
	t.Parallel()
	e, p, tel, lines := logged(t)
	panicked := turn.Recovered(pastTheBound(""))
	cause := fmt.Errorf("turn: execute round 2: %w", panicked)

	e.publishTurnCompleted(context.Background(), tel, runner.Spend{}, turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: types.GuardUnhandledException, Detail: panicked.Error()},
	}, cause)

	breach := only[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	headOf(t, "the breach's detail", breach.Detail, panicked.Error())
	line := oneCut(t, lines())
	if line["error"] != cause.Error() {
		t.Error("the line does not carry the turn's whole error")
	}
	if _, repeated := line["breach_detail"]; repeated {
		t.Error("the line repeats the panic's detail, which the error it carries already holds whole")
	}
}

// An exhausted chain's error is logged whole beside the turn's own error when
// the turn's does not quote it, and not repeated when it does.
func TestAnExhaustedChainPastTheBoundIsWholeInTheNodesLog(t *testing.T) {
	t.Parallel()
	exhausted := &chain.Error{
		Attempted: []string{"primary", "backup"},
		Err: &llm.Error{Kind: llm.KindServer, Provider: "p", Model: "m",
			Err: errors.New(pastTheBound("the provider answered: "))},
	}
	for _, tc := range []struct {
		name string
		err  error
		// quoted is whether the turn's error text holds the chain's.
		quoted bool
	}{
		{"quoted by the turn's error", fmt.Errorf("runner: execute: %w", exhausted), true},
		{"wrapped by an error that does not quote it", opaque{exhausted}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, p, tel, lines := logged(t)

			e.publishTurnCompleted(context.Background(), tel, runner.Spend{},
				turn.Result{Decision: phase.Failed}, tc.err)

			unavailable := only[*types.LLMUnavailable](t, p, "llm_unavailable")
			headOf(t, "llm_unavailable's last error", unavailable.LastError, exhausted.Error())
			line := oneCut(t, lines())
			last, carried := line["last_error"]
			switch {
			case tc.quoted && carried:
				t.Error("the line repeats the chain's error, which the turn's error it carries holds whole")
			case tc.quoted && !strings.Contains(fmt.Sprint(line["error"]), exhausted.Error()):
				t.Error("the line's error does not hold the chain's whole error")
			case !tc.quoted && last != exhausted.Error():
				t.Error("the line does not carry the chain's whole error, which nothing else it holds quotes")
			}
		})
	}
}

// opaque wraps an error without quoting it.
type opaque struct{ inner error }

func (o opaque) Error() string { return "the executor gave up" }
func (o opaque) Unwrap() error { return o.inner }

// A turn whose texts all fit logs no such line: its events carry them whole.
func TestATurnWhoseTextsFitLogsNoCut(t *testing.T) {
	t.Parallel()
	e, _, tel, lines := logged(t)

	e.publishTurnCompleted(context.Background(), tel, runner.Spend{}, turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: types.GuardMaxIter, Detail: "6 rounds, no done"},
	}, errors.New("runner: execute: the provider refused"))

	if got := lines(); len(got) != 0 {
		t.Errorf("%d turn_failure_cut lines for a turn whose texts all fit its events", len(got))
	}
}
