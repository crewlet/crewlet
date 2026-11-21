package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// GITLET'S ROWS, AS QUESTIONS: `code_repos`, `code_repo` and `code_pushes`, then
// a change's rows and routing (codechange.go), a repository's content
// (codegit.go), its merge queues and checks (codechecks.go), a search of its
// lines (codesearch.go) and section counts (codecounts.go). Each is decided PER
// REPOSITORY by the grant the serving node derives from its chart
// (gitlet.GrantOf, ADR-0088): a listing leaves out what the asker may not read,
// a read of one such repository is [NotPermitted], and the Gitlet node that
// answers judges the grant again. All sit BEHIND THE GITLET APP, registered
// [Requires] [iam.GrantGitlet], which the registry asks before an answer runs
// (ADR-0110). [DecidedBy] `code.read` would claim a decision none makes: they
// never ask the authority table, so it could gate neither REST nor the socket.

// `code_repo_readers` is the operator tool's one answer (builtin.ReadersOf), asked
// as a question of this registry so the route, the generic form and the socket
// are one implementation. It is registered [DecidedBy] its own verb and decided
// inside its answer ([Sources.codeRepoReaders]): an admin's, since it names the
// people, credentials and seats that read a unit's code, an audit no
// repository's grant gives.

// # Why questions, and not Gitlet's own routes
//
// A second implementation of what a question is, reachable only over REST, is
// what Gitlet's own routes were. As questions they are answered by one function
// on every transport, under one posture check, with the caller's freshness read
// through the one grammar ([Freshness]): a screen reads its own code writes
// back at `session`, the position the write answered with (the dashboard's
// SESSION_QUERIES). Their REST paths are the routes Gitlet had, and no other.

// # Found per question
//
// The code reads come up with the native runtime, at boot or at the first apply
// whose company sets code.backend: gitlet, after the API was assembled, so the
// source ([CodeSource]) is asked on every question and says, where this node
// serves none now, WHY: a company hosting no code is [NotFound] (`not_hosted`);
// a node applying, or failed to apply, a configuration that hosts code is
// [ErrUnavailable] under a code of its own (internal/api's code.go decides
// which, from the engine's reason, for the git routes too).

// The reasons a code question's `not_found` names ([NotFound.Reason]), a word
// a client acts on without reading the sentence, because the two are remedied
// differently:
//
//   - ReasonNotDeclared — nobody declared a repository of that name:
//     declaring one in code.repos, and waiting for the activation, is what
//     makes it;
//   - ReasonNotHosted — the company hosts no code at all (code.backend is not gitlet
//     set): declaring a repository changes nothing until it does.
//
// The git routes say the same in git's one text/plain line, where nothing
// reads a reason: the sentence is all git prints.
const (
	ReasonNotDeclared = "not_declared"
	ReasonNotHosted   = "not_hosted"
)

// RepoNotDeclared is the sentence a repository nobody declared is answered
// with, naming the setting that declares one.
func RepoNotDeclared(name string) string {
	return fmt.Sprintf("no repository named %q is declared in code.repos", name)
}

// CodeReads is the code domain's read, wherever its rows are served, and a
// read of a repository's content by object id, asked of its home Gitlet node;
// estate.CodeSeam satisfies it.
type CodeReads interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	// ResolveRef is the refs a question names by name, read in one
	// transaction at the level the query names (codegit.go).
	ResolveRef(ctx context.Context, q gitlet.RefQuery) (gitlet.RefAnswer, error)
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
	// MergePreview is what merging one commit into another would do, asked
	// of the repository's home Gitlet node by the merge its mergeability is
	// recorded by (code.merge_preview): a comparison's preview.
	MergePreview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error)
	// FloorOf is this node's own floor on a repository — the furthest of
	// its writes to it — which a read by object id is floored at beside the
	// records that moved what it reads (gitlet.ObjectFloor).
	FloorOf(repoID string) statelog.Position
}

