package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
)

// THE RELEASE REQUESTS (ADR-0125): an agent asks to merge a change or to
// deploy a commit with a tool that holds the call on its run, as ssh_operate
// does, so a merge and a deploy are put to a delegate or a person through the
// one path every held call takes: delegated, hidden until escalated, bound by
// the deadline, the kill switch, the audit and the digest.
//
// # What an approval carries out
//
// ask_to_merge's approval MERGES, by the engine, exactly the head the call
// named, as the requesting seat: a Gitlet change through code_merge_change
// (its target's own gate still decides, so no approval makes a seat a person
// the gate asks for), a GitHub pull request through GitHub's merge with that
// head's SHA, as the seat's own app. A change pushed to since is not merged.
//
// ask_to_deploy's approval is the approval itself, handed back to the run:
// Crewlet has no deploy path of its own, so the agent deploys with its own
// tools, and what was approved (the commit, the changes, the environment and
// the rollback) is the digest the verdict is bound to.
//
// # What a delegate's allow needs
//
// Read from the code host when the verdict is given ([ReleaseDeps.unmet]): for
// a merge, the head asked about, open, every check its target requires passed
// on that head, mergeable, every other precondition of the target's gate met,
// an approving review of that head by somebody other than its authors and the
// requester, and no always-escalate path; for a deploy, every change merged
// into one branch, the commit holding each and on that branch, every check the
// branch requires passed on the commit, and no always-escalate path in any
// change. GitLab merge requests are not release requests: GitLab records no
// approval of one commit, so nothing could bind an allow to the head shown.

// The release tools' names, each its authority row's action.
const (
	AskToMergeTool  = toolname.AskToMerge
	AskToDeployTool = toolname.AskToDeploy
)

// The release requests' bounds.
const (
	// releaseReasonMaxRunes is a reason's bound, ssh_operate's.
	releaseReasonMaxRunes = sshReasonMaxRunes
	// rollbackMaxRunes bounds a deploy's rollback note: a paragraph, the
	// command or commit to go back to and how to tell it worked.
	rollbackMaxRunes = 1000
	// deployMaxChanges bounds the changes one deploy names: a release train
	// is a handful; twenty is a week of merges, past which a deploy is a
	// person's to approve whole.
	deployMaxChanges = 20
	// environmentMaxBytes bounds an environment's name.
	environmentMaxBytes = 64
	// releaseReviewPages bounds the reviews a merge's conditions read, newest
	// first: an approval of the head is among a change's newest reviews.
	releaseReviewPages = 4
	// releaseFilePages bounds the pages of a revision's files read for the
	// always-escalate paths; past it, the change goes to people.
	releaseFilePages = 10
)

// The merge methods a release request names: the two both hosts take.
const (
	releaseMerge  = "merge"
	releaseSquash = "squash"
)

// environmentName is an environment's name, compiled at first use.
var environmentName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
})

// PullRef is a GitHub pull request, or with no number its repository, as its
// address names it on the company's GitHub.
type PullRef struct {
	Owner, Repo string
	Number      int
}

// String is the pull request as GitHub writes it, acme/web#42.
func (r PullRef) String() string {
	if r.Number == 0 {
		return r.Owner + "/" + r.Repo
	}
	return r.Owner + "/" + r.Repo + "#" + strconv.Itoa(r.Number)
}

// The results a required check has on a commit.
const (
	CheckPassed  = "passed"
	CheckFailed  = "failed"
	CheckPending = "pending"
	CheckMissing = "missing"
)

// CheckFact is one required check's result on a commit.
type CheckFact struct {
	Name, Result string
}

// PullFacts are a pull request as a release request's conditions read it.
// Author and Approvers are seat handles where a seat's app wrote them, and
// GitHub logins otherwise.
type PullFacts struct {
	Title, Author           string
	Open, Merged, Draft     bool
	Head, Base, MergeCommit string
	// Mergeable is nil while GitHub computes it; MergeableState is GitHub's
	// word for why (clean, blocked, behind, dirty, unstable).
	Mergeable      *bool
	MergeableState string
	Labels         []string
	// Approvers are the reviewers other than the author whose latest review
	// approved Head.
	Approvers []string
	// Required is each check the base branch requires, with its result on
	// Head; RequiredRead is false where the list could not be read.
	Required     []CheckFact
	RequiredRead bool
	// Paths are the paths it touches; PathsComplete false past GitHub's cap.
	Paths         []string
	PathsComplete bool
}

