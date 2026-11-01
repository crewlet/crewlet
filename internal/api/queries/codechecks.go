package queries

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A REPOSITORY'S MERGE QUEUES AND ITS CHECKS, AS QUESTIONS — what the
// dashboard's Queue and Checks sections and a change's checks tab read:
//
//   - `code_queue`, the live entries of merge queues beside the candidates
//     of the trains testing them: one repository's (`repo`), one of its
//     branches' (`target`, in full), or every repository's the asker reads;
//   - `code_checks`, attempts of checks: one repository's newest ended first
//     (`repo`), one change's of it (`change`) or one commit's (`oid`, for
//     one branch beside `target`, in full), a change's and a commit's at
//     each check's latest attempt alone where it asks (`latest=true`: what
//     each check stands at, one page), or — naming no repository — the
//     WINDOW, every repository's the asker reads whose attempts ended at or
//     after `since`; each shape narrowed by a check's `name` and a `state`;
//   - `code_check`, one attempt whole — its row, its definition, its tail,
//     the failures its ending named and the log a failed run kept — named on
//     its repository's path by its commit, its check and its attempt, beside
//     the branch it was checked for (`target`);
//   - `code_check_log`, a page of that log's bytes.
//
// Each is decided PER REPOSITORY as every code question is: registered
// [DecidedBy] `code.read`, and held to the grant the serving node derives
// from its chart by the reader — a listing leaves out what the asker may not
// read, a read of one repository it may not is refused [NotPermitted]. A
// narrowing is tested on the rows a page READ beside that grant, never in
// place of it ([code.CheckQuery.State]), so no filter widens what a page
// may hold.
//
// # Where the spec's paths moved
//
// An attempt's key holds the branch it was checked for (code.CheckKey): one
// commit is checked for every branch a change heading it merges into, so
// /checks/{oid}/{check}/{attempt} names one attempt per such branch. The
// branch is a ref — `refs/heads/release/1.x` — whose slashes a path segment
// cannot carry, so it rides the query as `target`, REQUIRED, as a change's
// listing names its repository. And the window is bounded by `since`, an
// RFC 3339 instant as `code_log`'s is, never a span counted back from
// whenever a page is read: a cursor's pages then share one bound.

// The reasons a check question's `not_found` names ([NotFound.Reason]):
//
//   - ReasonNoCheck — the repository holds no such attempt: never queued, or
//     past the thirty days an ended attempt's rows are kept
//     ([code.CheckHistory]);
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
// A failed run keeps at most [code.MaxCheckLogBytes] (64 MiB), so the whole
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
// each check's latest attempt alone is asked for, the window's lower bound, a
// check's name and a state — nil where it names none, the repository's or the
// window's whole listing. Which of them stand beside which is the reader's
// to refuse, naming the field.
func checksListing(p Params) (*code.CheckQuery, error) {
	var c code.CheckQuery
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
		c.State = code.CheckState(state)
		if !c.State.Valid() {
			return nil, fmt.Errorf("%w: the state %q is not one of %v", ErrBadParams, state, code.CheckStates())
		}
	}
	if since := strings.TrimSpace(p.String("since")); since != "" {
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return nil, fmt.Errorf("%w: since %q is not an RFC 3339 instant", ErrBadParams, since)
		}
		c.Since = at.UTC()
	}
	if c == (code.CheckQuery{}) {
		return nil, nil
	}
	return &c, nil
}

// checkAttempt is the one attempt a `code_check` or a `code_check_log`
// request names, refused [ErrBadParams] naming what is missing: its commit
// (`oid`) and its check (`check`) off the path, its attempt (`attempt`), a
// whole number from 1, and the branch it was checked for (`target`), in
// full. Whether each spells one is the reader's to refuse, naming it.
func checkAttempt(p Params) (*code.CheckQuery, error) {
	n, whole := p.WholeInt("attempt")
	if !whole || n < 1 {
		return nil, fmt.Errorf("%w: attempt %q is not an attempt's number: a check's attempts are numbered from 1",
			ErrBadParams, p.String("attempt"))
	}
	c := &code.CheckQuery{OID: strings.TrimSpace(p.String("oid")), Name: strings.TrimSpace(p.String("check")),
		Target: strings.TrimSpace(p.String("target")), Attempt: int(n)}
	if c.Target == "" {
		return nil, fmt.Errorf("%w: target: an attempt is named beside the branch it was checked for, in full "+
			"(refs/heads/main) — one commit is checked for every branch a change heading it merges into",
			ErrBadParams)
	}
	return c, nil
}

// codeCheckLog answers a page of the log a failed run of a check kept: the
// attempt read as the caller — at their freshness, held to their grant on
// its repository, as `code_check` reads it — then the page, `offset` (0
// where it names none) and `limit` bytes of it (at most, and by default,
// [CheckLogPageBytes]), read of the object store by the object the attempt's
// row names.
//
// IMMUTABLE WHERE THE REQUEST NAMES THE OBJECT (`object`, the key
// `code_check` answers as `log.object`), and bare otherwise: the attempt is
// NOT the bytes. A check's ending is a join (code.endsBefore) — a measurement
// replaces an errored ending a runner's sweep recorded, and of two failed
// endings across a writer's failover the earlier stands — and each ending
// keeps its own log, under a key of its own. An answer cached by the attempt
// alone would be served for good after the log it named was replaced; one
// cached by the object is a function of bytes no write can change. A request
// naming an object the attempt no longer keeps is refused `no_log`, naming
// the one it keeps, so a screen reads that one rather than one the collector
// may already have deleted.
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
	q, err := codeReadQuery(code.ReadCheck, codeAsker(principal), p)
	if err != nil {
		return nil, err
	}
	// THE LIMIT IS THE PAGE'S, in bytes of the log, and never the read's:
	// the attempt is read whole, which a read naming a limit is refused.
	q.Limit = 0
	var named *objstore.Key
	if raw := strings.TrimSpace(p.String("object")); raw != "" {
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
			"keeps the log %s, not %s: its ending was replaced since — ask for that one", c.Attempt, c.Name, c.OID,
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