// CodeSource is the code domain's read as THIS NODE has it, asked on every
// question: the read, or why this node serves none NOW, ALREADY in this
// surface's vocabulary — [NotFound] for a company that hosts no code,
// [ErrUnavailable] for a node that will serve it once something clears, and
// any other error a failure the surface answers as one.
//
// AN INTERFACE ANSWERING (value, error) — never a bool — because "absent" is
// several facts with several answers, and a typed nil inside an interface
// would pass a nil check and panic on the first question.
type CodeSource interface {
	CodeReads() (CodeReads, error)

	// RepoReaders is who read a `visibility: unit` repository — the usage
	// domain's readers and the company's clock a range that names no end
	// is cut on — or why this node serves none, on CodeReads' terms and in
	// the same vocabulary: the readers stand with the code reads.
	RepoReaders() (builtin.ReadersDeps, error)

	// CheckLogs is the object store a failed check's log is read from
	// (codechecks.go), or why this node serves none, on CodeReads' terms
	// and in the same vocabulary: a check's log stands with the rows that
	// name it.
	CheckLogs() (CheckLogs, error)

	// CodeSearcher is the code search over the fleet's Gitlet nodes
	// (codesearch.go), or why this node serves none, on CodeReads' terms
	// and in the same vocabulary: a search reads what the code reads do.
	CodeSearcher() (CodeSearcher, error)
}

// codeRead is the answer to one kind of code question.
//
// THE SOURCE FIRST, so a node that does not serve Gitlet says why whatever the
// request named; then the request, refused [ErrBadParams] naming what is not
// a parameter this read takes; then the read, asked AS THE CALLER and
// answered in this surface's vocabulary ([codeRefusal]).
func (s Sources) codeRead(kind gitlet.ReadKind, what string) Answer {
	return func(ctx context.Context, p Params) (any, error) {
		answer, err := s.readCode(ctx, kind, what, p)
		if err != nil {
			return nil, err
		}
		return answer, nil
	}
}

// readCode is [Sources.codeRead]'s read, its answer as the reader gave it.
func (s Sources) readCode(ctx context.Context, kind gitlet.ReadKind, what string, p Params) (gitlet.ReadAnswer,
	error) {

	reads, err := s.Code.CodeReads()
	if err != nil {
		return gitlet.ReadAnswer{}, err
	}
	// A CODE READ IS HELD TO WHO ASKS, so one asked for nobody is never
	// judged as somebody: the registry refuses an anonymous question and
	// one this node could not tell before any answer runs
	// ([Registry.AnswerWith]), [caller] holds the same rule for whatever
	// reaches this answer without it, and the reader refuses an asker no
	// principal can be.
	principal, err := caller(ctx, what)
	if err != nil {
		return gitlet.ReadAnswer{}, err
	}
	q, err := codeReadQuery(kind, codeAsker(principal), p)
	if err != nil {
		return gitlet.ReadAnswer{}, err
	}
	answer, err := reads.Read(ctx, q)
	if err != nil {
		return gitlet.ReadAnswer{}, codeRefusal(q.Repo, err)
	}
	return answer, nil
}

// codeAsker is who a code question asks as: the principal the guard resolved,
// written down as every surface writes its caller down ([builtin.ActorOf],
// over [iam.ActorFor]), then judged as the code tools judge theirs
// ([builtin.CodeAskerOf], the one conversion) — the credential it came
// through, and the human seat it holds, a person's own or the one the
// identity directory binds a token to, or none. Every principal a person
// presents reads every repository once the registry has admitted it on the
// gitlet app (ADR-0088, ADR-0110), so the answer a person gets is the whole
// listing; what the asker buys here is that the reader judges every
// read by who asked it, so a seat's read cannot be answered as a person's, nor
// a person's question and their assistant's tool call as two people.
func codeAsker(p iam.Principal) gitlet.Principal {
	return builtin.CodeAskerOf(builtin.ActorOf(p))
}