// PullRequests are the company's GitHub pull requests as the release requests
// read and merge them, each as the requesting seat's own app: declared by its
// consumer, the engine's GitHub wiring satisfies it.
type PullRequests interface {
	// Ref is the pull request address names on the company's GitHub under an
	// owner it names; false for any other address.
	Ref(address string) (PullRef, bool)
	// Repo is the repository address names there, likewise.
	Repo(address string) (PullRef, bool)
	// Facts are a pull request's facts, read as seat.
	Facts(ctx context.Context, seat string, ref PullRef) (PullFacts, error)
	// Contains reports whether commit holds ancestor, in repo.
	Contains(ctx context.Context, seat string, repo PullRef, commit, ancestor string) (bool, error)
	// OnBranch reports whether branch's tip holds commit, in repo.
	OnBranch(ctx context.Context, seat string, repo PullRef, branch, commit string) (bool, error)
	// Checks are the results on commit of every check branch requires, and
	// whether that list could be read.
	Checks(ctx context.Context, seat string, repo PullRef, branch, commit string) ([]CheckFact, bool, error)
	// Merge merges the pull request at exactly head, as seat's app, and
	// answers the commit its base moved to.
	Merge(ctx context.Context, seat string, ref PullRef, head, method string) (string, error)
}

// GitHub's refusals of a merge, as the engine's wiring names them.
var (
	ErrPullMoved        = errors.New("builtin: the pull request's head is not the one approved")
	ErrPullNotMergeable = errors.New("builtin: GitHub does not merge the pull request as it stands")
	ErrPullForbidden    = errors.New("builtin: the seat's GitHub app may not merge the pull request")
)

// ReleaseDeps are what the release requests read and act through: Gitlet's
// code tools for its changes, and the company's GitHub for pull requests. A
// company running neither is offered neither tool.
type ReleaseDeps struct {
	Code  CodeDeps
	Pulls PullRequests
}

// gitlet reports Gitlet served whole: its reads, which a request's conditions
// make, and its writes, which an approved merge makes.
func (d ReleaseDeps) gitlet() bool { return d.Code.Code != nil && d.Code.Writer != nil }

func (d ReleaseDeps) serves() bool { return d.gitlet() || d.Pulls != nil }

// releaseCandidates are the release tools as a registry's candidates.
func releaseCandidates(deps ReleaseDeps) []candidate {
	on := deps.serves()
	return []candidate{{&askToMerge{deps: deps}, on}, {&askToDeploy{deps: deps}, on}}
}

// releaseAnnotations classifies one release tool; false for a name that is
// not one. Each carries out what reaches production, so a worker never holds
// one.
func releaseAnnotations(name string) (tools.Annotations, bool) {
	switch name {
	case AskToMergeTool, AskToDeployTool:
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, Idempotent: mcp.No,
			OpenWorld: mcp.Yes}, true
	}
	return tools.Annotations{}, false
}

