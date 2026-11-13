package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// atlasLedger is the atlas's log as the write tools reach it, in memory: every
// claim recorded in order, each replacing its voice's older claim on its slot
// as the domain's applier does, an entry's version the position of its newest
// claim, and every operation answered once.
type atlasLedger struct {
	mu       sync.Mutex
	claims   []atlas.Claim
	versions map[string]uint64
	ops      map[string]statelog.Result
	purges   []string
	calls    []atlasdomain.Call
	// failAt refuses the record that would be the log's failAt-th claim, once,
	// as a node that lost its broker part of the way through a write does.
	failAt int
}

func newAtlasLedger() *atlasLedger {
	return &atlasLedger{versions: map[string]uint64{}, ops: map[string]statelog.Result{}}
}

var atlasStream = atlasdomain.Domain{}.Stream().Name

func (l *atlasLedger) Live(context.Context, statelog.Freshness) (atlasdomain.Live, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	type voice struct{ subject, slot, kind, name string }
	newest := map[voice]int{}
	for i, c := range l.claims {
		newest[voice{c.Subject.String(), c.Slot(), string(c.Source.Kind), c.Source.Name}] = i
	}
	var out []atlas.Claim
	for i, c := range l.claims {
		if newest[voice{c.Subject.String(), c.Slot(), string(c.Source.Kind), c.Source.Name}] == i {
			out = append(out, c)
		}
	}
	return atlasdomain.Live{Claims: out, Complete: true,
		Position: statelog.Unpack(atlasStream, int64(len(l.claims)))}, nil
}

func (l *atlasLedger) Entry(_ context.Context, id string, _ statelog.Freshness) (atlasdomain.EntryRow, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.versions[id]
	return atlasdomain.EntryRow{ID: id, Version: v}, ok, nil
}

func (l *atlasLedger) Record(_ context.Context, c atlasdomain.Call, claim atlas.Claim,
	ifMatch uint64) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// THE OPERATION IS HELD TO THE RULE THE LOG'S WRITER HOLDS IT TO, so a
	// step whose id outgrows it is refused here as it is there.
	if err := statelog.CheckCallerOpID(c.OpID); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the operation id %w", atlasdomain.ErrInvalid, err)
	}
	if res, ok := l.ops[c.OpID]; ok {
		return res, nil
	}
	if l.failAt == len(l.claims)+1 {
		l.failAt = 0
		return statelog.Result{}, errors.New("the broker went away")
	}
	id := claim.Subject.ID().String()
	switch {
	case ifMatch == atlasdomain.IfNoRecord && l.versions[id] != 0,
		ifMatch != atlasdomain.NoIfMatch && ifMatch != atlasdomain.IfNoRecord && l.versions[id] != ifMatch:
		return statelog.Result{}, fmt.Errorf("%w: at %d, decided from %d", atlasdomain.ErrStaleVersion,
			l.versions[id], ifMatch)
	}
	claim.Source = atlas.PersonSource(c.By.Name)
	claim.Position = uint64(len(l.claims) + 1)
	if err := claim.Validate(); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: %w", atlasdomain.ErrInvalid, err)
	}
	l.claims = append(l.claims, claim)
	l.calls = append(l.calls, c)
	l.versions[id] = claim.Position
	res := statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpID,
		Position: statelog.Unpack(atlasStream, int64(claim.Position)), Version: int64(claim.Position)}
	l.ops[c.OpID] = res
	return res, nil
}

func (l *atlasLedger) PurgeEntry(_ context.Context, c atlasdomain.Call, ref atlas.Ref) (statelog.Result, error) {
	return l.purge(c, "entry:"+ref.ID().String())
}

func (l *atlasLedger) PurgeDigest(_ context.Context, c atlasdomain.Call, digest string) (statelog.Result, error) {
	return l.purge(c, "value:"+digest)
}

func (l *atlasLedger) purge(c atlasdomain.Call, what string) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := statelog.CheckCallerOpID(c.OpID); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the operation id %w", atlasdomain.ErrInvalid, err)
	}
	l.purges = append(l.purges, what)
	l.calls = append(l.calls, c)
	return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpID,
		Position: statelog.Unpack(atlasStream, int64(len(l.claims)+len(l.purges)))}, nil
}

// atlasLogOnly is a node's record sources with the atlas's log alone among
// them, as the atlas tools read the atlas.
func atlasLogOnly(log gather.Log) func(context.Context) gather.Sources {
	return func(context.Context) gather.Sources { return gather.Sources{Log: log} }
}

