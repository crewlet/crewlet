package cliagent

import (
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// reportedAt is 16:16 in Vancouver on 2026-10-27, the minute Claude Code first
// refused a seat with "resets 4:30pm (America/Vancouver)".
var reportedAt = time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC)

// A RESET NAMED AS A TIME OF DAY IS THE OCCURRENCE OF IT NEAREST THE REPORT, in
// the zone the vendor named, and anything this reading cannot take is no
// instant at all rather than a guess.
//
// Mutation: read the time in UTC, and the Vancouver reset lands seven hours
// early; take today's occurrence always, and "2am" at 11 pm lands in the past.
func TestAClockResetIsTheNearestOccurrenceInItsZone(t *testing.T) {
	t.Parallel()
	clock := cliprofile.ResetReading{Separator: "resets ", Unit: cliprofile.ResetClock}
	lateNight := time.Date(2026, time.October, 28, 6, 0, 0, 0, time.UTC) // 11 pm in Vancouver
	for name, tc := range map[string]struct {
		said string
		now  time.Time
		want time.Time // zero for no instant
	}{
		"the line Claude Code printed": {
			said: "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)",
			now:  reportedAt, want: time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC),
		},
		"a whole hour on the twelve-hour clock": {
			said: "You've hit your 5-hour limit · resets 7pm (America/Vancouver)",
			now:  reportedAt, want: time.Date(2026, time.October, 28, 2, 0, 0, 0, time.UTC),
		},
		"the twenty-four-hour clock": {
			said: "Usage limit reached · resets 18:45 (America/Vancouver)",
			now:  reportedAt, want: time.Date(2026, time.October, 28, 1, 45, 0, 0, time.UTC),
		},
		"another zone": {
			said: "You've hit your 5-hour limit · resets 9:15am (Asia/Tokyo)",
			now:  reportedAt, want: time.Date(2026, time.October, 28, 0, 15, 0, 0, time.UTC),
		},
		"past midnight is the coming night's": {
			said: "You've hit your 5-hour limit · resets 2am (America/Vancouver)",
			now:  lateNight, want: time.Date(2026, time.October, 28, 9, 0, 0, 0, time.UTC),
		},
		"midnight itself": {
			said: "You've hit your 5-hour limit · resets 12am (America/Vancouver)",
			now:  lateNight, want: time.Date(2026, time.October, 28, 7, 0, 0, 0, time.UTC),
		},
		"a reset that has just gone by is no instant": {
			said: "You've hit your 5-hour limit · resets 4pm (America/Vancouver)",
			now:  reportedAt,
		},
		"no zone": {said: "You've hit your 5-hour limit · resets 4:30pm", now: reportedAt},
		"a zone the database does not know": {
			said: "You've hit your 5-hour limit · resets 4:30pm (Mars/Olympus)", now: reportedAt,
		},
		"an hour past twelve on the twelve-hour clock": {
			said: "resets 13pm (America/Vancouver)", now: reportedAt,
		},
		"an hour past the day": {said: "resets 25:00 (America/Vancouver)", now: reportedAt},
		"one digit of minutes": {said: "resets 4:3pm (America/Vancouver)", now: reportedAt},
		"words where the time is": {
			said: "You've hit your 5-hour limit · resets soon (America/Vancouver)", now: reportedAt,
		},
		"no reset in the sentence": {said: "You've hit your 5-hour limit", now: reportedAt},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := resetAt(tc.said, clock, tc.now)
			if !got.Equal(tc.want) {
				t.Errorf("resetAt(%q) = %v, want %v", tc.said, got, tc.want)
			}
		})
	}
}

// AN EPOCH OR A DELAY IS READ AGAINST THE REPORT'S CLOCK, and one already past
// is no instant: the window has turned over.
func TestAnEpochOrADelayResetIsReadAgainstTheReport(t *testing.T) {
	t.Parallel()
	epoch := cliprofile.ResetReading{Separator: "|", Unit: cliprofile.ResetEpoch}
	seconds := cliprofile.ResetReading{Separator: "retry after ", Unit: cliprofile.ResetSeconds}
	ahead := reportedAt.Add(37 * time.Minute)
	if got := resetAt("limit reached|"+strconv.FormatInt(ahead.Unix(), 10), epoch, reportedAt); !got.Equal(ahead) {
		t.Errorf("epoch reset = %v, want %v", got, ahead)
	}
	if got := resetAt("limit reached|"+strconv.FormatInt(reportedAt.Add(-time.Minute).Unix(), 10), epoch, reportedAt); !got.IsZero() {
		t.Errorf("a past epoch reset = %v, want no instant", got)
	}
	if got := resetAt("rate limited, retry after 90s", seconds, reportedAt); !got.Equal(reportedAt.Add(90 * time.Second)) {
		t.Errorf("seconds reset = %v, want 90 seconds on", got)
	}
}