// definitionDigest is a tool's definition's digest, which a resume holds a
// held call to.
func definitionDigest(t tools.Callable) string {
	data, err := json.Marshal(map[string]any{"name": t.Name(), "description": t.Description(),
		"parameters": t.Parameters()})
	if err != nil {
		panic(err) // a literal map of strings always encodes
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// releaseReason reads a call's reason: one line of up to
// [releaseReasonMaxRunes] characters, which the approver reads.
func releaseReason(tool string, args map[string]any) (string, *tools.Result) {
	reason := strings.TrimSpace(argString(args, "reason"))
	if n := utf8.RuneCountInString(reason); n == 0 || n > releaseReasonMaxRunes || hasControl(reason, false) {
		r := failed(fmt.Sprintf("%s didn't ask anything: `reason` is 1 to %d characters on one line, why it is "+
			"needed, which the approver reads.", tool, releaseReasonMaxRunes))
		return "", &r
	}
	return reason, nil
}

// commitArg reads a whole commit id from args[key], lowercase.
func commitArg(tool string, args map[string]any, key, says string) (string, *tools.Result) {
	oid := strings.ToLower(strings.TrimSpace(argString(args, key)))
	if !gitref.ValidOID(oid) {
		r := failed(fmt.Sprintf("%s didn't ask anything: `%s` is %s, a whole commit id.", tool, key, says))
		return "", &r
	}
	return oid, nil
}

// ---- ask_to_merge ------------------------------------------------------ //

type askToMerge struct{ deps ReleaseDeps }

var (
	_ tools.SeatCallable = (*askToMerge)(nil)
	_ fixedTool          = (*askToMerge)(nil)
)

func (t *askToMerge) Name() string { return AskToMergeTool }

func (t *askToMerge) Description() string {
	return "Ask for approval to merge a change at exactly the head you read, where your work or the company " +
		"asks for one before a merge: a Gitlet change (web!42) or a GitHub pull request by its address. The " +
		"call is put to the company's delegate for merges where it names one, or else to a person, and your " +
		"run is parked until they answer: end your turn when this says it was put to them. Once approved, " +
		"the engine merges exactly that head, as you; a change pushed to since is not merged, and you ask " +
		"again."
}

func (t *askToMerge) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"change": map[string]any{"type": "string",
				"description": "The change: a Gitlet change's reference (web!42), or a GitHub pull request's address."},
			"head": map[string]any{"type": "string",
				"description": "The change's head commit as you read it, a whole commit id: exactly this is merged."},
			"method": map[string]any{"type": "string", "enum": []string{releaseMerge, releaseSquash},
				"description": "A merge commit (the default) or one squashed commit."},
			"reason": map[string]any{"type": "string",
				"description": "Why it should merge now, in one line of up to 500 characters the approver reads."},
		},
		"required":             []string{"change", "head", "reason"},
		"additionalProperties": false,
	}
}

// mergeDigest is ask_to_merge's definition's digest.
var mergeDigest = sync.OnceValue(func() string { return definitionDigest(&askToMerge{}) })

// Digest is the tool's definition's digest.
func (t *askToMerge) Digest() string { return mergeDigest() }

// Approvers is who answers a call: an administrator, a lead of the seat, or a
// person the merge rule's escalate_to names, at a fresh step-up.
func (t *askToMerge) Approvers() string { return sandbox.HeldApproversRelease }

// Act is what the call is for (ADR-0125).
func (t *askToMerge) Act() delegated.Kind { return delegated.KindPRMerge }

// Binding is empty: the change, its head and the method are the arguments,
// which the digest binds already.
func (t *askToMerge) Binding(*turnctx.Turn, map[string]any) string { return "" }

// mergeAsk is one ask_to_merge call, read and held to its bounds: a Gitlet
// change by its repository's name and number, or a GitHub pull request.
type mergeAsk struct {
	repo           string
	number         int64
	pull           PullRef
	github         bool
	ref            string
	head, method   string
	reason, change string
}

// mergeAskOf reads a call's arguments, refusing what their bounds refuse.
func (d ReleaseDeps) mergeAskOf(tool string, args map[string]any) (mergeAsk, *tools.Result) {
	refuse := func(msg string) (mergeAsk, *tools.Result) {
		r := failed(tool + " didn't ask anything: " + msg)
		return mergeAsk{}, &r
	}
	a := mergeAsk{change: strings.TrimSpace(argString(args, "change")),
		method: strings.TrimSpace(argString(args, "method"))}
	if a.method == "" {
		a.method = releaseMerge
	}
	if a.method != releaseMerge && a.method != releaseSquash {
		return refuse("`method` is " + releaseMerge + " or " + releaseSquash + ".")
	}
	head, refusal := commitArg(tool, args, "head", "the change's head as you read it")
	if refusal != nil {
		return mergeAsk{}, refusal
	}
	a.head = head
	reason, refusal := releaseReason(tool, args)
	if refusal != nil {
		return mergeAsk{}, refusal
	}
	a.reason = reason
	if strings.HasPrefix(strings.ToLower(a.change), "http") {
		if d.Pulls == nil {
			return refuse("this company runs no GitHub, so `change` is a Gitlet change's reference (web!42).")
		}
		ref, ok := d.Pulls.Ref(a.change)
		if !ok {
			return refuse("`change` is not a pull request on this company's GitHub, under an owner it names. " +
				"A merge elsewhere (a GitLab merge request among them) is asked with " + toolname.AskAndWait + ".")
		}
		a.pull, a.github, a.ref = ref, true, ref.String()
		return a, nil
	}
	if !d.gitlet() {
		return refuse("this company runs no Gitlet, so `change` is a GitHub pull request's address.")
	}
	name, number, refusal := changeArg(tool, map[string]any{"change": a.change})
	if refusal != nil {
		return mergeAsk{}, refusal
	}
	a.repo, a.number, a.ref = name, number, code.ChangeRef(name, number)
	return a, nil
}

