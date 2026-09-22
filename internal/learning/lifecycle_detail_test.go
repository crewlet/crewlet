package learning

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The two places the episode lifecycle shortens a model's own words — the
// pattern it puts in a log line, and the answer it quotes in a parse failure —
// and the rule both of them answer to: a cut is bounded by a NAMED budget,
// MARKED where it fell, rune-safe, and never the only copy of the value.

// TestCompactedPatternIsPreviewedInTheLogAndKeptWholeOnTheRow states where the
// pattern sentence survives: the log field is a preview, the row is the value.
//
// A cut in a log line is legitimate only because of the second half. If
// buildCompacted ever started storing the shortened sentence — or if the
// preview stopped shortening and put a pasted document in front of the four
// counts beside it — this pass would be lying about one of the two.
func TestCompactedPatternIsPreviewedInTheLogAndKeptWholeOnTheRow(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cluster := []Episode{
		{ID: "e1", Handle: "ceo", Role: "CEO", ReviewOutcome: "done", StartedAt: at, EndedAt: at},
		{ID: "e2", Handle: "ceo", Role: "CEO", ReviewOutcome: "done", StartedAt: at, EndedAt: at},
	}
	// No leading or trailing space: buildCompacted trims the sentence, which
	// is normalisation rather than a cut, and this test is about the cut.
	full := "triages inbound alerts " + strings.Repeat("and hands them on, ", 40) + "then files it"
	if len(full) <= patternLogDetail {
		t.Fatalf("the fixture is shorter than the budget (%d bytes)", len(full))
	}

	row := (&Lifecycle{}).buildCompacted("ceo", cluster, nil, Summary{CommonTaskPattern: full})
	if row.CommonTaskPattern != full {
		t.Errorf("the row lost the sentence: kept %d bytes of %d", len(row.CommonTaskPattern), len(full))
	}

	shown := patternForLog(row.CommonTaskPattern)
	if len(shown) > patternLogDetail+len("…") {
		t.Errorf("the log preview is %d bytes, past patternLogDetail (%d) + the marker", len(shown), patternLogDetail)
	}
	if !strings.HasSuffix(shown, "…") {
		t.Errorf("the log preview is not marked as cut: %q", shown)
	}
	if !strings.HasPrefix(full, strings.TrimSuffix(shown, "…")) {
		t.Errorf("the log preview is not the head of the sentence: %q", shown)
	}
}

// TestTheCompactedLogLineNamesTheRowItPreviews holds the pair that makes the
// preview legitimate: a shortened pattern and the `episode_id` the whole
// sentence can be read back from.
//
// Drop the id and the line becomes a cut with no way back — the shape the rule
// against blind trimming exists to forbid. Nothing else in the tree can notice
// that, because a log call's argument list is not a value anything inspects.
func TestTheCompactedLogLineNamesTheRowItPreviews(t *testing.T) {
	t.Parallel()

	row := Episode{ID: "ep-42", CommonTaskPattern: strings.Repeat("triage, ", 60)}
	fields := map[string]any{}
	got := compactedLogFields("ceo", row, 9, 2, 7)
	if len(got)%2 != 0 {
		t.Fatalf("the line has an unpaired field: %v", got)
	}
	for i := 0; i < len(got); i += 2 {
		key, ok := got[i].(string)
		if !ok {
			t.Fatalf("field %d is not a key: %v", i, got[i])
		}
		fields[key] = got[i+1]
	}

	if fields["episode_id"] != row.ID {
		t.Errorf("the line does not name the row it previewed: episode_id = %v, want %q",
			fields["episode_id"], row.ID)
	}
	shown, ok := fields["pattern"].(string)
	if !ok {
		t.Fatalf("pattern is not a string: %v", fields["pattern"])
	}
	if shown == row.CommonTaskPattern {
		t.Error("the pattern field was not previewed at all")
	}
	if !strings.HasSuffix(shown, "…") {
		t.Errorf("the previewed pattern is not marked: %q", shown)
	}
	for _, key := range []string{"agent_handle", "cluster_size", "exemplars_kept", "raw_deleted"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the line lost %q", key)
		}
	}
}

