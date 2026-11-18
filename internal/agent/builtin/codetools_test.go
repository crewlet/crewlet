package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// ---- Gitlet as the cases read it --------------------------------------- //

const siteID = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e"

var (
	mainOID    = strings.Repeat("a", 40)
	featureOID = strings.Repeat("b", 40)
	blobOID    = strings.Repeat("c", 40)
)

// codeAt is a position on the code log.
func codeAt(seq uint64) statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
}

// siteDetail is the one repository Gitlet holds: `site`, the eng
// unit's, read by the company.
func siteDetail() gitlet.RepoDetail {
	return gitlet.RepoDetail{
		RepoSummary: gitlet.RepoSummary{
			RepoRef: gitlet.RepoRef{ID: siteID, Name: "site", Status: gitlet.RepoStatusActive,
				ManagedBy: gitlet.ManagedConfig, Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "eng"},
				Visibility: gitlet.VisibilityCompany, Search: gitlet.SearchIndexed, DefaultBranch: "main",
				Refs: 2, LivePacks: 1,
				LiveBytes: 4096, Position: codeAt(7)},
			Access: "read", Head: mainOID, Pushes: 3,
		},
		Branches: 2,
	}
}

// gitletHost is a routed Gitlet holding one repository and answering by
// object id, recording every question at the level it was asked.
type gitletHost struct {
	mu sync.Mutex

	detail  gitlet.RepoDetail
	listing []gitlet.RepoSummary // the repos listing; the one repository when nil
	refs    map[string]string    // whole ref → oid
	// moved is where each ref last moved (movedAt): below at, as every row
	// a read answers is below its checkpoint.
	moved map[string]statelog.Position
	floor statelog.Position
	at    statelog.Position

	// replaced, set, is what every read BY NAME after the first answers (the
	// name deleted and declared again); a read by object id of any other
	// repository is answered gone.
	replaced *gitlet.RepoRef
	readErr  error
	gitErr   error
	git      func(q gitlet.GitQuery) gitlet.GitAnswer
	// changes, set, answers the change reads (codechanges_test.go), under
	// the host's lock, recorded with the rest.
	changes func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	// checks is every attempt of every check: a `checks` read answers their
	// rows narrowed as the data node narrows them, a `check` read the one
	// its question names (codechecks_test.go). newGitletHost holds attempt
	// 1 of `unit` at main, errored.
	checks []gitlet.CheckDetail
	// before, set, is called with each question's kind before answering,
	// outside the host's lock, so a case can read what the asking turn held.
	before func(what string)
	// seats and ats are the seat that set a ref in its own namespace and
	// when: what a resolve answers beside the oid, and what a turn's hold on
	// a seat's branch decides its record from.
	seats map[string]string
	ats   map[string]time.Time

	reads    []gitlet.ReadQuery
	resolves []gitlet.RefQuery
	gits     []gitlet.GitQuery
}

var _ builtin.CodeReader = (*gitletHost)(nil)

// newGitletHost is Gitlet holding site: its refs each last moved at a
// position of its own, between this node's own floor and the checkpoint every
// read answers at, so a read's floor says which of the three it was given.
func newGitletHost() *gitletHost {
	return &gitletHost{
		detail: siteDetail(),
		refs: map[string]string{
			"refs/heads/main": mainOID, "refs/heads/feature": featureOID, "refs/tags/v1": mainOID,
		},
		moved: map[string]statelog.Position{
			"refs/heads/main": codeAt(10), "refs/heads/feature": codeAt(11), "refs/tags/v1": codeAt(8),
		},
		floor: codeAt(3), at: codeAt(12),
		checks: []gitlet.CheckDetail{{CheckRow: gitlet.CheckRow{CheckKey: gitlet.CheckKey{RepoID: siteID, OID: mainOID,
			Target: "refs/heads/main", Name: "unit", Attempt: 1}, State: gitlet.CheckStateErrored,
			Reason: gitlet.CheckReasonBoxFailed}}},
	}
}

// movedAt is where ref last moved: its own position, or the log's first for a
// ref a case added without one.
func (h *gitletHost) movedAt(ref string) statelog.Position {
	if at, ok := h.moved[ref]; ok {
		return at
	}
	return codeAt(1)
}

func (h *gitletHost) named(name string) (gitlet.RepoRef, error) {
	if name != h.detail.Name {
		return gitlet.RepoRef{}, fmt.Errorf("%w: %s", gitlet.ErrRepoNotFound, name)
	}
	if h.replaced != nil && len(h.reads)+len(h.resolves) > 1 {
		return *h.replaced, nil
	}
	return h.detail.RepoRef, nil
}

func (h *gitletHost) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	if h.before != nil {
		h.before("read:" + string(q.Kind))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, q)
	if h.readErr != nil {
		return gitlet.ReadAnswer{}, h.readErr
	}
	out := gitlet.ReadAnswer{Position: h.at, Level: q.Fresh.Level}
	switch q.Kind {
	case gitlet.ReadRepos:
		listing := h.listing
		if listing == nil {
			listing = []gitlet.RepoSummary{h.detail.RepoSummary}
		}
		// KEYSET BY NAME, as the reader's listing is.
		for _, s := range listing {
			if s.Name > q.Cursor {
				out.Repos = append(out.Repos, s)
			}
		}
		return out, nil
	case gitlet.ReadRepo, gitlet.ReadRefs:
		ref, err := h.named(q.Repo)
		if err != nil {
			return gitlet.ReadAnswer{}, err
		}
		if q.Kind == gitlet.ReadRepo || q.Cursor == "" {
			d := h.detail
			d.RepoRef = ref
			out.Repo = &d
		}
		if q.Kind == gitlet.ReadRefs {
			prefix := q.Prefix
			if prefix == "" {
				prefix = "refs/heads/"
			}
			var names []string
			for name := range h.refs {
				if strings.HasPrefix(name, prefix) && name > q.Cursor {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			for _, name := range names {
				out.Refs = append(out.Refs, gitlet.RefRow{Ref: name, OID: h.refs[name], Position: h.movedAt(name)})
			}
		}
		return out, nil
	case gitlet.ReadChanges, gitlet.ReadChange:
		if h.changes != nil {
			answer, err := h.changes(q)
			if err != nil || !q.Resolve {
				return answer, err
			}
			// A RESOLVING READ answers the repository its name names, as
			// the reader's does (gitlet.ReadQuery.Resolve).
			ref, err := h.named(q.Repo)
			if err != nil {
				return gitlet.ReadAnswer{}, err
			}
			answer.Resolved = &ref
			return answer, nil
		}
	case gitlet.ReadChecks, gitlet.ReadCheck:
		ref, err := h.named(q.Repo)
		if err != nil {
			return gitlet.ReadAnswer{}, err
		}
		if q.Resolve {
			out.Resolved = &ref
		}
		c := q.Checks
		for i := range h.checks {
			d := h.checks[i]
			switch {
			case c.OID != "" && d.OID != c.OID:
			case q.Kind == gitlet.ReadChecks && c.Name != "" && d.Name != c.Name:
			case q.Kind == gitlet.ReadChecks:
				out.Checks = append(out.Checks, d.CheckRow)
			case d.Target == c.Target && d.Name == c.Name && d.Attempt == c.Attempt:
				out.Check = &d
				return out, nil
			}
		}
		if q.Kind == gitlet.ReadCheck {
			return gitlet.ReadAnswer{}, fmt.Errorf("%w: attempt %d of check %s", gitlet.ErrCheckNotFound, c.Attempt,
				c.Name)
		}
		// A PAGE OF THEM, from the cursor its listing's walk names a row by
		// (gitlet.ReadQuery.ChecksCursor), as Gitlet pages a listing.
		if q.Cursor != "" {
			at := slices.IndexFunc(out.Checks, func(r gitlet.CheckRow) bool { return q.ChecksCursor(r) == q.Cursor })
			if at < 0 {
				return gitlet.ReadAnswer{}, fmt.Errorf("%w: %q", gitlet.ErrBadCursor, q.Cursor)
			}
			out.Checks = out.Checks[at+1:]
		}
		if q.Limit > 0 && len(out.Checks) > q.Limit {
			out.Checks = out.Checks[:q.Limit]
			out.Next = q.ChecksCursor(out.Checks[q.Limit-1])
		}
		return out, nil
	}
	return gitlet.ReadAnswer{}, fmt.Errorf("%w: a code tool read kind %s", gitlet.ErrInvalid, q.Kind)
}

func (h *gitletHost) ResolveRef(_ context.Context, q gitlet.RefQuery) (gitlet.RefAnswer, error) {
	if h.before != nil {
		h.before("resolve_ref")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolves = append(h.resolves, q)
	if h.readErr != nil {
		return gitlet.RefAnswer{}, h.readErr
	}
	ref, err := h.named(q.Repo)
	if err != nil {
		return gitlet.RefAnswer{}, err
	}
	out := gitlet.RefAnswer{Repo: ref, Position: h.at}
	for _, asked := range q.Refs {
		names := []string{asked}
		if !strings.HasPrefix(asked, "refs/") {
			names = []string{"refs/heads/" + asked, "refs/tags/" + asked}
		}
		resolved := gitlet.ResolvedRef{Name: asked, Missing: fmt.Sprintf("repository %s holds no live %s", q.Repo,
			strings.Join(names, " or "))}
		for _, name := range names {
			if oid, ok := h.refs[name]; ok {
				resolved = gitlet.ResolvedRef{Name: asked, Ref: name, OID: oid, Moved: h.movedAt(name),
					Seat: h.seats[name], At: h.ats[name]}
				break
			}
		}
		out.Refs = append(out.Refs, resolved)
	}
	return out, nil
}

func (h *gitletHost) GitRead(_ context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	if h.before != nil {
		h.before("git_read:" + string(q.Kind))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gits = append(h.gits, q)
	if h.gitErr != nil {
		return gitlet.GitAnswer{}, h.gitErr
	}
	if h.replaced != nil && q.RepoID != h.replaced.ID {
		// THE ID THE NAME NAMED BEFORE, answered purged as a Gitlet node does.
		return gitlet.GitAnswer{}, fmt.Errorf("%w: %s", gitlet.ErrRepoNotFound,
			gitlet.RepoGone{ID: q.RepoID, Name: h.detail.Name, Purged: true}.Detail())
	}
	if h.git != nil {
		return h.git(q), nil
	}
	out := gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Position: h.at}
	switch q.Kind {
	case gitlet.GitBlob:
		out.Rev = blobOID
		out.Blob = &gitlet.BlobAnswer{Path: q.Blob.Path, OID: blobOID, Size: 6, Lines: 1, StartLine: 1,
			EndLine: 1, Text: "hello\n"}
	case gitlet.GitTree:
		out.Rev = mainOID
		out.Tree = &gitlet.TreeAnswer{Tree: blobOID, Entries: []gitlet.TreeEntry{}}
	case gitlet.GitDiff:
		out.Diff = &gitlet.DiffAnswer{Base: q.Diff.Base, Head: q.Diff.Head, Files: []gitlet.DiffFile{}}
	case gitlet.GitCompare:
		out.Compare = &gitlet.CompareAnswer{Base: q.Compare.Base, Head: q.Compare.Head, Files: []gitlet.DiffFile{}}
	case gitlet.GitLandedIn:
		out.LandedIn = &gitlet.LandedInAnswer{Commit: q.LandedIn.Commit, Target: q.LandedIn.Target,
			TargetOID: q.LandedIn.TargetOID, At: q.LandedIn.At, Landed: true}
	}
	return out, nil
}

// FloorOf is this node's floor on site, the one repository it wrote to, and
// none on any other, such as the one a name declared again names.
func (h *gitletHost) FloorOf(repoID string) statelog.Position {
	if repoID != siteID {
		return statelog.Position{}
	}
	return h.floor
}

// asked is what the host was asked, copied under its lock.
func (h *gitletHost) asked() (reads []gitlet.ReadQuery, resolves []gitlet.RefQuery, gits []gitlet.GitQuery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reads), slices.Clone(h.resolves), slices.Clone(h.gits)
}

// readsOf counts the reads of kind.
func readsOf(reads []gitlet.ReadQuery, kind gitlet.ReadKind) int {
	n := 0
	for _, q := range reads {
		if q.Kind == kind {
			n++
		}
	}
	return n
}

// codeRequesters is who a code-reading turn may act for, composed as the
// engine composes them: the founder (a person), agent-ceo (a colleague seat)
// and "departed" (a person whose seat's holder was removed, acting as
// nobody). Any other handle is one this node cannot compose.
func codeRequesters() requesters {
	return requesters{
		"founder":   founderPrincipal(iam.Apps...),
		"agent-ceo": {Kind: iam.KindSeat, Seat: "agent-ceo", Stage: iam.StageActive, Grants: iam.Apps},
		"departed": {Kind: iam.KindPerson, Login: "departed@example.com", Seat: "departed",
			Stage: iam.StageRetired, Grants: iam.Apps},
		// A PIPELINE'S TOKEN acting as no seat, as this node resolves it.
		"login:token:ci": {Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive,
			Grants: iam.Apps},
	}
}

// codeDeps is the code read tools over host as every surface wires them:
// Gitlet beside the requesters and the chart (the requester ceiling),
// without either of which no code tool is offered. [writeDeps] adds writes.
func codeDeps(host builtin.CodeReader) builtin.CodeDeps {
	return builtin.CodeDeps{Code: host, Requesters: codeRequesters(), Chart: chartLeads}
}

// codeRegistry is a seat's registry holding the code tools over deps.
func codeRegistry(t *testing.T, deps builtin.CodeDeps) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Code: deps})); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// codeTurn is agent-cto's turn, with the lookups the engine builds every turn
// with.
func codeTurn(t *testing.T) *turnctx.Turn {
	t.Helper()
	turn, _ := readerTurn(t)
	turn.Lookups = turnctx.NewLookups()
	return turn
}

