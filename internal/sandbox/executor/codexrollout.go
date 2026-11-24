package executor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A CODEX SESSION IS ITS ROLLOUT: one append-only JSONL file per thread,
// `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<thread id>.jsonl`, each line
// `{"timestamp","ordinal","type","payload"}` (measured on 0.171.0). A resume
// finds it by that name anywhere under `sessions/`, needs nothing else in
// CODEX_HOME, and appends to it in place, so the file alone is the sealed
// session. Its readers take a line at a time to [codexRolloutLineMaxBytes],
// the live run's holding it ([readCodexRollout]) and a person's, the receipts'
// and the count's on the line walk, holding none ([codexReading]); a kind they
// do not name is skipped, and a last line with no newline is torn: still being
// written, never read as whole.

// codexRolloutLineMaxBytes bounds one rollout line. The longest line Codex
// writes is a steered note: turn/steer refuses more than 1,048,576 code points
// ([codexSteerMaxCodePoints]), at most 4 bytes each in UTF-8 or 6 as a JSON
// escape, so 6 MiB, plus the line's envelope and metadata. Tool output is cut
// by Codex itself far below this. A longer line is not a shape this build read,
// so the rollout is refused as unreadable rather than read without it.
const codexRolloutLineMaxBytes = 8 << 20

// codexSteerMaxCodePoints is turn/steer's input bound, which Codex 0.171.0
// enforces at the call (`input_too_large`, `max_chars` 1048576) and counts in
// Unicode code points, not bytes or UTF-16 units (measured with ASCII, CJK and
// emoji). [codexRolloutLineMaxBytes] is derived from it.
const codexSteerMaxCodePoints = 1 << 20

// errCodexRolloutLine is a rollout line past [codexRolloutLineMaxBytes].
var errCodexRolloutLine = errors.New("executor: codex rollout line past its bound")

// CodexHome is the directory a Codex run keeps its state in, pinned as
// CODEX_HOME: the box home's `.codex`, where Codex keeps it by default, named
// by the box's own home as every path the runner hands the box is. Every Codex
// path is derived from it.
func (p Paths) CodexHome() string { return p.Home + "/.codex" }

// CodexSessions is the directory under [Paths.CodexHome] Codex keeps its
// rollouts in, and the only one a resume searches.
func (p Paths) CodexSessions() string { return p.CodexHome() + "/sessions" }

// codexRolloutPath is the shell that prints the path of thread's rollout in
// the box, or nothing where the box holds none: the first file, in name order,
// under [Paths.CodexSessions] named `rollout-*-<thread>.jsonl`, the name a
// resume itself looks for. "" for a thread that is no session id
// ([sandbox.ValidSessionID]), whose characters a pattern could misread.
func codexRolloutPath(p Paths, threadID string) string {
	if !sandbox.ValidSessionID(threadID) {
		return ""
	}
	return `find ` + shellword.Quote(p.CodexSessions()) + ` -type f -name ` +
		shellword.Quote("rollout-*-"+threadID+".jsonl") + ` 2>/dev/null | LC_ALL=C sort | head -n 1`
}

// codexCarriedStamp is the timestamp a carried rollout is named with. A resume
// takes only `rollout-YYYY-MM-DDThh-mm-ss-<thread>.jsonl` at any depth under
// sessions/, with any valid time, and refuses another name (measured on
// 0.171.0: `rollout-carried-<id>.jsonl` was refused as no canonical rollout
// filename). This one was measured to resume; the thread id alone tells
// rollouts apart, and a restore removes the thread's others ([codexRestore]).
const codexCarriedStamp = "2020-01-01T00-00-00"

// codexCarriedName is the name a carried rollout of thread is restored under.
func codexCarriedName(threadID string) string {
	return "rollout-" + codexCarriedStamp + "-" + threadID + ".jsonl"
}

// codexRestoredRollout is where a carried rollout is restored in a box: the
// sessions root, under [codexCarriedName]. "" for an id [codexRolloutPath]
// refuses.
func codexRestoredRollout(p Paths, threadID string) string {
	if !sandbox.ValidSessionID(threadID) {
		return ""
	}
	return p.CodexSessions() + "/" + codexCarriedName(threadID)
}

// codexRollout is what [readCodexRollout] read of one rollout.
type codexRollout struct {
	threadID, cliVersion, model string
	contextWindow               int64
	limits                      codexLimits
	hasLimits                   bool
	turns                       []codexTurnUsage
	torn                        bool
}

