package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE HOST'S ROWS, AS QUESTIONS: `code_repos`, the repositories the asker
// may read, a page at a time; `code_repo`, one repository in detail; and
// `code_pushes`, one repository's push history. Each is decided PER
// REPOSITORY, by the grant the serving node derives from its chart
// (code.GrantOf, ADR-0058) — a listing leaves out what the asker may not read,
// and a read of one such repository is refused [NotPermitted] — so each is
// registered [DecidedBy] the authority table's `code.read`, which admits
// everybody the guard resolved and decides nothing a repository's grant does.
//
// AND A CHANGE'S ROWS AND ITS ROUTING (codechange.go): `code_changes`,
// `code_change`, `code_change_routing` and `code_reviews`, decided per
// repository alike; AND A REPOSITORY'S CONTENT (codegit.go): `code_refs`,
// `code_tree`, `code_blob`, `code_log`, `code_blame`, `code_diff`,
// `code_compare` and `code_commit`, decided per repository alike — the git
// node that answers judges the grant again; AND ITS MERGE QUEUES AND ITS CHECKS
// (codechecks.go): `code_queue`, `code_checks`, `code_check` and
// `code_check_log`, decided per repository alike.
//
// AND WHO READ A UNIT'S REPOSITORY, `code_repo_readers`: the operator tool's
// one answer (builtin.ReadersOf) — the repository read by name as the asker,
// then a page of its readers from the usage domain — asked as a question of
// this registry on the route an admin's script reads, so the route, the
// generic form and the socket are one implementation, as the rows are. It is
// registered [DecidedBy] its own verb, `code_repo_readers`, and decided inside
// its answer before anything is read ([Sources.codeRepoReaders]): an admin's
// and a person's, since the answer names the people, the credentials and the
// seats that read a unit's code — an audit no repository's grant gives.
//
// # Why questions, and not the code host's own routes
//
// They were answered by routes of the code host's own (internal/api's
// code.go), with a parameter reading and an error mapping of their own beside
// the registry's — a second implementation of what a question is, reachable
// only over REST. As questions they are answered by the same function on
// every transport, under the same posture check, and they read the caller's
// freshness through the one grammar every question does ([Freshness]): which
// is what lets a screen read its own code writes back at `session`, the
// position the write answered with as its floor (the dashboard's
// SESSION_QUERIES), as every other domain's questions are read back. Their
// REST paths are the ones the code host's routes had, through the named
// routes, and no other.
//
// # Found per question
//
// The code reads come up with the native runtime, at boot or at the first
// apply whose company sets code.native — after the API was assembled on a node
// that booted with no company — so the source ([CodeSource]) is asked for them
// on every question, and answers, where this node serves none now, WHY, in
// this surface's own vocabulary: a company hosting no code is [NotFound]
// (`not_hosted`), and a node applying, or failed to apply, a configuration
// that hosts code is [ErrUnavailable] under a code of its own (internal/api's
// code.go decides which, from the engine's reason, for the git routes too).

// The reasons a code question's `not_found` names ([NotFound.Reason]), a word
// a client acts on without reading the sentence, because the two are remedied
// differently:
//
//   - ReasonNotDeclared — nobody declared a repository of that name:
//     declaring one in code.repos, and waiting for the activation, is what
//     makes it;
//   - ReasonNotHosted — the company hosts no code at all (code.native is not
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
// read of a repository's content by object id, asked of its home git node;
// estate.CodeSeam satisfies it.
type CodeReads interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	// ResolveRef is the refs a question names by name, read in one
	// transaction at the level the query names (codegit.go).
	ResolveRef(ctx context.Context, q code.RefQuery) (code.RefAnswer, error)
	GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
	// MergePreview is what merging one commit into another would do, asked
	// of the repository's home git node by the merge its mergeability is
	// recorded by (code.merge_preview): a comparison's preview.
	MergePreview(ctx context.Context, q code.MergePreviewQuery) (code.MergePreviewAnswer, error)
	// FloorOf is this node's own floor on a repository — the furthest of
	// its writes to it — which a read by object id is floored at beside the
	// records that moved what it reads (code.ObjectFloor).
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
}

