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

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- a code host the cases read ---------------------------------------- //

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

// siteDetail is the one repository the code host holds: `site`, the eng
// unit's, read by the company.
func siteDetail() code.RepoDetail {
	return code.RepoDetail{
		RepoSummary: code.RepoSummary{
			RepoRef: code.RepoRef{ID: siteID, Name: "site", Status: code.RepoStatusActive,
				ManagedBy: code.ManagedConfig, Owner: code.Owner{Kind: code.OwnerUnit, Ref: "eng"},
				Visibility: code.VisibilityCompany, Search: code.SearchIndexed, DefaultBranch: "main",
				Refs: 2, LivePacks: 1,
				LiveBytes: 4096, Position: codeAt(7)},
			Access: "read", Head: mainOID, Pushes: 3,
		},
		Branches: 2,
	}
}

// codeHost is the routed code host as the code tools see it: one repository,
// its refs, and a git node answering by object id — recording every question
// it is asked, at the level it was asked.
type codeHost struct {
	mu sync.Mutex

	detail  code.RepoDetail
	listing []code.RepoSummary // the repos listing; the one repository when nil
	refs    map[string]string  // whole ref → oid
	// moved is where each ref last moved, the push that set it (movedAt):
	// below at, as every row a read answers is below its checkpoint.
	moved map[string]statelog.Position
	floor statelog.Position
	at    statelog.Position

	// replaced, set, is the repository every read BY NAME after the first
	// names: a name deleted and declared again between two reads — and a
	// read by object id of any other repository is answered gone.
	replaced *code.RepoRef
	readErr  error
	gitErr   error
	git      func(q code.GitQuery) code.GitAnswer
	// before, set, is called with each question's kind before it is
	// answered, outside the host's lock — so a case can read what the asking
	// turn held when the question arrived.
	before func(what string)

	reads    []code.ReadQuery
	resolves []code.RefQuery
	gits     []code.GitQuery
}

var _ builtin.CodeReader = (*codeHost)(nil)

// newCodeHost is the code host holding site: its refs each last moved at a
// position of its own, between this node's own floor and the checkpoint every
// read answers at, so a read's floor says which of the three it was given.
func newCodeHost() *codeHost {
	return &codeHost{
		detail: siteDetail(),
		refs: map[string]string{
			"refs/heads/main": mainOID, "refs/heads/feature": featureOID, "refs/tags/v1": mainOID,
		},
		moved: map[string]statelog.Position{
			"refs/heads/main": codeAt(10), "refs/heads/feature": codeAt(11), "refs/tags/v1": codeAt(8),
		},
		floor: codeAt(3), at: codeAt(12),
	}
}

// movedAt is where ref last moved: its own position, or the log's first for a
// ref a case added without one.
func (h *codeHost) movedAt(ref string) statelog.Position {
	if at, ok := h.moved[ref]; ok {
		return at
	}
	return codeAt(1)
}

func (h *codeHost) named(name string) (code.RepoRef, error) {
	if name != h.detail.Name {
		return code.RepoRef{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, name)
	}
	if h.replaced != nil && len(h.reads)+len(h.resolves) > 1 {
		return *h.replaced, nil
	}
	return h.detail.RepoRef, nil
}

func (h *codeHost) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if h.before != nil {
		h.before("read:" + string(q.Kind))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, q)
	if h.readErr != nil {
		return code.ReadAnswer{}, h.readErr
	}
	out := code.ReadAnswer{Position: h.at, Level: q.Fresh.Level}
	switch q.Kind {
	case code.ReadRepos:
		out.Repos = h.listing
		if out.Repos == nil {
			out.Repos = []code.RepoSummary{h.detail.RepoSummary}
		}
		return out, nil
	case code.ReadRepo, code.ReadRefs:
		ref, err := h.named(q.Repo)
		if err != nil {
			return code.ReadAnswer{}, err
		}
		if q.Kind == code.ReadRepo || q.Cursor == "" {
			d := h.detail
			d.RepoRef = ref
			out.Repo = &d
		}
		if q.Kind == code.ReadRefs {
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
				out.Refs = append(out.Refs, code.RefRow{Ref: name, OID: h.refs[name], Position: h.movedAt(name)})
			}
		}
		return out, nil
	}
	return code.ReadAnswer{}, fmt.Errorf("%w: a code tool read kind %s", code.ErrInvalid, q.Kind)
}

func (h *codeHost) ResolveRef(_ context.Context, q code.RefQuery) (code.RefAnswer, error) {
	if h.before != nil {
		h.before("resolve_ref")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolves = append(h.resolves, q)
	if h.readErr != nil {
		return code.RefAnswer{}, h.readErr
	}
	ref, err := h.named(q.Repo)
	if err != nil {
		return code.RefAnswer{}, err
	}
	out := code.RefAnswer{Repo: ref, Position: h.at}
	for _, asked := range q.Refs {
		names := []string{asked}
		if !strings.HasPrefix(asked, "refs/") {
			names = []string{"refs/heads/" + asked, "refs/tags/" + asked}
		}
		resolved := code.ResolvedRef{Name: asked, Missing: fmt.Sprintf("repository %s holds no live %s", q.Repo,
			strings.Join(names, " or "))}
		for _, name := range names {
			if oid, ok := h.refs[name]; ok {
				resolved = code.ResolvedRef{Name: asked, Ref: name, OID: oid, Moved: h.movedAt(name)}
				break
			}
		}
		out.Refs = append(out.Refs, resolved)
	}
	return out, nil
}

func (h *codeHost) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	if h.before != nil {
		h.before("git_read:" + string(q.Kind))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gits = append(h.gits, q)
	if h.gitErr != nil {
		return code.GitAnswer{}, h.gitErr
	}
	if h.replaced != nil && q.RepoID != h.replaced.ID {
		// THE ID THE NAME NAMED BEFORE, purged: what a git node answers a
		// read of it.
		return code.GitAnswer{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound,
			code.RepoGone{ID: q.RepoID, Name: h.detail.Name, Purged: true}.Detail())
	}
	if h.git != nil {
		return h.git(q), nil
	}
	out := code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Position: h.at}
	switch q.Kind {
	case code.GitBlob:
		out.Rev = blobOID
		out.Blob = &code.BlobAnswer{Path: q.Blob.Path, OID: blobOID, Size: 6, Lines: 1, StartLine: 1,
			EndLine: 1, Text: "hello\n"}
	case code.GitTree:
		out.Rev = mainOID
		out.Tree = &code.TreeAnswer{Tree: blobOID, Entries: []code.TreeEntry{}}
	case code.GitDiff:
		out.Diff = &code.DiffAnswer{Base: q.Diff.Base, Head: q.Diff.Head, Files: []code.DiffFile{}}
	case code.GitCompare:
		out.Compare = &code.CompareAnswer{Base: q.Compare.Base, Head: q.Compare.Head, Files: []code.DiffFile{}}
	case code.GitLandedIn:
		out.LandedIn = &code.LandedInAnswer{Commit: q.LandedIn.Commit, Target: q.LandedIn.Target,
			TargetOID: q.LandedIn.TargetOID, At: q.LandedIn.At, Landed: true}
	}
	return out, nil
}