// seededAtlasLedger is a log holding what the walks over every write change:
// two services and a server a person added.
func seededAtlasLedger() *atlasLedger {
	l := newAtlasLedger()
	for _, ref := range []atlas.Ref{atlas.MustRef(atlas.KindService, "billing"),
		atlas.MustRef(atlas.KindService, "billing-api"), atlas.MustRef(atlas.KindHost, "addr/web-2.acme.example")} {
		c, err := atlas.Exists(ref, atlas.PersonSource("founder"), atlasWriteAt)
		if err != nil {
			panic(err)
		}
		c.Position = uint64(len(l.claims) + 1)
		l.claims = append(l.claims, c)
		l.versions[ref.ID().String()] = c.Position
	}
	return l
}

// atlasSweepCalls is a call of every atlas write that is answered, not
// refused, over [seededAtlasLedger] in any order, by a person who holds atlas
// and is an administrator with a fresh proof.
func atlasSweepCalls() map[string]map[string]any {
	pair := map[string]any{"atlas_entry": "service:billing", "other": "service:billing-api"}
	relation := map[string]any{"atlas_entry": "service:billing", "relation": "runs_on",
		"to": "host:addr/web-2.acme.example"}
	return map[string]map[string]any{
		builtin.AtlasAddEntryTool: {"kind": "service", "key": "ledger", "name": "Ledger"},
		builtin.AtlasCorrectTool: {"atlas_entry": "service:billing", "fact": "environment",
			"value": "production"},
		builtin.AtlasConfirmTool:  {"atlas_entry": "service:billing"},
		builtin.AtlasRelateTool:   relation,
		builtin.AtlasUnrelateTool: relation,
		builtin.AtlasSetOwnerTool: {"atlas_entry": "service:billing", "clear": true},
		builtin.AtlasRetireTool:   {"atlas_entry": "service:billing-api"},
		builtin.AtlasRestoreTool:  {"atlas_entry": "service:billing-api"},
		builtin.AtlasMergeTool:    pair,
		builtin.AtlasSplitTool:    pair,
		builtin.AtlasPurgeTool:    {"value": "web-9.internal.acme.example"},
	}
}

// atlasLandedAnswers is every atlas write's answer, each landed or answered as
// changing nothing, over one log.
func atlasLandedAnswers(t *testing.T) map[string]tools.Result {
	t.Helper()
	w := atlasWrites(t, seededAtlasLedger())
	out := map[string]tools.Result{}
	for _, name := range builtin.AtlasWriteTools {
		got, _ := atlasCall(t, w[name], atlasSweepCalls()[name], iam.GrantAtlas, iam.GrantAdmin)
		if got.Failed {
			t.Fatalf("%s was refused: %s", name, got.Output)
		}
		out[name] = got
	}
	return out
}

// recorded is every claim the ledger holds, in order.
func (l *atlasLedger) recorded() []atlas.Claim {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.claims)
}

// atlasSettingsWithServer is acme's settings declaring one server.
func atlasSettingsWithServer(context.Context) (builtin.AtlasSettings, bool) {
	s := atlasCompany()
	s.Company.Integrations.SSH = &config.SSH{Hosts: []config.SSHHost{{ID: "web-1", Address: "10.0.0.5"}}}
	return s, true
}

// atlasToken is a credential of a common shape, assembled at run time so the
// source holds no string a secret scanner reads as a live one.
func atlasToken() string { return "gh" + "p_" + strings.Repeat("Ab3dE6gH9j", 4)[:36] }

// atlasWriteAt is the instant every case's claims are made at.
var atlasWriteAt = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// atlasWrites is the operator catalogue's atlas tools over ledger, deciding as
// every surface decides, keyed by name.
func atlasWrites(t *testing.T, ledger *atlasLedger) map[string]tools.SeatCallable {
	t.Helper()
	return atlasWritesAt(t, ledger, func() time.Time { return atlasWriteAt })
}

// atlasWritesAt is [atlasWrites] reading its claims' instant off now.
func atlasWritesAt(t *testing.T, ledger *atlasLedger, now func() time.Time) map[string]tools.SeatCallable {
	t.Helper()
	chart := organization(t)
	deps := codeDeps(newGitletHost())
	deps.Actor = builtin.PrincipalActor
	out := map[string]tools.SeatCallable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Code: deps, Authorize: builtin.Decide(chartLeads),
		Atlas: builtin.AtlasDeps{Settings: atlasSettingsWithServer, Records: atlasLogOnly(ledger)},
		AtlasWrite: builtin.AtlasWriteDeps{Log: ledger, Actor: builtin.PrincipalActor,
			Org: func() *org.Organization { return chart }, Now: now},
	}) {
		out[tool.Name()] = tool.(tools.SeatCallable)
	}
	for _, name := range builtin.AtlasWriteTools {
		if out[name] == nil {
			t.Fatalf("the operator catalogue holds no %s", name)
		}
	}
	return out
}