// callCode calls one of a seat's code tools in turn, and decodes its answer.
func callCode(t *testing.T, reg *tools.Registry, turn *turnctx.Turn, name string,
	args map[string]any) (tools.Result, map[string]any) {

	t.Helper()
	got := callIn(t, reg, turn, name, args)
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("%s answered no JSON object: %v\n%s", name, err, got.Output)
	}
	return got, answer
}

// refusedCode calls one of a seat's code tools in turn, expecting it refused.
func refusedCode(t *testing.T, reg *tools.Registry, turn *turnctx.Turn, name string,
	args map[string]any) tools.Result {

	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	got, err := entry.Tool.(tools.SeatCallable).CallForTurn(everyGrant(), turn, args)
	if err != nil || !got.Failed {
		t.Fatalf("%s %v answered %v %q, want a refusal", name, args, err, got.Output)
	}
	return got
}

// operatorCode is the operator catalogue's code tools over host, asked as
// whoever the call's context carries.
func operatorCode(t *testing.T, host builtin.CodeReader, clone func(string) string) map[string]tools.Callable {
	t.Helper()
	out := map[string]tools.Callable{}
	deps := writeDeps(host, &codeWrites{})
	deps.Actor, deps.CloneURL = builtin.PrincipalActor, clone
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Code:      deps,
		Authorize: builtin.Decide(chartLeads),
	}) {
		out[tool.Name()] = tool
	}
	return out
}

// asBoundPerson is a person signed in through one of their machine tokens,
// bound to the human seat they hold.
func asBoundPerson() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee3"),
		Kind: iam.KindPerson, Login: "jane.founder", Seat: "founder", Via: "pat:jane-laptop",
		Stage: iam.StageActive, Grants: iam.Apps,
	})
}

// asUnboundToken is a CI machine token bound to nobody, holding the gitlet
// grant a CI token fetches with (ADR-0110) and stepped up as the node composes
// every machine token (iam.Principal.MachineToken): a gesture asking a fresh
// proof is refused at the tool for the binding it lacks, never the app or proof.
func asUnboundToken() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee4"),
		Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantGitlet}, ReauthAt: time.Now().Add(time.Hour).UTC(),
	})
}

// ---- one list, two surfaces --------------------------------------------- //

// THE CODE TOOLS ARE ONE LIST: a seat's registry and the operator catalogue
// both take [builtin.CodeTools], so they hold the same code tools in the same
// order with byte-identical descriptions and schemas, but for the writes'
// `op_id`, which only the operator surface offers. A company hosting no code
// gets none, nor does a surface wiring Gitlet without the requesters or the
// chart, which would read for whoever messaged a seat whatever the seat may.
//
// Mutation: OperatorTools omitting codeCandidates, or building a list of its
// own with one schema edited; CodeTools serving over Gitlet alone; the
// writes' op_id offered to a seat → red.
func TestTheCodeToolsAreOneList(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	reg := codeRegistry(t, writeDeps(host, &codeWrites{}))
	operator := operatorCode(t, host, nil)
	every := slices.Concat(builtin.CodeToolNames(), []string{builtin.CodeSearchTool}, builtin.CodeWrites())
	for _, name := range every {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Errorf("a seat's registry holds no %s", name)
			continue
		}
		tool, ok := operator[name]
		if !ok {
			t.Errorf("the operator catalogue holds no %s", name)
			continue
		}
		write := slices.Contains(builtin.CodeWrites(), name)
		opParams := tool.Parameters()
		props, _ := opParams["properties"].(map[string]any)
		_, offered := props["op_id"]
		if _, seatOffers := propertiesOf(entry.Tool)["op_id"]; seatOffers || offered != write {
			t.Errorf("%s offers op_id to a seat: %v, and to the operator: %v — want the operator's writes alone",
				name, seatOffers, offered)
		}
		without := map[string]any{}
		for key, value := range props {
			if key != "op_id" {
				without[key] = value
			}
		}
		opParams["properties"] = without
		seatSchema, _ := json.Marshal(entry.Tool.Parameters())
		opSchema, _ := json.Marshal(opParams)
		if string(seatSchema) != string(opSchema) || entry.Tool.Description() != tool.Description() {
			t.Errorf("%s is described or shaped differently on the two surfaces", name)
		}
	}
	var listed []string
	for _, tool := range builtin.CodeTools(writeDeps(host, &codeWrites{})) {
		listed = append(listed, tool.Name())
	}
	if !slices.Equal(listed, every) {
		t.Errorf("CodeTools lists %v, want %v", listed, every)
	}

	none := codeRegistry(t, builtin.CodeDeps{})
	for _, name := range builtin.CodeToolNames() {
		if _, ok := none.Lookup(name); ok {
			t.Errorf("a company hosting no code was given %s", name)
		}
		if _, ok := operatorCode(t, nil, nil)[name]; ok {
			t.Errorf("an operator of a company hosting no code was offered %s", name)
		}
	}
	if len(builtin.CodeTools(builtin.CodeDeps{})) != 0 {
		t.Error("CodeTools without Gitlet listed tools")
	}
	for name, deps := range map[string]builtin.CodeDeps{
		"no requesters": {Code: host, Chart: chartLeads},
		"no chart":      {Code: host, Requesters: codeRequesters()},
	} {
		if served := builtin.CodeTools(deps); len(served) != 0 {
			t.Errorf("CodeTools over Gitlet with %s listed %d tools", name, len(served))
		}
		if _, ok := codeRegistry(t, deps).Lookup(builtin.CodeReadTool); ok {
			t.Errorf("a seat's registry over Gitlet with %s holds %s", name, builtin.CodeReadTool)
		}
	}
}

// EVERY CODE READ IS A PROVEN READ on both surfaces: every code tool but the
// writes carries the hint the delivery check reads and the operator catalogue
// audits nothing on. Mutation: one code read off codeAnnotations (the default
// arm's shared write) → red.
func TestEveryCodeToolIsAProvenRead(t *testing.T) {
	t.Parallel()
	reg := codeRegistry(t, writeDeps(newGitletHost(), &codeWrites{}))
	var reads []string
	for _, tool := range builtin.CodeTools(writeDeps(newGitletHost(), &codeWrites{})) {
		if !slices.Contains(builtin.CodeWrites(), tool.Name()) {
			reads = append(reads, tool.Name())
		}
	}
	if want := append(builtin.CodeToolNames(), builtin.CodeSearchTool); !slices.Equal(reads, want) {
		t.Fatalf("the code tools that are not writes are %v, want the reads %v", reads, want)
	}
	for _, name := range reads {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !mcp.ReadOnlyProven(entry.Annotations) || !mcp.ReadOnlyProven(builtin.AnnotationsFor(name)) ||
			entry.Annotations.Idempotent != mcp.Yes {
			t.Errorf("%s is annotated %+v, want a proven, idempotent read", name, entry.Annotations)
		}
		if mcp.WritesToSharedSurface(entry.Annotations) {
			t.Errorf("%s reads as a write to a shared surface, so no worker could be given it", name)
		}
	}
}

// ---- who asks ------------------------------------------------------------ //

// A CODE READ IS JUDGED AS ITS CALLER: a seat's turn asks as the seat, a
// person's assistant as their credential bound to their seat, an unbound
// token as itself (the conversion the REST reads use too), and an argument
// naming who reads is refused before anything is read.
//
// Mutation: the asker taken from the turn's requester, or a person asked as
// their seat with no credential → red.
func TestACodeReadIsJudgedAsItsCaller(t *testing.T) {
	t.Parallel()
	seatHost := newGitletHost()
	reg := codeRegistry(t, codeDeps(seatHost))
	turn := codeTurn(t)
	turn.Requester = "founder"
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": "main", "path": "README.md"})
	callCode(t, reg, turn, builtin.CodeListReposTool, map[string]any{})
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "agent-cto"}
	reads, resolves, gits := seatHost.asked()
	if len(reads) == 0 || len(resolves) == 0 || len(gits) == 0 {
		t.Fatalf("the seat's calls asked %d reads, %d refs and %d git reads", len(reads), len(resolves), len(gits))
	}
	for _, q := range reads {
		if q.Asker != seat {
			t.Errorf("a seat's %s read asked as %+v, want %+v", q.Kind, q.Asker, seat)
		}
	}
	for _, q := range resolves {
		if q.Asker != seat {
			t.Errorf("a seat's ref asked as %+v", q.Asker)
		}
	}
	for _, q := range gits {
		if q.Asker != seat {
			t.Errorf("a seat's git read asked as %+v", q.Asker)
		}
	}

	for name, c := range map[string]struct {
		ctx  context.Context
		want gitlet.Principal
	}{
		"a bound person": {asBoundPerson(), gitlet.Principal{Kind: gitlet.PrincipalOperator,
			OperatorID: "pat:jane-laptop", Seat: "founder"}},
		"an unbound token": {asUnboundToken(), gitlet.Principal{Kind: gitlet.PrincipalOperator,
			OperatorID: "token:ci"}},
	} {
		host := newGitletHost()
		tools := operatorCode(t, host, nil)
		if got, err := tools[builtin.CodeListReposTool].Call(c.ctx, map[string]any{}); err != nil || got.Failed {
			t.Fatalf("%s's listing: %v %s", name, err, got.Output)
		}
		reads, _, _ := host.asked()
		if len(reads) != 1 || reads[0].Asker != c.want {
			t.Errorf("%s asked as %+v, want %+v", name, reads, c.want)
		}
		if want := builtin.CodeAskerOf(mustActor(c.ctx, t)); want != c.want {
			t.Errorf("%s converts to %+v through CodeAskerOf, want %+v", name, want, c.want)
		}
	}

	// NO ARGUMENT NAMES WHO READS.
	host := newGitletHost()
	got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t),
		builtin.CodeListReposTool, map[string]any{"seat": "founder"})
	if !errors.Is(got.Cause, builtin.ErrUndeclaredArgument) {
		t.Fatalf("a call naming a seat to read as answered %+v", got)
	}
	if reads, _, _ := host.asked(); len(reads) != 0 {
		t.Errorf("a refused call asked Gitlet %+v", reads)
	}
}

// EVERY FILE'S PATCH IS ASKED BY ITS ARGUMENT: `patches` sets the diff's
// Patches, and beside `file` is refused before Gitlet is asked a git read.
//
// Mutation: drop Patches from code_diff's query → red.
func TestCodeDiffAsksEveryPatchByItsArgument(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	reg := codeRegistry(t, codeDeps(host))
	turn := codeTurn(t)
	callCode(t, reg, turn, builtin.CodeDiffTool, map[string]any{"repo": "site", "base": mainOID,
		"head": featureOID, "patches": true})
	_, _, gits := host.asked()
	if len(gits) != 1 || gits[0].Diff == nil || !gits[0].Diff.Patches {
		t.Fatalf("code_diff with patches asked %+v, want one diff of every file's patch", gits)
	}
	got := refusedCode(t, reg, turn, builtin.CodeDiffTool, map[string]any{"repo": "site", "base": mainOID,
		"head": featureOID, "patches": true, "file": "a.txt"})
	if !strings.Contains(got.Output, "patches") {
		t.Errorf("code_diff with patches and file answered %q, want the refusal naming patches", got.Output)
	}
	if _, _, gits = host.asked(); len(gits) != 1 {
		t.Errorf("a refused code_diff asked Gitlet a git read: %+v", gits[1:])
	}
}

