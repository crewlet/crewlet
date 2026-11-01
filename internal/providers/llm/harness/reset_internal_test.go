package harness

import (
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// reportedAt is 16:16 in Vancouver on 2026-10-27, the minute Claude Code first
// refused a seat with "resets 4:30pm (America/Vancouver)".
var reportedAt = time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC)

// A RESET NAMED AS A TIME OF DAY IS ITS NEXT OCCURRENCE AFTER THE REPORT, in
// the zone the vendor named, else in the zone the CLI was handed, and anything
// this reading cannot take is no instant at all rather than a guess. A weekday
// is that day's next occurrence strictly after the report; a date carries its
// own day.
//
// Mutation: read the time in UTC, and the Vancouver reset lands seven hours
// early; take today's occurrence always, and "2am" at 11 pm lands in the past;
// take the nearest occurrence, and a reset 13 or 23 hours ahead reads as this
// morning's, which is no instant; drop the grace, and one a minute gone by is
// tomorrow's; ignore the child's zone, and a zone-less sentence reads in UTC;
// let a weekday be today when its hour has passed, and a weekly reset is
// read as already over.
func TestAClockResetIsTheNextOccurrenceInItsZone(t *testing.T) {
	t.Parallel()
	clock := harnessprofile.ResetReading{Separator: "resets ", Unit: harnessprofile.ResetClock}
	codex := harnessprofile.ResetReading{Separator: "try again at ", Unit: harnessprofile.ResetClock}
	vancouver, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		t.Fatal(err)
	}
	lateNight := time.Date(2026, time.October, 28, 6, 0, 0, 0, time.UTC) // 11 pm in Vancouver
	tuesday := reportedAt                                                // Tuesday 4:16 pm in Vancouver
	for name, tc := range map[string]struct {
		said    string
		now     time.Time
		reading *harnessprofile.ResetReading // nil for Claude Code's "resets "
		child   *time.Location               // the zone the CLI was handed; nil for UTC
		want    time.Time                    // zero for no instant
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
		"thirteen hours ahead is tomorrow morning's": {
			said: "You've hit your weekly limit · resets 5:16am (America/Vancouver)",
			now:  reportedAt, want: time.Date(2026, time.October, 28, 12, 16, 0, 0, time.UTC),
		},
		"twenty-three hours ahead is tomorrow afternoon's": {
			said: "You've hit your weekly limit · resets 3:16pm (America/Vancouver)",
			now:  reportedAt, want: time.Date(2026, time.October, 28, 22, 16, 0, 0, time.UTC),
		},
		"a reset that has just gone by is no instant": {
			said: "You've hit your 5-hour limit · resets 4:15pm (America/Vancouver)",
			now:  reportedAt,
		},
		"a reset past the grace is tomorrow's": {
			said: "You've hit your 5-hour limit · resets 4pm (America/Vancouver)",
			now:  reportedAt, want: time.Date(2026, time.October, 28, 23, 0, 0, 0, time.UTC),
		},
		"no zone is the child's zone": {
			said: "You've hit your 5-hour limit · resets 4:30pm", now: reportedAt, child: vancouver,
			want: time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC),
		},
		"no zone in a UTC child": {
			said: "You've hit your 5-hour limit · resets 11:30pm", now: reportedAt,
			want: time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC),
		},
		"a named zone outranks the child's": {
			said: "resets 9:15am (Asia/Tokyo)", now: reportedAt, child: vancouver,
			want: time.Date(2026, time.October, 28, 0, 15, 0, 0, time.UTC),
		},
		"a weekday later in the week": {
			said: "You've hit your weekly limit · resets Thu 9am (America/Vancouver)", now: tuesday,
			want: time.Date(2026, time.October, 29, 16, 0, 0, 0, time.UTC),
		},
		"a weekday spelled out": {
			said: "resets Thursday 9:00 am (America/Vancouver)", now: tuesday,
			want: time.Date(2026, time.October, 29, 16, 0, 0, 0, time.UTC),
		},
		"today's weekday after its hour is next week's": {
			said: "resets Tue 9am (America/Vancouver)", now: tuesday,
			want: time.Date(2026, time.November, 3, 9, 0, 0, 0, vancouver),
		},
		"today's weekday before its hour is today's": {
			said: "resets Tue 5pm (America/Vancouver)", now: tuesday,
			want: time.Date(2026, time.October, 28, 0, 0, 0, 0, time.UTC),
		},
		"a date with its year, as Codex writes one": {
			said: "You've hit your usage limit. Try again at Nov 3rd, 2026 3:45 PM.", now: tuesday,
			reading: &codex, child: vancouver,
			want: time.Date(2026, time.November, 3, 15, 45, 0, 0, vancouver),
		},
		"a time of day as Codex writes one": {
			said: "You've hit your usage limit. Upgrade to Pro, visit the usage page or try again at 6:02 PM.",
			now:  tuesday, reading: &codex, child: vancouver,
			want: time.Date(2026, time.October, 28, 1, 2, 0, 0, time.UTC),
		},
		"a date with no year is its next": {
			said: "resets Jan 2nd, 9am (America/Vancouver)", now: tuesday,
			want: time.Date(2027, time.January, 2, 9, 0, 0, 0, vancouver),
		},
		"a date that does not exist": {said: "resets Feb 30th, 2027 9am (America/Vancouver)", now: tuesday},
		"a date in the past":         {said: "resets Oct 1st, 2026 9am (America/Vancouver)", now: tuesday},
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
			reading := clock
			if tc.reading != nil {
				reading = *tc.reading
			}
			got := resetAt(tc.said, reading, tc.now, tc.child)
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
	epoch := harnessprofile.ResetReading{Separator: "|", Unit: harnessprofile.ResetEpoch}
	seconds := harnessprofile.ResetReading{Separator: "retry after ", Unit: harnessprofile.ResetSeconds}
	ahead := reportedAt.Add(37 * time.Minute)
	if got := resetAt("limit reached|"+strconv.FormatInt(ahead.Unix(), 10), epoch, reportedAt, nil); !got.Equal(ahead) {
		t.Errorf("epoch reset = %v, want %v", got, ahead)
	}
	if got := resetAt("limit reached|"+strconv.FormatInt(reportedAt.Add(-time.Minute).Unix(), 10), epoch, reportedAt, nil); !got.IsZero() {
		t.Errorf("a past epoch reset = %v, want no instant", got)
	}
	if got := resetAt("rate limited, retry after 90s", seconds, reportedAt, nil); !got.Equal(reportedAt.Add(90 * time.Second)) {
		t.Errorf("seconds reset = %v, want 90 seconds on", got)
	}
}

