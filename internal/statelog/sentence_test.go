package statelog_test

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE THREE REFUSALS THAT MEAN ONE THING ARE TOLD IN WORDS FOR A PERSON,
// however a domain wrapped them, and nothing else is: no package prefix, no
// subject, and a node's condition never told as a lost race.
//
// Mutation: drop any arm of [statelog.Sentence] and its rows go red; answer
// the error's own text and every row carries the subject.
func TestTheFrameworksRefusalsAreToldInWordsForAPerson(t *testing.T) {
	t.Parallel()
	wrapped := errors.New("doclet: the page kept changing under this write")
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a lost race", fmt.Errorf("%w: crewlet.iam.directory kept changing under "+
			"this write", statelog.ErrConflict), statelog.LostRace},
		{"a lost race a domain wrapped", fmt.Errorf("%w: crewlet.pages.page.p-1: %w",
			wrapped, statelog.ErrConflict), statelog.LostRace},
		{"a create of what is there", fmt.Errorf("%w: crewlet.tracker.task.t-1",
			statelog.ErrExists), statelog.AlreadyThere},
		{"a deletion", fmt.Errorf("decide: %w", &statelog.Unavailable{
			Reason: statelog.ReasonDeleted,
			Detail: "crewlet.tracker.task.t-1 was deleted and stays deleted"}),
			statelog.Deleted},
	} {
		said, ok := statelog.Sentence(c.err)
		if !ok || said != c.want {
			t.Errorf("%s is told as %q (%v), want %q", c.name, said, ok, c.want)
		}
		for _, leak := range []string{"statelog", "crewlet.", "doclet:", "—"} {
			if strings.Contains(said, leak) {
				t.Errorf("%s is told with %q in it: %s", c.name, leak, said)
			}
		}
	}
	for _, err := range []error{
		nil,
		errors.New("disk I/O error"),
		&statelog.Unavailable{Reason: statelog.ReasonBehind, Detail: "behind"},
		&statelog.Unavailable{Reason: statelog.ReasonOpReused, Detail: "reused"},
		&statelog.Refused{Code: statelog.RefuseBehind},
	} {
		if said, ok := statelog.Sentence(err); ok {
			t.Errorf("%v is told as one of the three: %q", err, said)
		}
	}
}