// atlasAdmin is an administrator who proved who they are a moment ago, as a
// purge asks.
func atlasAdmin() iam.Principal {
	p := founderPrincipal(iam.GrantAdmin)
	p.ReauthAt = time.Now().Add(time.Hour)
	return p
}

// atlasCall makes one call as a person holding atlas, and the answer it gave.
func atlasCall(t *testing.T, tool tools.SeatCallable, args map[string]any, grants ...iam.Grant) (
	tools.Result, map[string]any) {
	t.Helper()
	if len(grants) == 0 {
		grants = []iam.Grant{iam.GrantAtlas}
	}
	ctx := atlasAs(grants...)
	if slices.Contains(grants, iam.GrantAdmin) {
		ctx = iam.WithPrincipal(context.Background(), atlasAdmin())
	}
	got, err := tool.CallForTurn(ctx, nil, args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	var answer map[string]any
	if !got.Failed {
		if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
			t.Fatalf("%s answered no JSON object: %v\n%s", tool.Name(), err, got.Output)
		}
	}
	return got, answer
}

// mustWrite is a call that lands, and its answer.
func mustWrite(t *testing.T, tool tools.SeatCallable, args map[string]any, grants ...iam.Grant) map[string]any {
	t.Helper()
	got, answer := atlasCall(t, tool, args, grants...)
	if got.Failed {
		t.Fatalf("%s %v was refused: %s", tool.Name(), args, got.Output)
	}
	return answer
}

// viewOf is the atlas as atlas_search answers a holder.
func viewOf(t *testing.T, tools map[string]tools.SeatCallable) atlasAnswer {
	t.Helper()
	return searchAtlas(atlasAs(iam.GrantAtlas), t, tools[builtin.AtlasSearchTool], map[string]any{"limit": 500})
}

func entryNamed(t *testing.T, a atlasAnswer, ref atlas.Ref) atlas.Entry {
	t.Helper()
	for _, e := range a.Entries {
		if slices.Contains(e.Members, ref) {
			return e
		}
	}
	t.Fatalf("the atlas shows no %s among %d entries", ref, len(a.Entries))
	return atlas.Entry{}
}

// A PERSON'S WRITE IS A CLAIM IN THEIR OWN NAME THAT THE ATLAS THEN SHOWS: an
// entry added with its name, a value set that outranks nothing yet, a relation
// drawn, each read back through atlas_search's own read, and a value corrected
// by the person replacing their own earlier one.
//
// Mutation: fold no log into the atlas's read (gather's Log source) and the
// added entry is never shown.
func TestAPersonsAtlasWriteIsTheirClaimAndTheAtlasShowsIt(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	service := atlas.MustRef(atlas.KindService, "checkout-api")
	added := mustWrite(t, w[builtin.AtlasAddEntryTool], map[string]any{"kind": "service", "key": "checkout-api",
		"name": "Checkout API"})
	if added["entry"] != service.ID().String() || added["outcome"] != string(statelog.OutcomeApplied) {
		t.Fatalf("the add answered %v", added)
	}
	mustWrite(t, w[builtin.AtlasCorrectTool], map[string]any{"atlas_entry": "service:checkout-api",
		"fact": "environment", "value": "staging"})
	answer := mustWrite(t, w[builtin.AtlasCorrectTool], map[string]any{"atlas_entry": service.ID().String(),
		"fact": "environment", "value": "production"})
	if answer["before"] == nil || answer["value"] != "production" {
		t.Errorf("a correction answered %v, want what it replaced and the value", answer)
	}
	mustWrite(t, w[builtin.AtlasRelateTool], map[string]any{"atlas_entry": "service:checkout-api",
		"relation": "runs_on", "to": "host:ssh/web-1"})

	e := entryNamed(t, viewOf(t, w), service)
	if e.Name != "Checkout API" || e.State != atlas.StateConfirmed {
		t.Errorf("the entry reads %q, %s; want Checkout API, confirmed", e.Name, e.State)
	}
	i := slices.IndexFunc(e.Facts, func(f atlas.FactView) bool { return f.Fact == atlas.FactEnvironment })
	if i < 0 || e.Facts[i].Value.Text != "production" || e.Facts[i].Source.Kind != atlas.SourcePerson {
		t.Errorf("the environment reads %+v, want production from a person", e.Facts)
	}
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	if !slices.ContainsFunc(e.Relations, func(ed atlas.Edge) bool {
		return ed.Relation == atlas.RelationRunsOn && ed.To == web.ID()
	}) {
		t.Errorf("the relation to web-1 is not shown: %+v", e.Relations)
	}
	for _, c := range ledger.recorded() {
		if c.Source.Kind != atlas.SourcePerson || c.Source.Name == "" {
			t.Errorf("a claim was recorded from %+v, want the person", c.Source)
		}
	}
}