// mustActor is the actor the operator surface writes ctx's principal down as.
func mustActor(ctx context.Context, t *testing.T) builtin.Actor {
	t.Helper()
	a, err := builtin.PrincipalActor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// ---- whom a turn reads for ------------------------------------------------ //

// everyCodeQuestion asks, in turn, each kind of question a code tool asks
// Gitlet: the listing, a repository's first page of branches, a file at a
// branch (a resolve, then a read by object id) and a diff of two branches.
func everyCodeQuestion(t *testing.T, reg *tools.Registry, turn *turnctx.Turn) {
	t.Helper()
	callCode(t, reg, turn, builtin.CodeListReposTool, map[string]any{})
	callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": "main", "path": "README.md"})
	callCode(t, reg, turn, builtin.CodeDiffTool, map[string]any{"repo": "site", "base": "main", "head": "feature"})
}

// readsFor holds every question host was asked to name ceiling as the seat it
// is made for ("" for none), and each kind to have been asked.
func readsFor(t *testing.T, host *gitletHost, ceiling string) {
	t.Helper()
	reads, resolves, gits := host.asked()
	if len(reads) == 0 || len(resolves) == 0 || len(gits) == 0 {
		t.Fatalf("the calls asked %d reads, %d resolves and %d git reads, want each kind", len(reads),
			len(resolves), len(gits))
	}
	for _, q := range reads {
		if q.CeilingSeat != ceiling {
			t.Errorf("a %s read was made for %q, want %q", q.Kind, q.CeilingSeat, ceiling)
		}
	}
	for _, q := range resolves {
		if q.CeilingSeat != ceiling {
			t.Errorf("a resolve of %v was made for %q, want %q", q.Refs, q.CeilingSeat, ceiling)
		}
	}
	for _, q := range gits {
		if q.CeilingSeat != ceiling {
			t.Errorf("a %s read by object id was made for %q, want %q", q.Kind, q.CeilingSeat, ceiling)
		}
	}
}

// A CODE READ IS DECIDED AGAIN FOR WHOEVER WOKE THE TURN (the requester
// ceiling, on a repository's grant). A turn nobody, the seat or a person woke
// reads for the seat alone: a person maintains every repository. A colleague's
// turn names that colleague on every question, for the node to judge it too.
// A stranger's or departed person's turn, whoever else spoke, is refused with
// the ceiling's reason unasked; an uncomposable requester is a node that could
// not tell, never nobody. The operator surface's assistant reads as its caller.
//
// Mutations (each red): CodeDeps.caller skipping the ceiling; the colleague
// off any kind of question; a person's seat as ceiling; an uncomposable
// requester as nobody; senders before requester; the seat a stranger; senders
// beside the seat's ignored.
func TestACodeReadIsDecidedAgainForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	// Senders: somebody nothing maps to, the seat itself (the outcome of its
	// own act) and a known requester.
	unknown := []types.CanonicalIdentity{{ExternalID: "U-out", Platform: "slack"}}
	itself := []types.CanonicalIdentity{{Handle: "agent-cto"}}
	speaker := func(handle string) []types.CanonicalIdentity {
		return []types.CanonicalIdentity{{Handle: handle}}
	}
	for _, c := range []struct {
		name      string
		requester string
		senders   []types.CanonicalIdentity
		ceiling   string
	}{
		{"a turn nobody woke", "", nil, ""},
		{"a turn the seat woke itself", "agent-cto", nil, ""},
		{"a turn the outcome of the seat's own act woke", "agent-cto", itself, ""},
		{"a turn a person woke", "founder", speaker("founder"), ""},
		{"a turn a colleague seat woke", "agent-ceo", speaker("agent-ceo"), "agent-ceo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newGitletHost()
			turn := codeTurn(t)
			turn.Requester = c.requester
			turn.Senders = c.senders
			everyCodeQuestion(t, codeRegistry(t, codeDeps(host)), turn)
			readsFor(t, host, c.ceiling)
		})
	}

	for _, c := range []struct {
		name      string
		requester string
		senders   []types.CanonicalIdentity
		want      tools.Refusal
		says      string
		ceiling   bool
	}{
		{"a turn a stranger woke", "", unknown, tools.RefusalForbidden,
			"woken by somebody the company does not know", true},
		// A STRANGER IN THE SEAT'S OWN THREAD still bounds it.
		{"a turn a stranger spoke in beside the seat itself", "agent-cto", append(itself, unknown...),
			tools.RefusalForbidden, "woken by somebody the company does not know", true},
		{"a turn a departed person woke", "departed", speaker("departed"), tools.RefusalForbidden,
			"may not act in the company now", true},
		{"a turn whose requester this node cannot compose", "agent-gone", nil, tools.RefusalUnavailable,
			"can't tell who this turn's work is for", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newGitletHost()
			reg := codeRegistry(t, codeDeps(host))
			turn := codeTurn(t)
			turn.Requester = c.requester
			turn.Senders = c.senders
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{builtin.CodeListReposTool, map[string]any{}},
				{builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID}},
			} {
				got := refusedCode(t, reg, turn, call.tool, call.args)
				if tools.RefusalOf(got) != c.want || !strings.Contains(got.Output, c.says) {
					t.Errorf("%s answered %s %q, want refused %s saying %q", call.tool, tools.RefusalOf(got),
						got.Output, c.want, c.says)
				}
				var refused *builtin.DecisionRefused
				if ceiling := errors.As(got.Cause, &refused) && refused.Decision.Reason == authz.ReasonCeiling; ceiling != c.ceiling {
					t.Errorf("%s's refusal carries the ceiling's reason: %v, want %v (%v)", call.tool, ceiling,
						c.ceiling, got.Cause)
				}
				if !c.ceiling && !errors.Is(got.Cause, builtin.ErrUndecidable) {
					t.Errorf("%s's refusal %v is not a decision this node could not take", call.tool, got.Cause)
				}
			}
			if reads, resolves, gits := host.asked(); len(reads)+len(resolves)+len(gits) != 0 {
				t.Errorf("the refused calls asked Gitlet %d reads, %d resolves and %d git reads, want "+
					"nothing", len(reads), len(resolves), len(gits))
			}
		})
	}

	t.Run("the operator surface", func(t *testing.T) {
		t.Parallel()
		host := newGitletHost()
		op := operatorCode(t, host, nil)
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{builtin.CodeListReposTool, map[string]any{}},
			{builtin.CodeGetRepoTool, map[string]any{"repo": "site"}},
			{builtin.CodeReadTool, map[string]any{"repo": "site", "rev": "main"}},
		} {
			if got, err := op[call.tool].Call(asBoundPerson(), call.args); err != nil || got.Failed {
				t.Fatalf("a person's %s: %v %s", call.tool, err, got.Output)
			}
		}
		readsFor(t, host, "")
	})
}

// A READ REFUSED FOR WHOM THE TURN READS IS THE CEILING'S REFUSAL: a node's
// ErrAboveCeiling for a colleague's read, of rows or by object id, is told
// forbidden with the ceiling reason over Gitlet's sentence naming the
// colleague and the visibility. Never the seat's own not_permitted: the seat
// reads the repository in a turn nobody woke, so more access changes nothing.
//
// Mutation: the ErrAboveCeiling arm dropped from codeFailure (a fault of this
// node), or answered as ErrNotPermitted (no ceiling) → red.
func TestAReadRefusedForWhomTheTurnReadsIsTheCeilings(t *testing.T) {
	t.Parallel()
	above := fmt.Errorf("estate: data-a answered: %w: seat \"agent-ceo\" holds no access to repository "+
		"\"site\": it is visibility: unit, read by the seats of unit \"eng\" and the leads above it and by "+
		"persons", gitlet.ErrAboveCeiling)
	for name, c := range map[string]struct {
		readErr, gitErr error
		tool            string
		args            map[string]any
	}{
		"the repository's rows": {readErr: above, tool: builtin.CodeGetRepoTool,
			args: map[string]any{"repo": "site"}},
		"a read by object id": {gitErr: above, tool: builtin.CodeReadTool,
			args: map[string]any{"repo": "site", "rev": mainOID}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := newGitletHost()
			host.readErr, host.gitErr = c.readErr, c.gitErr
			turn := codeTurn(t)
			turn.Requester = "agent-ceo"
			got := refusedCode(t, codeRegistry(t, codeDeps(host)), turn, c.tool, c.args)
			if tools.RefusalOf(got) != tools.RefusalForbidden {
				t.Fatalf("answered %s %q, want refused forbidden", tools.RefusalOf(got), got.Output)
			}
			var refused *builtin.DecisionRefused
			if !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling ||
				!errors.Is(got.Cause, gitlet.ErrAboveCeiling) || errors.Is(got.Cause, gitlet.ErrNotPermitted) {
				t.Errorf("the refusal's cause is %v, want the ceiling's reason over ErrAboveCeiling, and never "+
					"ErrNotPermitted", got.Cause)
			}
			for _, says := range []string{"the seat this turn is working for could not read this itself",
				`seat "agent-ceo" holds no access to repository "site"`, "visibility: unit"} {
				if !strings.Contains(got.Output, says) {
					t.Errorf("the refusal %q does not say %q", got.Output, says)
				}
			}
		})
	}
}

// ---- what a turn pays ----------------------------------------------------- //