// codexTokens is one of Codex's token counts, as its rollout writes it. Input
// is the whole prompt, its cached share and cache writes INCLUDED (total_tokens
// is input plus output); Output includes the reasoning.
type codexTokens struct {
	Input           int64 `json:"input_tokens"`
	CachedInput     int64 `json:"cached_input_tokens"`
	CacheWriteInput int64 `json:"cache_write_input_tokens"`
	Output          int64 `json:"output_tokens"`
	ReasoningOutput int64 `json:"reasoning_output_tokens"`
	Total           int64 `json:"total_tokens"`
}

// plus is t and o added field by field.
func (t codexTokens) plus(o codexTokens) codexTokens {
	return codexTokens{
		Input:           t.Input + o.Input,
		CachedInput:     t.CachedInput + o.CachedInput,
		CacheWriteInput: t.CacheWriteInput + o.CacheWriteInput,
		Output:          t.Output + o.Output,
		ReasoningOutput: t.ReasoningOutput + o.ReasoningOutput,
		Total:           t.Total + o.Total,
	}
}

// spend is t as a [runSpend]: the prompt not served from the cache, the
// cache's reads and writes, and the reply. Codex states no price.
func (t codexTokens) spend() runSpend {
	return runSpend{
		input:      int(t.Input - t.CachedInput - t.CacheWriteInput),
		cacheRead:  int(t.CachedInput),
		cacheWrite: int(t.CacheWriteInput),
		output:     int(t.Output),
	}
}

// codexTurnUsage is what one turn of the session spent: the last
// token_usage_record's turn_token_usage for that turn, which Codex keeps as
// the turn's running total across its requests.
type codexTurnUsage struct {
	TurnID string
	Tokens codexTokens
}

// codexLimitWindow is one plan window of token_count.rate_limits.
type codexLimitWindow struct {
	UsedPercent float64
	Window      time.Duration
	ResetsAt    time.Time
}

// codexLimits is the plan's limits as the session's newest token_count line
// stated them. A window the plan does not have is nil (a ChatGPT plan measured
// with one weekly window and no secondary).
type codexLimits struct {
	LimitID   string
	PlanType  string
	Primary   *codexLimitWindow
	Secondary *codexLimitWindow
}

// ThreadID is the session's id, the first session_meta's `id`: the thread id
// `exec --json` reports and a resume takes. "" where no session_meta was read.
func (r codexRollout) ThreadID() string { return r.threadID }

// CLIVersion is the release that began the session, the first session_meta's
// `cli_version`, as Codex wrote it; "" where none was read.
func (r codexRollout) CLIVersion() string { return r.cliVersion }

// Model is the model the session's newest turn ran on (turn_context.model).
func (r codexRollout) Model() string { return r.model }

// ContextWindow is the newest context window Codex reported holding the model
// to (token_count.info.model_context_window), 0 where none was.
func (r codexRollout) ContextWindow() int64 { return r.contextWindow }

// Limits is the plan's newest limits, and false where no token_count line
// carried any (an API key's run carries none).
func (r codexRollout) Limits() (codexLimits, bool) { return r.limits, r.hasLimits }

// Turns is every turn's spend, in the order the turns began.
func (r codexRollout) Turns() []codexTurnUsage { return r.turns }

// Usage is the whole session's spend: every turn's, summed. A continued run's
// own is this less what the rollout it was carried held.
func (r codexRollout) Usage() codexTokens {
	var sum codexTokens
	for _, t := range r.turns {
		sum = sum.plus(t.Tokens)
	}
	return sum
}

// Torn reports that the rollout's last line had no newline: a line Codex was
// still writing, left unread.
func (r codexRollout) Torn() bool { return r.torn }

// codexLine is one rollout line's envelope, its payload undecoded.
type codexLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// The payloads the readers decode, each to the fields they read.
type (
	codexSessionMetaWire struct {
		ID         string `json:"id"`
		CLIVersion string `json:"cli_version"`
	}
	codexTurnContextWire struct {
		Model string `json:"model"`
	}
	codexUsageRecordWire struct {
		TurnID string       `json:"turn_id"`
		Turn   *codexTokens `json:"turn_token_usage"`
	}
	codexEventWire struct {
		Type string `json:"type"`
		Info *struct {
			ModelContextWindow int64 `json:"model_context_window"`
		} `json:"info"`
		RateLimits *codexRateLimitsWire `json:"rate_limits"`
	}
	codexRateLimitsWire struct {
		LimitID   string           `json:"limit_id"`
		PlanType  *string          `json:"plan_type"`
		Primary   *codexWindowWire `json:"primary"`
		Secondary *codexWindowWire `json:"secondary"`
	}
	codexWindowWire struct {
		UsedPercent   float64 `json:"used_percent"`
		WindowMinutes int64   `json:"window_minutes"`
		ResetsAt      int64   `json:"resets_at"`
	}
)