// FloorOf is this node's floor on the one repository it wrote to, site's id,
// and none on any other: what a read of the repository a name names now — a
// name declared again — carries.
func (h *codeHost) FloorOf(repoID string) statelog.Position {
	if repoID != siteID {
		return statelog.Position{}
	}
	return h.floor
}

// asked is what the host was asked, copied under its lock.
func (h *codeHost) asked() (reads []code.ReadQuery, resolves []code.RefQuery, gits []code.GitQuery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reads), slices.Clone(h.resolves), slices.Clone(h.gits)
}

// readsOf counts the reads of kind.
func readsOf(reads []code.ReadQuery, kind code.ReadKind) int {
	n := 0
	for _, q := range reads {
		if q.Kind == kind {
			n++
		}
	}
	return n
}

// codeRequesters is who a turn reading code may be acting for, composed as the
// engine composes them: the founder, a person of the company; agent-ceo, a
// colleague seat; and "departed", a person whose seat's holder was removed,
// who acts as nobody now. Any other handle is a requester this node cannot
// compose.
func codeRequesters() requesters {
	return requesters{
		"founder":   founderPrincipal(iam.Apps...),
		"agent-ceo": {Kind: iam.KindSeat, Seat: "agent-ceo", Stage: iam.StageActive, Grants: iam.Apps},
		"departed": {Kind: iam.KindPerson, Login: "departed@example.com", Seat: "departed",
			Stage: iam.StageRetired, Grants: iam.Apps},
	}
}

// codeDeps is the code read tools wired over host as every surface wires them:
// the code host, beside who a turn is acting for and the chart that decides
// them (the requester ceiling), without either of which no code tool is
// offered. The writes join where the code host's writes are wired
// ([writeDeps]).
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

// asUnboundToken is a machine token bound to nobody, as CI holds one.
func asUnboundToken() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee4"),
		Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive,
	})
}

// ---- one list, two surfaces --------------------------------------------- //

// THE CODE TOOLS ARE ONE LIST: a seat's registry and the operator catalogue,
// built over the same deps, hold the same nineteen tools — the nine reads,
// then the ten writes, every one the authority table names, in the list's own
// order — with byte-identical descriptions and schemas but for the one
// argument the operator surface alone offers, the writes' `op_id`, which no
// read and no seat's tool offers; because both take [builtin.CodeTools]. A
// company that hosts no code is given none on either — nor is a surface that
// wired the code host and left out who a turn is acting for, or the chart that
// decides them, which would read for whoever messaged a seat whatever the seat
// itself may.
//
// Mutation: OperatorTools omitting codeCandidates, or building a list of its
// own with one schema edited; CodeTools serving over the code host alone; the
// writes' op_id offered to a seat → red.
func TestTheCodeToolsAreOneList(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	reg := codeRegistry(t, writeDeps(host, &codeWrites{}))
	operator := operatorCode(t, host, nil)
	every := slices.Concat(builtin.CodeToolNames(), builtin.CodeWrites())
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
		t.Error("CodeTools with no code host listed tools")
	}
	for name, deps := range map[string]builtin.CodeDeps{
		"no requesters": {Code: host, Chart: chartLeads},
		"no chart":      {Code: host, Requesters: codeRequesters()},
	} {
		if served := builtin.CodeTools(deps); len(served) != 0 {
			t.Errorf("CodeTools over a code host with %s listed %d tools", name, len(served))
		}
		if _, ok := codeRegistry(t, deps).Lookup(builtin.CodeReadTool); ok {
			t.Errorf("a seat's registry over a code host with %s holds %s", name, builtin.CodeReadTool)
		}
	}
}

// EVERY CODE READ IS A PROVEN READ, on both surfaces: the registry carries the
// hint the delivery check reads, and the operator catalogue advertises the one
// it audits nothing on — every code tool that is not one of the ten writes,
// on a registry that holds the writes too. Mutation: one code read off
// codeAnnotations (the default arm's shared write) → red.
func TestEveryCodeToolIsAProvenRead(t *testing.T) {
	t.Parallel()
	reg := codeRegistry(t, writeDeps(newCodeHost(), &codeWrites{}))
	var reads []string
	for _, tool := range builtin.CodeTools(writeDeps(newCodeHost(), &codeWrites{})) {
		if !slices.Contains(builtin.CodeWrites(), tool.Name()) {
			reads = append(reads, tool.Name())
		}
	}
	if !slices.Equal(reads, builtin.CodeToolNames()) {
		t.Fatalf("the code tools that are not writes are %v, want the reads %v", reads, builtin.CodeToolNames())
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

// A CODE READ IS JUDGED AS ITS CALLER and nobody else: a seat's turn asks as
// the seat by its handle, a person's assistant as the credential they came
// through bound to their seat, a token bound to nobody as itself — the one
// conversion the REST reads use too — and no argument names who reads: one
// trying is refused before anything is read.
//
// Mutation: the asker taken from the turn's requester, or a person asked as
// their seat with no credential → red.
func TestACodeReadIsJudgedAsItsCaller(t *testing.T) {
	t.Parallel()
	seatHost := newCodeHost()
	reg := codeRegistry(t, codeDeps(seatHost))
	turn := codeTurn(t)
	turn.Requester = "founder"
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": "main", "path": "README.md"})
	callCode(t, reg, turn, builtin.CodeListReposTool, map[string]any{})
	seat := code.Principal{Kind: code.PrincipalAgent, Seat: "agent-cto"}
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
		want code.Principal
	}{
		"a bound person": {asBoundPerson(), code.Principal{Kind: code.PrincipalOperator,
			OperatorID: "pat:jane-laptop", Seat: "founder"}},
		"an unbound token": {asUnboundToken(), code.Principal{Kind: code.PrincipalOperator,
			OperatorID: "token:ci"}},
	} {
		host := newCodeHost()
		tools := operatorCode(t, host, nil)
		if got, err := tools[builtin.CodeListReposTool].Call(c.ctx, map[string]any{}); err != nil || got.Failed {
			t.Fatalf("%s's listing: %v %s", name, err, got.Output)
		}
		reads, _, _ := host.asked()
		if len(reads) != 1 || reads[0].Asker != c.want {
			t.Errorf("%s asked as %+v, want %+v", name, reads, c.want)
		}
		if want := builtin.CodeAskerOf(mustActor(t, c.ctx)); want != c.want {
			t.Errorf("%s converts to %+v through CodeAskerOf, want %+v", name, want, c.want)
		}
	}

	// NO ARGUMENT NAMES WHO READS: one that tries is refused before the code
	// host is asked anything.
	host := newCodeHost()
	got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t),
		builtin.CodeListReposTool, map[string]any{"seat": "founder"})
	if !errors.Is(got.Cause, builtin.ErrUndeclaredArgument) {
		t.Fatalf("a call naming a seat to read as answered %+v", got)
	}
	if reads, _, _ := host.asked(); len(reads) != 0 {
		t.Errorf("a refused call asked the code host %+v", reads)
	}
}