// A FACT THE SETTINGS DECLARE IS CHANGED THERE, NEVER IN THE ATLAS: the two share
// the top rank, so a claim beside a declaration would decide by whichever was
// newer. A server the settings declare is not retired, its address not called
// wrong, and an entry the engine names is not added by hand; what the settings
// leave unsaid is a person's to say.
//
// Mutation: drop the declared check from planCorrect and the address is
// called wrong.
func TestAFactTheSettingsDeclareIsChangedThereNeverInTheAtlas(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.AtlasCorrectTool, map[string]any{"atlas_entry": "host:ssh/web-1", "fact": "address", "value": "10.0.0.5",
			"wrong": true}},
		{builtin.AtlasCorrectTool, map[string]any{"atlas_entry": "host:ssh/web-1", "fact": "port", "value": "2222"}},
		{builtin.AtlasRetireTool, map[string]any{"atlas_entry": "host:ssh/web-1"}},
		{builtin.AtlasPurgeTool, map[string]any{"atlas_entry": "host:ssh/web-1"}},
		{builtin.AtlasAddEntryTool, map[string]any{"kind": "host", "key": "ssh/web-2"}},
		{builtin.AtlasAddEntryTool, map[string]any{"kind": "tool_server", "key": "mcp/tracker"}},
	} {
		grants := []iam.Grant{iam.GrantAtlas}
		if c.tool == builtin.AtlasPurgeTool {
			grants = []iam.Grant{iam.GrantAdmin}
		}
		got, _ := atlasCall(t, w[c.tool], c.args, grants...)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalConflict {
			t.Errorf("%s %v = %q (%v), want refused as declared elsewhere", c.tool, c.args, got.Output, got.Cause)
		}
	}
	if n := len(ledger.recorded()); n != 0 || len(ledger.purges) != 0 {
		t.Fatalf("a refused write recorded %d claims and %d purges", n, len(ledger.purges))
	}
	mustWrite(t, w[builtin.AtlasCorrectTool], map[string]any{"atlas_entry": "host:ssh/web-1", "fact": "importance",
		"value": "critical"})
	if n := len(ledger.recorded()); n != 1 {
		t.Errorf("a fact the settings leave unsaid recorded %d claims, want one", n)
	}
}

// A KEY SHAPED LIKE A CREDENTIAL IS REFUSED BEFORE ANYTHING IS WRITTEN, and the
// refusal never repeats it: values go through the vocabulary's typed
// constructors, keys included, and nothing is stored masked.
//
// Mutation: build the add's ref with atlas.NewRef and the host is recorded.
func TestAnAtlasWriteShapedLikeACredentialIsRefusedNeverMasked(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	secret := atlasToken()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.AtlasAddEntryTool, map[string]any{"kind": "host", "key": "addr/" + secret}},
		{builtin.AtlasAddEntryTool, map[string]any{"kind": "service", "key": "billing", "name": secret}},
		{builtin.AtlasCorrectTool, map[string]any{"atlas_entry": "host:ssh/web-1", "fact": "purpose", "value": secret}},
	} {
		got, _ := atlasCall(t, w[c.tool], c.args)
		if !got.Failed || !errors.Is(got.Cause, atlas.ErrCredential) {
			t.Errorf("%s holding a token = %q (%v), want refused as a credential", c.tool, got.Output, got.Cause)
		}
		if strings.Contains(got.Output, secret[:12]) {
			t.Errorf("%s's refusal repeats the token", c.tool)
		}
	}
	for _, c := range ledger.recorded() {
		if strings.Contains(c.Subject.Key+c.Value.Text, secret[:12]) {
			t.Errorf("a claim holding the token was recorded: %+v", c)
		}
	}
}