// A TURN PAYS ONE BARRIER PER REPOSITORY, AND ONE PER CALL THAT NAMES REFS:
// rows are read linearizable, the repository once a turn, a call's refs in
// ONE resolve and an id never. A read by object id is floored at the furthest
// of the node's own floor and where its refs LAST MOVED, never the resolve's
// checkpoint, which is past every push and would make the copy sync for
// nothing. A landed-in read is judged up to its tip's own move. The operator
// surface has no turn and reads the repository once a call.
//
// Mutation: resolveRevs resolving an oid as a ref or a call's refs one read
// each, repo() reading past the turn's lookups, or a read floored at the
// resolve's checkpoint → red.
func TestATurnPaysOneBarrierPerRefItNames(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	reg := codeRegistry(t, codeDeps(host))
	turn := codeTurn(t)
	floorOf := func(q gitlet.GitQuery) []statelog.Position { return q.Fresh.Floors }
	main, feature := host.movedAt("refs/heads/main"), host.movedAt("refs/heads/feature")

	callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": "main", "path": "README.md"})
	_, _, gits := host.asked()
	if got := gits[len(gits)-1].Fresh; got.Level != statelog.ReadSession ||
		!slices.Equal(got.Floors, []statelog.Position{main}) {
		t.Errorf("a read by a ref carried %+v, want a session floor where the ref last moved, %v — never "+
			"where the resolve read it, %v", got, main, host.at)
	}
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID, "path": "README.md"})
	_, resolvesBefore, gits := host.asked()
	if got := gits[len(gits)-1].Fresh; !slices.Equal(got.Floors, []statelog.Position{host.floor}) {
		t.Errorf("a read by an id carried %+v, want this node's own floor %v", got, host.floor)
	}
	callCode(t, reg, turn, builtin.CodeDiffTool, map[string]any{"repo": "site", "base": "main", "head": "feature"})
	if _, _, gits = host.asked(); !slices.Equal(floorOf(gits[len(gits)-1]), []statelog.Position{feature}) {
		t.Errorf("a diff of main and feature was floored at %v, want the later of their moves, %v",
			floorOf(gits[len(gits)-1]), feature)
	}
	callCode(t, reg, turn, builtin.CodeResolveTool, map[string]any{"repo": "site", "ref": "v1"})
	callCode(t, reg, turn, builtin.CodeLandedInTool, map[string]any{"repo": "site", "commit": featureOID,
		"target": "main"})
	if _, _, gits = host.asked(); gits[len(gits)-1].LandedIn.At != main ||
		!slices.Equal(floorOf(gits[len(gits)-1]), []statelog.Position{main}) {
		t.Errorf("a landed-in read of main was judged up to %v and floored at %v, want both at main's own "+
			"move %v", gits[len(gits)-1].LandedIn.At, floorOf(gits[len(gits)-1]), main)
	}

	reads, resolves, gits := host.asked()
	// THE TURN'S ONE ROW READ is code_get_repo's page of branches, whose
	// detail resolved the repository: no read of kind repo.
	if len(reads) != 1 || reads[0].Kind != gitlet.ReadRefs || readsOf(reads, gitlet.ReadRepo) != 0 {
		t.Errorf("the turn read %+v, want one read, code_get_repo's page of branches, which resolved the "+
			"repository", reads)
	}
	// main for the first read; none for the id; base and head in ONE; v1;
	// main, the landed-in commit being an id.
	if len(resolvesBefore) != 1 || len(resolves) != 4 {
		t.Fatalf("the turn made %d ref resolves (%d before the diff), want 4 (1)", len(resolves),
			len(resolvesBefore))
	}
	if diff := resolves[1].Refs; !slices.Equal(diff, []string{"main", "feature"}) {
		t.Errorf("the diff resolved %v in its one read, want both its sides, main and feature", diff)
	}
	for _, q := range reads {
		if q.Fresh.Level != statelog.ReadLinearizable {
			t.Errorf("a %s read was asked at %q, want linearizable", q.Kind, q.Fresh.Level)
		}
	}
	for _, q := range resolves {
		if q.Fresh.Level != statelog.ReadLinearizable {
			t.Errorf("a ref was resolved at %q, want linearizable", q.Fresh.Level)
		}
	}
	for _, q := range gits {
		if q.RepoID != siteID {
			t.Errorf("a git read named repository %q, want the id the turn resolved", q.RepoID)
		}
		at := q.Fresh.Floor(topics.CodeLogStream)
		if before, err := at.Before(host.floor); err != nil || before {
			t.Errorf("a git read was floored at %v, below the node's own %v", at, host.floor)
		}
		if below, err := at.Before(host.at); err != nil || !below {
			t.Errorf("a git read was floored at %v, at or past the checkpoint %v its refs were read at", at,
				host.at)
		}
	}

	// A WORKER IS ITS PARENT'S TURN: it reuses what the executor resolved.
	worker := turn.WithCalls(turnctx.NewCallLog())
	callCode(t, reg, worker, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID})
	if reads, _, _ := host.asked(); len(reads) != 1 {
		t.Errorf("a worker read the repository its parent resolved again: %+v", reads)
	}

	// THE OPERATOR SURFACE: no turn, so once a call.
	opHost := newGitletHost()
	tools := operatorCode(t, opHost, nil)
	for range 2 {
		if got, err := tools[builtin.CodeReadTool].Call(asBoundPerson(), map[string]any{"repo": "site",
			"rev": mainOID}); err != nil || got.Failed {
			t.Fatalf("an operator read: %v %s", err, got.Output)
		}
	}
	if reads, _, _ := opHost.asked(); readsOf(reads, gitlet.ReadRepo) != 2 {
		t.Errorf("two operator calls read the repository %d times, want once a call", readsOf(reads, gitlet.ReadRepo))
	}
}