// TestPatternPreviewLeavesAShortSentenceAloneAndNeverSplitsARune covers the
// ordinary case and the one that only appears once a company stops writing
// ASCII.
//
// A sentence inside the budget must come back byte-identical — a marker on a
// value nothing shortened is a reader believing there is more to read. And a
// cut that lands inside a multi-byte rune yields invalid UTF-8, which a JSON
// handler silently replaces, so the log would show a replacement character
// where the model wrote a word.
func TestPatternPreviewLeavesAShortSentenceAloneAndNeverSplitsARune(t *testing.T) {
	t.Parallel()

	short := "triages inbound alerts"
	if got := patternForLog(short); got != short {
		t.Errorf("patternForLog(%q) = %q, want it untouched", short, got)
	}

	// One ASCII byte in front of three-byte runes, so the budget lands
	// INSIDE a rune rather than neatly between two of them — which is the
	// only arrangement in which a byte cut is visibly wrong.
	wide := "x" + strings.Repeat("面", patternLogDetail)
	if (patternLogDetail-len("x"))%len("面") == 0 {
		t.Fatalf("the fixture no longer straddles a rune at %d bytes", patternLogDetail)
	}
	got := patternForLog(wide)
	if !utf8.ValidString(got) {
		t.Errorf("patternForLog split a rune: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("patternForLog produced a replacement character: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a cut multi-byte pattern is not marked: %q", got)
	}
}

// TestUndecodableAnswerIsQuotedShortAndCarriedWhole is the recoverability rule
// on the one value in this package that has no other copy.
//
// The summary row is never written when the answer will not parse and no event
// carries a completion's content, so the error IS the answer's only home. Its
// message is bounded — an operator's log must not receive a model's essay —
// and the value behind it must not be.
func TestUndecodableAnswerIsQuotedShortAndCarriedWhole(t *testing.T) {
	t.Parallel()

	head := "I am sorry, I could not summarise these turns. "
	tail := "THE-TAIL-NOTHING-SHOULD-QUOTE"
	// Three-byte runes from an offset the budget does not divide, so the
	// quote's boundary falls INSIDE one: a byte cut is only visibly wrong
	// where the text is not ASCII, which is a matter of when rather than
	// whether in a company's own traffic.
	answer := "\n\n" + head + strings.Repeat("面", modelAnswerDetail) + tail
	if (modelAnswerDetail-len(head))%len("面") == 0 {
		t.Fatalf("the fixture no longer straddles a rune at %d bytes", modelAnswerDetail)
	}

	summary, err := ParseSummary(answer)
	if err == nil {
		t.Fatalf("ParseSummary accepted prose: %+v", summary)
	}

	var undecodable *UndecodableAnswerError
	if !errors.As(err, &undecodable) {
		t.Fatalf("ParseSummary(%T) is not an *UndecodableAnswerError: %v", err, err)
	}
	if undecodable.Answer != answer {
		t.Errorf("the error kept %d bytes of the %d-byte answer; it is the only copy",
			len(undecodable.Answer), len(answer))
	}

	msg := err.Error()
	// The fixed part of the message, measured rather than guessed, so this
	// bound is about the quote and not about the wording around it.
	overhead := len((&UndecodableAnswerError{}).Error())
	if len(msg) > overhead+modelAnswerDetail+len("…") {
		t.Errorf("the message is %d bytes, past modelAnswerDetail (%d) plus %d of wording",
			len(msg), modelAnswerDetail, overhead)
	}
	if strings.Contains(msg, tail) {
		t.Error("the whole answer reached the message")
	}
	if !strings.Contains(msg, head) {
		t.Errorf("the message quotes none of the answer's opening: %q", msg)
	}
	if !strings.Contains(msg, "…") {
		t.Errorf("the message does not say it cut the answer: %q", msg)
	}
	if !utf8.ValidString(msg) {
		t.Errorf("the message split a rune: %q", msg)
	}
	if strings.ContainsRune(msg, utf8.RuneError) {
		t.Errorf("the message carries a replacement character: %q", msg)
	}
	if !strings.Contains(msg, "compactor's answer") {
		t.Errorf("the message does not name what failed: %q", msg)
	}
}

// TestUndecodableAnswerMessageSpendsItsBudgetOnWords guards the one thing the
// trim in Error buys: a model that answered with a screenful of newlines and
// then a sentence must still have the sentence quoted.
func TestUndecodableAnswerMessageSpendsItsBudgetOnWords(t *testing.T) {
	t.Parallel()

	sentence := "there is no coherent pattern across these turns"
	answer := strings.Repeat("\n", modelAnswerDetail) + sentence
	err := &UndecodableAnswerError{Answer: answer}
	if !strings.Contains(err.Error(), sentence) {
		t.Errorf("the budget went on whitespace: %q", err.Error())
	}
	if err.Answer != answer {
		t.Error("Error mutated the answer it was holding")
	}
}