// BeforeHold refuses a call that would never merge, before anybody is asked:
// one no seat makes, one its bounds refuse, or one about a change that is not
// open or whose head is not the one named.
func (t *askToMerge) BeforeHold(ctx context.Context, turn *turnctx.Turn, args map[string]any) *tools.Result {
	handle, refusal := seatTurn(t.Name(), turn)
	if refusal != nil {
		return refusal
	}
	a, refusal := t.deps.mergeAskOf(t.Name(), args)
	if refusal != nil {
		return refusal
	}
	open, head, refusal := t.current(ctx, turn, handle, a)
	switch {
	case refusal != nil:
		return refusal
	case !open:
		r := refused(tools.RefusalConflict, t.Name()+" didn't ask anything: "+a.ref+" is not open, so there "+
			"is nothing to merge.")
		return &r
	case head != a.head:
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s didn't ask anything: %s's head is %s now, not "+
			"%s. Read the change again, and ask about the head it has.", t.Name(), a.ref, head, a.head))
		return &r
	}
	return nil
}

// current is whether a's change is open and its head now, read as handle on
// its code host.
func (t *askToMerge) current(ctx context.Context, turn *turnctx.Turn, handle string, a mergeAsk) (bool, string,
	*tools.Result) {

	if a.github {
		f, err := t.deps.Pulls.Facts(ctx, handle, a.pull)
		if err != nil {
			r := nodeFailure(ctx, t.Name(), err, t.Name()+" didn't ask anything: "+a.ref+" could not be read "+
				"from GitHub", "Call it again.")
			return false, "", &r
		}
		return f.Open, f.Head, nil
	}
	g, refusal := t.deps.changeRead(ctx, turn, t.Name(), a)
	if refusal != nil {
		return false, "", refusal
	}
	d, _, _, refusal := g.head(ctx, statelog.Position{})
	if refusal != nil {
		return false, "", refusal
	}
	return d.State.Live(), d.Head, nil
}

// changeRead is a read of a's Gitlet change as turn's seat, reading as the
// code tools' reads do ([authz.ActionCodeRead]).
func (d ReleaseDeps) changeRead(ctx context.Context, turn *turnctx.Turn, tool string, a mergeAsk) (*changeRead,
	*tools.Result) {

	c, refusal := d.Code.callerOn(ctx, turn, tool, authz.ActionCodeRead)
	if refusal != nil {
		return nil, refusal
	}
	return &changeRead{deps: d.Code, turn: turn, tool: tool, c: c, name: a.repo, number: a.number}, nil
}

// HeldWords is the question a person answers and its context: the change and
// its head in code, and the reason as redacted prose made inert.
func (t *askToMerge) HeldWords(turn *turnctx.Turn, args map[string]any) (question, context string) {
	handle := turn.Handle()
	a, _ := t.deps.mergeAskOf(t.Name(), args)
	question = fmt.Sprintf("May %s merge %s at %s?", handle, a.ref, abbrevOID(a.head))
	var b strings.Builder
	where := "the Gitlet change"
	if a.github {
		where = "the GitHub pull request"
	}
	fmt.Fprintf(&b, "%s asks to merge %s `%s` at head `%s`, as %s. Allow once merges exactly that head, "+
		"once, as %s; a change pushed to since is not merged. The target's own rules still apply.",
		handle, where, a.ref, a.head, map[string]string{releaseMerge: "a merge commit",
			releaseSquash: "one squashed commit"}[a.method], handle)
	b.WriteString("\n\n**Reason given**\n\n" + inertMarkdown(redact.Secrets(a.reason)))
	return question, b.String()
}