// CODE_GET_REPO ANSWERS WHERE A REPOSITORY'S PURGE AND ITS IMPORT STAND: the
// detail is the only place either outcome is read, so it answers the expunge
// (the purge asked, the last committed) and the remotes (a mirror's address,
// an import's source, chunks and the sentence it ended short on), every
// credential a ${VAR} reference as the row holds it.
//
// Mutation: leave Expunge out of code_get_repo's detail, or Remotes → red.
func TestCodeGetRepoAnswersWhereAPurgeAndAnImportStand(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	host.detail.Expunge = gitlet.ExpungeState{Request: "purge-7", Requested: codeAt(9),
		ExpungedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), DroppedObjects: 12, DroppedBytes: 4096}
	host.detail.Remotes = gitlet.RemoteState{
		MirrorFrom: &gitlet.MirrorSource{URL: "https://forge.example.com/up/site.git", Credential: "${SITE_TOKEN}"},
		Import: &gitlet.ImportState{Source: gitlet.ImportSource{URL: "https://forge.example.com/up/site.git",
			Credential: "${SITE_TOKEN}"}, Chunks: 3, DoneAt: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC),
			Error: "blob 0123abcd is 9 bytes, past code.limits.max_blob_bytes (8)"},
	}
	got, _ := callCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), builtin.CodeGetRepoTool,
		map[string]any{"repo": "site"})
	var answer struct {
		Detail struct {
			Expunge struct {
				Request        string    `json:"request"`
				ExpungedAt     time.Time `json:"expunged_at"`
				DroppedObjects int64     `json:"dropped_objects"`
			} `json:"expunge"`
			Remotes struct {
				MirrorFrom struct {
					URL        string `json:"url"`
					Credential string `json:"credential"`
				} `json:"mirror_from"`
				Import struct {
					Chunks int64  `json:"chunks"`
					Error  string `json:"error"`
				} `json:"import"`
			} `json:"remotes"`
		} `json:"detail"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("code_get_repo answered %q: %v", got.Output, err)
	}
	if e := answer.Detail.Expunge; e.Request != "purge-7" || e.ExpungedAt.IsZero() || e.DroppedObjects != 12 {
		t.Errorf("code_get_repo answered the expunge %+v, want the purge asked and the last one committed", e)
	}
	r := answer.Detail.Remotes
	if r.MirrorFrom.URL != "https://forge.example.com/up/site.git" || r.MirrorFrom.Credential != "${SITE_TOKEN}" {
		t.Errorf("code_get_repo answered the pull mirror %+v, want its address and its variable's name", r.MirrorFrom)
	}
	if r.Import.Chunks != 3 || !strings.Contains(r.Import.Error, "max_blob_bytes") {
		t.Errorf("code_get_repo answered the import %+v, want its chunks and why it ended short", r.Import)
	}
}

// CODE_GET_REPO'S FIRST CALL IN A TURN IS ONE READ: its first page of branches
// carries the detail, so it resolves the repository too (one barrier, not a
// repo read then the page), and later calls reuse it. A LATER page asked first
// carries no detail, so it resolves the repository by a read of its own. The
// operator surface, with no turn, pays the one read each call.
//
// Mutation: repoRead ignoring the call's first read (reading kind repo, then
// the page) → red.
func TestCodeGetReposFirstCallIsOneRead(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	reg := codeRegistry(t, codeDeps(host))
	turn := codeTurn(t)
	callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	if reads, _, _ := host.asked(); len(reads) != 1 || reads[0].Kind != gitlet.ReadRefs || reads[0].Cursor != "" {
		t.Fatalf("code_get_repo's first call read %+v, want its one page of branches", reads)
	}
	callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID})
	if reads, _, gits := host.asked(); len(reads) != 2 || readsOf(reads, gitlet.ReadRepo) != 0 ||
		len(gits) != 1 || gits[0].RepoID != siteID {
		t.Errorf("the turn's later calls read %+v and %+v, want the second call's page alone and a read of "+
			"%s by the id the first resolved", reads, gits, siteID)
	}

	// A LATER PAGE FIRST.
	later := newGitletHost()
	callCode(t, codeRegistry(t, codeDeps(later)), codeTurn(t), builtin.CodeGetRepoTool,
		map[string]any{"repo": "site", "branches_cursor": "refs/heads/feature"})
	if reads, _, _ := later.asked(); len(reads) != 2 || reads[0].Kind != gitlet.ReadRepo ||
		reads[1].Kind != gitlet.ReadRefs || reads[1].Cursor != "refs/heads/feature" {
		t.Errorf("a later page asked first read %+v, want the repository, then the page", reads)
	}

	// THE OPERATOR SURFACE.
	opHost := newGitletHost()
	op := operatorCode(t, opHost, nil)
	for range 2 {
		if got, err := op[builtin.CodeGetRepoTool].Call(asBoundPerson(), map[string]any{"repo": "site"}); err != nil ||
			got.Failed {
			t.Fatalf("an operator's detail: %v %s", err, got.Output)
		}
	}
	if reads, _, _ := opHost.asked(); len(reads) != 2 || readsOf(reads, gitlet.ReadRefs) != 2 {
		t.Errorf("two operator calls read %+v, want one page of branches each", reads)
	}
}

// A NAME THAT NAMES ANOTHER REPOSITORY MID-TURN IS RESOLVED AGAIN (deleted and
// declared again): a read by NAME answering another repository is refused
// stale, one by COMMIT ID that its Gitlet node answers gone is refused not
// found. Either way the turn forgets its resolution, so the call made again
// reads what the name names now rather than an id nothing serves.
//
// Mutation: sameRepo ignoring the id or refusing without forgetting (by name),
// or a read by id answered gone leaving the turn's resolution in place (by
// commit id) → red.
func TestANameReplacedMidTurnIsResolvedAgain(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		args map[string]any
		want tools.Refusal
		// asked is how many git reads the refused call makes: none by name,
		// one of the old id by commit id.
		asked int
	}{
		{"a read by name", map[string]any{"repo": "site", "rev": "main"}, tools.RefusalStaleVersion, 0},
		{"a read by commit id", map[string]any{"repo": "site", "rev": mainOID}, tools.RefusalNotFound, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newGitletHost()
			reborn := host.detail.RepoRef
			reborn.ID = "0192f6c1-6a7e-7c1b-9d4e-000000000002"
			reg := codeRegistry(t, codeDeps(host))
			turn := codeTurn(t)
			callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})

			host.mu.Lock()
			host.replaced = &reborn
			host.mu.Unlock()
			got := refusedCode(t, reg, turn, builtin.CodeReadTool, c.args)
			if tools.RefusalOf(got) != c.want {
				t.Fatalf("a read of a replaced repository answered %+v, want refused %s", got, c.want)
			}
			reads, _, gits := host.asked()
			if len(gits) != c.asked || (c.asked > 0 && gits[0].RepoID != siteID) {
				t.Fatalf("the refused read asked the Gitlet nodes %+v, want %d reads of %s", gits, c.asked, siteID)
			}
			resolved := readsOf(reads, gitlet.ReadRepo)
			callCode(t, reg, turn, builtin.CodeReadTool, c.args)
			reads, _, gits = host.asked()
			if readsOf(reads, gitlet.ReadRepo) != resolved+1 || len(gits) != c.asked+1 ||
				gits[len(gits)-1].RepoID != reborn.ID {
				t.Errorf("the call made again read the repository %d more times and asked %+v, want it "+
					"resolved again and read by %s", readsOf(reads, gitlet.ReadRepo)-resolved, gits, reborn.ID)
			}
		})
	}
}

// ---- refusals ------------------------------------------------------------ //

// A NODE THAT CANNOT JUDGE IS NEVER A REFUSAL OF THE CALLER (M3-O82): a data
// node with no chart, a Gitlet node unready and a walk nobody served are told
// unavailable, which a retry clears, never `not_permitted`, the grant's alone.
//
// Mutation: ErrNotServing or ErrGitUnready classed forbidden → red.
func TestANodeThatCannotJudgeIsNeverARefusalOfTheCaller(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		readErr, gitErr error
		want            tools.Refusal
	}{
		"a reader with no chart": {readErr: fmt.Errorf("%w: this node holds no chart",
			gitlet.ErrNotServing), want: tools.RefusalUnavailable},
		"a Gitlet node with no chart": {gitErr: &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNoChart,
			Detail: "n1 holds no chart yet"}, want: tools.RefusalUnavailable},
		"no Gitlet node answering": {gitErr: &estate.ErrUnserved{Git: true, Detail: "n1 syncing"},
			want: tools.RefusalUnavailable},
		"no data node answering": {readErr: &estate.ErrUnserved{Detail: "n1 out of service"},
			want: tools.RefusalUnavailable},
		"the grant": {readErr: fmt.Errorf("%w: seat \"agent-cto\" holds no access to repository \"site\"",
			gitlet.ErrNotPermitted), want: tools.RefusalForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := newGitletHost()
			host.readErr, host.gitErr = c.readErr, c.gitErr
			got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t),
				builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID})
			if tools.RefusalOf(got) != c.want {
				t.Fatalf("answered %+v, want refused %s", got, c.want)
			}
			if c.want == tools.RefusalForbidden && !strings.Contains(got.Output, "holds no access") {
				t.Errorf("the grant's refusal lost the domain's sentence: %s", got.Output)
			}
		})
	}
}

// EVERY REFUSAL IS THE DOMAIN'S, IN ITS CLASS: an undeclared repository, an
// unheld ref, a missing object and a read too large (with its narrowing) are
// told in Gitlet's own sentence, classed as what the caller must change; a
// call missing what it reads is refused before anything is asked.
//
// Mutation: a domain refusal told as this node's fault, or its sentence
// dropped → red.
func TestEveryCodeRefusalIsTheDomainsInItsClass(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		tool            string
		args            map[string]any
		readErr, gitErr error
		want            tools.Refusal
		says            string
		asksNothing     bool
	}{
		"a repository nobody declared": {tool: builtin.CodeGetRepoTool, args: map[string]any{"repo": "nope"},
			want: tools.RefusalNotFound, says: `there is no repository "nope"`},
		"a ref nothing holds": {tool: builtin.CodeResolveTool, args: map[string]any{"repo": "site",
			"ref": "gone"}, want: tools.RefusalNotFound, says: "holds no live refs/heads/gone"},
		"an object the repository lacks": {tool: builtin.CodeReadTool, args: map[string]any{"repo": "site",
			"rev": mainOID}, gitErr: fmt.Errorf("%w: no object %s", gitlet.ErrObjectNotFound, mainOID),
			want: tools.RefusalNotFound, says: "no object " + mainOID},
		"a read too large to answer whole": {tool: builtin.CodeDiffTool, args: map[string]any{"repo": "site",
			"base": mainOID, "head": featureOID, "file": "a.txt"},
			gitErr: fmt.Errorf("%w: line 9 of the patch is past a page; read the file's lines instead",
				gitlet.ErrSliceTooLarge), want: tools.RefusalInvalid, says: "read the file's lines instead"},
		"a call naming no revision": {tool: builtin.CodeReadTool, args: map[string]any{"repo": "site"},
			want: tools.RefusalInvalid, says: "needs `rev`", asksNothing: true},
		"a name no repository can have": {tool: builtin.CodeGetRepoTool, args: map[string]any{"repo": "Not_A_Name"},
			want: tools.RefusalInvalid, says: "is not a repository's name", asksNothing: true},
		"a commit id as the branch asked about": {tool: builtin.CodeLandedInTool,
			args: map[string]any{"repo": "site", "commit": mainOID, "target": featureOID},
			want: tools.RefusalInvalid, says: "reads as a commit id", asksNothing: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := newGitletHost()
			host.readErr, host.gitErr = c.readErr, c.gitErr
			got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), c.tool, c.args)
			if tools.RefusalOf(got) != c.want || !strings.Contains(got.Output, c.says) {
				t.Errorf("answered %s %q, want refused %s saying %q", tools.RefusalOf(got), got.Output,
					c.want, c.says)
			}
			reads, resolves, gits := host.asked()
			if asked := len(reads) + len(resolves) + len(gits); c.asksNothing && asked != 0 {
				t.Errorf("a call refused for its arguments asked Gitlet %d questions", asked)
			}
		})
	}
}

// A PERSON IS HANDED THE CLONE URL, AND A SEAT IS NOT: a seat could not
// authenticate to it, while a person's or a token's git clones from it.
func TestAPersonIsHandedTheCloneURLAndASeatIsNot(t *testing.T) {
	t.Parallel()
	clone := func(name string) string { return "https://crewlet.example.com/git/" + name + ".git" }
	deps := codeDeps(newGitletHost())
	deps.CloneURL = clone
	reg := codeRegistry(t, deps)
	_, seat := callCode(t, reg, codeTurn(t), builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	if _, ok := seat["clone_url"]; ok {
		t.Errorf("a seat was handed a clone URL: %v", seat["clone_url"])
	}
	for name, ctx := range map[string]context.Context{"a person": asBoundPerson(), "a token": asUnboundToken()} {
		got, err := operatorCode(t, newGitletHost(), clone)[builtin.CodeGetRepoTool].Call(ctx,
			map[string]any{"repo": "site"})
		if err != nil || got.Failed {
			t.Fatalf("%s's detail: %v %s", name, err, got.Output)
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
			t.Fatal(err)
		}
		if answer["clone_url"] != "https://crewlet.example.com/git/site.git" {
			t.Errorf("%s was handed clone URL %v", name, answer["clone_url"])
		}
		if branches, _ := answer["branches"].([]any); len(branches) != 2 {
			t.Errorf("%s's detail listed branches %v, want main and feature", name, answer["branches"])
		}
	}
}

// ---- one page, one answer ------------------------------------------------ //

// longRef is the longest ref a push may create, in a byte JSON writes as six:
// the most a resolved ref weighs in an answer.
var longRef = "refs/heads/" + strings.Repeat("<", gitlet.MaxRefNameBytes-len("refs/heads/"))

// widest is a position at its widest encoding.
var widest = statelog.Position{Stream: topics.CodeLogStream, Generation: math.MaxUint32,
	Seq: math.MaxUint64}

// atTheBound is the fullest page fill(a, n) makes (n units: text bytes or list
// entries) within gitlet.MaxGitAnswerBytes of JSON, one unit more past it: cut
// where a Gitlet node cuts. Every other field is at its widest.
func atTheBound(t *testing.T, a gitlet.GitAnswer, fill func(a gitlet.GitAnswer, n int) gitlet.GitAnswer) gitlet.GitAnswer {
	t.Helper()
	a.Position, a.Next = widest, fmt.Sprintf("b:%d:%d", math.MaxInt, math.MaxInt)
	a.Coverage = &gitlet.Coverage{From: widest, Through: widest, GapFrom: widest, GapThrough: widest}
	a.PushedBy = map[string]gitlet.PushedBy{mainOID: {PushID: uuid.NewString(), Position: widest,
		Principal: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: strings.Repeat("s", 40)}, Via: gitlet.ViaGit}}
	size := func(n int) int {
		encoded, err := json.Marshal(fill(a, n))
		if err != nil {
			t.Fatal(err)
		}
		return len(encoded)
	}
	fits, over := 0, gitlet.MaxGitAnswerBytes
	for over-fits > 1 {
		if mid := fits + (over-fits)/2; size(mid) <= gitlet.MaxGitAnswerBytes {
			fits = mid
		} else {
			over = mid
		}
	}
	if size(fits) > gitlet.MaxGitAnswerBytes || size(fits+1) <= gitlet.MaxGitAnswerBytes {
		t.Fatalf("the fixture's fullest page is %d bytes and one unit more %d, which is not a page cut "+
			"at %d", size(fits), size(fits+1), gitlet.MaxGitAnswerBytes)
	}
	return fill(a, fits)
}

// A GIT ANSWER FITS ONE TOOL ANSWER: a page cut at the Gitlet node's bound,
// wrapped as each code tool wraps it (the longest repository name, every ref
// at its longest in six-byte JSON characters, the fence and its provenance
// envelope), is answered whole within builtin.ToolAnswerBytes. This measures
// what gitlet.MaxGitAnswerBytes leaves for the envelope.
//
// Mutation: rendering the page indented (jsonAnswer) → the tree and stat pages
// refused for their structure → red.
func TestAGitAnswerFitsOneToolAnswer(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("r", gitref.MaxRepoNameBytes)
	repoWith := func() *gitletHost {
		h := newGitletHost()
		h.detail.Name = name
		h.refs = map[string]string{longRef: mainOID}
		return h
	}
	files := func(n int) []gitlet.DiffFile {
		out := make([]gitlet.DiffFile, n)
		for i := range out {
			out[i] = gitlet.DiffFile{Path: fmt.Sprintf("f%04d", i), Status: "M", Added: 1, Deleted: 1,
				OldOID: blobOID, NewOID: mainOID, OldMode: "100644", NewMode: "100644"}
		}
		return out
	}
	for _, c := range []struct {
		name string
		tool string
		args map[string]any
		page func(t *testing.T, q gitlet.GitQuery) gitlet.GitAnswer
	}{
		{"a file's lines", builtin.CodeReadTool, map[string]any{"rev": longRef, "path": "README.md"},
			func(t *testing.T, q gitlet.GitQuery) gitlet.GitAnswer {
				return atTheBound(t, gitlet.GitAnswer{Kind: gitlet.GitBlob, RepoID: q.RepoID, Rev: blobOID},
					func(a gitlet.GitAnswer, n int) gitlet.GitAnswer {
						a.Blob = &gitlet.BlobAnswer{Path: "README.md", OID: blobOID, Size: math.MaxInt64,
							Lines: math.MaxInt, StartLine: math.MaxInt - 1, EndLine: math.MaxInt,
							StartByte: math.MaxInt, Text: strings.Repeat("x", n), Continues: true}
						return a
					})
			}},
		{"a tree's entries", builtin.CodeReadTool, map[string]any{"rev": longRef, "recursive": true},
			func(t *testing.T, q gitlet.GitQuery) gitlet.GitAnswer {
				return atTheBound(t, gitlet.GitAnswer{Kind: gitlet.GitTree, RepoID: q.RepoID, Rev: mainOID},
					func(a gitlet.GitAnswer, n int) gitlet.GitAnswer {
						// SHORT ENTRIES: the most structure, which indenting grows.
						entries := make([]gitlet.TreeEntry, n)
						for i := range entries {
							entries[i] = gitlet.TreeEntry{Mode: "100644", Type: "blob", OID: blobOID,
								Path: fmt.Sprintf("%06d", i), Size: 1}
						}
						a.Tree = &gitlet.TreeAnswer{Tree: blobOID, Entries: entries, Total: math.MaxInt}
						return a
					})
			}},
		{"a diff's patch", builtin.CodeDiffTool, map[string]any{"base": longRef, "head": longRef, "file": "f"},
			func(t *testing.T, q gitlet.GitQuery) gitlet.GitAnswer {
				return atTheBound(t, gitlet.GitAnswer{Kind: gitlet.GitDiff, RepoID: q.RepoID},
					func(a gitlet.GitAnswer, n int) gitlet.GitAnswer {
						a.Diff = &gitlet.DiffAnswer{Base: mainOID, Head: mainOID, Files: files(1),
							Total: math.MaxInt, Added: math.MaxInt, Deleted: math.MaxInt,
							Patch: strings.Repeat("x", n)}
						return a
					})
			}},
		{"a diff's stat", builtin.CodeDiffTool, map[string]any{"base": longRef, "head": longRef},
			func(t *testing.T, q gitlet.GitQuery) gitlet.GitAnswer {
				return atTheBound(t, gitlet.GitAnswer{Kind: gitlet.GitDiff, RepoID: q.RepoID},
					func(a gitlet.GitAnswer, n int) gitlet.GitAnswer {
						a.Diff = &gitlet.DiffAnswer{Base: mainOID, Head: mainOID, Files: files(n),
							Total: math.MaxInt, Added: math.MaxInt, Deleted: math.MaxInt}
						return a
					})
			}},
		{"a comparison", builtin.CodeCompareTool, map[string]any{"base": longRef, "head": longRef},
			func(t *testing.T, q gitlet.GitQuery) gitlet.GitAnswer {
				return atTheBound(t, gitlet.GitAnswer{Kind: gitlet.GitCompare, RepoID: q.RepoID},
					func(a gitlet.GitAnswer, n int) gitlet.GitAnswer {
						a.Compare = &gitlet.CompareAnswer{Base: mainOID, Head: mainOID, MergeBase: mainOID,
							BaseIsAncestor: true, HeadIsAncestor: true, Ahead: math.MaxInt, Behind: math.MaxInt,
							Files: files(n), Total: math.MaxInt, Added: math.MaxInt, Deleted: math.MaxInt}
						return a
					})
			}},
		{"a landing", builtin.CodeLandedInTool, map[string]any{"commit": longRef, "target": longRef},
			func(t *testing.T, q gitlet.GitQuery) gitlet.GitAnswer {
				return atTheBound(t, gitlet.GitAnswer{Kind: gitlet.GitLandedIn, RepoID: q.RepoID},
					func(a gitlet.GitAnswer, n int) gitlet.GitAnswer {
						by := a.PushedBy[mainOID]
						a.LandedIn = &gitlet.LandedInAnswer{Commit: mainOID, Target: longRef, TargetOID: mainOID,
							At: widest, Landed: true, LandedBy: &by, IntroducedBy: &by,
							Move:   &gitlet.RefMove{Ref: longRef, Old: mainOID, New: mainOID, Forced: true},
							Probes: math.MaxInt, Detail: strings.Repeat("x", n)}
						return a
					})
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := repoWith()
			host.git = func(q gitlet.GitQuery) gitlet.GitAnswer { return c.page(t, q) }
			args := map[string]any{"repo": name}
			for k, v := range c.args {
				args[k] = v
			}
			got, _ := callCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), c.tool, args)
			if len(got.Output) > builtin.ToolAnswerBytes {
				t.Fatalf("the answer is %d bytes, past %d", len(got.Output), builtin.ToolAnswerBytes)
			}
			t.Logf("%s at its widest: %d bytes of %d (%d spare)", c.name, len(got.Output),
				builtin.ToolAnswerBytes, builtin.ToolAnswerBytes-len(got.Output))
		})
	}

	// AND A HISTORY'S AND A BLAME'S PAGES, fencing a field per commit or line
	// (a hundred 4 KiB subjects, five hundred 8 KiB lines), cut by a Gitlet
	// node's own query server (historyNode): every page whole, in one answer.
	//
	// Mutation: the log's page bounded by its limit alone (a hundred fenced
	// subjects in one page) → its arm refused for its size, red.
	subject := closerSpelled(gitlet.MaxLogSubjectBytes)
	for _, arm := range []struct {
		name, tool string
		node       historyNode
		args       map[string]any
		count      func(page map[string]any) int
		want       int
	}{
		{"a history's commits", builtin.CodeLogTool,
			historyNode{commits: gitlet.MaxLogLimit, subject: subject},
			map[string]any{"rev": mainOID, "limit": gitlet.MaxLogLimit},
			func(page map[string]any) int { return len(page["log"].(map[string]any)["commits"].([]any)) },
			gitlet.MaxLogLimit},
		{"a blame's lines", builtin.CodeBlameTool,
			historyNode{lines: gitlet.MaxBlameLines, subject: subject, text: strings.Repeat("x", gitlet.MaxBlameLineBytes)},
			map[string]any{"rev": mainOID, "path": "f.txt"},
			func(page map[string]any) int { return len(page["blame"].(map[string]any)["lines"].([]any)) },
			gitlet.MaxBlameLines},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			n := arm.node
			tool := operatorCode(t, n.host(t), nil)[arm.tool]
			read, pages := 0, 0
			for cursor := ""; ; pages++ {
				args := map[string]any{"repo": "site", "cursor": cursor}
				for k, v := range arm.args {
					args[k] = v
				}
				got, err := tool.Call(asUnboundToken(), args)
				if err != nil || got.Failed {
					t.Fatalf("page %d: %v %.300s", pages, err, got.Output)
				}
				if len(got.Output) > builtin.ToolAnswerBytes {
					t.Fatalf("page %d is %d bytes, past %d", pages, len(got.Output), builtin.ToolAnswerBytes)
				}
				var page map[string]any
				if err := json.Unmarshal([]byte(got.Output), &page); err != nil {
					t.Fatal(err)
				}
				read += arm.count(page)
				next, _ := page["next"].(string)
				if next == "" {
					break
				}
				cursor = next
			}
			if read != arm.want {
				t.Errorf("the pages hold %d, want %d", read, arm.want)
			}
			t.Logf("%s: %d in %d pages", arm.name, read, pages+1)
		})
	}
}

// CODE_LOG NARROWS BY TIME AND BY WHO PUSHED: `until` and `pushed_by` reach
// Gitlet as given and `unjudged` comes back; a bad `until` or handle is
// refused unasked. Mutation: the tool dropping `until` or `pushed_by`.
func TestCodeLogNarrowsByTimeAndWhoPushed(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	none := 3
	host.git = func(q gitlet.GitQuery) gitlet.GitAnswer {
		return gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Rev: mainOID, Position: host.at,
			Log: &gitlet.LogAnswer{Commits: []gitlet.LogCommit{}, Complete: true, Unjudged: &none}}
	}
	reg := codeRegistry(t, codeDeps(host))
	_, answer := callCode(t, reg, codeTurn(t), builtin.CodeLogTool, map[string]any{"repo": "site", "rev": mainOID,
		"since": "2026-09-01T00:00:00Z", "until": "2026-10-01T00:00:00Z", "pushed_by": " agent-ceo "})
	_, _, gits := host.asked()
	until := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if len(gits) != 1 || gits[0].Log == nil || !gits[0].Log.Until.Equal(until) ||
		gits[0].Log.PushedBy != "agent-ceo" {
		t.Fatalf("the tool asked %+v, want one history until %s pushed by agent-ceo", gits, until)
	}
	if log, _ := answer["log"].(map[string]any); log == nil || log["unjudged"] != float64(none) {
		t.Errorf("the answer carries %v, want the page's %d unjudged", answer["log"], none)
	}
	for _, c := range []struct {
		args map[string]any
		says string
	}{
		{map[string]any{"until": "yesterday"}, "`until` is a time in RFC 3339"},
		{map[string]any{"pushed_by": "Not A Seat"}, "pushed_by"},
		{map[string]any{"since": "2026-10-01T00:00:00Z", "until": "2026-09-01T00:00:00Z"}, "until"},
	} {
		args := map[string]any{"repo": "site", "rev": mainOID}
		for k, v := range c.args {
			args[k] = v
		}
		got := refusedCode(t, reg, codeTurn(t), builtin.CodeLogTool, args)
		if !strings.Contains(got.Output, c.says) {
			t.Errorf("%v was refused with %q, want it to say %q", c.args, got.Output, c.says)
		}
	}
	if _, _, after := host.asked(); len(after) != 1 {
		t.Errorf("a refused call asked Gitlet %d more reads", len(after)-1)
	}
}

// closerSpelled spells the fence's closer as often as fits within bound once
// neutralised (each spelling paying its escape); past it, a subject would be
// described by its size instead.
func closerSpelled(bound int) string {
	const closer = "[/untrusted:AAAAAAAAAAAAAAAA] "
	var b strings.Builder
	for len(provenance.Neutralise("", b.String()+closer)) <= bound {
		b.WriteString(closer)
	}
	return b.String()
}

// A FENCED PAGE THAT OVERFLOWS IS REFUSED WITH ITS NARROWING, never cut: a
// page within the Gitlet node's bound whose every line is a fence marker,
// grown past one tool answer by escaping, is refused naming a narrower line
// range; such a patch, naming the file's lines to read instead.
//
// Mutation: the tool cutting the text to fit, or answering it unfenced → red.
func TestAFencedPageThatOverflowsIsRefusedWithItsNarrowing(t *testing.T) {
	t.Parallel()
	const marker = "[untrusted\n"
	// A LINE AS ITS PAGE WEIGHS IT: the newline is two bytes in JSON.
	encoded, _ := json.Marshal(marker)
	lines := (gitlet.MaxGitAnswerBytes - 1024) / (len(encoded) - len(`""`))
	text := strings.Repeat(marker, lines)
	for _, c := range []struct {
		name, tool string
		args       map[string]any
		answer     func(q gitlet.GitQuery) gitlet.GitAnswer
		says       string
	}{
		{"a file's lines", builtin.CodeReadTool, map[string]any{"rev": mainOID, "path": "a.txt"},
			func(q gitlet.GitQuery) gitlet.GitAnswer {
				return gitlet.GitAnswer{Kind: gitlet.GitBlob, RepoID: q.RepoID, Rev: blobOID,
					Blob: &gitlet.BlobAnswer{Path: "a.txt", OID: blobOID, Size: int64(len(text)), Lines: lines,
						StartLine: 1, EndLine: lines, Text: text}}
			}, fmt.Sprintf("read lines 1 to %d", 1+(lines-1)/2)},
		{"a patch", builtin.CodeDiffTool, map[string]any{"base": mainOID, "head": featureOID, "file": "a.txt"},
			func(q gitlet.GitQuery) gitlet.GitAnswer {
				return gitlet.GitAnswer{Kind: gitlet.GitDiff, RepoID: q.RepoID, Diff: &gitlet.DiffAnswer{
					Base: mainOID, Head: featureOID, Files: []gitlet.DiffFile{{Path: "a.txt", Status: "M"}},
					Patch: text}}
			}, "with " + builtin.CodeReadTool + " and a line range"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newGitletHost()
			host.git = c.answer
			args := map[string]any{"repo": "site"}
			for k, v := range c.args {
				args[k] = v
			}
			if page, _ := json.Marshal(c.answer(gitlet.GitQuery{RepoID: siteID})); len(page) > gitlet.MaxGitAnswerBytes {
				t.Fatalf("the fixture page is %d bytes, past what a Gitlet node sends", len(page))
			}
			got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), c.tool, args)
			if tools.RefusalOf(got) != tools.RefusalInvalid {
				t.Fatalf("an overflowing page answered %s, %d bytes, want refused invalid",
					tools.RefusalOf(got), len(got.Output))
			}
			if !strings.Contains(got.Output, c.says) {
				t.Errorf("the refusal does not name the narrowing %q: %s", c.says, got.Output)
			}
		})
	}
}

// branchesPastAnAnswer is how many of the longest branches the listing case
// gives a repository: at about 490 bytes each, half again one answer.
const branchesPastAnAnswer = 200

// A LISTING IS CUT WHOLE-ROW AT THE ANSWER: a page of repositories, or of a
// repository's branches, past one tool answer is answered with the whole rows
// that fit and `next` naming the last kept, where the reader's keyset goes on.
//
// Mutation: answering the reader's page whole (refused for its size), or next
// left as the reader's → red.
func TestAListingIsCutWholeRowAtTheAnswer(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	for i := range 100 {
		s := siteDetail().RepoSummary
		s.Name, s.Description = fmt.Sprintf("repo-%03d", i), strings.Repeat(`"`, gitlet.MaxDescriptionBytes)
		host.listing = append(host.listing, s)
	}
	// BRANCHES AT THEIR LONGEST, in `"`, which JSON writes as two bytes: the
	// answers are encoded plainly, so `<` would weigh one.
	for i := range branchesPastAnAnswer {
		host.refs[fmt.Sprintf("refs/heads/%03d-", i)+strings.Repeat(`"`, gitref.MaxBranchBytes-4)] = mainOID
	}
	reg := codeRegistry(t, codeDeps(host))
	got, listing := callCode(t, reg, codeTurn(t), builtin.CodeListReposTool, map[string]any{"limit": 100})
	repos, _ := listing["repos"].([]any)
	if len(repos) == 0 || len(repos) == 100 || len(got.Output) > builtin.ToolAnswerBytes {
		t.Fatalf("the listing kept %d of 100 rows in %d bytes", len(repos), len(got.Output))
	}
	last, _ := repos[len(repos)-1].(map[string]any)
	if listing["next"] != last["name"] {
		t.Errorf("the cut listing's next is %v, want the last kept row %v", listing["next"], last["name"])
	}

	got, detail := callCode(t, reg, codeTurn(t), builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	branches, _ := detail["branches"].([]any)
	if len(branches) == 0 || len(branches) >= branchesPastAnAnswer || len(got.Output) > builtin.ToolAnswerBytes {
		t.Fatalf("the detail kept %d branches in %d bytes", len(branches), len(got.Output))
	}
	lastBranch, _ := branches[len(branches)-1].(map[string]any)
	if detail["branches_next"] != "refs/heads/"+lastBranch["name"].(string) {
		t.Errorf("the cut branches' next is %v, want the last kept branch %v", detail["branches_next"],
			lastBranch["name"])
	}
}