// THE CLAUDE CODE PROFILE CLASSIFIES THE LIMIT BY THE STATUS ITS CLI WRITES,
// not by the sentence. The line below is the result event Claude Code 2.1.312
// printed when a subscription's five-hour window ran out; its wording matches
// no sentinel the profile shipped before this change, which is how every seat
// on the entry came to retry the refusal once a second.
//
// Mutation: drop limit_events from the profile, and the line classifies as
// nothing (a server failure).
func TestClaudeCodesLimitIsReadFromItsStatusWithTheReset(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const line = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,` +
		`"duration_ms":412,"result":"You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)"}`
	hit, ok := classifyMarkers(p, harnessOutput{said: "", stdout: line, failed: true, at: reportedAt})
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
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const answer = `Here is what a refusal looks like: {"type":"result","is_error":true,` +
		`"api_error_status":429}. Nothing like it happened today.`
	stdout := `{"type":"result","subtype":"success","is_error":false,"result":` +
		strconv.Quote(answer) + `}`
	if hit, ok := classifyMarkers(p, harnessOutput{said: answer, stdout: stdout, at: reportedAt}); ok {
		t.Fatalf("a healthy answer classified as %+v", hit)
	}
}

// AN ANSWER THAT OPENS WITH THE VENDOR'S WORDS IS AN ANSWER. Claude Code
// 2.1.312 opens its limit report with "You've hit your", and so does an answer
// about a quarterly target; a sentinel on those words matched the answer where
// it opens a clean run, and benched a working entry for every seat on the
// node. The report is read from the status its result event carries, which a
// clean run's does not.
//
// Mutation: add "You've hit your" back as a limit sentinel, and the answer
// classifies as a limit.
func TestAnAnswerOpeningWithTheLimitsWordsIsAnAnswer(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const answer = "You've hit your Q3 target with two weeks to spare."
	stdout := `{"type":"result","subtype":"success","is_error":false,"result":` +
		strconv.Quote(answer) + `}`
	if hit, ok := classifyMarkers(p, harnessOutput{said: answer, stdout: stdout, at: reportedAt}); ok {
		t.Fatalf("a clean answer classified as %+v", hit)
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
			if _, err := harnessprofile.Load("claude-code", overrides); err == nil {
				t.Error("the profile loaded; want it refused")
			}
		})
	}
}