// abbrevOID is a commit id as a person reads it, its first twelve digits.
func abbrevOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

func (t *askToMerge) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// CallForTurn merges an approved call's change at exactly its head: never one
// nobody approved.
func (t *askToMerge) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	handle, refusal := seatTurn(t.Name(), turn)
	if refusal != nil {
		return *refusal, nil
	}
	approval, approved := tools.ApprovalOf(ctx)
	if !approved || approval.HeldID == "" {
		return refused(tools.RefusalForbidden, t.Name()+" didn't merge anything: it merges only once the call "+
			"is approved, and this call carries no approval."), nil
	}
	a, refusal := t.deps.mergeAskOf(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	if !a.github {
		// THE CODE TOOLS' OWN MERGE, as the seat: the target's gate, the
		// seat's grant and the head named decide it there.
		merge := &codeMergeChange{deps: t.deps.Code}
		res, _, err := tools.Invoke(ctx, tools.Invocation{Name: merge.Name(), Tool: merge,
			Args: tools.ArgsOf(merge, false, map[string]any{"repo": a.repo, "change": strconv.FormatInt(a.number, 10),
				"expected_head": a.head, "method": map[string]string{releaseMerge: string(code.MergeMethodMerge),
					releaseSquash: string(code.MergeMethodSquash)}[a.method]}),
			AsSeat: true, Turn: turn, Where: t.Name()})
		if err != nil {
			return faulted(ctx, t.Name(), err, t.Name()+" was approved and the merge did not finish"), nil
		}
		return res, nil
	}
	commit, err := t.deps.Pulls.Merge(ctx, handle, a.pull, a.head, a.method)
	switch {
	case errors.Is(err, ErrPullMoved):
		return refused(tools.RefusalConflict, t.Name()+" didn't merge "+a.ref+": its head is no longer "+
			a.head+", the one approved. Read it again and ask about the head it has."), nil
	case errors.Is(err, ErrPullNotMergeable):
		return refused(tools.RefusalConflict, t.Name()+" didn't merge "+a.ref+": GitHub does not merge it as "+
			"it stands (a conflict, a failing required check, or a rule of its branch). Nothing was merged."), nil
	case errors.Is(err, ErrPullForbidden):
		return refused(tools.RefusalForbidden, t.Name()+" didn't merge "+a.ref+": your GitHub app may not "+
			"merge it (it needs Contents write, and its branch's rules must let it). Nothing was merged."), nil
	case err != nil:
		return nodeFailure(ctx, t.Name(), err, t.Name()+" was approved and could not tell whether "+a.ref+
			" merged", "Read the pull request before asking again."), nil
	}
	return tools.Result{Output: fmt.Sprintf("Merged %s at %s, approved by %s: its base branch moved to %s.",
		a.ref, a.head, approval.By, commit)}, nil
}

// ---- ask_to_deploy ----------------------------------------------------- //

type askToDeploy struct{ deps ReleaseDeps }

var (
	_ tools.SeatCallable = (*askToDeploy)(nil)
	_ fixedTool          = (*askToDeploy)(nil)
)

func (t *askToDeploy) Name() string { return AskToDeployTool }

func (t *askToDeploy) Description() string {
	return "Ask for approval to deploy one commit, holding changes that are already merged, to an environment, " +
		"with how to roll it back. The call is put to the company's delegate for deploys where it names one, " +
		"or else to a person, and your run is parked until they answer: end your turn when this says it was " +
		"put to them. Once approved, you are told so, and you deploy exactly that commit with your own " +
		"tools; Crewlet does not deploy it for you."
}