// AN OWNER IS A SEAT OR A UNIT OF THE CHART, NEVER FREE TEXT, and setting one
// replaces the owner a person named before: the old owner's claim is denied,
// the new one asserted, and the atlas shows the one.
//
// Mutation: drop the chart lookup in planOwner and a seat nobody holds is
// recorded.
func TestAnAtlasOwnerIsASeatOrAUnitOfTheChart(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	mustWrite(t, w[builtin.AtlasAddEntryTool], map[string]any{"kind": "service", "key": "billing"})
	for _, args := range []map[string]any{
		{"atlas_entry": "service:billing", "seat": "the payments team"},
		{"atlas_entry": "service:billing", "unit": "no-such-unit"},
		{"atlas_entry": "service:billing", "seat": "agent-cto", "unit": "nimbus"},
		{"atlas_entry": "service:billing"},
	} {
		if got, _ := atlasCall(t, w[builtin.AtlasSetOwnerTool], args); !got.Failed {
			t.Errorf("an owner named %v was recorded", args)
		}
	}
	mustWrite(t, w[builtin.AtlasSetOwnerTool], map[string]any{"atlas_entry": "service:billing", "seat": "agent-cto"})
	mustWrite(t, w[builtin.AtlasSetOwnerTool], map[string]any{"atlas_entry": "service:billing", "seat": "agent-ceo"})
	e := entryNamed(t, viewOf(t, w), atlas.MustRef(atlas.KindService, "billing"))
	var owners []string
	for _, ed := range e.Relations {
		if ed.Relation == atlas.RelationOwnedBy {
			owners = append(owners, string(ed.External)+":"+ed.Name)
		}
	}
	if !slices.Equal(owners, []string{"seat:agent-ceo"}) {
		t.Errorf("billing is owned by %v, want the seat named last alone", owners)
	}
}

// A PERSON'S "NOT THE SAME" ALWAYS WINS OVER A MERGE IT DID NOT MAKE, and a
// person's merge withdraws one that stands: two entries merged are one, split
// they are two, and merged again they are one.
//
// Mutation: drop the withdrawals from planMerge and the second merge leaves
// them apart.
func TestASplitAlwaysWinsAndAMergeWithdrawsIt(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	a, b := atlas.MustRef(atlas.KindService, "orders"), atlas.MustRef(atlas.KindService, "orders-api")
	for _, ref := range []atlas.Ref{a, b} {
		mustWrite(t, w[builtin.AtlasAddEntryTool], map[string]any{"kind": "service", "key": ref.Key})
	}
	one := func() bool { return entryNamed(t, viewOf(t, w), a).ID == entryNamed(t, viewOf(t, w), b).ID }
	pair := map[string]any{"atlas_entry": a.ID().String(), "other": b.ID().String()}
	mustWrite(t, w[builtin.AtlasMergeTool], pair)
	if !one() {
		t.Fatal("two entries a person merged are shown apart")
	}
	mustWrite(t, w[builtin.AtlasSplitTool], pair)
	if one() {
		t.Fatal("two entries a person split are shown as one")
	}
	mustWrite(t, w[builtin.AtlasMergeTool], pair)
	if !one() {
		t.Error("a merge after a split left the entries apart")
	}
	if got, _ := atlasCall(t, w[builtin.AtlasMergeTool], map[string]any{"atlas_entry": a.ID().String(),
		"other": "host:ssh/web-1"}); !got.Failed {
		t.Error("a service was merged with a host")
	}
}

// A MERGE WITHDRAWS EVERY "NOT THE SAME" THAT KEEPS THE TWO ENTRIES APART, one
// between any member of either, since any one of them blocks the join: a
// and b are one entry, b and c were said not to be the same, and merging a
// with c makes the three one.
//
// Mutation: withdraw only a "not the same" between the two members the call
// names, and c stays apart.
func TestAMergeWithdrawsWhatKeepsEitherEntrysMembersApart(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	ref := func(key string) atlas.Ref { return atlas.MustRef(atlas.KindService, key) }
	for _, key := range []string{"orders", "orders-api", "orders-worker"} {
		mustWrite(t, w[builtin.AtlasAddEntryTool], map[string]any{"kind": "service", "key": key})
	}
	mustWrite(t, w[builtin.AtlasMergeTool], map[string]any{"atlas_entry": "service:orders",
		"other": "service:orders-api"})
	mustWrite(t, w[builtin.AtlasSplitTool], map[string]any{"atlas_entry": "service:orders-api",
		"other": "service:orders-worker"})
	mustWrite(t, w[builtin.AtlasMergeTool], map[string]any{"atlas_entry": "service:orders",
		"other": "service:orders-worker"})
	view := viewOf(t, w)
	if one, other := entryNamed(t, view, ref("orders")), entryNamed(t, view, ref("orders-worker")); one.ID != other.ID {
		t.Errorf("orders and orders-worker are shown apart after a merge: members %v and %v",
			one.Members, other.Members)
	}
}