// widestGate is a merge gate just short of gitlet.MaxGateBytes of JSON: its
// required checks at their count and length, its human paths in `"`, which
// every JSON encoder writes as two. Not `&`: the record's encoder writes it as
// six and the answers' as one, so its answer would be a fraction of the bound.
func widestGate(t *testing.T) gitlet.MergeGate {
	t.Helper()
	g := gitlet.MergeGate{MergeBy: gitlet.MergeByPersons, Approvals: gitlet.ApprovalsOne,
		Landing:      gitlet.Landing{Depth: gitlet.MaxQueueDepth, Batch: gitlet.MaxQueueBatch},
		DeleteSource: gitlet.DeleteSeats, OwnersApprove: true, AgentsEditChecks: true}
	for i := range gitlet.MaxRequiredChecks {
		g.Checks = append(g.Checks, fmt.Sprintf("%02d", i)+strings.Repeat("x", 62))
	}
	weigh := func() int {
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		return len(raw)
	}
	// WHAT A HUMAN PATH WEIGHS BESIDE ITS GLOB'S BYTES: at most thirty, for
	// the first (`,"human_paths":[`, `{"glob":"`, two digits, `"}]`).
	const frame = 30
	for len(g.HumanPaths) < gitlet.MaxHumanPaths {
		room := gitlet.MaxGateBytes - weigh() - frame
		if room < 2 {
			break
		}
		quotes := min(room/2, gitref.MaxPathGlobBytes-2)
		g.HumanPaths = append(g.HumanPaths, gitlet.HumanPath{
			Glob: fmt.Sprintf("%02d", len(g.HumanPaths)) + strings.Repeat(`"`, quotes)})
	}
	if n := weigh(); n > gitlet.MaxGateBytes || n < gitlet.MaxGateBytes-64 {
		t.Fatalf("the widest gate weighs %d bytes, want just short of %d", n, gitlet.MaxGateBytes)
	}
	return g
}

