package queries

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/words"
)

// A REPOSITORY'S MERGE QUEUES AND ITS CHECKS, AS QUESTIONS:
//   - `code_queue`: live entries beside the candidates of the trains testing
//     them, per `repo`, branch (`target`, in full) or every readable repository;
//   - `code_checks`: attempts by `repo`, `change` or `oid`, at each check's
//     latest alone with `latest=true`, or the WINDOW over every readable
//     repository between `since` and `until`; narrowed by `name` and `state`,
//     or as `standing`, `recent`, `failing`;
//   - `code_check` (one attempt whole, beside its `target`), `code_check_log`;
//   - `code_commit_marks`: a history page's checks and landings, never held
//     immutable since both move.

// Each check question is decided PER REPOSITORY: registered [Requires] the
// gitlet app, held to the reader's grant (a listing omits what the asker may
// not read, a read is refused [NotPermitted]). A narrowing is the reader's
// statement's, never a filter of a page already read
// ([gitlet.CheckQuery.State]).

// Where the spec's paths moved: one commit is checked for every branch a
// change heading it merges into (gitlet.CheckKey), so an attempt's path names
// one per branch; the branch is a ref whose slashes a segment cannot carry,
// so `target` rides the query, REQUIRED. The window's `since` and `until` are
// RFC 3339 instants, never a span counted back from now, so a cursor's pages
// share one bound.

// The reasons a check question's `not_found` names ([NotFound.Reason]):
//
//   - ReasonNoCheck — the repository holds no such attempt: never queued, or
//     past the thirty days an ended attempt's rows are kept
//     ([gitlet.CheckHistory]);
//   - ReasonNoLog — the attempt holds no log, or no longer the one asked
//     for: only a failed run keeps one, and an ending a later one replaced
//     (a runner declared lost that was only slow) keeps the log of the
//     ending it now holds, whose object the sentence names.
const (
	ReasonNoCheck = "no_check"
	ReasonNoLog   = "no_log"
)

// CheckLogPageBytes is the most of a check's log one `code_check_log` answer
// carries, and what it carries where the request names no `limit`: 256 KiB.
// A failed run keeps at most [gitlet.MaxCheckLogBytes] (64 MiB), so the whole
// of the largest is 256 pages, and its tail — where a failure is printed — is
// one read from `size` back; a page is some 3,300 lines of 80 columns, which a
// screen draws as one block, and the object store reads it inside the budget
// it gives every byte up to the page's end (objstore.ReadBudget).
const CheckLogPageBytes = 256 << 10

// CheckLogs is a page of an object's bytes, as the object store reads one
// ([objstore.Store.ReadAt], which satisfies it): what `code_check_log` reads
// a failed run's log through.
type CheckLogs interface {
	ReadAt(ctx context.Context, o objstore.Object, off, n int64) ([]byte, error)
}

// CheckLogPage is one page of a check's log: the object the attempt's row
// names and its whole size, where the page starts, its bytes as text, and
// where the next page starts — absent on the page that reaches the end.
//
// EVERY FIELD IS A FUNCTION OF THE OBJECT AND THE PAGE ASKED FOR, and of
// nothing the read stamps — no position, no level, no state of the attempt —
// which is what lets an answer naming its object be [Immutable]: an object's
// key is minted for one upload and never reused (objstore.Key), so the same
// key, offset and limit are the same bytes on every node, for as long as the
// object is held.
type CheckLogPage struct {
	Object objstore.Key `json:"object"`
	Size   int64        `json:"size"`
	Offset int64        `json:"offset"`
	// Text is the page's bytes. A page that would end inside a character
	// ends before it, and Next resumes there, so no character is split
	// across two pages; a byte that is no character's is read as U+FFFD.
	Text string `json:"text"`
	Next int64  `json:"next,omitempty"`
}