func (t *askToDeploy) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{"type": "string",
				"description": "The repository: a Gitlet repository's name, or a GitHub repository's address."},
			"commit": map[string]any{"type": "string",
				"description": "The commit to deploy, a whole commit id on the branch the changes merged into."},
			"changes": map[string]any{"type": "array", "minItems": 1, "maxItems": deployMaxChanges,
				"items":       map[string]any{"type": "string"},
				"description": "The merged changes it ships, by number on `repo` (42, or web!42 on Gitlet)."},
			"environment": map[string]any{"type": "string",
				"description": "Where it is deployed, such as production or staging."},
			"rollback": map[string]any{"type": "string",
				"description": "How to roll it back if it goes wrong: what to run or redeploy, and how to tell it worked. Up to 1000 characters."},
			"reason": map[string]any{"type": "string",
				"description": "Why it should deploy now, in one line of up to 500 characters the approver reads."},
		},
		"required":             []string{"repo", "commit", "changes", "environment", "rollback", "reason"},
		"additionalProperties": false,
	}
}

// deployDigest is ask_to_deploy's definition's digest.
var deployDigest = sync.OnceValue(func() string { return definitionDigest(&askToDeploy{}) })

// Digest is the tool's definition's digest.
func (t *askToDeploy) Digest() string { return deployDigest() }

// Approvers is who answers a call, as for a merge.
func (t *askToDeploy) Approvers() string { return sandbox.HeldApproversRelease }

// Act is what the call is for (ADR-0125).
func (t *askToDeploy) Act() delegated.Kind { return delegated.KindDeploy }

// Binding is empty: everything a deploy is approved for is its arguments.
func (t *askToDeploy) Binding(*turnctx.Turn, map[string]any) string { return "" }

// deployAsk is one ask_to_deploy call, read and held to its bounds.
type deployAsk struct {
	repo                 string
	pulls                PullRef
	github               bool
	commit               string
	changes              []int64
	environment          string
	rollback, reason     string
	repoSaid, changeRefs string
}

// deployAskOf reads a call's arguments, refusing what their bounds refuse.
func (d ReleaseDeps) deployAskOf(tool string, args map[string]any) (deployAsk, *tools.Result) {
	refuse := func(msg string) (deployAsk, *tools.Result) {
		r := failed(tool + " didn't ask anything: " + msg)
		return deployAsk{}, &r
	}
	a := deployAsk{repoSaid: strings.TrimSpace(argString(args, "repo")),
		environment: strings.TrimSpace(argString(args, "environment")),
		rollback:    strings.TrimSpace(argString(args, "rollback"))}
	switch {
	case strings.HasPrefix(strings.ToLower(a.repoSaid), "http"):
		if d.Pulls == nil {
			return refuse("this company runs no GitHub, so `repo` is a Gitlet repository's name.")
		}
		repo, ok := d.Pulls.Repo(a.repoSaid)
		if !ok {
			return refuse("`repo` is not a repository on this company's GitHub, under an owner it names.")
		}
		a.pulls, a.github = repo, true
	case !d.gitlet():
		return refuse("this company runs no Gitlet, so `repo` is a GitHub repository's address.")
	default:
		name, refusal := repoArg(tool, map[string]any{"repo": a.repoSaid})
		if refusal != nil {
			return deployAsk{}, refusal
		}
		a.repo = name
	}
	commit, refusal := commitArg(tool, args, "commit", "the commit to deploy")
	if refusal != nil {
		return deployAsk{}, refusal
	}
	a.commit = commit
	list, isList := argval.List(args["changes"])
	if !isList || len(list) == 0 || len(list) > deployMaxChanges {
		return refuse(fmt.Sprintf("`changes` lists 1 to %d merged changes by number.", deployMaxChanges))
	}
	var refs []string
	for _, item := range list {
		text, _ := argval.String(item)
		text = strings.TrimSpace(text)
		if name, n, ok := strings.Cut(text, "!"); ok && !a.github {
			if name != a.repo {
				return refuse(fmt.Sprintf("change %q is not on %s; a deploy names changes of one repository.",
					clip(text), a.repo))
			}
			text = n
		}
		text = strings.TrimPrefix(text, "#")
		whole, ok := argval.Whole(text)
		if !ok || whole < 1 {
			return refuse(fmt.Sprintf("change %q is not a change's number.", clip(text)))
		}
		n := int64(whole)
		if !containsInt(a.changes, n) {
			a.changes = append(a.changes, n)
			refs = append(refs, a.changeRef(n))
		}
	}
	a.changeRefs = words.And(refs)
	if !environmentName().MatchString(a.environment) {
		return refuse(fmt.Sprintf("`environment` is a name of up to %d letters, digits, dots, hyphens and "+
			"underscores, such as production.", environmentMaxBytes))
	}
	if n := utf8.RuneCountInString(a.rollback); n == 0 || n > rollbackMaxRunes || hasControl(a.rollback, true) {
		return refuse(fmt.Sprintf("`rollback` is 1 to %d characters: how to roll the deploy back, which the "+
			"approver reads.", rollbackMaxRunes))
	}
	reason, refusal := releaseReason(tool, args)
	if refusal != nil {
		return deployAsk{}, refusal
	}
	a.reason = reason
	return a, nil
}