// codeReadQuery is a read's request, asked as asker, refused [ErrBadParams]
// naming the parameter that is not one this read takes. The freshness is the
// tree's one grammar ([Freshness]), so a stale listing is the default, a
// client that must not conclude "not declared" from an old copy asks for
// linearizable, and a screen reading its own write back asks for `session`
// beside the position the write answered with.
func codeReadQuery(kind gitlet.ReadKind, asker gitlet.Principal, p Params) (gitlet.ReadQuery, error) {
	fresh, err := Freshness(p)
	if err != nil {
		return gitlet.ReadQuery{}, err
	}
	q := gitlet.ReadQuery{Asker: asker, Kind: kind, Cursor: p.String("cursor"), Push: p.String("push"),
		Fresh: fresh}
	// THE REPOSITORY IS THE PATH'S, and only the reads of one take it: a
	// listing named no repository on its route, and a `name` beside it is
	// read as nothing, as the route always read it. A listing of changes
	// names its repository, where it names one, as one of its filters —
	// beside the state, the owner, the work item, the reviewer and what its
	// changes wait on, each the reader's to refuse — and a reviewer's
	// reviews are listed across repositories, by the reviewer they name.
	switch kind {
	case gitlet.ReadRepos:
		// ONE KIND, AND AN ORDER: the reader refuses a value it does not know.
		q.RepoKind, q.RepoSort = gitlet.RepoKind(p.String("kind")), gitlet.RepoSort(p.String("sort"))
	case gitlet.ReadChanges:
		q.Repo, q.State, q.Owner = p.String("repo"), gitlet.ChangeState(p.String("state")), p.String("owner")
		q.Work, q.Reviewer, q.Needs = p.String("work"), p.String("reviewer"), gitlet.Need(p.String("needs"))
		q.Window, q.ChangeSort = gitlet.ChangeWindow(p.String("window")), gitlet.ChangeSort(p.String("sort"))
		q.Readiness = gitlet.ReadinessBucket(p.String("readiness"))
		q.Text = p.String("text")
	case gitlet.ReadReviews:
		q.Reviewer = p.String("reviewer")
	case gitlet.ReadQueue:
		// A QUEUE NAMES ITS REPOSITORY AS A FILTER, as a listing of changes
		// does — every repository the asker reads where it names none — and
		// one of its branches' queues beside it, in full (refs/heads/main),
		// the reader's to refuse where it is no branch a change may name.
		q.Repo, q.Target = p.String("repo"), strings.TrimSpace(p.String("target"))
	case gitlet.ReadChecks:
		q.Repo = p.String("repo")
		if q.Checks, err = checksListing(p); err != nil {
			return gitlet.ReadQuery{}, err
		}
	case gitlet.ReadCheck:
		// ONE ATTEMPT IS THE PATH'S — its repository, its commit, its check
		// and its attempt — beside the branch it was checked for, which a
		// path cannot carry (codechecks.go).
		q.Repo = p.String("name")
		if q.Checks, err = checkAttempt(p); err != nil {
			return gitlet.ReadQuery{}, err
		}
	case gitlet.ReadCounts:
		// A SCOPE AND ITS PARTS: one repository by name, or none for every one the caller reads.
		q.Repo = p.String("repo")
		if q.Counts, err = countsParams(p); err != nil {
			return gitlet.ReadQuery{}, err
		}
	default:
		q.Repo = p.String("name")
	}
	// A REFS LISTING NAMES ITS PREFIX, and a repositories listing the
	// opening of the names it lists — a picker's typed text — each the
	// reader's to refuse where it is none it lists under.
	if kind == gitlet.ReadRefs || kind == gitlet.ReadRepos {
		q.Prefix = strings.TrimSpace(p.String("prefix"))
	}
	// A REFS LISTING'S ORDER, WINDOW, FIND AND DETAIL, each the reader's to refuse.
	if kind == gitlet.ReadRefs {
		if q.RefListing, err = refListingParams(p); err != nil {
			return gitlet.ReadQuery{}, err
		}
	}
	// A PUSH HISTORY NARROWED to one pusher and a window of instants, each the reader's to refuse.
	if kind == gitlet.ReadPushes {
		if q.PushFilter, err = pushFilterParams(p); err != nil {
			return gitlet.ReadQuery{}, err
		}
	}
	// A CHANGE IS THE PATH'S TOO, by its number on the repository — web!42's
	// 42 — on the reads of one change.
	if kind == gitlet.ReadChange || kind == gitlet.ReadChangeRouting {
		n, whole := p.WholeInt("change")
		if !whole || n < 1 {
			return gitlet.ReadQuery{}, fmt.Errorf("%w: change %q is not a change's number: a repository numbers "+
				"its changes from 1", ErrBadParams, p.String("change"))
		}
		q.Change = n
	}
	// A HISTORY PAGE'S COMMITS are the request's `commits`: comma-separated
	// in a query string, or a list of strings off a socket frame, each the
	// reader's to refuse (a whole id, once, at most gitlet.MaxCommitMarks).
	if kind == gitlet.ReadCommitMarks {
		listed, ok := p.List("commits")
		if !ok {
			return gitlet.ReadQuery{}, fmt.Errorf("%w: the commits parameter is whole commit ids, "+
				"comma-separated", ErrBadParams)
		}
		for _, v := range listed {
			for oid := range strings.SplitSeq(v, ",") {
				if oid = strings.TrimSpace(oid); oid != "" {
					q.CommitMarks = append(q.CommitMarks, oid)
				}
			}
		}
	}
	// WHO OWNS A REPOSITORY'S PATHS AND ITS RULES' MERGE GATES are answered
	// to a question that ASKS for them, `path_owners=true` and `gates=true`
	// ([gitlet.ReadQuery.PathOwners], [gitlet.ReadQuery.Gates]), and to no other:
	// at their bound they are a quarter and half a mebibyte of JSON, and a
	// question of one repository is mostly asked for its row — a screen's
	// header, polled every minute, a comparison learning its default branch.
	// Each on a read that answers no repository's detail is the reader's to
	// refuse, naming it. A HISTORY IN BRIEF ([gitlet.ReadQuery.Brief]) is
	// asked the same way, `brief=true`: a page a screen polls is several
	// hundred kilobytes heavier read whole, and `brief` and `push` on a read
	// that is no push history are the reader's to refuse.
	if q.PathOwners, err = askedFor(p, "path_owners"); err != nil {
		return gitlet.ReadQuery{}, err
	}
	if q.Gates, err = askedFor(p, "gates"); err != nil {
		return gitlet.ReadQuery{}, err
	}
	if q.Brief, err = askedFor(p, "brief"); err != nil {
		return gitlet.ReadQuery{}, err
	}
	// THE PROTECTED REPOSITORIES ALONE ([gitlet.ReadQuery.Protected]) are a
	// listing's, asked the same way, `protected=true`: the reader walks an
	// index of those alone, so a page of them costs what they cost however
	// many repositories no rule protects, and on any other read the filter
	// is the reader's to refuse, naming it.
	if q.Protected, err = askedFor(p, "protected"); err != nil {
		return gitlet.ReadQuery{}, err
	}
	// A REPOSITORY'S FACTS, asked for: the reader refuses them on any read but a listing and a detail.
	if q.RepoFacts, err = askedFor(p, "repo_facts"); err != nil {
		return gitlet.ReadQuery{}, err
	}
	// A LISTING'S CARDS are asked, `cards=true`; the reader refuses them on any other read.
	if q.Cards, err = askedFor(p, "cards"); err != nil {
		return gitlet.ReadQuery{}, err
	}
	// EVERY REPOSITORY'S TRAINS AND EJECTIONS on a queue read are asked, `trains=true`; the reader refuses them on any other read.
	if q.Trains, err = askedFor(p, "trains"); err != nil {
		return gitlet.ReadQuery{}, err
	}
	for v := range strings.SplitSeq(p.String("status"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		status := gitlet.RepoStatus(v)
		if !status.Valid() {
			return gitlet.ReadQuery{}, fmt.Errorf("%w: the status %q is not one of %s", ErrBadParams,
				v, words.Or(gitlet.RepoStatuses()))
		}
		q.Status = append(q.Status, status)
	}
	// THE ASKER'S DAYS, which the reader refuses on any read that counts no pushes by day.
	if q.DayBounds, err = dayBoundsParams(p); err != nil {
		return gitlet.ReadQuery{}, err
	}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		// int is int64 on every platform the store builds for
		// (internal/store/platform.go), so the conversion below is exact,
		// and a limit past a page's bound is the reader's to refuse, naming
		// its bound.
		if !whole || n < 0 {
			return gitlet.ReadQuery{}, fmt.Errorf("%w: the limit %q is not a whole number of zero or more "+
				"(0 is the read's own page size)", ErrBadParams, p.String("limit"))
		}
		q.Limit = int(n)
	}
	return q, nil
}