// window is w as a [codexLimitWindow], nil for none.
func (w *codexWindowWire) window() *codexLimitWindow {
	if w == nil {
		return nil
	}
	out := &codexLimitWindow{UsedPercent: w.UsedPercent, Window: time.Duration(w.WindowMinutes) * time.Minute}
	if w.ResetsAt > 0 {
		out.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
	}
	return out
}

// codexLines hands each whole line of the rollout r to each, in order, by its
// number, trimmed and valid until the next line (a blank line is passed over);
// and a line past [codexRolloutLineMaxBytes], read past and never held, to
// long. It reports whether the last line was torn (no newline), which neither
// is handed. An error from either ends the scan with it.
func codexLines(r io.Reader, each func(n int, body []byte) error, long func(n int) error) (torn bool, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	for n := 1; ; n++ {
		line, past, err := codexRolloutLine(br)
		switch {
		case errors.Is(err, io.EOF):
			return past || len(line) > 0, nil
		case err != nil:
			return false, fmt.Errorf("executor: reading codex rollout line %d: %w", n, err)
		case past:
			err = long(n)
		default:
			if body := bytes.TrimSpace(line); len(body) > 0 {
				err = each(n, body)
			}
		}
		if err != nil {
			return false, err
		}
	}
}

// codexRolloutLine is br's next line with its newline, valid until the next
// read; at the end, what is left with no newline and io.EOF. A line past
// [codexRolloutLineMaxBytes] is read to its end and dropped, past true.
func codexRolloutLine(br *bufio.Reader) (line []byte, past bool, err error) {
	chunk, err := br.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return chunk, false, err
	}
	long := append([]byte(nil), chunk...)
	for errors.Is(err, bufio.ErrBufferFull) {
		chunk, err = br.ReadSlice('\n')
		switch {
		case past:
		case len(long)+len(chunk) > codexRolloutLineMaxBytes+1:
			past, long = true, nil
		default:
			long = append(long, chunk...)
		}
	}
	return long, past, err
}

// readCodexRollout reads the rollout r ([codexLines]): a line that is no JSON
// object, or a payload of a shape the readers do not read, is skipped as an
// unknown kind is; a line past its bound fails the read ([errCodexRolloutLine]).
func readCodexRollout(r io.Reader) (codexRollout, error) {
	var out codexRollout
	turnAt := map[string]int{}
	torn, err := codexLines(r, func(_ int, body []byte) error {
		var env codexLine
		if wireDecode(body, &env) == nil {
			out.take(env, turnAt)
		}
		return nil
	}, func(n int) error {
		return fmt.Errorf("%w: line %d is over %d bytes", errCodexRolloutLine, n, codexRolloutLineMaxBytes)
	})
	if err != nil {
		return codexRollout{}, err
	}
	out.torn = torn
	return out, nil
}

// take reads one line into r; turnAt is each turn's place in r.turns.
func (r *codexRollout) take(line codexLine, turnAt map[string]int) {
	switch line.Type {
	case "session_meta":
		var meta codexSessionMetaWire
		if r.threadID == "" && wireDecode(line.Payload, &meta) == nil {
			r.threadID, r.cliVersion = meta.ID, meta.CLIVersion
		}
	case "turn_context":
		var tc codexTurnContextWire
		if wireDecode(line.Payload, &tc) == nil && tc.Model != "" {
			r.model = tc.Model
		}
	case "token_usage_record":
		var rec codexUsageRecordWire
		if wireDecode(line.Payload, &rec) != nil || rec.Turn == nil {
			return
		}
		if at, ok := turnAt[rec.TurnID]; ok {
			r.turns[at].Tokens = *rec.Turn
			return
		}
		turnAt[rec.TurnID] = len(r.turns)
		r.turns = append(r.turns, codexTurnUsage{TurnID: rec.TurnID, Tokens: *rec.Turn})
	case "event_msg":
		var ev codexEventWire
		if wireDecode(line.Payload, &ev) != nil || ev.Type != "token_count" {
			return
		}
		if ev.Info != nil && ev.Info.ModelContextWindow > 0 {
			r.contextWindow = ev.Info.ModelContextWindow
		}
		if rl := ev.RateLimits; rl != nil {
			r.hasLimits = true
			r.limits = codexLimits{LimitID: rl.LimitID, Primary: rl.Primary.window(), Secondary: rl.Secondary.window()}
			if rl.PlanType != nil {
				r.limits.PlanType = *rl.PlanType
			}
		}
	}
}