// A WRITE OVER A CHANGE IT DID NOT SEE IS REFUSED: a version stated with the
// call that the entry has moved past writes nothing, and the version the
// answer carries is the one the next write states.
//
// Mutation: ignore `if_match` and the stale correction lands.
func TestAnAtlasWriteOverAChangeItDidNotSeeIsRefused(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	added := mustWrite(t, w[builtin.AtlasAddEntryTool], map[string]any{"kind": "service", "key": "search"})
	version := added["version"]
	if version == nil {
		t.Fatalf("the add answered no version: %v", added)
	}
	mustWrite(t, w[builtin.AtlasCorrectTool], map[string]any{"atlas_entry": "service:search", "fact": "importance",
		"value": "standard", "if_match": version})
	got, _ := atlasCall(t, w[builtin.AtlasCorrectTool], map[string]any{"atlas_entry": "service:search",
		"fact": "importance", "value": "critical", "if_match": version})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalStaleVersion {
		t.Fatalf("a correction decided from a moved version = %q (%v), want stale", got.Output, got.Cause)
	}
	if n := len(ledger.recorded()); n != 2 {
		t.Errorf("the log holds %d claims, want the add and the first correction", n)
	}
}

// A VALUE'S PURGE IS AN ADMINISTRATOR'S, NAMES THE VALUE BY ITS DIGEST, AND NO
// ANSWER REPEATS IT: an atlas holder who is not an administrator is refused,
// and the log is handed the digest alone.
//
// Mutation: answer the value in planPurge's account and the receipt carries it.
func TestAValuesPurgeIsAnAdministratorsAndNeverRepeatsTheValue(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	const value = "orders-db.internal.acme.example"
	got, _ := atlasCall(t, w[builtin.AtlasPurgeTool], map[string]any{"value": value})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden {
		t.Fatalf("a purge by a holder of atlas alone = %q (%v), want forbidden", got.Output, got.Cause)
	}
	got, answer := atlasCall(t, w[builtin.AtlasPurgeTool], map[string]any{"value": value}, iam.GrantAdmin)
	if got.Failed {
		t.Fatalf("an administrator's purge was refused: %s", got.Output)
	}
	if strings.Contains(got.Output, value) || answer["purged"] != "value" {
		t.Errorf("the purge answered %s, want no value", got.Output)
	}
	if !slices.Equal(ledger.purges, []string{"value:" + atlasdomain.Digest(value)}) {
		t.Errorf("the log was handed %v, want the value's digest", ledger.purges)
	}
}

// NO AGENT CHANGES THE ATLAS THROUGH THESE TOOLS: no seat's registry holds one,
// the authority table refuses every one to an agent whatever it holds, and the
// requester ceiling (ADR-0109) admits none of them to a seat whoever woke it.
//
// Mutation: drop humanOnly from any atlas write's row, or register the atlas
// writes for a seat, and this goes red naming it.
func TestNoAgentChangesTheAtlasThroughAPersonsTools(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(fullDeps(t))); err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(reg.Names()) < 10 {
		t.Fatalf("a seat's registry holds %d tools; this is not reading it", len(reg.Names()))
	}
	woke := founderPrincipal(iam.GrantAtlas)
	for _, name := range builtin.AtlasWriteTools {
		if _, held := reg.Lookup(name); held {
			t.Errorf("a seat's registry holds %s", name)
		}
		only, known := authz.HumanOnlyOf(authz.Action(name))
		if !known || !only {
			t.Errorf("%s is not a person's alone in the authority table", name)
		}
		agent := iam.Principal{Kind: iam.KindSeat, Seat: "agent-cto", Stage: iam.StageActive, Grants: iam.Apps}
		d := authz.Decide(context.Background(), agent, authz.Action(name),
			authz.Object{Kind: authz.KindAtlasEntry}, chartLeads, atlasWriteAt)
		if d.Allowed || d.Reason != authz.ReasonSeatRefused {
			t.Errorf("an agent holding every app on %s decided %+v, want refused as a person's", name, d)
		}
		within := authz.DecideWithin(context.Background(), agent,
			authz.Ceiling{Kind: authz.CeilingRequester, Principal: woke}, authz.Action(name),
			authz.Object{Kind: authz.KindAtlasEntry}, chartLeads, atlasWriteAt)
		if within.Allowed {
			t.Errorf("an agent woken by a person holding atlas was admitted to %s", name)
		}
	}
}

