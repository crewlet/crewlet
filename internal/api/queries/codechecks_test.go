package queries_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// checkLogArgs are the arguments that name stubCode's one failed attempt.
func checkLogArgs(extra map[string]any) map[string]any {
	args := map[string]any{"name": "web", "oid": stubOID, "check": "unit", "attempt": "1",
		"target": "refs/heads/main"}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// A CHECK'S LOG IS IMMUTABLE BY ITS OBJECT, NEVER BY ITS ATTEMPT. A page asked
// naming the object the attempt keeps (`object`, as `code_check` answers it)
// is answered queries.Immutable — sent with a strong entity tag and kept by a
// browser for good — and its bytes are the same however the read that found
// the attempt was stamped: two reads answering at different positions and
// levels give one answer. A page asked without the object is answered bare,
// since the attempt's ending — and with it the log — may be replaced by a
// later one (a runner declared lost that was only slow), and a request naming
// an object the attempt no longer keeps is refused `no_log`, naming the one
// it keeps, rather than answered with the other's bytes. A page never ends
// inside a character: it ends before it, and the next page starts on it.
//
// Mutations: answer Immutable whether or not the request named the object →
// the bare page is Immutable, red; drop the check of the named object against
// the row's → a replaced log's old key is answered with the new one's bytes,
// red; stamp the read's position on the page → the two reads differ, red; cut
// a page at its byte limit whatever it splits → the page ends inside "é",
// red.
func TestACheckLogAnswerIsImmutable(t *testing.T) {
	t.Parallel()
	at := func(seq uint64, level statelog.ReadLevel) *stubCode {
		stub := &stubCode{}
		stub.answer = func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
			return gitlet.ReadAnswer{Level: level, Position: statelog.Position{Stream: "CREWLET_CODE_LOG",
				Generation: 1, Seq: seq}, Check: stubCheck(q, int64(len(stubLog)))}, nil
		}
		return stub
	}
	named := checkLogArgs(map[string]any{"object": stubLogKey.String()})
	var bodies []string
	for _, stub := range []*stubCode{at(10, statelog.ReadStale), at(4711, statelog.ReadLinearizable)} {
		answer, err := askNative(t, queries.Sources{Code: stub}, "code_check_log", named)
		if err != nil {
			t.Fatalf("a page naming its object: %v", err)
		}
		fixed, immutable := answer.(queries.Immutable)
		if !immutable {
			t.Fatalf("a page naming its object answered %T, want queries.Immutable", answer)
		}
		page, ok := fixed.Answer.(queries.CheckLogPage)
		if !ok || page.Text != stubLog || page.Object != stubLogKey || page.Next != 0 {
			t.Fatalf("a page naming its object answered %+v, want the whole log of %s", fixed.Answer, stubLogKey)
		}
		raw, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(raw))
		if len(stub.paged) != 1 || stub.paged[0].object.Key != stubLogKey {
			t.Fatalf("the object store was asked %+v, want one page of %s", stub.paged, stubLogKey)
		}
	}
	if bodies[0] != bodies[1] {
		t.Errorf("one page of one object read at two positions answered\n  %s\n  %s\nwant the same bytes",
			bodies[0], bodies[1])
	}

	answer, err := askNative(t, queries.Sources{Code: at(10, statelog.ReadStale)}, "code_check_log",
		checkLogArgs(nil))
	if _, immutable := answer.(queries.Immutable); err != nil || immutable {
		t.Errorf("a page naming no object answered (%T, %v), want a bare page: an attempt's log may be "+
			"replaced", answer, err)
	}

	replaced := objstore.KeyAt(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	stub := at(10, statelog.ReadStale)
	_, err = askNative(t, queries.Sources{Code: stub}, "code_check_log",
		checkLogArgs(map[string]any{"object": replaced.String()}))
	var nf *queries.NotFound
	if !errors.As(err, &nf) || nf.Reason != queries.ReasonNoLog || !strings.Contains(nf.Detail, stubLogKey.String()) ||
		len(stub.paged) != 0 {
		t.Errorf("a page naming a log the attempt no longer keeps = %v, the store asked %d times; want "+
			"not_found no_log naming %s, and nothing read", err, len(stub.paged), stubLogKey)
	}

	// "é" IS TWO BYTES, the first at the page's last: the page ends before
	// it and the next starts on it.
	accented := &stubCode{logs: []byte("abcé" + strings.Repeat("x", 10))}
	answer, err = askNative(t, queries.Sources{Code: accented}, "code_check_log",
		checkLogArgs(map[string]any{"limit": "4"}))
	page, ok := answer.(queries.CheckLogPage)
	if err != nil || !ok || page.Text != "abc" || page.Next != 3 || page.Size != 15 || !utf8.ValidString(page.Text) {
		t.Errorf("a page whose limit ends inside a character answered (%+v, %v), want \"abc\" and the next "+
			"page at 3", answer, err)
	}
	answer, err = askNative(t, queries.Sources{Code: accented}, "code_check_log",
		checkLogArgs(map[string]any{"offset": "3", "limit": "4"}))
	if page, ok := answer.(queries.CheckLogPage); err != nil || !ok || page.Text != "éxx" || page.Next != 7 {
		t.Errorf("the page after it answered (%+v, %v), want \"éxx\" and the next at 7", answer, err)
	}
}