// codeRead is the answer to one kind of code question.
//
// THE SOURCE FIRST, so a node that serves no code host says why whatever the
// request named; then the request, refused [ErrBadParams] naming what is not
// a parameter this read takes; then the read, asked AS THE CALLER and
// answered in this surface's vocabulary ([codeRefusal]).
func (s Sources) codeRead(kind code.ReadKind, what string) Answer {
	return func(ctx context.Context, p Params) (any, error) {
		reads, err := s.Code.CodeReads()
		if err != nil {
			return nil, err
		}
		// A CODE READ IS HELD TO WHO ASKS, so one asked for nobody is never
		// judged as somebody: the registry refuses an anonymous question and
		// one this node could not tell before any answer runs
		// ([Registry.AnswerWith]), [caller] holds the same rule for whatever
		// reaches this answer without it, and the reader refuses an asker no
		// principal can be.
		principal, err := caller(ctx, what)
		if err != nil {
			return nil, err
		}
		q, err := codeReadQuery(kind, codeAsker(principal), p)
		if err != nil {
			return nil, err
		}
		answer, err := reads.Read(ctx, q)
		if err != nil {
			return nil, codeRefusal(q.Repo, err)
		}
		return answer, nil
	}
}

// codeAsker is who a code question asks as: the principal the guard resolved,
// written down as every surface writes its caller down ([builtin.ActorOf],
// over [iam.ActorFor]), then judged as the code tools judge theirs
// ([builtin.CodeAskerOf], the one conversion) — the credential it came
// through, and the human seat it holds, a person's own or the one the
// identity directory binds a token to, or none. Every principal a person
// presents reads every repository (ADR-0058), so the answer a person gets is
// the whole listing; what the asker buys here is that the reader judges every
// read by who asked it, so a seat's read cannot be answered as a person's, nor
// a person's question and their assistant's tool call as two people.
func codeAsker(p iam.Principal) code.Principal {
	return builtin.CodeAskerOf(builtin.ActorOf(p))
}

// codeReadQuery is a read's request, asked as asker, refused [ErrBadParams]
// naming the parameter that is not one this read takes. The freshness is the
// tree's one grammar ([Freshness]), so a stale listing is the default, a
// client that must not conclude "not declared" from an old copy asks for
// linearizable, and a screen reading its own write back asks for `session`
// beside the position the write answered with.
func codeReadQuery(kind code.ReadKind, asker code.Principal, p Params) (code.ReadQuery, error) {
	fresh, err := Freshness(p)
	if err != nil {
		return code.ReadQuery{}, err
	}
	q := code.ReadQuery{Asker: asker, Kind: kind, Cursor: p.String("cursor"), Push: p.String("push"),
		Fresh: fresh}
	// THE REPOSITORY IS THE PATH'S, and only the reads of one take it: a
	// listing named no repository on its route, and a `name` beside it is
	// read as nothing, as the route always read it. A listing of changes
	// names its repository, where it names one, as one of its filters —
	// beside the state, the owner, the work item, the reviewer and what its
	// changes wait on, each the reader's to refuse — and a reviewer's
	// reviews are listed across repositories, by the reviewer they name.
	switch kind {
	case code.ReadRepos:
	case code.ReadChanges:
		q.Repo, q.State, q.Owner = p.String("repo"), code.ChangeState(p.String("state")), p.String("owner")
		q.Work, q.Reviewer, q.Needs = p.String("work"), p.String("reviewer"), code.Need(p.String("needs"))
	case code.ReadReviews:
		q.Reviewer = p.String("reviewer")
	case code.ReadQueue:
		// A QUEUE NAMES ITS REPOSITORY AS A FILTER, as a listing of changes
		// does — every repository the asker reads where it names none — and
		// one of its branches' queues beside it, in full (refs/heads/main),
		// the reader's to refuse where it is no branch a change may name.
		q.Repo, q.Target = p.String("repo"), strings.TrimSpace(p.String("target"))
	case code.ReadChecks:
		q.Repo = p.String("repo")
		checks, err := checksListing(p)
		if err != nil {
			return code.ReadQuery{}, err
		}
		q.Checks = checks
	case code.ReadCheck:
		// ONE ATTEMPT IS THE PATH'S — its repository, its commit, its check
		// and its attempt — beside the branch it was checked for, which a
		// path cannot carry (codechecks.go).
		q.Repo = p.String("name")
		attempt, err := checkAttempt(p)
		if err != nil {
			return code.ReadQuery{}, err
		}
		q.Checks = attempt
	default:
		q.Repo = p.String("name")
	}
	// A REFS LISTING NAMES ITS PREFIX, the reader's to refuse where it is
	// none it lists under.
	if kind == code.ReadRefs {
		q.Prefix = strings.TrimSpace(p.String("prefix"))
	}
	// A CHANGE IS THE PATH'S TOO, by its number on the repository — web!42's
	// 42 — on the reads of one change.
	if kind == code.ReadChange || kind == code.ReadChangeRouting {
		n, whole := p.WholeInt("change")
		if !whole || n < 1 {
			return code.ReadQuery{}, fmt.Errorf("%w: change %q is not a change's number: a repository numbers "+
				"its changes from 1", ErrBadParams, p.String("change"))
		}
		q.Change = n
	}
	// ONE REPOSITORY IN DETAIL ANSWERS WHO OWNS ITS PATHS AND ITS RULES'
	// MERGE GATES: this question is the read of those
	// ([code.ReadQuery.PathOwners], [code.ReadQuery.Gates]), where a tool's
	// resolution of a repository reads its detail for its row alone.
	q.PathOwners = kind == code.ReadRepo
	q.Gates = kind == code.ReadRepo
	// A HISTORY IN BRIEF ([code.ReadQuery.Brief]) is asked as `true`, and
	// anything else but `false` is refused rather than read as a whole
	// history: a page a screen polls is several hundred kilobytes heavier
	// read whole. `brief` and `push` on a read that is no push history are
	// the reader's to refuse, naming them.
	switch brief := strings.TrimSpace(p.String("brief")); brief {
	case "", "false":
	case "true":
		q.Brief = true
	default:
		return code.ReadQuery{}, badParams("brief", brief, []string{"true", "false"})
	}
	for v := range strings.SplitSeq(p.String("status"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		status := code.RepoStatus(v)
		if !status.Valid() {
			return code.ReadQuery{}, fmt.Errorf("%w: the status %q is not one of %v", ErrBadParams,
				v, code.RepoStatuses())
		}
		q.Status = append(q.Status, status)
	}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		// int is int64 on every platform the store builds for
		// (internal/store/platform.go), so the conversion below is exact,
		// and a limit past a page's bound is the reader's to refuse, naming
		// its bound.
		if !whole || n < 0 {
			return code.ReadQuery{}, fmt.Errorf("%w: the limit %q is not a whole number of zero or more "+
				"(0 is the read's own page size)", ErrBadParams, p.String("limit"))
		}
		q.Limit = int(n)
	}
	return q, nil
}