// AN ADD RECORDS WHAT ITS CALLER KNOWS OF THE ENTRY, AND THE SAME CALL FINISHES
// ONE THAT STOPPED PART OF THE WAY: the entry, its name, its purpose, the
// account it belongs to, its owner and the page that documents it, each a
// claim of the caller's own. A second add that would record nothing is refused.
//
// Mutation: refuse an add of an entry the caller's own claim stands on, as an
// add of anybody's is, and the call that stopped can never be finished.
func TestAnAddRecordsWhatIsKnownAndFinishesWhereItStopped(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	w := atlasWrites(t, ledger)
	mustWrite(t, w[builtin.AtlasAddEntryTool], map[string]any{"kind": "vendor_account", "key": "other/acme-cdn",
		"name": "Acme CDN"})
	const page = "0b9d2f0e-6a57-4c58-9d0b-3f1f0a2b7c11"
	args := map[string]any{"kind": "datastore", "key": "acme/orders-db", "name": "Orders", "purpose": "Holds every order",
		"belongs_to": "vendor_account:other/acme-cdn", "seat": "agent-cto", "doclet_page": page}
	// The entry and its name land, and the third record is lost.
	ledger.failAt = len(ledger.recorded()) + 3
	stopped, _ := atlasCall(t, w[builtin.AtlasAddEntryTool], args)
	if !stopped.Failed || !strings.Contains(stopped.Output, "The first 2 of its 6 records landed") {
		t.Fatalf("an add that lost its third record answered %q", stopped.Output)
	}
	finished := mustWrite(t, w[builtin.AtlasAddEntryTool], args)
	if finished["outcome"] != string(statelog.OutcomeApplied) || finished["purpose"] != "Holds every order" {
		t.Fatalf("the same call again answered %v", finished)
	}
	orders := atlas.MustRef(atlas.KindDatastore, "acme/orders-db")
	e := entryNamed(t, viewOf(t, w), orders)
	said := map[string]bool{}
	for _, f := range e.Facts {
		said[string(f.Fact)+"="+f.Value.Text] = f.Source.Kind == atlas.SourcePerson
	}
	for _, ed := range e.Relations {
		target := string(ed.External) + ":" + ed.Name
		if ed.External == "" {
			target = ed.To.String()
		}
		said[string(ed.Relation)+"="+target] = ed.Source.Kind == atlas.SourcePerson
	}
	for _, want := range []string{"name=Orders", "purpose=Holds every order", "owned_by=seat:agent-cto",
		"documented_in=page:" + page, "belongs_to=" + atlas.MustRef(atlas.KindAccount, "other/acme-cdn").ID().String()} {
		if !said[want] {
			t.Errorf("the added entry does not say %s as a person's claim: %v", want, said)
		}
	}
	if n := len(ledger.recorded()); n != 2+6 {
		t.Errorf("the log holds %d claims, want the account's two and the entry's six, none twice", n)
	}
	if again, _ := atlasCall(t, w[builtin.AtlasAddEntryTool], args); !again.Failed ||
		!strings.Contains(again.Output, "in the atlas already") {
		t.Errorf("an add that would record nothing answered %q, want the refusal of a second add", again.Output)
	}
	for _, bad := range []map[string]any{
		{"kind": "service", "key": "ledger", "seat": "agent-cto", "unit": "nimbus"},
		{"kind": "service", "key": "ledger", "doclet_page": "the runbook"},
		{"kind": "service", "key": "ledger", "belongs_to": "datastore:acme/orders-db"},
		{"kind": "service", "key": "ledger", "purpose": atlasToken()},
	} {
		if got, _ := atlasCall(t, w[builtin.AtlasAddEntryTool], bad); !got.Failed {
			t.Errorf("an add with %v was recorded", bad)
		}
	}
	if _, found := atlasFind(viewOf(t, w), atlas.MustRef(atlas.KindService, "ledger")); found {
		t.Error("a refused add left its entry in the atlas")
	}
}

// atlasFind is the entry ref names in a, where it shows one.
func atlasFind(a atlasAnswer, ref atlas.Ref) (atlas.Entry, bool) {
	for _, e := range a.Entries {
		if slices.Contains(e.Members, ref) {
			return e, true
		}
	}
	return atlas.Entry{}, false
}