// AN ATTEMPT IS NAMED BESIDE THE BRANCH IT WAS CHECKED FOR. One commit is
// checked for every branch a change heading it merges into, so a commit, a
// check and an attempt name one attempt per branch: a `code_check` or a
// `code_check_log` request naming no `target` is refused bad_params naming
// it, before the reader is asked — never answered with whichever branch's
// attempt a read found first — and so is an attempt that is no attempt's
// number, a log page past one page's bytes and a passed run's log, which
// keeps none.
//
// Mutation: read the attempt with whatever target the request names, none
// included → the reader is asked, red.
func TestAnAttemptIsNamedBesideItsBranch(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"code_check", "code_check_log"} {
		for name, args := range map[string]map[string]any{
			"no target":           checkLogArgs(map[string]any{"target": ""}),
			"attempt 0":           checkLogArgs(map[string]any{"attempt": "0"}),
			"an attempt of words": checkLogArgs(map[string]any{"attempt": "first"}),
		} {
			stub := &stubCode{}
			if _, err := askNative(t, queries.Sources{Code: stub}, what, args); !errors.Is(err,
				queries.ErrBadParams) || len(stub.asked) != 0 {
				t.Errorf("%s, %s: answered %v, the reader asked %d times; want bad_params before any read",
					what, name, err, len(stub.asked))
			}
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_check_log", checkLogArgs(map[string]any{
		"limit": "262145"})); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a log page past %d bytes answered %v, want bad_params", queries.CheckLogPageBytes, err)
	}
	passed := &stubCode{answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		c := stubCheck(q, 0)
		c.State, c.Log = gitlet.CheckStatePassed, nil
		return gitlet.ReadAnswer{Check: c}, nil
	}}
	var nf *queries.NotFound
	if _, err := askNative(t, queries.Sources{Code: passed}, "code_check_log", checkLogArgs(nil)); !errors.As(err,
		&nf) || nf.Reason != queries.ReasonNoLog {
		t.Errorf("a passed run's log answered %v, want not_found no_log", err)
	}
}