// checksListing is a `code_checks` request's narrowing, refused
// [ErrBadParams] naming what is not one: a change's number, a commit's whole
// id and the branch it was checked for on the repository it names, whether
// each check's latest attempt alone is asked for, the window's two bounds, a
// check's name, a state and where each check stands in which order
// (`standing`); nil where it names none, the repository's or the window's
// whole listing. Which of them stand beside which, and an order it does not
// know, is the reader's to refuse, naming the field.
func checksListing(p Params) (*gitlet.CheckQuery, error) {
	var c gitlet.CheckQuery
	if p.Has("change") {
		n, whole := p.WholeInt("change")
		if !whole || n < 1 {
			return nil, fmt.Errorf("%w: change %q is not a change's number: a repository numbers its changes "+
				"from 1", ErrBadParams, p.String("change"))
		}
		c.Change = n
	}
	c.OID = strings.TrimSpace(p.String("oid"))
	c.Target = strings.TrimSpace(p.String("target"))
	c.Name = strings.TrimSpace(p.String("name"))
	if p.Has("latest") {
		latest, err := strconv.ParseBool(strings.TrimSpace(p.String("latest")))
		if err != nil {
			return nil, fmt.Errorf("%w: latest %q is not true or false", ErrBadParams, p.String("latest"))
		}
		c.Latest = latest
	}
	if state := strings.TrimSpace(p.String("state")); state != "" {
		c.State = gitlet.CheckState(state)
		if !c.State.Valid() {
			return nil, fmt.Errorf("%w: the state %q is not one of %s", ErrBadParams, state,
				words.Or(gitlet.CheckStates()))
		}
	}
	var err error
	if c.Since, err = instantParam(p, "since"); err != nil {
		return nil, err
	}
	if c.Until, err = instantParam(p, "until"); err != nil {
		return nil, err
	}
	c.Standing = gitlet.StandingOrder(strings.TrimSpace(p.String("standing")))
	if c == (gitlet.CheckQuery{}) {
		return nil, nil
	}
	return &c, nil
}

// checkAttempt is the one attempt a `code_check` or a `code_check_log`
// request names, refused [ErrBadParams] naming what is missing: its commit
// (`oid`) and its check (`check`) off the path, its attempt (`attempt`), a
// whole number from 1, and the branch it was checked for (`target`), in
// full. Whether each spells one is the reader's to refuse, naming it.
func checkAttempt(p Params) (*gitlet.CheckQuery, error) {
	n, whole := p.WholeInt("attempt")
	if !whole || n < 1 {
		return nil, fmt.Errorf("%w: attempt %q is not an attempt's number: a check's attempts are numbered from 1",
			ErrBadParams, p.String("attempt"))
	}
	c := &gitlet.CheckQuery{OID: strings.TrimSpace(p.String("oid")), Name: strings.TrimSpace(p.String("check")),
		Target: strings.TrimSpace(p.String("target")), Attempt: int(n)}
	if c.Target == "" {
		return nil, fmt.Errorf("%w: target: an attempt is named beside the branch it was checked for, in full "+
			"(refs/heads/main): one commit is checked for every branch a change heading it merges into",
			ErrBadParams)
	}
	return c, nil
}