// changeRef is change n of the deploy's repository as a person names it.
func (a deployAsk) changeRef(n int64) string {
	if a.github {
		return PullRef{Owner: a.pulls.Owner, Repo: a.pulls.Repo, Number: int(n)}.String()
	}
	return code.ChangeRef(a.repo, n)
}

// where is the deploy's repository as a person names it.
func (a deployAsk) where() string {
	if a.github {
		return a.pulls.String()
	}
	return a.repo
}

// containsInt reports whether list holds n.
func containsInt(list []int64, n int64) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

// BeforeHold refuses a call that names nothing a deploy could be approved for:
// one no seat makes, or one its bounds refuse. Whether its changes merged is
// read when it is decided, by a delegate or a person.
func (t *askToDeploy) BeforeHold(_ context.Context, turn *turnctx.Turn, args map[string]any) *tools.Result {
	if _, refusal := seatTurn(t.Name(), turn); refusal != nil {
		return refusal
	}
	_, refusal := t.deps.deployAskOf(t.Name(), args)
	return refusal
}

// HeldWords is the question a person answers and its context: the commit, the
// changes and the environment in code, the rollback and the reason as
// redacted prose made inert.
func (t *askToDeploy) HeldWords(turn *turnctx.Turn, args map[string]any) (question, context string) {
	handle := turn.Handle()
	a, _ := t.deps.deployAskOf(t.Name(), args)
	question = fmt.Sprintf("May %s deploy %s at %s to %s?", handle, a.where(), abbrevOID(a.commit), a.environment)
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks to deploy commit `%s` of `%s` to `%s`, shipping %s. Allow once tells %s it may "+
		"deploy exactly that commit there, once; Crewlet does not deploy it.", handle, a.commit, a.where(),
		a.environment, a.changeRefs, handle)
	b.WriteString("\n\n**Rollback**\n\n" + inertMarkdown(redact.Secrets(a.rollback)))
	b.WriteString("\n\n**Reason given**\n\n" + inertMarkdown(redact.Secrets(a.reason)))
	return question, b.String()
}

func (t *askToDeploy) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// CallForTurn answers an approved deploy with the approval itself: never one
// nobody approved.
func (t *askToDeploy) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	handle, refusal := seatTurn(t.Name(), turn)
	if refusal != nil {
		return *refusal, nil
	}
	approval, approved := tools.ApprovalOf(ctx)
	if !approved || approval.HeldID == "" {
		return refused(tools.RefusalForbidden, t.Name()+" didn't approve anything: this call carries no "+
			"approval."), nil
	}
	a, refusal := t.deps.deployAskOf(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	return tools.Result{Output: fmt.Sprintf("Approved by %s: %s may deploy commit %s of %s to %s, shipping %s, "+
		"once. Deploy exactly that commit now with your own tools, and roll back as you said if it goes "+
		"wrong. A deploy of any other commit, or to another environment, is asked again.", approval.By, handle,
		a.commit, a.where(), a.environment, a.changeRefs)}, nil
}