// EVERY REPOSITORY AT THE DOMAIN'S BOUNDS IS ANSWERED: gitlet.MaxProtectionsPerRepo
// rules at gitref's longest in two-byte JSON characters, each with a
// gitlet.MaxGateBytes gate, are half a mebibyte. code_list_repos lists it with
// its gateless rules and every repository after it; code_get_repo answers its
// detail, a page of branches and the rules that fit, then the rest through
// protections_next, each rule once, in order, with its gate and source. Every
// answer fits ToolAnswerBytes and makes progress. A row whose rules alone are
// past an answer is built past today's limits, as a raised limit would make.
//
// Mutations: such a row refused with no `next` (zed unlisted); protections_next
// dropped; no room kept for a branch on the first page; a rule's source left
// off either page → red.
func TestEveryRepositoryAtTheDomainsBoundsIsAnswered(t *testing.T) {
	t.Parallel()
	wide := func(n int) string { return strings.Repeat(`"`, n) }
	gate := widestGate(t)
	host := newGitletHost()
	d := siteDetail()
	d.Description = wide(gitlet.MaxDescriptionBytes)
	for i := range gitlet.MaxProtectionsPerRepo {
		head := fmt.Sprintf("refs/heads/%02d", i)
		rule := gitlet.PushRule{Pattern: head + wide(gitref.MaxRefNameBytes-len(head)), AllowForce: true,
			AllowDelete: true, DirectPush: gitlet.DirectPersons}
		d.Protections = append(d.Protections, rule)
		d.Gates = append(d.Gates, gitlet.RuleGate{Pattern: rule.Pattern, Gate: gate})
		d.RuleSources = append(d.RuleSources, gitlet.RuleSource{Pattern: rule.Pattern, SetBy: wide(iam.MaxLogin),
			SetByKind: gitlet.ActorOperator, SetAt: time.Date(2026, 10, 30, 9, i, 0, 0, time.UTC)})
	}
	// ITS REMOTES AND EXPUNGE AT THEIR WIDEST TOO: addresses and variables at
	// their bounds, a push mirror's ref patterns at their longest, and an
	// import's sentence (the engine's few hundred bytes) held at a kilobyte.
	remote := func() (string, string) {
		return wide(gitlet.MaxRemoteURLBytes), wide(gitlet.MaxCredentialRefBytes)
	}
	from, fromVar := remote()
	to, toVar := remote()
	source, sourceVar := remote()
	d.Remotes = gitlet.RemoteState{
		MirrorFrom: &gitlet.MirrorSource{URL: from, Credential: fromVar},
		MirrorTo:   &gitlet.MirrorTarget{URL: to, Credential: toVar, As: "agent-cto"},
		Import: &gitlet.ImportState{Source: gitlet.ImportSource{URL: source, Credential: sourceVar}, Chunks: 1 << 20,
			DoneAt: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), Error: wide(1024)},
		MirrorSyncedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
	}
	for i := range gitlet.MaxMirrorRefs {
		head := fmt.Sprintf("refs/heads/%02d", i)
		d.Remotes.MirrorTo.Refs = append(d.Remotes.MirrorTo.Refs, head+wide(gitref.MaxRefNameBytes-len(head)))
	}
	d.Expunge = gitlet.ExpungeState{Open: "e" + wide(35), Request: "p" + wide(35), Requested: codeAt(9),
		ExpungedAt: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC), DroppedObjects: 1 << 40, DroppedBytes: 1 << 50}
	host.detail = d
	for i := range 100 {
		host.refs[fmt.Sprintf("refs/heads/%03d", i)+wide(gitref.MaxBranchBytes-3)] = mainOID
	}
	wideRow := d.RepoSummary
	wideRow.Name = "big"
	alpha, zed := siteDetail().RepoSummary, siteDetail().RepoSummary
	alpha.Name, zed.Name = "alpha", "zed"
	host.listing = []gitlet.RepoSummary{alpha, wideRow, zed}
	reg := codeRegistry(t, codeDeps(host))
	turn := codeTurn(t)

	// THE LISTING, page after page: every repository once, and big's rules
	// either whole or left out with where they are read said.
	type listedRow struct {
		Name               string            `json:"name"`
		Protections        []json.RawMessage `json:"protections"`
		ProtectionsLeftOut string            `json:"protections_left_out"`
	}
	list := func(reg *tools.Registry) (listed []string, big listedRow) {
		t.Helper()
		for cursor, pages := "", 0; ; pages++ {
			if pages > 3 {
				t.Fatalf("the listing took more than %d pages: %v", pages, listed)
			}
			got, _ := callCode(t, reg, turn, builtin.CodeListReposTool, map[string]any{"cursor": cursor,
				"limit": 100})
			if len(got.Output) > builtin.ToolAnswerBytes {
				t.Fatalf("a listing page is %d bytes, past one answer", len(got.Output))
			}
			var page struct {
				Repos []listedRow `json:"repos"`
				Next  string      `json:"next"`
			}
			if err := json.Unmarshal([]byte(got.Output), &page); err != nil {
				t.Fatal(err)
			}
			for _, r := range page.Repos {
				listed = append(listed, r.Name)
				if r.Name == "big" {
					big = r
				}
			}
			if page.Next == "" {
				return listed, big
			}
			cursor = page.Next
		}
	}
	// AT THE BOUNDS, A ROW FITS WHOLE: written plainly, a pattern weighs at
	// most twice its bytes, and listed rules carry no gate.
	listed, big := list(reg)
	if want := []string{"alpha", "big", "zed"}; !slices.Equal(listed, want) {
		t.Errorf("the listing answered %v, want %v", listed, want)
	}
	if len(big.Protections) != len(d.Protections) || big.ProtectionsLeftOut != "" {
		t.Errorf("big is listed with %d of its %d rules and %q, want them whole", len(big.Protections),
			len(d.Protections), big.ProtectionsLeftOut)
	}
	// AND A ROW PAST ONE ANSWER (no record today's limits take) is listed
	// without its rules, saying where they are read, and the rest after it.
	past := wideRow
	for len(past.Protections)*2*gitref.MaxRefNameBytes <= builtin.ToolAnswerBytes {
		past.Protections = append(slices.Clone(past.Protections), d.Protections...)
	}
	overHost := newGitletHost()
	overHost.listing = []gitlet.RepoSummary{alpha, past, zed}
	listed, big = list(codeRegistry(t, codeDeps(overHost)))
	if want := []string{"alpha", "big", "zed"}; !slices.Equal(listed, want) {
		t.Errorf("past one answer, the listing answered %v, want %v", listed, want)
	}
	if big.Protections != nil || !strings.Contains(big.ProtectionsLeftOut, builtin.CodeGetRepoTool) {
		t.Errorf("big past one answer is listed with %d rules and %q, want its rules left out and where "+
			"they are read said", len(big.Protections), big.ProtectionsLeftOut)
	}

	// THE DETAIL: its first page, then its rules a page at a time.
	type answeredRule struct {
		gitlet.Protection
		SetBy     string           `json:"set_by"`
		SetByKind gitlet.ActorKind `json:"set_by_kind"`
		SetAt     time.Time        `json:"set_at"`
	}
	type rulesPage struct {
		Protections     []answeredRule    `json:"protections"`
		ProtectionsNext string            `json:"protections_next"`
		Branches        []json.RawMessage `json:"branches"`
	}
	answered := func(args map[string]any) rulesPage {
		t.Helper()
		got, _ := callCode(t, reg, turn, builtin.CodeGetRepoTool, args)
		if len(got.Output) > builtin.ToolAnswerBytes {
			t.Fatalf("code_get_repo %v answered %d bytes, past one answer", args, len(got.Output))
		}
		var page rulesPage
		if err := json.Unmarshal([]byte(got.Output), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := answered(map[string]any{"repo": "site"})
	switch {
	case len(first.Branches) == 0:
		t.Fatal("code_get_repo's first page answers no branch")
	case len(first.Protections) == 0 || len(first.Protections) == len(d.Protections):
		t.Fatalf("code_get_repo's first page answers %d of %d rules, want those that fit and the rest after",
			len(first.Protections), len(d.Protections))
	}
	if reads, _, _ := host.asked(); len(reads) == 0 || !reads[len(reads)-1].Gates {
		t.Error("code_get_repo's first page did not ask for the rules' merge gates")
	}
	rules, next := first.Protections, first.ProtectionsNext
	for pages := 0; next != ""; pages++ {
		if pages > len(d.Protections) {
			t.Fatalf("the rules took more than %d pages", pages)
		}
		page := answered(map[string]any{"repo": "site", "protections_cursor": next})
		if len(page.Protections) == 0 || page.Branches != nil {
			t.Fatalf("a page of rules after %s answered %d rules and %d branches, want rules alone",
				next, len(page.Protections), len(page.Branches))
		}
		rules, next = append(rules, page.Protections...), page.ProtectionsNext
	}
	if len(rules) != len(d.Protections) {
		t.Fatalf("the pages answered %d rules, want every one of the %d", len(rules), len(d.Protections))
	}
	for i, r := range rules {
		if r.PushRule != d.Protections[i] || r.Gate == nil || !r.Gate.Equal(gate) {
			t.Fatalf("rule %d answered as %s with a gate %v, want %s with its gate", i, r.Pattern, r.Gate != nil,
				d.Protections[i].Pattern)
		}
		if src := d.RuleSources[i]; r.SetBy != src.SetBy || r.SetByKind != src.SetByKind || !r.SetAt.Equal(src.SetAt) {
			t.Fatalf("rule %d (%s) answered as set by %q (%s) at %s, want its source, %q (%s) at %s", i, r.Pattern,
				r.SetBy, r.SetByKind, r.SetAt, src.SetBy, src.SetByKind, src.SetAt)
		}
	}

	// ONE CURSOR AT A TIME: the two continue two lists.
	refusedCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site",
		"protections_cursor": first.ProtectionsNext, "branches_cursor": "refs/heads/000"})
}

// ---- the fences ---------------------------------------------------------- //

// codeFenced runs one code read whose page answer builds from the turn's
// boundary, so its text can try to break out of the fence, and returns the
// fences seen (TestEveryUntrustedReadIsFenced's code rows).
func codeFenced(t *testing.T, tool string, args map[string]any, answer func(b provenance.Boundary,
	q gitlet.GitQuery) gitlet.GitAnswer) ([]fenceSeen, map[string]any) {

	t.Helper()
	turn, b := readerTurn(t)
	turn.Lookups = turnctx.NewLookups()
	host := newGitletHost()
	host.git = func(q gitlet.GitQuery) gitlet.GitAnswer { return answer(b, q) }
	args["repo"] = "site"
	got := callIn(t, codeRegistry(t, codeDeps(host)), turn, tool, args)
	return fencesIn(t, b, got.Output)
}

// ---- the catalogue, the guidance and the docs --------------------------- //

// codeCatalogueBudget is the most bytes the forty code tools'
// definitions may add to a run's tool list ([listedBytes]): a full_access
// seat's (the one tier offered every write, CodeWritesWithheld, but the
// person-only code_report_check, code_purge_unreachable and code_import_repo)
// and the operator catalogue's. Other tiers are held by
// [codeReviewCatalogueBudget] and [codeReadsCatalogueBudget].
//
// 57 500 is ~6% over 54 223, measured on the larger form a harness's prompt
// once carried (indented, HTML-escaped), so it holds for compact tools/list
// JSON (42 235 at last measure). Headroom admits a sentence, not a tool: a
// tool added raises this deliberately, with its measured cost (the executor
// prompt's rule).
const codeCatalogueBudget = 57_500

