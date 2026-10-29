package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/usage"
)

// WHO READ A REPOSITORY A UNIT KEEPS TO ITSELF: the code host's fetch audit, as
// the operator tool `code_repo_readers` and the query registry's question of
// the same name — on the REST route `GET /code/repos/{name}/readers`, the
// generic form and the socket — both answer it: ONE answer ([ReadersOf]), so a
// person's script and their assistant are told the same thing in the same pages.
//
// Every read of a `visibility: unit` repository's contents is recorded where it
// was served — a git fetch whose answer carried a pack, a code tool's answer
// (types.CodeFetched) — and the usage domain keeps each node's day of them as
// one record per repository naming every reader (internal/usage, kind
// repo_read). The answer sums them across every node and day of a range: each
// reader — a credential and the seat it acted as, or a seat — with how often it
// read and how many bytes it was sent. A `visibility: company` repository's
// readers are every seat of the company by declaration, and are not recorded;
// its answer says so, and how to have them recorded.
//
// A PERSON'S QUESTION AND AN ADMIN'S (authz.ActionCodeRepoReaders): its answer
// names people and their credentials, so a seat is never given the tool — and a
// person who is no admin is refused it at the gate, before anything is read.

// CodeRepoReadersTool is the operator tool's name — its authority row's action.
const CodeRepoReadersTool = string(authz.ActionCodeRepoReaders)

// RepoReadersDefaultDays is how many company days a readers question covers
// when it names no range: THIRTY, ending today — the month a person auditing a
// unit's code asks about first, as a page's readers are asked over
// (queries.PageReadsMaxDays). A range a question names may reach back as far as
// the rows keep ([usage.RepoReadsMaxDays]).
const RepoReadersDefaultDays = 30

// RepoReadersPageBytes is the most one page of readers carries, measured as its
// rows encode ([usage.Fill]): HALF A TOOL ANSWER ([ToolAnswerBytes]), the other
// half the answer's envelope and what defusing its strings can add — and the
// same page on the REST route, so the two surfaces page alike. A page can run
// past it by one reader, whose row is under 2 KiB (usage.ReadersPerRepoDay's
// measure).
const RepoReadersPageBytes = ToolAnswerBytes / 2

// RepoRowReader is the one code read a readers question makes — the
// repository's row by name, as its asker — declared here by its consumer:
// estate.CodeSeam and the code questions' reads (internal/api/queries)
// satisfy it.
type RepoRowReader interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
}

// RepoReaders is the usage domain's readers of a repository, a page at a time —
// estate.Usage, declared here by its consumer.
type RepoReaders interface {
	RepoReaders(ctx context.Context, q usage.RepoReadsQuery, cursor string, pageBytes int) (
		estate.RepoReadersAnswer, error)
}

// ReadersQuery is a readers question as both surfaces take it: the repository by
// name, a range of company day labels — the [RepoReadersDefaultDays] ending
// today where it names neither end, ending today where it names no `to`, and
// [RepoReadersDefaultDays] back from `to` where it names no `from` — and the
// cursor of the page before ("" for the first).
type ReadersQuery struct {
	Repo     string
	From, To string
	Cursor   string
}

// ReadersAnswer is who read a repository, one page of them.
type ReadersAnswer struct {
	// Repo is the repository's name.
	Repo string `json:"repo"`

	// Recorded is whether the repository's readers are recorded at all —
	// false for a `visibility: company` repository, whose Detail says why
	// and how to have them recorded.
	Recorded bool   `json:"recorded"`
	Detail   string `json:"detail,omitempty"`

	// From and To are the range read, both company days inclusive.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	// Readers are the page's readers, in reader order: present, and empty,
	// for a recorded repository nobody read in the range.
	Readers []usage.RepoReader `json:"readers,omitzero"`

	// ReadersElided is, on the first page, how many readers the per-day cap
	// dropped over the range on every node (usage.ReadersPerRepoDay): zero
	// says the answer is complete.
	ReadersElided *int64 `json:"readers_elided,omitempty"`

	// Next is the cursor of the page after this one, absent on the last.
	Next string `json:"next,omitempty"`
}