// codeCheckLog answers a page (`offset`, `limit` bytes, at most and by default
// [CheckLogPageBytes]) of the log a failed run kept, the attempt read as the
// caller and held to their grant as `code_check` reads it.
//
// IMMUTABLE WHERE THE REQUEST NAMES THE OBJECT (`object`, `log.object` of
// `code_check`), bare otherwise: the attempt is NOT the bytes. An ending is a
// join (gitlet.endsBefore) that may replace an earlier one, and each ending
// keeps its own log under its own key, so an answer cached by the attempt
// would outlive the log it named; one cached by the object is a function of
// bytes no write changes. A request naming an object the attempt no longer
// keeps is refused `no_log`, naming the one it keeps, never one the
// collector may have deleted.
func (s Sources) codeCheckLog(ctx context.Context, p Params) (any, error) {
	const what = "code_check_log"
	reads, err := s.Code.CodeReads()
	if err != nil {
		return nil, err
	}
	logs, err := s.Code.CheckLogs()
	if err != nil {
		return nil, err
	}
	principal, err := caller(ctx, what)
	if err != nil {
		return nil, err
	}
	offset, limit, err := logPage(p)
	if err != nil {
		return nil, err
	}
	q, err := codeReadQuery(gitlet.ReadCheck, codeAsker(principal), p)
	if err != nil {
		return nil, err
	}
	// THE LIMIT IS THE PAGE'S, in bytes of the log, and never the read's:
	// the attempt is read whole, which a read naming a limit is refused.
	q.Limit = 0
	var named *objstore.Key
	if raw := strings.TrimSpace(p.String("object")); raw != "" {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		k, err := objstore.ParseKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: object %q is not an object's key", ErrBadParams, raw)
		}
		named = &k
	}
	answer, err := reads.Read(ctx, q)
	if err != nil {
		return nil, codeRefusal(q.Repo, err)
	}
	c := answer.Check
	switch {
	case c == nil:
		return nil, fmt.Errorf("queries: %s: the read of %s's attempt answered no attempt", what, q.Repo)
	case c.Log == nil:
		return nil, &NotFound{Reason: ReasonNoLog, Detail: fmt.Sprintf("attempt %d of check %s of %s for %s "+
			"kept no log: a failed run keeps one, and this one is %s", c.Attempt, c.Name, c.OID, c.Target, c.State)}
	case named != nil && *named != c.Log.Object:
		return nil, &NotFound{Reason: ReasonNoLog, Detail: fmt.Sprintf("attempt %d of check %s of %s for %s "+
			"keeps the log %s, not %s: its ending was replaced since; ask for that one", c.Attempt, c.Name, c.OID,
			c.Target, c.Log.Object, *named)}
	case offset > c.Log.Size:
		return nil, fmt.Errorf("%w: offset %d is past the end of a log of %d bytes", ErrBadParams, offset,
			c.Log.Size)
	}
	raw, err := logs.ReadAt(ctx, c.Log.Content(), offset, limit)
	if err != nil {
		return nil, fmt.Errorf("queries: %s: read %s: %w", what, c.Log.Object, err)
	}
	pg := CheckLogPage{Object: c.Log.Object, Size: c.Log.Size, Offset: offset}
	end := offset + int64(len(raw))
	if end < c.Log.Size {
		raw = wholeCharacters(raw)
		pg.Next = offset + int64(len(raw))
	}
	pg.Text = string(raw)
	if named != nil {
		return Immutable{Answer: pg}, nil
	}
	return pg, nil
}

// logPage is a `code_check_log` request's page: its `offset`, 0 or more and
// 0 where it names none, and its `limit`, 1 to [CheckLogPageBytes] and
// [CheckLogPageBytes] where it names none — refused [ErrBadParams] naming the
// one that is not.
func logPage(p Params) (offset, limit int64, err error) {
	limit = CheckLogPageBytes
	if p.Has("offset") {
		n, whole := p.WholeInt("offset")
		if !whole || n < 0 {
			return 0, 0, fmt.Errorf("%w: offset %q is not a whole number of bytes from the log's start",
				ErrBadParams, p.String("offset"))
		}
		offset = n
	}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		if !whole || n < 1 || n > CheckLogPageBytes {
			return 0, 0, fmt.Errorf("%w: the limit %q is not a whole number of bytes from 1 to %d, one page of "+
				"a log", ErrBadParams, p.String("limit"), CheckLogPageBytes)
		}
		limit = n
	}
	return offset, limit, nil
}

// wholeCharacters is page cut before a character its last bytes begin and do
// not finish, so the next page starts on that character — and page whole
// where cutting would leave nothing, a page shorter than one character, which
// the reader asked for and is answered as it is.
func wholeCharacters(page []byte) []byte {
	for i := len(page) - 1; i >= 0 && i >= len(page)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(page[i]) {
			continue
		}
		if !utf8.FullRune(page[i:]) && i > 0 {
			return page[:i]
		}
		return page
	}
	return page
}