// mustActor is the actor the operator surface writes ctx's principal down as.
func mustActor(t *testing.T, ctx context.Context) builtin.Actor {
	t.Helper()
	a, err := builtin.PrincipalActor(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// ---- whom a turn reads for ------------------------------------------------ //

// everyCodeQuestion makes, in turn, a call asking each kind of question a code
// tool asks the code host: the listing, a repository's first page of branches
// (which reads its detail), a file at a branch (a ref resolved, then a read by
// object id) and a diff of two branches.
func everyCodeQuestion(t *testing.T, reg *tools.Registry, turn *turnctx.Turn) {
	t.Helper()
	callCode(t, reg, turn, builtin.CodeListReposTool, map[string]any{})
	callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": "main", "path": "README.md"})
	callCode(t, reg, turn, builtin.CodeDiffTool, map[string]any{"repo": "site", "base": "main", "head": "feature"})
}

// readsFor holds every question host was asked to name ceiling as the seat it
// is made for — "" for none — and to have asked each kind at least once.
func readsFor(t *testing.T, host *codeHost, ceiling string) {
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

// A CODE READ IS DECIDED AGAIN FOR WHOEVER WOKE THE TURN — the requester
// ceiling the artifacts tools apply, here on a repository's grant. A turn
// nobody woke, one the seat woke itself — the outcome of its own act among
// them, with the seat as its sender — and one a person woke read for nobody
// but the seat, since a person maintains every repository and so bounds
// nothing; a turn a colleague seat woke names that seat on every question it
// asks — the listing, a repository, the refs it resolves, every read by object
// id — for the node that answers to judge the colleague's grant beside the
// seat's own. A turn a stranger woke, or a person who may not act in the
// company now, reads nothing: refused with the ceiling's reason before the
// code host is asked anything, whoever else spoke in the turn, the seat
// itself included. A requester this node cannot compose is told as a node
// that could not tell, and never read as a turn nobody woke. The operator
// surface has no turn: a person's assistant reads as its caller, for nobody
// else.
//
// Mutations: CodeDeps.caller skipping the ceiling → the stranger's and the
// departed person's turns read; the colleague left off any one kind of
// question (CodeDeps.read, resolveRef, gitRead) → red; a person's seat sent
// as the ceiling → red; a requester this node cannot compose read as nobody →
// red; the turn's senders read before its requester → the person's and the
// colleague's turns refused as a stranger's; the seat's own voice counted as
// a stranger's → the outcome of its own act refused; every sender beside the
// seat's own ignored → the stranger in its thread reads.
func TestACodeReadIsDecidedAgainForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	// A turn's senders: somebody no seat or person maps to, the seat itself —
	// the sender of the outcome of its own act — and a known requester.
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
			host := newCodeHost()
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
		// A STRANGER WHO SPOKE IN THE SEAT'S OWN THREAD still bounds it:
		// only the seat's own voice is no stranger's.
		{"a turn a stranger spoke in beside the seat itself", "agent-cto", append(itself, unknown...),
			tools.RefusalForbidden, "woken by somebody the company does not know", true},
		{"a turn a departed person woke", "departed", speaker("departed"), tools.RefusalForbidden,
			"may not act in the company now", true},
		{"a turn whose requester this node cannot compose", "agent-gone", nil, tools.RefusalUnavailable,
			"could not establish who this turn's work is for", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newCodeHost()
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
				t.Errorf("the refused calls asked the code host %d reads, %d resolves and %d git reads, want "+
					"nothing", len(reads), len(resolves), len(gits))
			}
		})
	}

	t.Run("the operator surface", func(t *testing.T) {
		t.Parallel()
		host := newCodeHost()
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

// A READ REFUSED FOR WHOM THE TURN READS IS THE CEILING'S REFUSAL: a node that
// answers a read made for a colleague ErrAboveCeiling — the colleague's own
// grant reads nothing of the repository — is told as the requester ceiling
// refuses, forbidden, carrying the authority table's ceiling reason over the
// code host's own sentence, which names the colleague and the visibility that
// shuts it out; whether the read was of the repository's rows or one by object
// id. Never as the seat's own not_permitted: the same seat reads the
// repository in a turn nobody woke, and asking for more access would change
// nothing.
//
// Mutation: the ErrAboveCeiling arm dropped from codeFailure → the refusal is
// a fault of this node, red; answered as ErrNotPermitted → it carries no
// ceiling, red.
func TestAReadRefusedForWhomTheTurnReadsIsTheCeilings(t *testing.T) {
	t.Parallel()
	above := fmt.Errorf("estate: data-a answered: %w: seat \"agent-ceo\" holds no access to repository "+
		"\"site\": it is visibility: unit, read by the seats of unit \"eng\" and the leads above it and by "+
		"persons", code.ErrAboveCeiling)
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
			host := newCodeHost()
			host.readErr, host.gitErr = c.readErr, c.gitErr
			turn := codeTurn(t)
			turn.Requester = "agent-ceo"
			got := refusedCode(t, codeRegistry(t, codeDeps(host)), turn, c.tool, c.args)
			if tools.RefusalOf(got) != tools.RefusalForbidden {
				t.Fatalf("answered %s %q, want refused forbidden", tools.RefusalOf(got), got.Output)
			}
			var refused *builtin.DecisionRefused
			if !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling ||
				!errors.Is(got.Cause, code.ErrAboveCeiling) || errors.Is(got.Cause, code.ErrNotPermitted) {
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

// A TURN PAYS ONE BARRIER PER REPOSITORY, AND ONE PER CALL THAT NAMES REFS.
// Every row read is at the seat's level (linearizable); the repository is read
// once a turn and never again in it; the refs a call names are resolved in ONE
// read — a diff of two branches pays one barrier, not two — and an id never
// is; and every read by object id carries the furthest of the node's own
// floor and the positions its refs LAST MOVED at, so the copy that answers
// holds both — never the checkpoint a resolve read its ref at, which its own
// barrier takes past every push, so a copy current to them all would sync for
// nothing to answer it. A landed-in read is judged up to its tip's own move.
// The operator surface has no turn, and reads the repository once a call.
//
// Mutation: resolveRevs resolving an oid as a ref or a call's refs one read
// each, repo() reading past the turn's lookups, or a read floored at where its
// ref was read (the resolve's checkpoint) → red.
func TestATurnPaysOneBarrierPerRefItNames(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	reg := codeRegistry(t, codeDeps(host))
	turn := codeTurn(t)
	floorOf := func(q code.GitQuery) []statelog.Position { return q.Fresh.Floors }
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
	// THE TURN'S ONE ROW READ OF THE REPOSITORY is code_get_repo's page of
	// branches, whose detail resolved it: no read of kind repo, ever.
	if len(reads) != 1 || reads[0].Kind != code.ReadRefs || readsOf(reads, code.ReadRepo) != 0 {
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

	// A WORKER IS ITS PARENT'S TURN: what the executor resolved, a worker it
	// delegated to reads with no barrier of its own.
	worker := turn.WithCalls(turnctx.NewCallLog())
	callCode(t, reg, worker, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID})
	if reads, _, _ := host.asked(); len(reads) != 1 {
		t.Errorf("a worker read the repository its parent resolved again: %+v", reads)
	}

	// THE OPERATOR SURFACE: no turn, so once a call.
	opHost := newCodeHost()
	tools := operatorCode(t, opHost, nil)
	for range 2 {
		if got, err := tools[builtin.CodeReadTool].Call(asBoundPerson(), map[string]any{"repo": "site",
			"rev": mainOID}); err != nil || got.Failed {
			t.Fatalf("an operator read: %v %s", err, got.Output)
		}
	}
	if reads, _, _ := opHost.asked(); readsOf(reads, code.ReadRepo) != 2 {
		t.Errorf("two operator calls read the repository %d times, want once a call", readsOf(reads, code.ReadRepo))
	}
}

// CODE_GET_REPO'S FIRST CALL IN A TURN IS ONE READ: its first page of branches
// carries the repository in detail, so that page resolves the repository too —
// one linearizable read and one barrier, not a read of kind repo and then the
// page — and every later call in the turn reuses the resolution. A call asking
// a LATER page first carries no detail, so it resolves the repository by a
// read of its own before the page. The operator surface, which has no turn,
// pays the one read each call.
//
// Mutation: repoRead ignoring the call's first read (reading kind repo, then
// the page) → red.
func TestCodeGetReposFirstCallIsOneRead(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	reg := codeRegistry(t, codeDeps(host))
	turn := codeTurn(t)
	callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	if reads, _, _ := host.asked(); len(reads) != 1 || reads[0].Kind != code.ReadRefs || reads[0].Cursor != "" {
		t.Fatalf("code_get_repo's first call read %+v, want its one page of branches", reads)
	}
	callCode(t, reg, turn, builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	callCode(t, reg, turn, builtin.CodeReadTool, map[string]any{"repo": "site", "rev": mainOID})
	if reads, _, gits := host.asked(); len(reads) != 2 || readsOf(reads, code.ReadRepo) != 0 ||
		len(gits) != 1 || gits[0].RepoID != siteID {
		t.Errorf("the turn's later calls read %+v and %+v, want the second call's page alone and a read of "+
			"%s by the id the first resolved", reads, gits, siteID)
	}

	// A LATER PAGE FIRST: the repository is resolved by a read of its own.
	later := newCodeHost()
	callCode(t, codeRegistry(t, codeDeps(later)), codeTurn(t), builtin.CodeGetRepoTool,
		map[string]any{"repo": "site", "branches_cursor": "refs/heads/feature"})
	if reads, _, _ := later.asked(); len(reads) != 2 || reads[0].Kind != code.ReadRepo ||
		reads[1].Kind != code.ReadRefs || reads[1].Cursor != "refs/heads/feature" {
		t.Errorf("a later page asked first read %+v, want the repository, then the page", reads)
	}

	// THE OPERATOR SURFACE: no turn, one read a call.
	opHost := newCodeHost()
	op := operatorCode(t, opHost, nil)
	for range 2 {
		if got, err := op[builtin.CodeGetRepoTool].Call(asBoundPerson(), map[string]any{"repo": "site"}); err != nil ||
			got.Failed {
			t.Fatalf("an operator's detail: %v %s", err, got.Output)
		}
	}
	if reads, _, _ := opHost.asked(); len(reads) != 2 || readsOf(reads, code.ReadRefs) != 2 {
		t.Errorf("two operator calls read %+v, want one page of branches each", reads)
	}
}

// A NAME THAT NAMES ANOTHER REPOSITORY MID-TURN IS RESOLVED AGAIN, whichever
// read finds it out — the name deleted and declared again since the turn
// resolved it. A ref read by NAME that answers another repository reads nothing
// and is refused stale; a read by COMMIT ID of the repository the turn resolved,
// which its git node answers gone, is refused not found. Either way the turn
// forgets its resolution, so the same call made again reads the repository the
// name names now, rather than asking again about an id nothing serves.
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
		// asked is how many git reads the refused call makes: none for a
		// ref that already names another repository, one of the old id for
		// a read by commit id.
		asked int
	}{
		{"a read by name", map[string]any{"repo": "site", "rev": "main"}, tools.RefusalStaleVersion, 0},
		{"a read by commit id", map[string]any{"repo": "site", "rev": mainOID}, tools.RefusalNotFound, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newCodeHost()
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
				t.Fatalf("the refused read asked the git nodes %+v, want %d reads of %s", gits, c.asked, siteID)
			}
			resolved := readsOf(reads, code.ReadRepo)
			callCode(t, reg, turn, builtin.CodeReadTool, c.args)
			reads, _, gits = host.asked()
			if readsOf(reads, code.ReadRepo) != resolved+1 || len(gits) != c.asked+1 ||
				gits[len(gits)-1].RepoID != reborn.ID {
				t.Errorf("the call made again read the repository %d more times and asked %+v, want it "+
					"resolved again and read by %s", readsOf(reads, code.ReadRepo)-resolved, gits, reborn.ID)
			}
		})
	}
}

// ---- refusals ------------------------------------------------------------ //

// A NODE THAT CANNOT JUDGE IS NEVER A REFUSAL OF THE CALLER (M3-O82): a data
// node with no chart answers a seat's read about itself, a git node unready,
// and a walk nobody served the fleet's condition — each told unavailable, which
// a retry clears, and never `not_permitted`, which only the grant answers.
//
// Mutation: ErrNotServing or ErrGitUnready classed forbidden → red.
func TestANodeThatCannotJudgeIsNeverARefusalOfTheCaller(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		readErr, gitErr error
		want            tools.Refusal
	}{
		"a reader with no chart": {readErr: fmt.Errorf("%w: this node holds no chart",
			code.ErrNotServing), want: tools.RefusalUnavailable},
		"a git node with no chart": {gitErr: &code.ErrGitUnready{Reason: code.UnreadyNoChart,
			Detail: "n1 holds no chart yet"}, want: tools.RefusalUnavailable},
		"no git node answering": {gitErr: &estate.ErrUnserved{Git: true, Detail: "n1 syncing"},
			want: tools.RefusalUnavailable},
		"no data node answering": {readErr: &estate.ErrUnserved{Detail: "n1 out of service"},
			want: tools.RefusalUnavailable},
		"the grant": {readErr: fmt.Errorf("%w: seat \"agent-cto\" holds no access to repository \"site\"",
			code.ErrNotPermitted), want: tools.RefusalForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := newCodeHost()
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

// EVERY REFUSAL IS THE DOMAIN'S, IN ITS CLASS: a repository nobody declared, a
// ref no branch or tag holds, an object the repository lacks and a read too
// large to answer whole are each told in the code host's own sentence — the
// narrowing a too-large read names included — classed as what the caller must
// change, and a call missing what it reads is refused before anything is
// asked.
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
			"rev": mainOID}, gitErr: fmt.Errorf("%w: no object %s", code.ErrObjectNotFound, mainOID),
			want: tools.RefusalNotFound, says: "no object " + mainOID},
		"a read too large to answer whole": {tool: builtin.CodeDiffTool, args: map[string]any{"repo": "site",
			"base": mainOID, "head": featureOID, "file": "a.txt"},
			gitErr: fmt.Errorf("%w: line 9 of the patch is past a page; read the file's lines instead",
				code.ErrSliceTooLarge), want: tools.RefusalInvalid, says: "read the file's lines instead"},
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
			host := newCodeHost()
			host.readErr, host.gitErr = c.readErr, c.gitErr
			got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), c.tool, c.args)
			if tools.RefusalOf(got) != c.want || !strings.Contains(got.Output, c.says) {
				t.Errorf("answered %s %q, want refused %s saying %q", tools.RefusalOf(got), got.Output,
					c.want, c.says)
			}
			reads, resolves, gits := host.asked()
			if asked := len(reads) + len(resolves) + len(gits); c.asksNothing && asked != 0 {
				t.Errorf("a call refused for its arguments asked the code host %d questions", asked)
			}
		})
	}
}