// THE CLAUDE CODE PROFILE CLASSIFIES THE LIMIT BY THE STATUS ITS CLI WRITES,
// not by the sentence. The line below is the result event Claude Code 2.1.312
// printed when a subscription's five-hour window ran out; its wording matches
// no sentinel the profile shipped before this change, which is how every seat
// on the entry came to retry the refusal once a second.
//
// Mutation: drop limit_events from the profile and the sentinel that names the
// new wording, and the line classifies as nothing (a server failure).
func TestClaudeCodesLimitIsReadFromItsStatusWithTheReset(t *testing.T) {
	t.Parallel()
	p, err := cliprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const line = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,` +
		`"duration_ms":412,"result":"You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)"}`
	hit, ok := classifyMarkers(p, cliOutput{said: "", stdout: line, failed: true, at: reportedAt})
	if !ok || hit.Kind != llm.KindRateLimit {
		t.Fatalf("classified %+v, %v; want a rate limit", hit, ok)
	}
	if hit.Source != sourceEvent {
		t.Errorf("matched in %q, want the CLI's own event", hit.Source)
	}
	if want := time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC); !hit.Reset.Equal(want) {
		t.Errorf("reset %v, want %v", hit.Reset, want)
	}
	if hit.Said != "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)" {
		t.Errorf("said %q, want the vendor's sentence alone", hit.Said)
	}
}

// A MODEL CANNOT WRITE THE STATUS. A healthy run whose answer spells the whole
// report out, status and all, is an answer: the fields the match reads are the
// CLI's, and the model's words are inside one string of the event.
//
// Mutation: search the line as text for the status, and the answer classifies.
func TestAnAnswerThatSpellsOutTheStatusIsAnAnswer(t *testing.T) {
	t.Parallel()
	p, err := cliprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const answer = `Here is what a refusal looks like: {"type":"result","is_error":true,` +
		`"api_error_status":429}. Nothing like it happened today.`
	stdout := `{"type":"result","subtype":"success","is_error":false,"result":` +
		strconv.Quote(answer) + `}`
	if hit, ok := classifyMarkers(p, cliOutput{said: answer, stdout: stdout, at: reportedAt}); ok {
		t.Fatalf("a healthy answer classified as %+v", hit)
	}
}

// A STRUCTURED REPORT IS ONLY DECLARED WHERE A MODEL CANNOT FORGE IT: a JSON
// event stream with a discriminator, matched on it and on a field beside it.
func TestALimitEventIsRefusedWhereAModelCouldWriteIt(t *testing.T) {
	t.Parallel()
	report := map[string]any{"match": []any{
		map[string]any{"path": []any{"type"}, "equals": "result"},
		map[string]any{"path": []any{"api_error_status"}, "equals": 429},
	}}
	for name, overrides := range map[string]map[string]any{
		"on a text stream": {
			"output": "text", "event_type_path": []any{}, "text_events": []any{},
			"limit_events": []any{report},
		},
		"matched on the discriminator alone": {
			"limit_events": []any{map[string]any{"match": []any{
				map[string]any{"path": []any{"type"}, "equals": "result"},
			}}},
		},
		"with a reset and nowhere to read it": {
			"limit_events": []any{map[string]any{"match": report["match"],
				"reset_separator": "resets ", "reset_unit": "clock"}},
		},
		"with a unit no reading takes": {
			"limit_events": []any{map[string]any{"match": report["match"], "said_path": []any{"result"},
				"reset_separator": "resets ", "reset_unit": "fortnights"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := cliprofile.Load("claude-code", overrides); err == nil {
				t.Error("the profile loaded; want it refused")
			}
		})
	}
}