// ReadersOf answers q as asker: the repository read by name at the seat's level
// — linearizable, as every read a person's surface makes is — and, for a
// `visibility: unit` one, one page of its readers over the range, the first
// page with how many the per-day cap dropped. today is the company's day now,
// which a range that names no end ends on.
//
// Its errors are the domain's: code.ErrRepoNotFound for a repository nobody
// declared, usage.ErrBadRange or usage.ErrBadCursor for a range or a cursor the
// read cannot take, and a routed read's own refusal where no node answered.
func ReadersOf(ctx context.Context, rows RepoRowReader, readers RepoReaders, asker code.Principal,
	q ReadersQuery, today period.Window) (ReadersAnswer, error) {

	from, to, err := readersRange(q.From, q.To, today)
	if err != nil {
		return ReadersAnswer{}, err
	}
	// THE RANGE AND THE CURSOR ARE THE CALLER'S, refused by name before
	// anything is read.
	if err := usage.ValidRange(from, to); err != nil {
		return ReadersAnswer{}, err
	}
	if _, err := usage.ParseCursor(q.Cursor); err != nil {
		return ReadersAnswer{}, err
	}
	answer, err := rows.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Asker: asker, Repo: q.Repo, Fresh: seatRead})
	if err != nil {
		return ReadersAnswer{}, err
	}
	if answer.Repo == nil {
		return ReadersAnswer{}, fmt.Errorf("builtin: the code host answered repository %s with no detail", q.Repo)
	}
	repo := answer.Repo.RepoRef
	if repo.Visibility != code.VisibilityUnit {
		return ReadersAnswer{Repo: repo.Name, Detail: fmt.Sprintf("%s is visibility: %s, so its readers are "+
			"not recorded — every seat of the company reads it by declaration. Declare it visibility: unit "+
			"in code.repos to have who reads it recorded from then on.", repo.Name, repo.Visibility)}, nil
	}
	page, err := readers.RepoReaders(ctx, usage.RepoReadsQuery{RepoID: repo.ID, From: from, To: to}, q.Cursor,
		RepoReadersPageBytes)
	if err != nil {
		return ReadersAnswer{}, err
	}
	out := ReadersAnswer{Repo: repo.Name, Recorded: true, From: from, To: to, Readers: page.Readers,
		ReadersElided: page.Elided, Next: page.Next}
	if out.Readers == nil {
		out.Readers = []usage.RepoReader{}
	}
	return out, nil
}

// readersRange is a question's range as company day labels, its missing ends
// filled from today ([ReadersQuery]); a label that is not a day is
// usage.ErrBadRange, as every other range the read refuses is.
func readersRange(from, to string, today period.Window) (string, string, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if to == "" {
		to = today.Label
	}
	if from == "" {
		end, err := usage.ParseDay("to", to)
		if err != nil {
			return "", "", err
		}
		from = end.Shift(-(RepoReadersDefaultDays - 1)).Label
	}
	return from, to, nil
}

// ReadersRefusal is the sentence a readers question's refusal quotes, and
// whether err is one: a range or a cursor the read cannot take
// (usage.ErrBadRange, usage.ErrBadCursor) — this node's judgement or a
// peer's — in the words the usage domain wrote for the caller past its
// sentinel. Anything else is a code read's failure.
//
// EXPORTED for the registry's question (internal/api/queries), which answers
// the same refusals of the same read on the REST route and the socket: one
// reading, so a person's script and their assistant quote the same words.
func ReadersRefusal(err error) (string, bool) {
	for _, sentinel := range []error{usage.ErrBadRange, usage.ErrBadCursor} {
		if !errors.Is(err, sentinel) {
			continue
		}
		if said, ok := RefusalSentence(err, sentinel); ok {
			return said, true
		}
		return err.Error(), true
	}
	return "", false
}