// A PERSON IS HANDED THE CLONE URL, AND A SEAT IS NOT: a seat reads through the
// tools and could not authenticate to the address, while a person's git — and
// a token's — clones from it.
func TestAPersonIsHandedTheCloneURLAndASeatIsNot(t *testing.T) {
	t.Parallel()
	clone := func(name string) string { return "https://crewlet.example.com/git/" + name + ".git" }
	deps := codeDeps(newCodeHost())
	deps.CloneURL = clone
	reg := codeRegistry(t, deps)
	_, seat := callCode(t, reg, codeTurn(t), builtin.CodeGetRepoTool, map[string]any{"repo": "site"})
	if _, ok := seat["clone_url"]; ok {
		t.Errorf("a seat was handed a clone URL: %v", seat["clone_url"])
	}
	for name, ctx := range map[string]context.Context{"a person": asBoundPerson(), "a token": asUnboundToken()} {
		got, err := operatorCode(t, newCodeHost(), clone)[builtin.CodeGetRepoTool].Call(ctx,
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

// longRef is the longest ref a push may create, every byte one JSON writes as
// six: what an answer naming a resolved ref weighs at its most.
var longRef = "refs/heads/" + strings.Repeat("<", code.MaxRefNameBytes-len("refs/heads/"))

// widest is a position at its widest encoding.
var widest = statelog.Position{Stream: topics.CodeLogStream, Generation: math.MaxUint32,
	Seq: math.MaxUint64}

// atTheBound is the fullest page fill makes — fill(a, n) the page holding n
// units of its filler, a byte of text or an entry of a list — whose JSON a git
// node still sends: at most code.MaxGitAnswerBytes, with one unit more past
// it, so the page is cut where the git node cuts one. Every other field is at
// its widest.
func atTheBound(t *testing.T, a code.GitAnswer, fill func(a code.GitAnswer, n int) code.GitAnswer) code.GitAnswer {
	t.Helper()
	a.Position, a.Next = widest, fmt.Sprintf("b:%d:%d", math.MaxInt, math.MaxInt)
	a.Coverage = &code.Coverage{From: widest, Through: widest, GapFrom: widest, GapThrough: widest}
	a.PushedBy = map[string]code.PushedBy{mainOID: {PushID: uuid.NewString(), Position: widest,
		Principal: code.Principal{Kind: code.PrincipalAgent, Seat: strings.Repeat("s", 40)}, Via: code.ViaGit}}
	size := func(n int) int {
		encoded, err := json.Marshal(fill(a, n))
		if err != nil {
			t.Fatal(err)
		}
		return len(encoded)
	}
	fits, over := 0, code.MaxGitAnswerBytes
	for over-fits > 1 {
		if mid := fits + (over-fits)/2; size(mid) <= code.MaxGitAnswerBytes {
			fits = mid
		} else {
			over = mid
		}
	}
	if size(fits) > code.MaxGitAnswerBytes || size(fits+1) <= code.MaxGitAnswerBytes {
		t.Fatalf("the fixture's fullest page is %d bytes and one unit more %d, which is not a page cut "+
			"at %d", size(fits), size(fits+1), code.MaxGitAnswerBytes)
	}
	return fill(a, fits)
}

// A GIT ANSWER FITS ONE TOOL ANSWER: a page the git node cut at its bound,
// wrapped as each code tool wraps it — the longest repository name, every ref
// its call names at the longest a ref may be and spelled in the characters JSON
// writes six bytes for, the fence's label and closer and the answer's
// provenance envelope where its text is fenced — is answered whole, and within
// builtin.ToolAnswerBytes. What code.MaxGitAnswerBytes leaves for the envelope
// is what this measures.
//
// Mutation: rendering the page indented (jsonAnswer) → the tree and stat pages
// refused for their structure → red.
func TestAGitAnswerFitsOneToolAnswer(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("r", gitref.MaxRepoNameBytes)
	repoWith := func() *codeHost {
		h := newCodeHost()
		h.detail.Name = name
		h.refs = map[string]string{longRef: mainOID}
		return h
	}
	files := func(n int) []code.DiffFile {
		out := make([]code.DiffFile, n)
		for i := range out {
			out[i] = code.DiffFile{Path: fmt.Sprintf("f%04d", i), Status: "M", Added: 1, Deleted: 1,
				OldOID: blobOID, NewOID: mainOID, OldMode: "100644", NewMode: "100644"}
		}
		return out
	}
	for _, c := range []struct {
		name string
		tool string
		args map[string]any
		page func(t *testing.T, q code.GitQuery) code.GitAnswer
	}{
		{"a file's lines", builtin.CodeReadTool, map[string]any{"rev": longRef, "path": "README.md"},
			func(t *testing.T, q code.GitQuery) code.GitAnswer {
				return atTheBound(t, code.GitAnswer{Kind: code.GitBlob, RepoID: q.RepoID, Rev: blobOID},
					func(a code.GitAnswer, n int) code.GitAnswer {
						a.Blob = &code.BlobAnswer{Path: "README.md", OID: blobOID, Size: math.MaxInt64,
							Lines: math.MaxInt, StartLine: math.MaxInt - 1, EndLine: math.MaxInt,
							StartByte: math.MaxInt, Text: strings.Repeat("x", n), Continues: true}
						return a
					})
			}},
		{"a tree's entries", builtin.CodeReadTool, map[string]any{"rev": longRef, "recursive": true},
			func(t *testing.T, q code.GitQuery) code.GitAnswer {
				return atTheBound(t, code.GitAnswer{Kind: code.GitTree, RepoID: q.RepoID, Rev: mainOID},
					func(a code.GitAnswer, n int) code.GitAnswer {
						// SHORT ENTRIES, the most structure a page can carry:
						// what indenting would grow by.
						entries := make([]code.TreeEntry, n)
						for i := range entries {
							entries[i] = code.TreeEntry{Mode: "100644", Type: "blob", OID: blobOID,
								Path: fmt.Sprintf("%06d", i), Size: 1}
						}
						a.Tree = &code.TreeAnswer{Tree: blobOID, Entries: entries, Total: math.MaxInt}
						return a
					})
			}},
		{"a diff's patch", builtin.CodeDiffTool, map[string]any{"base": longRef, "head": longRef, "file": "f"},
			func(t *testing.T, q code.GitQuery) code.GitAnswer {
				return atTheBound(t, code.GitAnswer{Kind: code.GitDiff, RepoID: q.RepoID},
					func(a code.GitAnswer, n int) code.GitAnswer {
						a.Diff = &code.DiffAnswer{Base: mainOID, Head: mainOID, Files: files(1),
							Total: math.MaxInt, Added: math.MaxInt, Deleted: math.MaxInt,
							Patch: strings.Repeat("x", n)}
						return a
					})
			}},
		{"a diff's stat", builtin.CodeDiffTool, map[string]any{"base": longRef, "head": longRef},
			func(t *testing.T, q code.GitQuery) code.GitAnswer {
				return atTheBound(t, code.GitAnswer{Kind: code.GitDiff, RepoID: q.RepoID},
					func(a code.GitAnswer, n int) code.GitAnswer {
						a.Diff = &code.DiffAnswer{Base: mainOID, Head: mainOID, Files: files(n),
							Total: math.MaxInt, Added: math.MaxInt, Deleted: math.MaxInt}
						return a
					})
			}},
		{"a comparison", builtin.CodeCompareTool, map[string]any{"base": longRef, "head": longRef},
			func(t *testing.T, q code.GitQuery) code.GitAnswer {
				return atTheBound(t, code.GitAnswer{Kind: code.GitCompare, RepoID: q.RepoID},
					func(a code.GitAnswer, n int) code.GitAnswer {
						a.Compare = &code.CompareAnswer{Base: mainOID, Head: mainOID, MergeBase: mainOID,
							BaseIsAncestor: true, HeadIsAncestor: true, Ahead: math.MaxInt, Behind: math.MaxInt,
							Files: files(n), Total: math.MaxInt, Added: math.MaxInt, Deleted: math.MaxInt}
						return a
					})
			}},
		{"a landing", builtin.CodeLandedInTool, map[string]any{"commit": longRef, "target": longRef},
			func(t *testing.T, q code.GitQuery) code.GitAnswer {
				return atTheBound(t, code.GitAnswer{Kind: code.GitLandedIn, RepoID: q.RepoID},
					func(a code.GitAnswer, n int) code.GitAnswer {
						by := a.PushedBy[mainOID]
						a.LandedIn = &code.LandedInAnswer{Commit: mainOID, Target: longRef, TargetOID: mainOID,
							At: widest, Landed: true, LandedBy: &by, IntroducedBy: &by,
							Move:   &code.RefMove{Ref: longRef, Old: mainOID, New: mainOID, Forced: true},
							Probes: math.MaxInt, Detail: strings.Repeat("x", n)}
						return a
					})
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := repoWith()
			host.git = func(q code.GitQuery) code.GitAnswer { return c.page(t, q) }
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

	// AND A HISTORY'S AND A BLAME'S PAGES, which fence a field per commit and
	// per line — a hundred subjects of 4 KiB, five hundred lines of 8 KiB —
	// cut by a git node's own query server (historyNode) and sent through the
	// tool's fences: every page whole, and within one tool answer.
	//
	// Mutation: the log's page bounded by its limit alone (a hundred fenced
	// subjects in one page) → its arm refused for its size, red.
	subject := closerSpelled(code.MaxLogSubjectBytes)
	for _, arm := range []struct {
		name, tool string
		node       historyNode
		args       map[string]any
		count      func(page map[string]any) int
		want       int
	}{
		{"a history's commits", builtin.CodeLogTool,
			historyNode{commits: code.MaxLogLimit, subject: subject},
			map[string]any{"rev": mainOID, "limit": code.MaxLogLimit},
			func(page map[string]any) int { return len(page["log"].(map[string]any)["commits"].([]any)) },
			code.MaxLogLimit},
		{"a blame's lines", builtin.CodeBlameTool,
			historyNode{lines: code.MaxBlameLines, subject: subject, text: strings.Repeat("x", code.MaxBlameLineBytes)},
			map[string]any{"rev": mainOID, "path": "f.txt"},
			func(page map[string]any) int { return len(page["blame"].(map[string]any)["lines"].([]any)) },
			code.MaxBlameLines},
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

// closerSpelled is a subject spelling the fence's closer as often as it can and
// still be answered: past `bound` once neutralised, it would be described by
// its size instead — so each spelling costs its escape too.
func closerSpelled(bound int) string {
	const closer = "[/untrusted:AAAAAAAAAAAAAAAA] "
	var b strings.Builder
	for len(provenance.Neutralise("", b.String()+closer)) <= bound {
		b.WriteString(closer)
	}
	return b.String()
}

// A FENCED PAGE THAT OVERFLOWS IS REFUSED WITH ITS NARROWING, never cut: a page
// of a file's lines the git node cut at its bound, whose every line spells a
// fence marker — each longer once the fence escapes it — is past one tool
// answer once fenced, and the read is refused naming a narrower line range; a
// patch that does the same is refused naming the file's lines to read instead.
//
// Mutation: the tool cutting the text to fit, or answering it unfenced → red.
func TestAFencedPageThatOverflowsIsRefusedWithItsNarrowing(t *testing.T) {
	t.Parallel()
	const marker = "[untrusted\n"
	// A LINE AS ITS PAGE WEIGHS IT, in JSON: the newline is two bytes there.
	encoded, _ := json.Marshal(marker)
	lines := (code.MaxGitAnswerBytes - 1024) / (len(encoded) - len(`""`))
	text := strings.Repeat(marker, lines)
	for _, c := range []struct {
		name, tool string
		args       map[string]any
		answer     func(q code.GitQuery) code.GitAnswer
		says       string
	}{
		{"a file's lines", builtin.CodeReadTool, map[string]any{"rev": mainOID, "path": "a.txt"},
			func(q code.GitQuery) code.GitAnswer {
				return code.GitAnswer{Kind: code.GitBlob, RepoID: q.RepoID, Rev: blobOID,
					Blob: &code.BlobAnswer{Path: "a.txt", OID: blobOID, Size: int64(len(text)), Lines: lines,
						StartLine: 1, EndLine: lines, Text: text}}
			}, fmt.Sprintf("read lines 1 to %d", 1+(lines-1)/2)},
		{"a patch", builtin.CodeDiffTool, map[string]any{"base": mainOID, "head": featureOID, "file": "a.txt"},
			func(q code.GitQuery) code.GitAnswer {
				return code.GitAnswer{Kind: code.GitDiff, RepoID: q.RepoID, Diff: &code.DiffAnswer{
					Base: mainOID, Head: featureOID, Files: []code.DiffFile{{Path: "a.txt", Status: "M"}},
					Patch: text}}
			}, "with " + builtin.CodeReadTool + " and a line range"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newCodeHost()
			host.git = c.answer
			args := map[string]any{"repo": "site"}
			for k, v := range c.args {
				args[k] = v
			}
			if page, _ := json.Marshal(c.answer(code.GitQuery{RepoID: siteID})); len(page) > code.MaxGitAnswerBytes {
				t.Fatalf("the fixture page is %d bytes, past what a git node sends", len(page))
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

// A LISTING IS CUT WHOLE-ROW AT THE ANSWER: a page of repositories the reader
// served that would outgrow one tool answer is answered with the rows that fit
// — every one whole — and `next` naming the last kept, where the reader's own
// keyset continues; so are a repository's branches.
//
// Mutation: answering the reader's page whole (refused for its size), or next
// left as the reader's → red.
func TestAListingIsCutWholeRowAtTheAnswer(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	for i := range 100 {
		s := siteDetail().RepoSummary
		s.Name, s.Description = fmt.Sprintf("repo-%03d", i), strings.Repeat("<", code.MaxDescriptionBytes)
		host.listing = append(host.listing, s)
	}
	for i := range 100 {
		host.refs[fmt.Sprintf("refs/heads/%s-%03d", strings.Repeat("<", 200), i)] = mainOID
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
	if len(branches) == 0 || len(branches) >= 100 || len(got.Output) > builtin.ToolAnswerBytes {
		t.Fatalf("the detail kept %d branches in %d bytes", len(branches), len(got.Output))
	}
	lastBranch, _ := branches[len(branches)-1].(map[string]any)
	if detail["branches_next"] != "refs/heads/"+lastBranch["name"].(string) {
		t.Errorf("the cut branches' next is %v, want the last kept branch %v", detail["branches_next"],
			lastBranch["name"])
	}
}

// A REPOSITORY WHOSE RULES ARE PAST ONE ANSWER NAMES WHERE THEY ARE READ:
// sixty-four rules whose patterns are at gitref's longest, each byte one JSON
// writes as six, outweigh one tool answer as a push is held to them — with no
// merge gate, which no listing row carries — so such a repository is listed
// whole on no page and its detail leaves no room for a branch; and the
// refusal says where its rules are read instead: a declared repository's in
// the company's configuration, and those of one a tool made, which no
// configuration holds, in its detail over the API. Its detail alone past one
// answer is refused so whether it has branches or none, since no branch
// prefix narrows what its rules weigh.
//
// Mutation: name the company's configuration whatever made the repository —
// the tool-made repository's refusals name code.repos, red.
func TestARepositoryWhoseRulesArePastOneAnswerNamesWhereTheyAreRead(t *testing.T) {
	t.Parallel()
	rules := make([]code.PushRule, code.MaxProtectionsPerRepo)
	for i := range rules {
		head := fmt.Sprintf("refs/heads/%02d", i)
		rules[i] = code.PushRule{Pattern: head + strings.Repeat("&", gitref.MaxRefNameBytes-len(head)),
			DirectPush: code.DirectPersons}
	}
	for _, tc := range []struct {
		managed     code.ManagedBy
		says, never string
	}{
		{code.ManagedConfig, "company's configuration (code.repos)", "GET /code/repos/{name}"},
		{code.ManagedTools, "GET /code/repos/{name}", "code.repos"},
	} {
		for _, branches := range []bool{true, false} {
			host := newCodeHost()
			host.detail.ManagedBy, host.detail.Protections = tc.managed, rules
			if !branches {
				host.refs = map[string]string{}
			}
			reg := codeRegistry(t, codeDeps(host))
			asked := map[string]map[string]any{builtin.CodeGetRepoTool: {"repo": "site"}}
			if branches {
				asked[builtin.CodeListReposTool] = map[string]any{}
			}
			for tool, args := range asked {
				got := refusedCode(t, reg, codeTurn(t), tool, args)
				if !strings.Contains(got.Output, tc.says) || strings.Contains(got.Output, tc.never) {
					t.Errorf("%s of a %s repository, branches %v, refused %q, want it to say %q and never %q",
						tool, tc.managed, branches, got.Output, tc.says, tc.never)
				}
			}
		}
	}
}

// ---- the fences ---------------------------------------------------------- //

// codeFenced runs one code read whose page answer builds — handed the turn's
// boundary, so its text can try to break out of the fence — and holds the
// answer to the fence's property (TestEveryUntrustedReadIsFenced's code rows).
func codeFenced(t *testing.T, tool string, args map[string]any, answer func(b provenance.Boundary,
	q code.GitQuery) code.GitAnswer) ([]fenceSeen, map[string]any) {

	t.Helper()
	turn, b := readerTurn(t)
	turn.Lookups = turnctx.NewLookups()
	host := newCodeHost()
	host.git = func(q code.GitQuery) code.GitAnswer { return answer(b, q) }
	args["repo"] = "site"
	got := callIn(t, codeRegistry(t, codeDeps(host)), turn, tool, args)
	return fencesIn(t, b, got.Output)
}

// ---- the catalogue, the guidance and the docs --------------------------- //

// codeCatalogueBudget is the most bytes the nineteen code tools' definitions
// may add to a tool catalogue as a CLI's prompt renders it.
//
// 26 800: measured at 24 990 (~6 248 tokens), so ~7% headroom — a sentence can
// be added and a tool cannot, the executor prompt's rule (a code tool a later
// part adds raises this deliberately, with its measured cost). 10 654 → 9 570
// when the revision grammar, written into each of the six revision arguments,
// was stated once in code_resolve's description; 9 570 → 9 627 when
// code_list_repos said that a turn a colleague woke lists only what the
// colleague may read too, so a seat reading a short page for them knows why.
// The budget 10 300 → 14 000 when code_log and code_blame joined, 9 627 →
// 13 070 measured: 3 443 bytes (~861 tokens) for the two, each a history's or
// a blame's question answered on the server in place of a clone, with their
// descriptions cut to what a model needs (how a page goes on is `cursor`'s to
// say, and only it says it). And 14 000 → 24 400 when the ten code writes
// joined the list, deliberately, with their measured cost: 13 070 → 22 802,
// the writes' 9 732 bytes after each argument was cut to what its name and
// type do not say (15 120 as first written: an owner's and a path owner's
// nested schemas spelled out three times over); 22 802 → 22 787 when
// code_protect_branch's direct_push said what leaving it out sends. And
// 24 400 → 26 800 when code_protect_branch took a branch's merge gate — its
// ten fields and remove_gate — deliberately, with their measured cost: 22 787
// → 24 990, 2 203 bytes (~551 tokens), each field its type and its values
// alone (what a field does is the code host's documentation), the call's
// field-at-a-time rule said once in the tool's description.
const codeCatalogueBudget = 26_800

// THE CODE TOOLS' CATALOGUE STAYS UNDER ITS BUDGET. A seat of a company hosting
// code carries the nineteen definitions on every round of every turn: a CLI's
// catalogue rides in the user text (cliagent.RenderPrompt), so nothing caches
// them, and each byte is paid again every round. Measured as that catalogue
// renders them — indented, HTML-escaped, the nineteen added to a catalogue
// already holding a tool, so each one's separator and indentation count — and
// held to a budget, so a later description edit is a decision rather than
// drift, as the executor prompt's budgets make a sentence one
// (internal/agent/prompts budget_test.go).
func TestTheCodeToolsCatalogueStaysUnderBudget(t *testing.T) {
	t.Parallel()
	rendered := func(defs []llm.ToolDef) int {
		t.Helper()
		text, err := cliagent.RenderPrompt(llm.Request{Tools: defs})
		if err != nil {
			t.Fatalf("render the catalogue: %v", err)
		}
		return len(text)
	}
	other := llm.ToolDef{Name: "lookup_colleague", Description: "Who to ask.",
		Parameters: map[string]any{"type": "object"}}
	with := []llm.ToolDef{other}
	for _, tool := range builtin.CodeTools(writeDeps(newCodeHost(), &codeWrites{})) {
		with = append(with, llm.ToolDef{Name: tool.Name(), Description: tool.Description(),
			Parameters: tool.Parameters()})
	}
	got := rendered(with) - rendered([]llm.ToolDef{other})
	if got > codeCatalogueBudget {
		t.Errorf("the code tools add %d bytes (~%d tokens) to every round's catalogue, past their budget of %d",
			got, cliagent.EstimateTokens(strings.Repeat(" ", got)), codeCatalogueBudget)
		return
	}
	t.Logf("the code tools add %d bytes (~%d tokens) to every round's catalogue (budget %d, headroom %d)", got,
		cliagent.EstimateTokens(strings.Repeat(" ", got)), codeCatalogueBudget, codeCatalogueBudget-got)
}

// ---- the guidance and the docs ------------------------------------------ //

// THE CODE GUIDANCE NAMES THE CODE TOOLS: every tool the executor's guidance
// is gated on is one this package registers as a code tool, so the section
// never sends a seat to a tool that does not exist — prompts cannot import this
// package to read the list itself.
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

// THE PAGES NAME EVERY CODE TOOL THE REGISTRY HOLDS: the tools guide's table of
// them lists exactly the nine, and the operator reference's account of what
// `/operator/mcp` serves names each — two hand-kept copies of one list, which is
// how a page comes to describe a catalogue the engine no longer serves.
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
	every := slices.Concat(builtin.CodeToolNames(), builtin.CodeWrites())
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