// codeReadsCatalogueBudget is the most bytes the thirteen code reads'
// definitions may add to a run's tool list: what every seat of a company
// hosting code carries every round, whatever its tier.
//
// 22 100 is ~6% over 20 878, measured on the same larger form as
// [codeCatalogueBudget] (18 165 as served at last measure); a read added
// raises it as a tool raises that.
const codeReadsCatalogueBudget = 22_100

// codeReviewCatalogueBudget is the most bytes a review seat's code tools may
// add to a run's tool list: the thirteen reads and the four conversation
// writes its tier reaches (CodeWritesWithheld), the most below full_access,
// since a read_only seat carries only the comment of the four.
//
// 23 800 is ~6% over 22 443 measured as the session serves them, the rule
// [codeCatalogueBudget] states.
const codeReviewCatalogueBudget = 23_800

// THE CODE TOOLS' CATALOGUE STAYS UNDER ITS BUDGET: every listed tool's name,
// description, schema and hints reach the model's context and are paid again
// on every request. A seat of a company hosting code gets the reads every
// run, a review seat four conversation writes beside them, full_access
// twenty-four. Each
// set is measured as a client receives it through a real MCP session (what a
// harness is handed, not what was registered) and held to a budget, so a
// description edit is a decision rather than drift, as the executor prompt's
// budgets make a sentence one (internal/agent/prompts budget_test.go).
func TestTheCodeToolsCatalogueStaysUnderBudget(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		what        string
		got, budget int
	}{
		{"the thirteen code reads (every seat's)",
			listedBytes(t, builtin.CodeTools(withSearch(codeDeps(newGitletHost()), &codeSearches{}))),
			codeReadsCatalogueBudget},
		{"a review seat's thirteen reads and four conversation writes",
			listedBytes(t, offeredAt(gitlet.TierReview, builtin.CodeTools(withSearch(writeDeps(newGitletHost(),
				&codeWrites{}), &codeSearches{})))), codeReviewCatalogueBudget},
		{"all forty code tools (a full_access seat's and the operator's)",
			listedBytes(t, builtin.CodeTools(writeDeps(newGitletHost(), &codeWrites{}))), codeCatalogueBudget},
	} {
		tokens := harness.EstimateTokens(strings.Repeat(" ", c.got))
		if c.got > c.budget {
			t.Errorf("%s add %d bytes (~%d tokens) to every run's tool list, past their budget of %d", c.what,
				c.got, tokens, c.budget)
			continue
		}
		t.Logf("%s add %d bytes (~%d tokens) to every run's tool list (budget %d, headroom %d)", c.what, c.got,
			tokens, c.budget, c.budget-c.got)
	}
}

// offeredAt is the tools of every that a seat at tier is offered: every one
// but the code writes its tier withholds (CodeWritesWithheld).
func offeredAt(tier gitlet.Tier, every []tools.Callable) []tools.Callable {
	withheld := builtin.CodeWritesWithheld(tier)
	return slices.DeleteFunc(every, func(tool tools.Callable) bool {
		return slices.Contains(withheld, tool.Name())
	})
}

// listedBytes is what offered costs in a run's tools/list answer: each tool's
// JSON as an MCP client receives it, summed, served by the session's own
// server over the SDK's in-memory transport with the registry's hints
// (builtin.AnnotationsFor).
func listedBytes(t *testing.T, offered []tools.Callable) int {
	t.Helper()
	catalogue := make(listedOnly, 0, len(offered))
	for _, tool := range offered {
		catalogue = append(catalogue, toolsession.Tool{
			Def:         llm.ToolDef{Name: tool.Name(), Description: tool.Description(), Parameters: tool.Parameters()},
			Annotations: builtin.AnnotationsFor(tool.Name())})
	}
	srv := toolsession.NewServer("crewlet", "Crewlet", "", catalogue)
	srv.Sync()
	ctx := t.Context()
	serverEnd, clientEnd := sdkmcp.NewInMemoryTransports()
	served, err := srv.MCP().Connect(ctx, serverEnd, nil)
	if err != nil {
		t.Fatalf("serve the tool list: %v", err)
	}
	t.Cleanup(func() { _ = served.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "catalogue", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientEnd, nil)
	if err != nil {
		t.Fatalf("connect to the tool list: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list the tools: %v", err)
	}
	if len(listed.Tools) != len(offered) {
		t.Fatalf("the session listed %d tools of the %d offered", len(listed.Tools), len(offered))
	}
	total := 0
	for _, tool := range listed.Tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("encode %s as it was listed: %v", tool.Name, err)
		}
		total += len(raw)
	}
	return total
}

// listedOnly is a tool session's dispatcher that offers a fixed catalogue and
// runs nothing: what [listedBytes] measures is the list, never a call.
type listedOnly []toolsession.Tool

func (l listedOnly) Offered() []toolsession.Tool { return l }

func (listedOnly) Revision() uint64 { return 1 }

func (listedOnly) Call(toolsession.Call) toolsession.Result {
	return toolsession.Result{Output: "this session lists tools and runs none", Failed: true}
}

// ---- the guidance and the docs ------------------------------------------ //

// THE CODE GUIDANCE NAMES THE CODE TOOLS: every tool the executor's guidance
// is gated on is a code tool here, so it never sends a seat to a missing one;
// prompts cannot import this package to check for itself.
func TestTheCodeGuidanceNamesTheCodeTools(t *testing.T) {
	t.Parallel()
	if len(prompts.CodeHistoryTools()) == 0 {
		t.Fatal("the guidance names no tool, so this certifies nothing")
	}
	for _, name := range prompts.CodeHistoryTools() {
		if !slices.Contains(builtin.CodeToolNames(), name) {
			t.Errorf("the executor's code guidance is gated on %s, which is no code tool", name)
		}
	}
}

// THE PAGES NAME EVERY CODE TOOL THE REGISTRY HOLDS: the tools guide's table
// lists exactly the code tools and the operator reference's account of
// `/operator/mcp` names each, two hand-kept copies that otherwise drift.
func TestThePagesNameEveryCodeTool(t *testing.T) {
	t.Parallel()
	guide := readPage(t, "docs/guides/tools-and-mcp.md")
	start := strings.Index(guide, "### The company's code")
	end := strings.Index(guide, "### Per-Role MCP Servers")
	if start < 0 || end < start {
		t.Fatal("the tools guide has no code tools section to read")
	}
	var rows []string
	for _, m := range regexp.MustCompile("\\| `(code_[a-z_]+)` \\|").FindAllStringSubmatch(guide[start:end], -1) {
		rows = append(rows, m[1])
	}
	every := slices.Concat(builtin.CodeToolNames(), []string{builtin.CodeSearchTool}, builtin.CodeWrites())
	if !slices.Equal(rows, every) {
		t.Errorf("the tools guide's code table lists %v, want %v in that order", rows, every)
	}
	reference := readPage(t, "docs/reference/api-endpoints.md")
	served := reference[strings.Index(reference, "### What it serves"):]
	served = served[:strings.Index(served, "**An argument a tool does not take is refused")]
	for _, name := range every {
		if !strings.Contains(served, "`"+name+"`") {
			t.Errorf("docs/reference/api-endpoints.md's account of what /operator/mcp serves does not name %s", name)
		}
	}
}

// A CODE READ IN A TURN A PERSON WITHOUT GITLET WOKE IS REFUSED (ADR-0110).
//
// A woken seat uses only the apps its waker uses (ADR-0109), so each code read
// is refused for a person holding every app but Gitlet, naming Gitlet and who
// can give it, and Gitlet is asked nothing: no listing, file or count leaks.
// A credential acting as no seat without Gitlet (a `[worklet]` token) is
// refused the same way, naming the credential. The CONTROL is the founder,
// who holds Gitlet.
//
// Mutation: put the code rows back on ClassEverybody, and the contractor's
// turn and the token's list the repositories.
func TestACodeReadInATurnAPersonWithoutGitletWokeIsRefused(t *testing.T) {
	t.Parallel()
	var others []iam.Grant
	for _, app := range iam.Apps {
		if app != iam.GrantGitlet {
			others = append(others, app)
		}
	}
	people := codeRequesters()
	people["contractor"] = iam.Principal{Kind: iam.KindPerson, Login: "contractor@example.com",
		Seat: "contractor", Stage: iam.StageActive, Grants: others}
	people["login:token:worklet"] = iam.Principal{Kind: iam.KindMachine, Login: "token:worklet",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}}
	host := newGitletHost()
	reg := codeRegistry(t, builtin.CodeDeps{Code: host, Requesters: people, Chart: chartLeads})
	turn := codeTurn(t)
	turn.Requester, turn.Senders = "contractor", []types.CanonicalIdentity{{Handle: "contractor"}}
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.CodeListReposTool, map[string]any{}},
		{builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID}},
		{builtin.CodeGetRepoTool, map[string]any{"repo": "site"}},
	} {
		got := refusedCode(t, reg, turn, call.tool, call.args)
		var refused *builtin.DecisionRefused
		if tools.RefusalOf(got) != tools.RefusalForbidden || !errors.As(got.Cause, &refused) ||
			refused.Decision.Reason != authz.ReasonCeiling ||
			!strings.Contains(got.Output, "does not use Gitlet") ||
			!strings.Contains(got.Output, "an administrator can give them Gitlet") {
			t.Errorf("%s in a turn a person without Gitlet woke answered %s %q (%v), want refused for them, "+
				"naming Gitlet", call.tool, tools.RefusalOf(got), got.Output, got.Cause)
		}
	}
	token := codeTurn(t)
	token.RequesterLogin, token.RequesterApps = "token:worklet", []iam.Grant{iam.GrantWorklet}
	token.Senders = []types.CanonicalIdentity{{ExternalID: "token:worklet", Platform: "worklet",
		Login: "token:worklet"}}
	got := refusedCode(t, reg, token, builtin.CodeListReposTool, map[string]any{})
	var refused *builtin.DecisionRefused
	if tools.RefusalOf(got) != tools.RefusalForbidden || !errors.As(got.Cause, &refused) ||
		refused.Decision.Reason != authz.ReasonCeiling ||
		!strings.Contains(got.Output, "the credential this turn is working for does not use Gitlet") {
		t.Errorf("a listing in a turn a [worklet] token woke answered %s %q (%v), want refused for the "+
			"credential, naming Gitlet", tools.RefusalOf(got), got.Output, got.Cause)
	}
	if reads, resolves, gits := host.asked(); len(reads)+len(resolves)+len(gits) != 0 {
		t.Errorf("the refused reads asked Gitlet %d reads, %d resolves and %d git reads, want nothing",
			len(reads), len(resolves), len(gits))
	}
	control := codeTurn(t)
	control.Requester, control.Senders = "founder", []types.CanonicalIdentity{{Handle: "founder"}}
	callCode(t, reg, control, builtin.CodeListReposTool, map[string]any{})
}

// A CODE_LOG ANSWER NAMES ITS COMMITS' TAGS where the node's index holds them.
//
// Mutation: clear Tagged in the tool's answer → red.
func TestACodeLogAnswerNamesItsCommitsTags(t *testing.T) {
	t.Parallel()
	marks := &gitlet.TagMarks{Commits: map[string]gitlet.CommitTags{mainOID: {Refs: []string{"refs/tags/v1"},
		More: 2}}}
	for _, tagged := range []*gitlet.TagMarks{marks, nil} {
		host := newGitletHost()
		host.git = func(q gitlet.GitQuery) gitlet.GitAnswer {
			return gitlet.GitAnswer{Kind: gitlet.GitLog, RepoID: q.RepoID, Rev: mainOID, Tagged: tagged,
				Log: &gitlet.LogAnswer{Scanned: 1, Commits: []gitlet.LogCommit{{OID: mainOID, Subject: "one"}}}}
		}
		_, answer := callCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), builtin.CodeLogTool,
			map[string]any{"repo": "site", "rev": mainOID})
		got, carried := answer["tagged"].(map[string]any)
		switch {
		case tagged == nil && carried:
			t.Errorf("a page from a node keeping no tag index carries tagged %v", got)
		case tagged != nil:
			commit, _ := got["commits"].(map[string]any)[mainOID].(map[string]any)
			if refs, _ := commit["refs"].([]any); len(refs) != 1 || refs[0] != "refs/tags/v1" || commit["more"] != 2.0 {
				t.Errorf("the page carries tagged %v, want v1 and 2 more for the commit", answer["tagged"])
			}
		}
	}
}