// THE PAGE THAT DOCUMENTS AN ENTRY IS NAMED BY ITS ID AND REMOVED THE SAME WAY,
// through the relation tools, and is never an entry: a page's id in `to`, an
// entry's relation with a `page`, and anything that is not an id are refused.
//
// Mutation: send documented_in down the entry-to-entry path in planRelate and
// the page is refused as an entry the atlas does not hold.
func TestAPageDocumentsAnEntryByItsIdAndIsRemovedTheSameWay(t *testing.T) {
	t.Parallel()
	w := atlasWrites(t, seededAtlasLedger())
	const page = "0b9d2f0e-6a57-4c58-9d0b-3f1f0a2b7c11"
	documented := func() bool {
		e := entryNamed(t, viewOf(t, w), atlas.MustRef(atlas.KindService, "billing"))
		return slices.ContainsFunc(e.Relations, func(ed atlas.Edge) bool {
			return ed.Relation == atlas.RelationDocumentedIn && ed.External == atlas.ExternalPage && ed.Name == page
		})
	}
	for _, bad := range []map[string]any{
		{"atlas_entry": "service:billing", "relation": "documented_in", "doclet_page": "Runbook"},
		{"atlas_entry": "service:billing", "relation": "documented_in", "doclet_page": page, "to": "service:billing-api"},
		{"atlas_entry": "service:billing", "relation": "depends_on", "to": "service:billing-api", "doclet_page": page},
		{"atlas_entry": "service:billing", "relation": "documented_in"},
	} {
		if got, _ := atlasCall(t, w[builtin.AtlasRelateTool], bad); !got.Failed {
			t.Errorf("a relation named %v was recorded", bad)
		}
	}
	link := map[string]any{"atlas_entry": "service:billing", "relation": "documented_in", "doclet_page": page}
	if drawn := mustWrite(t, w[builtin.AtlasRelateTool], link); drawn["doclet_page"] != page || !documented() {
		t.Fatalf("the page was not drawn: %v", drawn)
	}
	if again := mustWrite(t, w[builtin.AtlasRelateTool], link); again["unchanged"] != true {
		t.Errorf("the same page drawn again answered %v, want unchanged", again)
	}
	mustWrite(t, w[builtin.AtlasUnrelateTool], link)
	if documented() {
		t.Error("the page is still shown after it was removed")
	}
}

// CONFIRMING A VALUE A NEWER REPORT CONTRADICTS SAYS IT AGAIN, so the entry
// stops waiting: a person's value is contested by an integration's later
// report, the entry waits for review, and the person's confirmation is a new
// claim newer than the report, which settles it.
//
// Mutation: answer a confirmation of the person's own shown value as changing
// nothing whatever its standing, and the entry waits for good.
func TestConfirmingAContestedValueSettlesIt(t *testing.T) {
	t.Parallel()
	ledger := newAtlasLedger()
	now := atlasWriteAt
	w := atlasWritesAt(t, ledger, func() time.Time { return now })
	service := atlas.MustRef(atlas.KindService, "checkout-api")
	mustWrite(t, w[builtin.AtlasAddEntryTool], map[string]any{"kind": "service", "key": "checkout-api",
		"name": "Checkout"})
	reported, err := atlas.Says(service, atlas.FactName, "Checkout API", atlas.IntegrationSource("datadog"),
		atlasWriteAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	reported.Position = uint64(len(ledger.claims) + 1)
	ledger.claims = append(ledger.claims, reported)
	ledger.versions[service.ID().String()] = reported.Position
	ledger.mu.Unlock()
	name := func() atlas.FactView {
		e := entryNamed(t, viewOf(t, w), service)
		i := slices.IndexFunc(e.Facts, func(f atlas.FactView) bool { return f.Fact == atlas.FactName })
		if i < 0 {
			t.Fatalf("the entry shows no name: %+v", e.Facts)
		}
		if e.Review != (e.Facts[i].Standing != atlas.StandingSettled) {
			t.Errorf("the entry waits for review: %v, and its name is %s", e.Review, e.Facts[i].Standing)
		}
		return e.Facts[i]
	}
	now = atlasWriteAt.Add(2 * time.Hour)
	if got := name(); got.Standing != atlas.StandingContested || got.Value.Text != "Checkout" || len(got.Others) != 1 {
		t.Fatalf("the name reads %+v, want the person's value contested by the report beside it", got)
	}
	kept := mustWrite(t, w[builtin.AtlasConfirmTool], map[string]any{"atlas_entry": "service:checkout-api",
		"fact": "name", "value": "Checkout"})
	if kept["unchanged"] == true {
		t.Fatalf("confirming a contested value recorded nothing: %v", kept)
	}
	if got := name(); got.Standing != atlas.StandingSettled || got.Value.Text != "Checkout" {
		t.Errorf("after the confirmation the name reads %+v, want the person's value, settled", got)
	}
	// And the other value is taken the same way: it becomes the person's own.
	mustWrite(t, w[builtin.AtlasConfirmTool], map[string]any{"atlas_entry": "service:checkout-api",
		"fact": "name", "value": "Checkout API"})
	if got := name(); got.Standing != atlas.StandingSettled || got.Value.Text != "Checkout API" ||
		got.Source.Kind != atlas.SourcePerson {
		t.Errorf("after taking the report's value the name reads %+v", got)
	}
}