// askedFor reads a parameter that asks a read for something more, or less,
// than its default answer: `true` asks, `false` or nothing does not, and
// anything else is refused [ErrBadParams] naming the two spellings rather than
// read as either — a typo that read as "not asked" would answer without what
// the caller asked for and never say so.
func askedFor(p Params, key string) (bool, error) {
	switch v := strings.TrimSpace(p.String(key)); v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, badParams(key, v, []string{"true", "false"})
	}
}

// codeRefusal is a code read's failure in this surface's ONE vocabulary:
// [NotFound] for an undeclared repository (`not_declared`), a change with no
// such number (`no_change`), a check attempt with no row (`no_check`), a ref no
// live ref holds (`no_ref`) and an object or path a revision lacks
// (`no_object`); [ErrUnavailable] when no Gitlet node can answer now;
// [NotPermitted] for a repository the grant does not cover; [ErrBadParams] for
// what the reader refuses, with its own sentence; everything else is the
// registry's to classify ([ClassifyFailure]).
//
// NOT PERMITTED IS A REFUSAL OF THE CALLER, never an unnamed failure: every
// node judging the same grant makes it alike, so no retry or other node
// answers otherwise, and its sentence is all the caller can act on.
func codeRefusal(repo string, err error) error {
	switch {
	case errors.Is(err, gitlet.ErrRepoNotFound):
		return &NotFound{Reason: ReasonNotDeclared, Detail: RepoNotDeclared(repo)}
	case errors.Is(err, gitlet.ErrCheckNotFound):
		return &NotFound{Reason: ReasonNoCheck, Detail: refusalSentence(err, gitlet.ErrCheckNotFound)}
	case errors.Is(err, gitlet.ErrChangeNotFound):
		return &NotFound{Reason: ReasonNoChange, Detail: fmt.Sprintf("repository %q holds no change %s", repo,
			refusalSentence(err, gitlet.ErrChangeNotFound))}
	case errors.Is(err, gitlet.ErrNotPermitted):
		return &NotPermitted{Detail: refusalSentence(err, gitlet.ErrNotPermitted)}
	case errors.Is(err, gitlet.ErrRefNotFound):
		return &NotFound{Reason: ReasonNoRef, Detail: refusalSentence(err, gitlet.ErrRefNotFound)}
	case errors.Is(err, gitlet.ErrObjectNotFound), errors.Is(err, gitlet.ErrNotATree), errors.Is(err, gitlet.ErrNotABlob):
		return &NotFound{Reason: ReasonNoObject, Detail: objectSentence(err)}
	case errors.Is(err, gitlet.ErrBadLevel), errors.Is(err, gitlet.ErrBadCursor),
		errors.Is(err, gitlet.ErrInvalid), errors.Is(err, gitlet.ErrSliceTooLarge):
		return fmt.Errorf("%w: %w", ErrBadParams, err)
	case errors.As(err, new(*gitlet.ErrGitUnready)), errors.Is(err, gitlet.ErrNotServing):
		// NO GITLET NODE COULD ANSWER NOW — none holding the repository in
		// time, every one behind its configuration or stopping — which is
		// never an answer about the repository: a wait clears it.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

// objectSentence is a read by object id's absence in the domain's own words:
// an object id the repository holds no object of, or a path its revision
// holds no such directory or file at.
func objectSentence(err error) string {
	for _, sentinel := range []error{gitlet.ErrObjectNotFound, gitlet.ErrNotATree, gitlet.ErrNotABlob} {
		if said, ok := builtin.RefusalSentence(err, sentinel); ok {
			return said
		}
	}
	return err.Error()
}

// refusalSentence is the sentence err carries past its sentinel's own Go text,
// read by the one reading the code tools quote it by ([builtin.RefusalSentence]),
// and err's text whole for a sentinel that carries no sentence of its own.
func refusalSentence(err, sentinel error) string {
	if sentence, ok := builtin.RefusalSentence(err, sentinel); ok {
		return sentence
	}
	return err.Error()
}

// codeRepoReaders answers who read a `visibility: unit` repository over a range
// of company days, a page at a time: `name`, `from` and `to` (company days,
// inclusive; [builtin.RepoReadersDefaultDays] ending today if neither is
// named) and `cursor` (the previous page's `next`). It is THE OPERATOR TOOL'S
// ONE ANSWER ([builtin.ReadersOf]): the repository read by name as the caller,
// linearizable (a just-declared repository is never answered undeclared from an
// old copy; the page comes from the usage domain, so no read level is the
// caller's), then one page of readers summed over every node. A `visibility:
// company` repository's readers are not recorded, and its answer says so.
// DECIDED FIRST, by [authz.ActionCodeRepoReaders], on grants alone
// ([authz.NoChart]), so every node refuses alike. A bad range or cursor is
// [ErrBadParams] in [builtin.ReadersRefusal]'s words; else [codeRefusal].
func (s Sources) codeRepoReaders(ctx context.Context, p Params) (any, error) {
	const what = "code_repo_readers"
	asking, err := caller(ctx, what)
	if err != nil {
		return nil, err
	}
	d := authz.Decide(ctx, asking, authz.ActionCodeRepoReaders, authz.Object{}, authz.NoChart{}, s.clock())
	switch {
	case d.Unknown():
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, d.Err)
	case !d.Allowed:
		return nil, NewRefusal(what, d)
	}
	reads, err := s.Code.CodeReads()
	if err != nil {
		return nil, err
	}
	readers, err := s.Code.RepoReaders()
	if err != nil {
		return nil, err
	}
	q := builtin.ReadersQuery{Repo: p.String("name"), From: p.String("from"), To: p.String("to"),
		Cursor: p.String("cursor")}
	answer, err := builtin.ReadersOf(ctx, reads, readers.Readers, codeAsker(asking), q, readers.Today())
	if said, refusedHere := builtin.ReadersRefusal(err); refusedHere {
		return nil, fmt.Errorf("%w: %s", ErrBadParams, said)
	}
	if err != nil {
		return nil, codeRefusal(q.Repo, err)
	}
	return answer, nil
}

// registerCode registers the code questions over s.Code — see the file's
// doc, codechange.go's for a change's, codegit.go's for a repository's
// content, codechecks.go's for its merge queues and its checks and
// codesearch.go's for a search of its lines, for what each answers and why
// each is asked under the posture it is.
func registerCode(r *Registry, s Sources) {
	gated := Requires(iam.GrantGitlet)
	r.Register("code_repos", gated, s.codeRead(gitlet.ReadRepos, "code_repos"))
	r.Register("code_repo", gated, s.codeRepo())
	r.Register("code_pushes", gated, s.codeRead(gitlet.ReadPushes, "code_pushes"))
	r.Register("code_repo_readers", DecidedBy(authz.ActionCodeRepoReaders), s.codeRepoReaders)
	r.Register("code_changes", gated, s.codeRead(gitlet.ReadChanges, "code_changes"))
	r.Register("code_change", gated, s.codeChange)
	r.Register("code_change_routing", gated,
		s.codeRead(gitlet.ReadChangeRouting, "code_change_routing"))
	r.Register("code_reviews", gated, s.codeRead(gitlet.ReadReviews, "code_reviews"))
	r.Register("code_refs", gated, s.codeRead(gitlet.ReadRefs, "code_refs"))
	r.Register("code_tree", gated, s.codeTree)
	r.Register("code_find", gated, s.codeFind)
	r.Register("code_blob", gated, s.codeBlob)
	r.Register("code_log", gated, s.codeLog)
	r.Register("code_blame", gated, s.codeBlame)
	r.Register("code_tree_commits", gated, s.codeTreeCommits)
	r.Register("code_diff", gated, s.codeDiff)
	r.Register("code_tips", gated, s.codeTips)
	r.Register("code_tags", gated, s.codeTags)
	r.Register("code_compare", gated, s.codeCompare)
	r.Register("code_commit", gated, s.codeCommit)
	r.Register("code_queue", gated, s.codeRead(gitlet.ReadQueue, "code_queue"))
	r.Register("code_checks", gated, s.codeRead(gitlet.ReadChecks, "code_checks"))
	r.Register("code_check", gated, s.codeRead(gitlet.ReadCheck, "code_check"))
	r.Register("code_check_log", gated, s.codeCheckLog)
	r.Register("code_commit_marks", gated, s.codeRead(gitlet.ReadCommitMarks, "code_commit_marks"))
	r.Register("code_counts", gated, s.codeRead(gitlet.ReadCounts, "code_counts"))
	r.Register("code_search", gated, s.codeSearch)
}