// A CHECKS LISTING HANDS THE READER ITS NARROWING AS ASKED: the repository,
// a change's number, a commit and the branch it was checked for, each
// check's latest attempt alone, the window's two bounds, a check's name, a
// state and where each check stands, each on the read the reader holds to the
// grant; and a state that is no check's, a bound that is no instant, or a
// `latest` that is neither true nor false is refused before it is asked,
// while an order the reader does not know comes back as its refusal.
//
// Mutations: drop `state` from the narrowing → the reader is asked for every
// state, red; drop `latest` → the reader is asked for every attempt, red;
// drop `target` → the reader is asked for every branch's, red; drop
// `until` → the window is asked with no end, red; drop `standing` → the
// window is asked for its plain listing, red.
func TestAChecksListingNamesItsNarrowing(t *testing.T) {
	t.Parallel()
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_checks", map[string]any{"repo": "web",
		"change": "42", "name": "unit", "state": "failed", "oid": stubOID}); err != nil {
		t.Fatal(err)
	}
	got := stub.last()
	if got.Kind != gitlet.ReadChecks || got.Repo != "web" || got.Checks == nil || *got.Checks != (gitlet.CheckQuery{
		Change: 42, OID: stubOID, Name: "unit", State: gitlet.CheckStateFailed}) {
		t.Errorf("the reader was asked %+v of %q, want web's change 42 at %s, unit's failed attempts",
			got.Checks, got.Repo, stubOID)
	}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_checks", map[string]any{"repo": "web",
		"oid": stubOID, "target": "refs/heads/main", "latest": "true"}); err != nil {
		t.Fatal(err)
	}
	if got := stub.last(); got.Checks == nil || *got.Checks != (gitlet.CheckQuery{OID: stubOID,
		Target: "refs/heads/main", Latest: true}) {
		t.Errorf("the reader was asked %+v, want %s's checks for main at their latest attempts", got.Checks,
			stubOID)
	}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_checks", map[string]any{
		"since": "2026-10-01T00:00:00Z", "until": "2026-10-02T00:00:00Z"}); err != nil || stub.last().Repo != "" ||
		stub.last().Checks == nil || !stub.last().Checks.Since.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
		!stub.last().Checks.Until.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the window asked %+v (%v), want every repository's on 2026-10-01", stub.last(), err)
	}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_checks", map[string]any{"standing": "failing",
		"since": "2026-10-01T00:00:00Z"}); err != nil || stub.last().Checks == nil ||
		stub.last().Checks.Standing != gitlet.StandingFailing || stub.last().Repo != "" {
		t.Errorf("a standing window asked %+v (%v), want every repository's in the failing-first order", stub.last(),
			err)
	}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_checks", map[string]any{"repo": "web",
		"standing": " "}); err != nil || stub.last().Checks != nil {
		t.Errorf("an empty standing asked %+v (%v), want web's whole listing", stub.last().Checks, err)
	}
	refusing := &stubCode{answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		if q.Checks == nil || q.Checks.Standing != "sideways" {
			return gitlet.ReadAnswer{}, nil
		}
		return gitlet.ReadAnswer{}, fmt.Errorf("%w: checks.standing: %q is not one of \"recent\" or \"failing\"",
			gitlet.ErrInvalid, q.Checks.Standing)
	}}
	if _, err := askNative(t, queries.Sources{Code: refusing}, "code_checks", map[string]any{
		"standing": "sideways"}); !errors.Is(err, queries.ErrBadParams) || len(refusing.asked) != 1 {
		t.Errorf("an unknown order answered %v after %d asks, want the reader's refusal as bad_params",
			err, len(refusing.asked))
	}
	for name, args := range map[string]map[string]any{
		"a state that is no check's":  {"state": "green"},
		"a bound that is no instant":  {"since": "yesterday"},
		"an end that is no instant":   {"until": "tomorrow"},
		"a latest that is no boolean": {"repo": "web", "change": "42", "latest": "newest"},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_checks", args); !errors.Is(err,
			queries.ErrBadParams) || len(stub.asked) != 0 {
			t.Errorf("%s answered %v, the reader asked %d times; want bad_params", name, err, len(stub.asked))
		}
	}
}

// A HISTORY PAGE'S MARKS ARE THE READER'S ANSWER: `commits` (comma-separated or a
// frame's list) reach one bare `commit_marks` read; a session read with no floor is refused.
// Mutation: drop `commits` from codeReadQuery, answer it Immutable, or split no comma.
func TestCommitMarksAreTheReadersAnswer(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	marks := []gitlet.CommitMark{{Commit: a}, {Commit: b, Checks: []gitlet.TargetChecks{{
		Target: "refs/heads/main", CheckRollup: gitlet.CheckRollup{State: gitlet.RollupPassed}}}}}
	stub := &stubCode{answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		return gitlet.ReadAnswer{CommitMarks: marks, Level: q.Fresh.Level}, nil
	}}
	for _, commits := range []any{a + "," + b, []any{a, b}} {
		answer, err := askNative(t, queries.Sources{Code: stub}, "code_commit_marks", map[string]any{
			"name": "web", "commits": commits, "read_level": "linearizable"})
		if err != nil {
			t.Fatalf("the marks of %v: %v", commits, err)
		}
		got, ok := answer.(gitlet.ReadAnswer)
		if !ok || len(got.CommitMarks) != 2 || got.CommitMarks[1].Checks[0].State != gitlet.RollupPassed {
			t.Fatalf("the marks of %v answered %T %+v, want the reader's answer bare", commits, answer, answer)
		}
		asked := stub.last()
		if asked.Kind != gitlet.ReadCommitMarks || asked.Repo != "web" || len(asked.CommitMarks) != 2 ||
			asked.CommitMarks[0] != a || asked.CommitMarks[1] != b ||
			asked.Fresh.Level != statelog.ReadLinearizable {
			t.Errorf("the marks of %v asked %+v, want one commit_marks read of web's a and b, linearizable",
				commits, asked)
		}
	}
	none := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: none}, "code_commit_marks", map[string]any{"name": "web",
		"commits": a, "read_level": "session"}); !errors.Is(err, queries.ErrBadParams) || len(none.asked) != 0 {
		t.Errorf("a session read with no floor answered %v having read %d, want bad_params", err,
			len(none.asked))
	}
	if _, err := askNative(t, queries.Sources{Code: none}, "code_commit_marks", map[string]any{"name": "web",
		"commits": 7}); !errors.Is(err, queries.ErrBadParams) || len(none.asked) != 0 {
		t.Errorf("commits given as a number answered %v having read %d, want bad_params", err, len(none.asked))
	}
}