// ReadersDeps are what `code_repo_readers` reads through beside the code tools'
// own deps (which resolve the repository and name the caller). The zero value
// serves no tool: a node that reaches no usage copy.
type ReadersDeps struct {
	// Readers is the usage domain's readers of a repository. Nil omits the
	// tool.
	Readers RepoReaders

	// Zone is the company's ONE clock, read per call, which a range that
	// names no end is cut on; nil is UTC. Now nil is the wall clock.
	Zone func() *time.Location
	Now  func() time.Time
}

// Today is the company's day now — what a range that names no end ends on, on
// both surfaces.
func (d ReadersDeps) Today() period.Window {
	now, zone := time.Now, time.UTC
	if d.Now != nil {
		now = d.Now
	}
	if d.Zone != nil {
		if z := d.Zone(); z != nil {
			zone = z
		}
	}
	return period.At(period.Day, now(), zone)
}

// ---- code_repo_readers ---------------------------------------------------- //

type codeRepoReaders struct {
	code CodeDeps
	deps ReadersDeps
}

var _ tools.SeatCallable = (*codeRepoReaders)(nil)

func (t *codeRepoReaders) Name() string { return CodeRepoReadersTool }

func (t *codeRepoReaders) Description() string {
	return fmt.Sprintf("Who has read a `visibility: unit` repository's contents: every credential — and the "+
		"person it is bound to — and every seat that fetched it or read it through a code tool, with how "+
		"often and how many bytes, summed over a range of company days on every node, a page at a time "+
		"(pass `next` back as `cursor` until none comes back). The first page says how many readers the "+
		"record's per-day cap left out. The last %d days by default. A `visibility: company` "+
		"repository's readers are not recorded, and its answer says so.", RepoReadersDefaultDays)
}

func (t *codeRepoReaders) Parameters() map[string]any {
	day := func(what string) map[string]any {
		return map[string]any{
			"type": "string",
			"description": what + ", a company day as YYYY-MM-DD, at most " +
				fmt.Sprint(usage.RepoReadsMaxDays) + " days apart.",
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":   codeRepoArg(),
			"from":   day(fmt.Sprintf("The range's first day; %d days back from `to` when left out", RepoReadersDefaultDays)),
			"to":     day("The range's last day; today when left out"),
			"cursor": codeCursorArg(),
		},
		"required": []string{"repo"},
	}
}

func (t *codeRepoReaders) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeRepoReaders) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	_, asker, refusal := t.code.asker(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	answer, err := ReadersOf(ctx, t.code.Code, t.deps.Readers, asker, ReadersQuery{Repo: name,
		From: argString(args, "from"), To: argString(args, "to"),
		Cursor: strings.TrimSpace(argString(args, "cursor"))}, t.deps.Today())
	if err != nil {
		return readersFailure(ctx, t.Name(), name, err), nil
	}
	// A PAGE CUT WHOLE-ROW AT THE ANSWER, as it is sent: the domain's page is
	// cut at its rows' own encoding, and defusing a reader's strings can
	// lengthen it. The next page begins after the last reader kept.
	page := func(k int) any {
		cut := answer
		if answer.Readers != nil {
			cut.Readers = answer.Readers[:k]
			if k > 0 && k < len(answer.Readers) {
				cut.Next = answer.Readers[k-1].Group().Cursor()
			}
		}
		return cut
	}
	read := newForeignRead(turn, asker.Seat)
	k, rendered, err := read.fit(len(answer.Readers), page)
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(answer.Readers) > 0:
		return failed(fmt.Sprintf("%s: the first reader of %q alone is past one answer, so no page names "+
			"it whole. Read the readers through GET /code/repos/%s/readers.", t.Name(), name, name)), nil
	}
	return compactAnswer(rendered, "Ask for a shorter range with `from` and `to`.")
}

// readersFailure explains a readers question that did not answer: a range or a
// cursor the read cannot take in the caller's terms, and everything else as
// every code read explains it ([codeFailure]).
func readersFailure(ctx context.Context, tool, repo string, err error) tools.Result {
	if said, ok := ReadersRefusal(err); ok {
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused what it was asked: %s. Change "+
			"the arguments — the same ones are refused again.", tool, said))
	}
	return codeFailure(ctx, tool, repo, err)
}