// EVERY CONDITION THIS BUILD NAMES IS TOLD IN WORDS FOR A PERSON, agreeing
// with the answer's Retry-After. The walk is over [statelog.Reasons] and
// [statelog.ReadRefusals], so a code added without a sentence fails here. Each
// sentence is its own (not the unknown code's); carries nothing of the detail,
// the framework's words, a code's spelling, a position or an em dash; reads as
// a sentence whose first half a tool can carry; says "try again in a moment"
// exactly where [statelog.RetryAfter] is above zero; never says "this node" or
// "ask an administrator"; and is logged at WARN exactly where no wait clears it.
//
// Mutation: delete a row of either table, give a code the wrong advice, answer
// the detail, say "this node" again, or drop the copy table or its hint, and
// rows go red.
func TestEveryConditionIsToldInWordsForAPerson(t *testing.T) {
	t.Parallel()
	const detail = "the write needs this node to have applied 3:4182 on " +
		"crewlet.tracker.task.t-1"
	unknownWrite, _ := statelog.ConditionSentence(&statelog.Unavailable{
		Reason: statelog.Reason("from_a_newer_build"), Detail: detail})
	unknownRead, _ := statelog.ConditionSentence(&statelog.Refused{
		Code: statelog.ReadRefusal("from_a_newer_build"), Detail: detail})
	type condition struct {
		name      string
		err       error
		retryable bool
		unknown   string
	}
	var conditions []condition
	for _, r := range statelog.Reasons() {
		conditions = append(conditions, condition{"write " + string(r),
			fmt.Errorf("worklet: decide: %w", &statelog.Unavailable{
				Reason: r, Detail: detail, OpID: "op-1"}),
			r.Retryable(), unknownWrite})
	}
	for _, c := range statelog.ReadRefusals {
		conditions = append(conditions, condition{"read " + string(c),
			fmt.Errorf("doclet: read: %w", &statelog.Refused{Code: c,
				Level: statelog.ReadLinearizable, Detail: detail,
				RetryAfter: statelog.RetryHint(c, 10, 1)}),
			c.Retryable(), unknownRead})
	}
	if len(conditions) != len(statelog.Reasons())+len(statelog.ReadRefusals) ||
		len(conditions) < 31 {
		t.Fatalf("walked %d conditions", len(conditions))
	}
	for _, c := range conditions {
		said, ok := statelog.ConditionSentence(c.err)
		if !ok {
			t.Errorf("%s is not told at all", c.name)
			continue
		}
		if said == c.unknown {
			t.Errorf("%s is told as a code this build does not know: %q", c.name, said)
		}
		for _, leak := range []string{"statelog", "crewlet.", "3:4182", "op-1",
			"worklet:", "doclet:", "_", "—", "(", ":"} {
			if strings.Contains(said, leak) {
				t.Errorf("%s is told with %q in it: %s", c.name, leak, said)
			}
		}
		first, _ := utf8.DecodeRuneInString(said)
		if !unicode.IsUpper(first) || !strings.HasSuffix(said, ".") {
			t.Errorf("%s does not read as a sentence: %q", c.name, said)
		}
		what, next, ok := statelog.ConditionWords(c.err)
		if !ok || what == "" || unicode.IsUpper([]rune(what)[0]) ||
			strings.HasSuffix(what, ".") {
			t.Errorf("%s's first half is not a clause a tool can carry: %q", c.name, what)
		}
		if !strings.HasPrefix(said, strings.ToUpper(what[:1])+what[1:]) ||
			!strings.HasSuffix(said, next) {
			t.Errorf("%s's halves %q and %q are not its sentence %q", c.name,
				what, next, said)
		}
		waits := statelog.RetryAfter(c.err, time.Second) > 0
		if waits != c.retryable {
			t.Fatalf("%s: RetryAfter and Retryable disagree", c.name)
		}
		if soon := next == statelog.TryAgainSoon; soon != waits {
			t.Errorf("%s carries a Retry-After: %v, but tells a person %q", c.name,
				waits, next)
		}
		if !waits && strings.Contains(said, "in a moment") {
			t.Errorf("%s clears on no wait and asks for one: %q", c.name, said)
		}
		// NOT "THIS NODE": the estate router hands a data node's refusal
		// back as the asking node's answer. Nor an administrator asked for on
		// a surface whose reader is one, nor every record called full when
		// one area's log is.
		for _, wrong := range []string{"this node", "check this one",
			"readmit this one", "ask an administrator", "records are full"} {
			if strings.Contains(strings.ToLower(said), wrong) {
				t.Errorf("%s says %q: %s", c.name, wrong, said)
			}
		}
		if level := statelog.ConditionLevel(c.err, slog.LevelDebug); (level ==
			slog.LevelWarn) == waits || (waits && level != slog.LevelDebug) {
			t.Errorf("%s (Retry-After %v) is logged at %s", c.name, waits, level)
		}
	}
	// ANOTHER NODE'S COPY under a gate that blames its writer waits out the
	// log's duplicate window, asks for a resend in a few minutes (never a
	// readmit or a remake), and logs at the cleared level.
	for _, r := range statelog.Reasons() {
		if !r.BlamesWriter() {
			continue
		}
		err := fmt.Errorf("worklet: decide: %w", &statelog.Unavailable{
			Reason: r, Detail: detail, CopyWriter: "node-b",
			RetryAfter: 2 * time.Minute})
		said, _ := statelog.ConditionSentence(err)
		_, next, _ := statelog.ConditionWords(err)
		if statelog.RetryAfter(err, time.Second) != 2*time.Minute ||
			next != statelog.TryAgainLater || strings.Contains(said, "node-b") ||
			strings.Contains(said, "readmit") || strings.Contains(said, "make the change") {
			t.Errorf("another node's %s copy is told %q and waits %s", r, said,
				statelog.RetryAfter(err, time.Second))
		}
		if level := statelog.ConditionLevel(err, slog.LevelInfo); level != slog.LevelInfo {
			t.Errorf("another node's %s copy is logged at %s", r, level)
		}
	}
	// A DELETION IS TOLD AS [statelog.Sentence] TELLS IT, since both are the
	// same refusal.
	if said, _ := statelog.ConditionSentence(&statelog.Unavailable{
		Reason: statelog.ReasonDeleted, Detail: detail}); said != statelog.Deleted {
		t.Errorf("a deletion is told as %q, want %q", said, statelog.Deleted)
	}
	// A CODE FROM A NEWER PEER is told, and told that no wait clears it, as
	// [statelog.RetryAfter] answers it.
	for _, said := range []string{unknownWrite, unknownRead} {
		if said == "" || strings.Contains(said, "from_a_newer_build") ||
			strings.Contains(said, "in a moment") {
			t.Errorf("a code this build does not know is told as %q", said)
		}
	}
	// A BARE SENTINEL a caller wrapped is a condition all the same, which
	// says nothing about which one, and [statelog.RetryAfter] answers it
	// with the caller's own hint — so it asks for a try in a moment.
	bare := fmt.Errorf("chatlet: read: %w", statelog.ErrUnavailable)
	if said, ok := statelog.ConditionSentence(bare); !ok ||
		!strings.HasSuffix(said, statelog.TryAgainSoon) ||
		strings.Contains(said, "statelog") || statelog.RetryAfter(bare, time.Second) == 0 {
		t.Errorf("a bare sentinel is told as %q (%v)", said, ok)
	}
	// NOTHING ELSE IS A CONDITION: an error that wraps none is not one, and a
	// lost race is one of the three [statelog.Sentence] tells.
	for _, err := range []error{nil, statelog.ErrConflict, statelog.ErrExists,
		errors.New("disk I/O error")} {
		if said, ok := statelog.ConditionSentence(err); ok {
			t.Errorf("%v is told as a condition: %q", err, said)
		}
	}
}