// codeRefusal is a code read's failure in this surface's ONE vocabulary: a
// repository that is not declared is [NotFound] (`not_declared`), naming it,
// and so is a change it holds no such number of (`no_change`), an attempt of
// a check it holds no row of (`no_check`), a ref no live
// ref holds (`no_ref`) and an object or a path a revision does not hold
// (`no_object`); no git node able to answer now is [ErrUnavailable];
// a repository the asker's grant does not cover is [NotPermitted] with the
// reader's own sentence; a level, a cursor or a parameter the reader refuses
// is [ErrBadParams] with the reader's own sentence; and everything else — a
// read this node could not serve, a floor on another log — is the registry's
// to classify ([ClassifyFailure]), never a second list here of what clears by
// waiting.
//
// NOT PERMITTED IS A REFUSAL OF THE CALLER and never the failure everything
// unnamed falls into: every node judging the same grant from the same chart
// makes it alike, so no retry and no other node answers it otherwise, and its
// sentence — who asked, which repository, the visibility that shut the asker
// out and who does read it — is the whole of what the caller can act on.
func codeRefusal(repo string, err error) error {
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		return &NotFound{Reason: ReasonNotDeclared, Detail: RepoNotDeclared(repo)}
	case errors.Is(err, code.ErrCheckNotFound):
		return &NotFound{Reason: ReasonNoCheck, Detail: refusalSentence(err, code.ErrCheckNotFound)}
	case errors.Is(err, code.ErrChangeNotFound):
		return &NotFound{Reason: ReasonNoChange, Detail: fmt.Sprintf("repository %q holds no change %s", repo,
			refusalSentence(err, code.ErrChangeNotFound))}
	case errors.Is(err, code.ErrNotPermitted):
		return &NotPermitted{Detail: refusalSentence(err, code.ErrNotPermitted)}
	case errors.Is(err, code.ErrRefNotFound):
		return &NotFound{Reason: ReasonNoRef, Detail: refusalSentence(err, code.ErrRefNotFound)}
	case errors.Is(err, code.ErrObjectNotFound), errors.Is(err, code.ErrNotATree), errors.Is(err, code.ErrNotABlob):
		return &NotFound{Reason: ReasonNoObject, Detail: objectSentence(err)}
	case errors.Is(err, code.ErrBadLevel), errors.Is(err, code.ErrBadCursor),
		errors.Is(err, code.ErrInvalid), errors.Is(err, code.ErrSliceTooLarge):
		return fmt.Errorf("%w: %w", ErrBadParams, err)
	case errors.As(err, new(*code.ErrGitUnready)), errors.Is(err, code.ErrNotServing):
		// NO GIT NODE COULD ANSWER NOW — none holding the repository in
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
	for _, sentinel := range []error{code.ErrObjectNotFound, code.ErrNotATree, code.ErrNotABlob} {
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
// of company days, a page at a time: `name` (the path's repository), `from` and
// `to` (company days, both inclusive — the [builtin.RepoReadersDefaultDays]
// ending today where the request names neither), and `cursor` (the `next` of
// the page before).
//
// THE OPERATOR TOOL'S ONE ANSWER ([builtin.ReadersOf]), so a person's script
// and their assistant are told the same thing in the same pages: the
// repository read by name as the caller, linearizable — a repository declared
// a moment ago is never answered undeclared from an old copy, and no read
// level is the caller's to choose, since what the page is read from is the
// usage domain and not the code log — and, for a unit's repository, one page
// of its readers summed over every node, the first page with how many readers
// the per-day cap left out. A `visibility: company` repository's readers are
// not recorded, and its answer says so.
//
// DECIDED FIRST, by its own verb ([authz.ActionCodeRepoReaders]) and before
// anything else — whether this node serves the code host included — so a
// caller who may not ask is refused alike by every node, as a route's mount
// refused them: on grants alone ([authz.NoChart]), since the verb is admin's
// and asks no relation. Then the source, so a node serving none says why;
// then the read. A range or a cursor the read cannot take is [ErrBadParams] in
// the tool's own words ([builtin.ReadersRefusal]) — this node's judgement,
// before anything is read, or a peer's; everything else is a code read's
// refusal ([codeRefusal]).
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
		return nil, refused(what, d)
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
// content and codechecks.go's for its merge queues and its checks, for what
// each answers and why each is decided by the verb it is.
func registerCode(r *Registry, s Sources) {
	r.Register("code_repos", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadRepos, "code_repos"))
	r.Register("code_repo", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadRepo, "code_repo"))
	r.Register("code_pushes", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadPushes, "code_pushes"))
	r.Register("code_repo_readers", DecidedBy(authz.ActionCodeRepoReaders), s.codeRepoReaders)
	r.Register("code_changes", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadChanges, "code_changes"))
	r.Register("code_change", DecidedBy(authz.ActionCodeRead), s.codeChange)
	r.Register("code_change_routing", DecidedBy(authz.ActionCodeRead),
		s.codeRead(code.ReadChangeRouting, "code_change_routing"))
	r.Register("code_reviews", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadReviews, "code_reviews"))
	r.Register("code_refs", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadRefs, "code_refs"))
	r.Register("code_tree", DecidedBy(authz.ActionCodeRead), s.codeTree)
	r.Register("code_blob", DecidedBy(authz.ActionCodeRead), s.codeBlob)
	r.Register("code_log", DecidedBy(authz.ActionCodeRead), s.codeLog)
	r.Register("code_blame", DecidedBy(authz.ActionCodeRead), s.codeBlame)
	r.Register("code_diff", DecidedBy(authz.ActionCodeRead), s.codeDiff)
	r.Register("code_compare", DecidedBy(authz.ActionCodeRead), s.codeCompare)
	r.Register("code_commit", DecidedBy(authz.ActionCodeRead), s.codeCommit)
	r.Register("code_queue", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadQueue, "code_queue"))
	r.Register("code_checks", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadChecks, "code_checks"))
	r.Register("code_check", DecidedBy(authz.ActionCodeRead), s.codeRead(code.ReadCheck, "code_check"))
	r.Register("code_check_log", DecidedBy(authz.ActionCodeRead), s.codeCheckLog)
}
