package assistantapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// answered is one refusal as a client reads it.
type answered struct {
	status int
	retry  string
	body   map[string]any
	log    string
}

// answer runs one of the surface's failure writers against err with a logger
// of its own, and reads back what it wrote.
func answer(t *testing.T, write func(http.ResponseWriter, *http.Request, *slog.Logger)) answered {
	t.Helper()
	var out lockedBuffer
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := httptest.NewRecorder()
	write(rec, httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil), logger)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not the envelope: %s", rec.Body)
	}
	return answered{status: rec.Code, retry: rec.Header().Get("Retry-After"), body: body,
		log: out.String()}
}

// A CONDITION OF THE NODE IS TOLD AS ONE ON THE ASSISTANT'S SURFACE, AND A
// FAULT IS A FAULT.
//
// Every failure the assistant met in its own records, its counter or its
// authority table was `503 assistant_unavailable` under a Retry-After of two
// seconds and no detail: a log at its ceiling was asked again every two
// seconds, and a store that was broken was said to be coming back. A
// condition is `503 unavailable` in a person's words for it
// ([builtin.ConditionSentence]), under its own hint and with none where no
// wait clears it; anything no condition claims is `500 internal_error`; the
// assistant's own reasons keep `assistant_unavailable`, with no Retry-After;
// and a write whose outcome is unknown says so, naming the key a retry sends.
// None carries its error's own text, which goes to the surface's log.
//
// A read whose own write of the engine's nobody can confirm (a lapsed turn
// closed as its transcript is read) is told to come back, since the read asked
// again settles it; it sent no key, so it is no unknown outcome of the
// caller's.
//
// Mutation: drop either AnswerCondition arm from fail and the conditions
// answer 500 or `assistant_unavailable`; answer a write's unknown outcome
// through fail and it answers no `outcome`; drop fail's own unknown arm and
// the read's row answers 500; put a wait back on the assistant's own reason
// and its row goes red.
func TestAConditionIsToldAsOneAndAFaultAsAFault(t *testing.T) {
	t.Parallel()
	behind := fmt.Errorf("assistantdomain: read: %w", &statelog.Unavailable{
		Reason: statelog.ReasonBehind, Detail: "the write needs 3:4182 applied"})
	full := fmt.Errorf("assistantdomain: publish: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: "raise stream.assistant_log_max_bytes"})
	blink := fmt.Errorf("dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable)
	sentence := func(err error) string {
		said, ok := builtin.ConditionSentence(err)
		if !ok {
			t.Fatalf("%v is no condition", err)
		}
		return said
	}
	surface := func(logger *slog.Logger) *Surface {
		return &Surface{log: logger, now: func() time.Time { return time.Now().UTC() }}
	}
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
		says   string
		retry  bool
	}{
		"a copy behind its log": {behind, http.StatusServiceUnavailable, "unavailable",
			sentence(behind), true},
		"a log at its ceiling": {full, http.StatusServiceUnavailable, "unavailable",
			sentence(full), false},
		"the assistant's own reason beneath a condition": {&assistant.Unavailable{Cause: blink},
			http.StatusServiceUnavailable, "unavailable", sentence(blink), true},
		"the assistant's own reason": {&assistant.Unavailable{Cause: errors.New(
			`engine: assistant.llm names "claude", which the running company does not build`)},
			http.StatusServiceUnavailable, "assistant_unavailable", "", false},
		"a read whose own write nobody can confirm": {fmt.Errorf("%w: operation x",
			assistant.ErrOutcomeUnknown), http.StatusServiceUnavailable, "unavailable",
			unsettledRead, true},
		"a fault": {errors.New("open /var/lib/crewlet/replicated.db: disk I/O error"),
			http.StatusInternalServerError, "internal_error", "", false},
	} {
		got := answer(t, func(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
			surface(logger).fail(w, r, c.err)
		})
		if got.status != c.status || got.body["error"] != c.code ||
			(c.says != "" && got.body["detail"] != c.says) || got.body["outcome"] != nil {
			t.Errorf("%s answered %d %v, want %d %s saying %q", name, got.status, got.body,
				c.status, c.code, c.says)
		}
		if retry := got.retry != ""; retry != c.retry {
			t.Errorf("%s: Retry-After %q, want one: %v", name, got.retry, c.retry)
		}
		if message, _ := got.body["message"].(string); c.code == "assistant_unavailable" &&
			strings.Contains(message, "in a moment") {
			t.Errorf("%s says %q, which no wait clears", name, message)
		}
		raw, _ := json.Marshal(got.body)
		if text := string(raw); strings.Contains(text, "10.0.0.7") ||
			strings.Contains(text, "/var/lib") || strings.Contains(text, "3:4182") ||
			strings.Contains(text, "assistant_log_max_bytes") ||
			strings.Contains(text, "running company") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
		if !strings.Contains(got.log, "path=/assistant/conversations") {
			t.Errorf("%s logged %q, want the route it answered and its error", name, got.log)
		}
	}

	key := "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a14"
	unknown := answer(t, func(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
		surface(logger).failWrite(w, r, key, fmt.Errorf("%w: operation %s",
			assistant.ErrOutcomeUnknown, key))
	})
	if unknown.status != http.StatusServiceUnavailable || unknown.body["outcome"] != "unknown" ||
		unknown.body["op_id"] != key || unknown.retry == "" {
		t.Errorf("a write nobody can account for answered %d %v (Retry-After %q), want 503 "+
			"with outcome unknown and its key", unknown.status, unknown.body, unknown.retry)
	}
	refused := answer(t, func(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
		surface(logger).failWrite(w, r, key, full)
	})
	if refused.status != http.StatusServiceUnavailable || refused.body["outcome"] != nil ||
		refused.body["detail"] != sentence(full) || refused.retry != "" {
		t.Errorf("a write the log refused answered %d %v (Retry-After %q), want the "+
			"condition, which wrote nothing", refused.status, refused.body, refused.retry)
	}
}

// A DECISION THE STORE REFUSED WROTE NOTHING, SO IT IS NO UNKNOWN OUTCOME.
//
// The approval path told every store error as a decision nobody could
// confirm: a person whose approval the log refused at its ceiling was told it
// may have landed, and asked to approve again under a Retry-After, into the
// same refusal. A refusal is the condition it is, a fault is a fault, and only
// a decision that was sent and unanswered is `outcome: unknown`.
//
// Mutation: drop the condition arm from refuse and the refusal answers 500;
// tell it as Unconfirmed in approval's move and this surface answers it
// unknown.
func TestADecisionTheStoreRefusedIsNoUnknownOutcome(t *testing.T) {
	t.Parallel()
	full := fmt.Errorf("approval: record the move to approved: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: "raise stream.assistant_log_max_bytes"})
	said, _ := builtin.ConditionSentence(full)
	key := "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a14"
	decide := func(err error) answered {
		return answer(t, func(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
			(&Decisions{log: logger}).refuse(w, r, key, err)
		})
	}
	if got := decide(full); got.status != http.StatusServiceUnavailable ||
		got.body["error"] != "unavailable" || got.body["outcome"] != nil ||
		got.body["detail"] != said || got.retry != "" {
		t.Errorf("a decision the log refused answered %d %v (Retry-After %q), want the "+
			"condition in a person's words with no wait", got.status, got.body, got.retry)
	}
	if got := decide(&approval.Unconfirmed{OpID: key}); got.status != http.StatusServiceUnavailable ||
		got.body["outcome"] != "unknown" || got.body["op_id"] != key {
		t.Errorf("a decision nobody can confirm answered %d %v, want outcome unknown",
			got.status, got.body)
	}
	if got := decide(errors.New("open /var/lib/crewlet/replicated.db: disk I/O error")); got.status != http.StatusInternalServerError || got.body["error"] != "internal_error" {
		t.Errorf("a fault answered %d %v, want 500", got.status, got.body)
	}
}

// lockedBuffer is a log sink the logger may write from any goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
