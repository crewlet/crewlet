package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// codeReposDoc is a company hosting code, with an Engineering unit to own
// repositories, declaring repos as code.repos (none when empty) and units as
// the chart's extra units.
func codeReposDoc(repos, units string) string {
	doc := nativeCleanupCompany + `units:
  - name: Engineering
    project: ENG
    roles:
      - name: SWE
        handle: swe
        llm: zulu
` + units + `code:
  backend: gitlet
`
	if repos != "" {
		doc += "  repos:\n" + repos
	}
	return doc
}

// webRepo is a repository owned by the Engineering unit, with two protection
// rules declared OUT OF the canonical order and description as given.
func webRepo(description string) string {
	out := "    - name: web\n      owner: {unit: Engineering}\n"
	if description != "" {
		out += "      description: " + description + "\n"
	}
	return out + "      protect:\n        - branch: release/*\n        - branch: main\n"
}

// apiRepo is a repository owned by a seat.
const apiRepo = "    - name: api\n      owner: {seat: swe}\n      protect: [{branch: main}]\n"

// repoNamed reads one repository through e's own router, linearizably, with
// its path owners.
func repoNamed(t *testing.T, e *Engine, name string) (*gitlet.RepoDetail, error) {
	t.Helper()
	answer, err := e.router.Code().Read(t.Context(), gitlet.ReadQuery{
		Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-1", Seat: "ceo"}, Kind: gitlet.ReadRepo, Repo: name,
		PathOwners: true, Gates: true, Fresh: statelog.Freshness{Level: statelog.ReadLinearizable},
	})
	if err != nil {
		return nil, err
	}
	return answer.Repo, nil
}

// mustRepo is [repoNamed] for a repository that must be there.
func mustRepo(t *testing.T, e *Engine, name string) *gitlet.RepoDetail {
	t.Helper()
	repo, err := repoNamed(t, e, name)
	if err != nil {
		t.Fatalf("read repository %s: %v", name, err)
	}
	return repo
}

// reconciled waits for e's reconciles of code.repos to finish: an apply only
// asks for one ([Engine.applyCodeRepos]), so what it decides lands after.
func reconciled(t *testing.T, e *Engine) {
	t.Helper()
	waitUntil(t, 30*time.Second, "the reconcile of code.repos to finish", e.codeRepos.idle)
}

// applyReconciled is [applyOK] followed by the reconcile it asked for.
func applyReconciled(t *testing.T, e *Engine, doc string) {
	t.Helper()
	applyOK(t, e, doc)
	reconciled(t, e)
}

// codeLogEnd is the last sequence e's code log holds.
func codeLogEnd(t *testing.T, e *Engine) uint64 {
	t.Helper()
	end, err := e.core.Load().log.Domain(gitlet.Domain{}.Name()).log.End(t.Context())
	if err != nil {
		t.Fatalf("read the code log's end: %v", err)
	}
	return end
}

// codeLogWrites is every record's subject on e's code log after the sequence
// after, less barriers and trim records, which readers and the trim append
// whatever a reconcile does.
func codeLogWrites(t *testing.T, e *Engine, after uint64) []gitlet.Subject {
	t.Helper()
	running := e.core.Load().log.Domain(gitlet.Domain{}.Name())
	end := codeLogEnd(t, e)
	var out []gitlet.Subject
	for seq := after + 1; seq <= end; seq++ {
		wire, _, _, ok, err := running.log.At(t.Context(), seq)
		if err != nil || !ok {
			t.Fatalf("read the code log at %d: held %v, %v", seq, ok, err)
		}
		subject, ok := gitlet.ParseSubject(wire)
		if !ok {
			t.Fatalf("the code log holds %q at %d, which is no code subject", wire, seq)
		}
		if subject.Kind != gitlet.KindBarrier && subject.Kind != gitlet.KindTrimmed {
			out = append(out, subject)
		}
	}
	return out
}

// THE REPOSITORIES code.repos DECLARES ARE RECONCILED AT EVERY ACTIVATION, AND
// ONE THAT CHANGES NOTHING WRITES ONE RECORD.
//
// The boot creates what the document declares: the owner as its unit's key,
// the default branch defaulted, the rules as namespaced patterns in canonical
// order (declared out of order here; the domain refuses any other, so an
// unsorted reconcile creates nothing). A changed description settles. An
// unchanged activation writes only its fence, which already keeps an older
// activation from walking a newer one back, so no repository is re-stamped.
func TestDeclaredRepositoriesAreReconciledAtActivation(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo(""), "")))
	servesCodeReads(t, e)
	reconciled(t, e)
	web := mustRepo(t, e, "web")
	if web.Status != gitlet.RepoStatusActive || web.ManagedBy != gitlet.ManagedConfig {
		t.Errorf("web is %s, managed by %s; want active, managed by the configuration",
			web.Status, web.ManagedBy)
	}
	// A unit stating no key is named, in every durable row, by the one
	// minted from its name.
	if want := (gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: config.MintUnitID("Engineering")}); web.Owner != want {
		t.Errorf("web's owner is %+v, want the unit's key %+v", web.Owner, want)
	}
	if web.DefaultBranch != config.DefaultCodeBranch {
		t.Errorf("web's default branch is %q, want the default %q", web.DefaultBranch, config.DefaultCodeBranch)
	}
	var patterns []string
	for _, p := range web.Protections {
		patterns = append(patterns, p.Pattern)
		if p.DirectPush != gitlet.DirectPersons {
			t.Errorf("rule %s takes direct pushes from %q, want an unset direct_push to read as %q",
				p.Pattern, p.DirectPush, gitlet.DirectPersons)
		}
	}
	if want := []string{"refs/heads/main", "refs/heads/release/*"}; !slices.Equal(patterns, want) {
		t.Errorf("web's rules are %v, want %v", patterns, want)
	}

	applyReconciled(t, e, codeReposDoc(webRepo("The customer-facing web app."), ""))
	if got := mustRepo(t, e, "web"); got.Description != "The customer-facing web app." ||
		got.ID != web.ID {
		t.Errorf("after an activation that describes it, web is %q (id %s), want the "+
			"description settled on the same repository %s", got.Description, got.ID, web.ID)
	}

	before := codeLogEnd(t, e)
	applyReconciled(t, e, codeReposDoc(webRepo("The customer-facing web app."), ""))
	wrote := codeLogWrites(t, e, before)
	if !slices.Equal(wrote, []gitlet.Subject{gitlet.CompanySubject()}) {
		t.Errorf("an activation that changed nothing wrote %v to the code log, want its "+
			"activation fence alone, %v", wrote, gitlet.CompanySubject())
	}
}

// A REPOSITORY code.repos NO LONGER DECLARES IS DELETED, AND RESTORED WHOLE BY
// DECLARING IT AGAIN: the same repository, under its old id, rather than a new
// one under its old name — the retention is the window an undo has.
func TestARemovedRepositoryIsDeletedAndRestorable(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo("")+apiRepo, "")))
	servesCodeReads(t, e)
	reconciled(t, e)
	api := mustRepo(t, e, "api")

	applyReconciled(t, e, codeReposDoc(webRepo(""), ""))
	deleted := mustRepo(t, e, "api")
	if deleted.Status != gitlet.RepoStatusDeleted || deleted.DeletedAt.IsZero() {
		t.Fatalf("after an activation that no longer declares it, api is %s deleted at %v, "+
			"want deleted, with the instant", deleted.Status, deleted.DeletedAt)
	}
	if web := mustRepo(t, e, "web"); web.Status != gitlet.RepoStatusActive {
		t.Errorf("web, still declared, is %s", web.Status)
	}

	applyReconciled(t, e, codeReposDoc(webRepo("")+apiRepo, ""))
	restored := mustRepo(t, e, "api")
	if restored.Status != gitlet.RepoStatusActive || restored.ID != api.ID {
		t.Errorf("after an activation that declares it again, api is %s with id %s, want "+
			"active under its old id %s", restored.Status, restored.ID, api.ID)
	}
}

// A PUSH TO A REMOVED REPOSITORY IS TOLD WHEN ITS PURGE MAY BEGIN UNDER THE
// EPOCH'S OWN RETENTION.
//
// The seam held: the committer's CommitterOptions.Retention is the epoch's
// code.deleted_retention_days ([Engine.codeDeletedRetention]), which the
// code-retention duty purges by, so the retention here is anything but the
// default (told the default week's instant, a company keeping thirty days
// would restore weeks before its purge may begin), and the push goes
// through the node's router to the runtime's committer.
//
// Mutation: wire the committer's Retention to gitlet.DefaultDeletedRetention and
// the push is told the week's instant, and fails.
func TestAPushToARemovedRepositoryIsToldTheEpochsRetention(t *testing.T) {
	t.Parallel()
	const days = 30
	retained := func(doc string) string {
		return strings.Replace(doc, "code:\n  backend: gitlet\n",
			fmt.Sprintf("code:\n  backend: gitlet\n  deleted_retention_days: %d\n", days), 1)
	}
	e := newSandboxNode(t, parseCompany(t, retained(codeReposDoc(webRepo("")+apiRepo, ""))))
	servesCodeReads(t, e)
	reconciled(t, e)
	retention := days * 24 * time.Hour
	if got := e.codeDeletedRetention(); got != retention || got == gitlet.DefaultDeletedRetention {
		t.Fatalf("the premise: the epoch keeps a deleted repository %v, want %v — anything "+
			"but the default %v", got, retention, gitlet.DefaultDeletedRetention)
	}

	applyReconciled(t, e, retained(codeReposDoc(webRepo(""), "")))
	api := mustRepo(t, e, "api")
	if api.Status != gitlet.RepoStatusDeleted || api.DeletedAt.IsZero() {
		t.Fatalf("the premise: api, no longer declared, is %s deleted at %v", api.Status,
			api.DeletedAt)
	}
	push := gitlet.Proposal{
		PushID: uuid.Must(uuid.NewV7()).String(), RepoID: api.ID,
		Principal: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-1", Seat: "ceo"},
		Via:       gitlet.ViaGit, Node: e.id, ReceivedAt: time.Now().UTC(),
		Commands: []gitlet.Command{{Ref: "refs/heads/main", Old: gitlet.ZeroOID,
			New: "0123456789abcdef0123456789abcdef01234567"}},
	}
	res, err := e.router.Code().Propose(t.Context(), push)
	if err != nil {
		t.Fatalf("a push to the deleted repository: %v; want it answered", err)
	}
	if res.Answer.Refused != gitlet.RefusalRepoDeleted {
		t.Fatalf("a push to the deleted repository is answered %+v, want it refused %s",
			res.Answer, gitlet.RefusalRepoDeleted)
	}
	want := "repository api is deleted; declaring it again in code.repos restores it as it " +
		"was, until its purge, which may begin from " +
		api.DeletedAt.Add(retention).UTC().Format(time.RFC3339)
	if !strings.Contains(res.Answer.Detail, want) {
		t.Fatalf("the push is told %q, want %q: the deletion plus the epoch's %d days",
			res.Answer.Detail, want, days)
	}
}

// ONLY A REPOSITORY THE CONFIGURATION MANAGES IS DELETED FOR BEING UNDECLARED,
// and only a live one. A repository another manager made (as a newer build may
// write) is no declaration's to retire: this build is its older peer in a
// rolling upgrade. One already deleted is not deleted again.
func TestOnlyAConfigManagedRepositoryIsDeletedForBeingUndeclared(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		managed gitlet.ManagedBy
		status  gitlet.RepoStatus
		retired bool
	}{
		{gitlet.ManagedConfig, gitlet.RepoStatusActive, true},
		{gitlet.ManagedConfig, gitlet.RepoStatusArchived, true},
		{gitlet.ManagedConfig, gitlet.RepoStatusDeleted, false},
		{gitlet.ManagedConfig, gitlet.RepoStatusPurging, false},
		{gitlet.ManagedBy("tools"), gitlet.RepoStatusActive, false},
		{gitlet.ManagedBy("tools"), gitlet.RepoStatusArchived, false},
	} {
		l := gitlet.RepoLife{ID: "id", Name: "old", Status: tc.status, ManagedBy: tc.managed}
		if got := retiredByOmission(l); got != tc.retired {
			t.Errorf("an undeclared %s repository managed by %q is retired: %v, want %v — "+
				"only the live repositories the configuration manages go for being absent",
				tc.status, tc.managed, got, tc.retired)
		}
	}
}

// A PURGE THAT FREES A NAME THE CURRENT EPOCH DECLARES AGAIN CREATES IT AT ONCE.
//
// A re-declaration that meets a purge can only wait it out
// (code_repo_awaits_purge), and nothing schedules another activation, so the
// retention pass hands every purged name to the engine, which creates it where
// the current epoch declares it and leaves it free where it does not.
func TestAPurgedRepositoryIsDeclaredAgainByTheDuty(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo(""), "")))
	servesCodeReads(t, e)
	reconciled(t, e)
	old := mustRepo(t, e, "web")
	applyReconciled(t, e, codeReposDoc("", ""))
	if got := mustRepo(t, e, "web"); got.Status != gitlet.RepoStatusDeleted {
		t.Fatalf("the premise: an activation that no longer declares web leaves it %s", got.Status)
	}

	// THE DUTY'S PASS as the node builds it, with a nanosecond retention: the
	// subject is what a purge hands on, not the clock.
	d := e.newCodeRetentionDuty(e.writeSide().code, e.native.Load().codeReader, e.core.Load().nodeID)
	d.pass.Retention = func() time.Duration { return time.Nanosecond }
	report, err := d.pass.Pass(t.Context())
	if err != nil || !slices.Equal(report.Purged, []string{"web"}) {
		t.Fatalf("the pass purged %v (%v), want web", report.Purged, err)
	}
	// THE CURRENT EPOCH DOES NOT DECLARE IT: the name is simply free.
	if _, err := repoNamed(t, e, "web"); !errors.Is(err, gitlet.ErrRepoNotFound) {
		t.Fatalf("a purged name the epoch does not declare reads %v, want it free", err)
	}

	// A RE-DECLARATION THAT RACED THE PURGE, installed directly: a purge of a
	// few rows is one record, so the race cannot be staged through the store.
	// The purge's hand-on is what creates it.
	again, err := NewCompany(parseCompany(t, codeReposDoc(webRepo(""), "")))
	if err != nil {
		t.Fatalf("build the epoch: %v", err)
	}
	stampActivation(again, time.Now())
	e.installEpoch(t.Context(), again)
	if err := e.redeclareCode(t.Context(), "web"); err != nil {
		t.Fatalf("hand the purged name on: %v", err)
	}
	web := mustRepo(t, e, "web")
	if web.Status != gitlet.RepoStatusActive || web.ID == old.ID {
		t.Errorf("after its purge was handed on, web is %s with id %s, want a new active "+
			"repository (the purged one was %s)", web.Status, web.ID, old.ID)
	}
}

// THE CODE-RETENTION DUTY RUNS WHERE A PURGE MAY BE WRITTEN, AND ONLY THERE: on
// a publishing data node hosting code — never a company hosting none, nor a
// node in a maintenance mode, whose lifecycle store is withheld. A turn
// holding the duty on a node at the fleet's revision runs the claim, its
// renewal and the purge.
func TestTheCodeRetentionDutyRunsWhereAPurgeMayBeWritten(t *testing.T) {
	t.Parallel()
	t.Run("hosting code", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo(""), "")))
		if e.codeRetention == nil {
			t.Fatal("a data node hosting code armed no code-retention duty: a removed " +
				"repository would be deleted and never purged")
		}
		// The armed loop is stopped so it cannot race the test's own duty.
		e.stopCodeRetention()
		servesCodeReads(t, e)
		reconciled(t, e)
		r := fleetReconciler(t, e)
		tickApplies(t, e, r, activateRevision(t, e, codeReposDoc("", "")))
		d := e.newCodeRetentionDuty(e.writeSide().code, e.native.Load().codeReader, e.core.Load().nodeID)
		d.pass.Retention = func() time.Duration { return time.Nanosecond }
		report, ran := d.turn(t.Context())
		if !ran || !slices.Equal(report.Purged, []string{"web"}) {
			t.Fatalf("a turn holding the duty ran %v and purged %v, want web purged", ran, report.Purged)
		}
	})
	t.Run("hosting none", func(t *testing.T) {
		t.Parallel()
		if e := newSandboxNode(t, parseCompany(t, nativeCleanupCompany)); e.codeRetention != nil {
			t.Error("a company hosting no code armed a code-retention duty")
		}
	})
	t.Run("a maintenance mode", func(t *testing.T) {
		t.Parallel()
		if e, _ := aNodeOf(t, gitletHostCompany, statelog.ModeMaintenance); e.codeRetention != nil {
			t.Error("a node in a maintenance mode armed a code-retention duty, whose every " +
				"purge step is a publish the mode exists to rule out")
		}
	})
}

// THE RUNTIME'S APPLIES DO NOT WAIT FOR THE INBOUND EDGE.
//
// The chart's projects, the knowledge containers and the repositories go
// through the runtime's own writers, so a node whose edge is not running still
// applies them. The edge is only the parsers' ([Engine.reconcileNative]); the
// three are clauses of [Engine.followCompany], run here in its order with the
// edge withheld.
func TestTheReconcileDoesNotDependOnTheInboundEdge(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo(""), "")))
	servesCodeReads(t, e)
	reconciled(t, e)
	// No inbound edge for one reconcile; restored for the engine's stop.
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.service = nil
	e.notify.mu.Unlock()
	t.Cleanup(func() {
		e.notify.mu.Lock()
		e.notify.service = svc
		e.notify.mu.Unlock()
	})
	next, err := NewCompany(parseCompany(t, codeReposDoc(webRepo("")+apiRepo,
		"  - name: Operations\n    project: OPS\n    roles:\n      - name: SRE\n        handle: sre\n        llm: zulu\n")))
	if err != nil {
		t.Fatalf("build the epoch: %v", err)
	}
	stampActivation(next, time.Now())
	e.reconcileNative(t.Context(), next)
	e.applyChart(t.Context(), next)
	e.applyContainers(t.Context(), next)
	e.applyCodeRepos(t.Context(), next, next.ActivatedAt)
	reconciled(t, e)
	if api, err := repoNamed(t, e, "api"); err != nil || api.Status != gitlet.RepoStatusActive {
		t.Errorf("a reconcile on a node with no inbound edge left api %+v (%v), want it created",
			api, err)
	}
	var key string
	err = e.backends.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT key FROM tracker_projects WHERE key = 'OPS'`).Scan(&key)
	})
	if err != nil {
		t.Errorf("a reconcile on a node with no inbound edge left the chart's OPS project "+
			"unapplied: %v", err)
	}
}

// EVERY direct_push A DOCUMENT CAN DECLARE IS ONE THE DOMAIN RECORDS, AND THE
// OTHER WAY ROUND. config cannot import the domain that imports it, so each
// keeps the closed set; a value in one only would pass `crewlet validate` and
// fail every reconcile, or be recorded and never declarable.
func TestEveryDeclaredDirectPushIsOneTheDomainRecords(t *testing.T) {
	t.Parallel()
	for _, v := range config.CodeDirectPushes {
		if !gitlet.DirectPush(v.Resolved()).Valid() {
			t.Errorf("code.repos may declare direct_push %q, which the code domain refuses", v)
		}
	}
	if !gitlet.DirectPush(config.CodeDirectPush("").Resolved()).Valid() {
		t.Error("an unset direct_push resolves to a value the code domain refuses")
	}
	for _, v := range gitlet.DirectPushes() {
		if !config.CodeDirectPush(v).Valid() {
			t.Errorf("the code domain records direct_push %q, which no document may declare", v)
		}
	}
}

// EVERY VISIBILITY A DOCUMENT MAY DECLARE IS ONE THE CODE DOMAIN RECORDS, and
// the other way round (the closed sets are spelled twice, as for direct_push).
// One the domain refused fails every reconcile; one no document may declare
// could never be narrowed or widened back by configuration. Unset resolves to
// one the domain records, since the domain takes no unset value.
func TestEveryDeclaredVisibilityIsOneTheDomainRecords(t *testing.T) {
	t.Parallel()
	for _, v := range config.CodeVisibilities {
		if !gitlet.Visibility(v.Resolved()).Valid() {
			t.Errorf("code.repos may declare visibility %q, which the code domain refuses", v)
		}
	}
	if !gitlet.Visibility(config.CodeVisibility("").Resolved()).Valid() {
		t.Error("an unset visibility resolves to a value the code domain refuses")
	}
	for _, v := range gitlet.Visibilities() {
		if !config.CodeVisibility(v).Valid() {
			t.Errorf("the code domain records visibility %q, which no document may declare", v)
		}
	}
	if len(config.CodeVisibilities) != len(gitlet.Visibilities()) {
		t.Errorf("config declares %v, the code domain records %v", config.CodeVisibilities,
			gitlet.Visibilities())
	}
}

// EVERY SEARCH MODE A DOCUMENT MAY DECLARE IS ONE THE CODE DOMAIN RECORDS, and
// the other way round, in order (the closed sets are spelled twice, as for
// direct_push). One the domain refused fails every reconcile; one no document
// may declare is a mode no declared repository could be put in. Unset resolves
// to one the domain records, since the domain takes no unset value.
func TestEveryDeclaredSearchModeIsOneTheDomainRecords(t *testing.T) {
	t.Parallel()
	sameClosedSet(t, "search", config.CodeSearchModes, gitlet.SearchModes())
	for _, m := range config.CodeSearchModes {
		if !gitlet.SearchMode(m.Resolved()).Valid() {
			t.Errorf("code.repos may declare search %q, which the code domain refuses", m)
		}
	}
	if !gitlet.SearchMode(config.CodeSearchMode("").Resolved()).Valid() {
		t.Error("an unset search resolves to a value the code domain refuses")
	}
}

// A DECLARED REPOSITORY IS INDEXED BY DEFAULT: an entry saying nothing of
// search is recorded `indexed` (a named project is code a search reads), one
// saying `grep` so. A changed mode settles in one settings record beside the
// fence, an unchanged revision writes the fence alone, and dropping the field
// restores the default.
//
// Mutations: resolve an unset search to grep (config.CodeSearchMode.Resolved)
// — web reads grep, red; compare settings without their mode
// (gitlet.RepoSettings.equal) — the changed mode never reaches the log, red;
// leave the mode out of the reconcile's batch read (gitlet's reposRowsSQL) — the
// unchanged activation writes a settings record, red.
func TestADeclaredRepositoryIsIndexedByDefault(t *testing.T) {
	t.Parallel()
	doc := func(webSearch string) string {
		web := "    - name: web\n      owner: {unit: Engineering}\n"
		if webSearch != "" {
			web += "      search: " + webSearch + "\n"
		}
		return codeReposDoc(web+"    - name: api\n      owner: {seat: swe}\n      search: grep\n", "")
	}
	e := newSandboxNode(t, parseCompany(t, doc("")))
	servesCodeReads(t, e)
	reconciled(t, e)
	web := mustRepo(t, e, "web")
	if web.Search != gitlet.SearchIndexed {
		t.Errorf("web, declaring no search, is recorded at %q, want %s", web.Search, gitlet.SearchIndexed)
	}
	if api := mustRepo(t, e, "api"); api.Search != gitlet.SearchGrep {
		t.Errorf("api, declared search: grep, is recorded at %q, want %s", api.Search, gitlet.SearchGrep)
	}

	// THE MODE CHANGED: one settings record beside the fence.
	before := codeLogEnd(t, e)
	applyReconciled(t, e, doc("grep"))
	if got := mustRepo(t, e, "web").Search; got != gitlet.SearchGrep {
		t.Errorf("after an activation declaring search: grep, web is recorded at %q", got)
	}
	if wrote := codeLogWrites(t, e, before); !slices.Equal(wrote,
		[]gitlet.Subject{gitlet.CompanySubject(), gitlet.RepoSubject(web.ID)}) {
		t.Errorf("an activation changing web's search wrote %v to the code log, want its fence and one "+
			"settings record", wrote)
	}

	// NOTHING CHANGED: the fence alone.
	before = codeLogEnd(t, e)
	applyReconciled(t, e, doc("grep"))
	if wrote := codeLogWrites(t, e, before); !slices.Equal(wrote, []gitlet.Subject{gitlet.CompanySubject()}) {
		t.Errorf("an activation changing nothing wrote %v to the code log, want its fence alone, %v", wrote,
			gitlet.CompanySubject())
	}

	// DROPPED: the default again.
	applyReconciled(t, e, doc(""))
	if got := mustRepo(t, e, "web").Search; got != gitlet.SearchIndexed {
		t.Errorf("after an activation that names no search, web is recorded at %q, want %s", got,
			gitlet.SearchIndexed)
	}
}

// sameClosedSet holds one of code.repos' closed sets as config declares it to
// the code domain's: the same values, in the same order.
func sameClosedSet[C, D ~string](t *testing.T, field string, declared []C, recorded []D) {
	t.Helper()
	d := make([]string, len(declared))
	for i, v := range declared {
		d[i] = string(v)
	}
	r := make([]string, len(recorded))
	for i, v := range recorded {
		r[i] = string(v)
	}
	if !slices.Equal(d, r) {
		t.Errorf("code.repos may declare %s %v, and the code domain records %v", field, d, r)
	}
}

// EVERY VALUE A MERGE GATE MAY DECLARE IS ONE THE CODE DOMAIN RECORDS, AND THE
// OTHER WAY ROUND, in order — merge_by, approvals, delete_source, the change
// classes and the path classes an unless list takes (spelled twice, as for
// direct_push). Each unset value resolves to one the domain records, since the
// domain takes none.
func TestEveryDeclaredMergeByIsOneTheDomainRecords(t *testing.T) {
	t.Parallel()
	sameClosedSet(t, "merge_by", config.CodeMergeBys, gitlet.MergeBys())
	sameClosedSet(t, "approvals", config.CodeApprovalCounts, gitlet.ApprovalCounts())
	sameClosedSet(t, "delete_source", config.CodeDeleteSources, gitlet.DeleteSources())
	sameClosedSet(t, "a change class", config.CodeChangeClasses, gitlet.ChangeClasses())
	sameClosedSet(t, "an exempting class", config.CodePathClasses, gitlet.PathClasses())
	for field, valid := range map[string]bool{
		"merge_by":      gitlet.MergeBy(config.CodeMergeBy("").Resolved()).Valid(),
		"approvals":     gitlet.Approvals(config.CodeApprovals("").Resolved()).Valid(),
		"delete_source": gitlet.DeleteSource(config.CodeDeleteSource("").Resolved()).Valid(),
	} {
		if !valid {
			t.Errorf("an unset %s resolves to a value the code domain refuses", field)
		}
	}
}

// A DECLARED GATE WEIGHS WHAT THE DOMAIN RECORDS: config bounds a gate at
// CodeMaxGateBytes by its resolved JSON (config.CodeGate.EncodedBytes), the
// domain at MaxGateBytes by the record's, so lengths and bounds must agree or
// `crewlet validate` and the reconcile disagree. Held over a one-field gate and
// an every-field one with lists out of order (sorting changes no length) and
// globs spelled long.
func TestADeclaredGateWeighsWhatTheDomainRecords(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, codeReposDoc(webRepo(""), "")))
	if err != nil {
		t.Fatalf("build the epoch: %v", err)
	}
	if config.CodeMaxGateBytes != gitlet.MaxGateBytes {
		t.Fatalf("config bounds a gate at %d bytes and the domain at %d", config.CodeMaxGateBytes,
			gitlet.MaxGateBytes)
	}
	for name, rule := range map[string]config.CodeProtection{
		"one field": {Branch: "main", MergeBy: config.CodeMergeByPersons},
		"every field, spelled long": {
			Branch: "main", MergeBy: config.CodeMergeByMaintainers, Approvals: config.CodeApprovalsOne,
			ApprovalsUnless: []config.CodeChangeClass{config.CodeChangeClassWhitespaceOnly,
				config.CodeChangeClassCommentOnly},
			HumanPaths: []config.CodeHumanPath{
				{Glob: "z&<>/**", Unless: []config.CodeChangeClass{config.CodeChangeClassWhitespaceOnly,
					config.CodeChangeClassCommentOnly}},
				{Glob: "a/été/**"},
			},
			Checks:        []string{"unit", "lint", "e2e.linux"},
			Landing:       config.CodeLanding{Depth: 8, Batch: 1},
			OwnersApprove: true, DeleteSource: config.CodeDeleteNone, AgentsEditChecks: true,
		},
	} {
		repo := c.Config.Code.Repos[0]
		repo.Protect = []config.CodeProtection{rule}
		declared, err := declaredRepo(c.Org.Index(), repo)
		if err != nil {
			t.Fatalf("%s: declare web: %v", name, err)
		}
		if err := declared.Validate(); err != nil {
			t.Fatalf("%s: the declared gate is not one the domain records: %v", name, err)
		}
		recorded, err := json.Marshal(declared.Settings.Protections[0].Gate)
		if err != nil {
			t.Fatal(err)
		}
		if got := rule.Gate().EncodedBytes(); got != len(recorded) {
			t.Errorf("%s: config weighs the gate at %d bytes and the domain records %d: %s", name, got,
				len(recorded), recorded)
		}
	}
}

// DECLARED PATH OWNERS WEIGH WHAT THE DOMAIN RECORDS: config bounds them at
// CodeMaxPathOwnersBytes by their recorded form's JSON
// (config.CodeRepo.PathOwnersBytes), the domain at MaxPathOwnersBytes by the
// record's, so lengths and bounds must agree. Held over a unit named by its
// name (the record carries its shorter key), a seat, and long, unsorted globs.
//
// Mutation: weigh a unit by the name the document gave it (config's
// recordedOwner) — the two lengths part, red.
func TestDeclaredPathOwnersWeighWhatTheDomainRecords(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, codeReposDoc(webRepo(""), "  - name: The Platform Team\n    id: plat\n")))
	if err != nil {
		t.Fatalf("build the epoch: %v", err)
	}
	if config.CodeMaxPathOwnersBytes != gitlet.MaxPathOwnersBytes {
		t.Fatalf("config bounds path owners at %d bytes and the domain at %d", config.CodeMaxPathOwnersBytes,
			gitlet.MaxPathOwnersBytes)
	}
	repo := c.Config.Code.Repos[0]
	repo.PathOwners = []config.CodePathOwner{
		{Paths: []string{"z&<>/**", "a/été/**", "web/**"}, Unit: "The Platform Team"},
		{Paths: []string{"Makefile"}, Seat: "swe"},
	}
	declared, err := declaredRepo(c.Org.Index(), repo)
	if err != nil {
		t.Fatalf("declare web: %v", err)
	}
	if err := declared.Validate(); err != nil {
		t.Fatalf("the declared path owners are not ones the domain records: %v", err)
	}
	if key := declared.Settings.PathOwners[1].Owner.Ref; key != "plat" {
		t.Fatalf("the premise: the unit The Platform Team is recorded by its key plat, and here as %q", key)
	}
	recorded, err := json.Marshal(declared.Settings.PathOwners)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := repo.PathOwnersBytes(c.Org.Index()); !ok || got != len(recorded) {
		t.Errorf("config weighs the path owners at %d bytes (resolved %t) and the domain records %d: %s", got, ok,
			len(recorded), recorded)
	}
}

// A DECLARATION IS RECORDED IN ONE CANONICAL FORM, whatever its rules' order,
// so every node forms one record; an owner the chart does not resolve is named
// rather than recorded empty.
func TestADeclarationIsRecordedInOneCanonicalForm(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, codeReposDoc(webRepo("")+apiRepo, "")))
	if err != nil {
		t.Fatalf("build the epoch: %v", err)
	}
	web, err := declaredRepo(c.Org.Index(), c.Config.Code.Repos[0])
	if err != nil {
		t.Fatalf("declare web: %v", err)
	}
	if err := web.Validate(); err != nil {
		t.Errorf("web's declaration is not one the domain records: %v", err)
	}
	reordered := c.Config.Code.Repos[0]
	reordered.Protect = slices.Clone(reordered.Protect)
	slices.Reverse(reordered.Protect)
	again, err := declaredRepo(c.Org.Index(), reordered)
	if err != nil || !slices.Equal(again.Settings.Protections, web.Settings.Protections) {
		t.Errorf("the same rules in another order are declared as %+v (%v), want %+v",
			again.Settings.Protections, err, web.Settings.Protections)
	}
	api, err := declaredRepo(c.Org.Index(), c.Config.Code.Repos[1])
	if err != nil || api.Settings.Owner != (gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "swe"}) {
		t.Errorf("api's owner is %+v (%v), want the seat swe", api.Settings.Owner, err)
	}
	// AN UNSET VISIBILITY IS RECORDED AS company (config's Resolved; the
	// domain takes no unset value), a declared one as itself.
	if web.Settings.Visibility != gitlet.VisibilityCompany {
		t.Errorf("web, declaring no visibility, is recorded at %q, want company", web.Settings.Visibility)
	}
	narrowed := c.Config.Code.Repos[0]
	narrowed.Visibility = config.CodeVisibilityUnit
	if unit, err := declaredRepo(c.Org.Index(), narrowed); err != nil ||
		unit.Settings.Visibility != gitlet.VisibilityUnit {
		t.Errorf("web at visibility: unit is declared as %+v (%v), want unit", unit.Settings, err)
	}
	stray := c.Config.Code.Repos[0]
	stray.Owner = config.CodeOwner{Unit: "Marketing"}
	if _, err := declaredRepo(c.Org.Index(), stray); err == nil || !strings.Contains(err.Error(), "Marketing") {
		t.Errorf("an owner the chart does not resolve was declared (%v), want it named", err)
	}

	// PATH OWNERS, by kind then reference with globs sorted, a unit recorded
	// by the key the chart resolves for its name.
	pathed := c.Config.Code.Repos[0]
	pathed.PathOwners = []config.CodePathOwner{
		{Paths: []string{"web/**", "docs/**"}, Unit: "Engineering"},
		{Paths: []string{"Makefile"}, Seat: "swe"},
	}
	owned, err := declaredRepo(c.Org.Index(), pathed)
	want := []gitlet.PathOwner{
		{Owner: gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "swe"}, Paths: []string{"Makefile"}},
		{Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: config.MintUnitID("Engineering")},
			Paths: []string{"docs/**", "web/**"}},
	}
	if err != nil || !reflect.DeepEqual(owned.Settings.PathOwners, want) {
		t.Errorf("web's path owners are declared as %+v (%v), want %+v", owned.Settings.PathOwners, err, want)
	}
	if err := owned.Validate(); err != nil {
		t.Errorf("web's declaration with its path owners is not one the domain records: %v", err)
	}
	pathed.PathOwners = []config.CodePathOwner{{Paths: []string{"web/**"}, Unit: "Marketing"}}
	if _, err := declaredRepo(c.Org.Index(), pathed); err == nil ||
		!strings.Contains(err.Error(), "path_owners[0].unit") || !strings.Contains(err.Error(), "Marketing") {
		t.Errorf("a path owner the chart does not resolve was declared (%v), want it named at its field", err)
	}

	// A MERGE GATE with every unset field resolved and its lists in canonical
	// order; a rule with no gate field records none.
	gated := c.Config.Code.Repos[0]
	gated.Protect = []config.CodeProtection{{Branch: "release/*"}, {Branch: "main", Checks: []string{"unit", "lint"},
		HumanPaths: []config.CodeHumanPath{{Glob: "deploy/**", Unless: []config.CodeChangeClass{
			config.CodeChangeClassWhitespaceOnly, config.CodeChangeClassCommentOnly}}, {Glob: ".crewlet/**"}}}}
	recorded, err := declaredRepo(c.Org.Index(), gated)
	if err != nil {
		t.Fatalf("declare web with a gate: %v", err)
	}
	wantGate := gitlet.MergeGate{MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone,
		HumanPaths: []gitlet.HumanPath{{Glob: ".crewlet/**"}, {Glob: "deploy/**", Unless: []gitlet.ChangeClass{
			gitlet.ChangeClassCommentOnly, gitlet.ChangeClassWhitespaceOnly}}},
		Checks: []string{"lint", "unit"},
		Landing: gitlet.Landing{Depth: config.CodeDefaultQueueDepth,
			Batch: config.CodeDefaultQueueBatch}, DeleteSource: gitlet.DeleteSeats}
	switch rules := recorded.Settings.Protections; {
	case len(rules) != 2 || rules[0].Pattern != "refs/heads/main" || rules[1].Gate != nil:
		t.Errorf("web's rules are declared as %+v, want main's gated and release/*'s with none", rules)
	case rules[0].Gate == nil || !rules[0].Gate.Equal(wantGate):
		t.Errorf("main's gate is declared as %+v, want %+v", rules[0].Gate, wantGate)
	}
	if err := recorded.Validate(); err != nil {
		t.Errorf("web's declaration with its gate is not one the domain records: %v", err)
	}
}

// PATH OWNERS RECONCILE FROM THE DOCUMENT in their canonical form, each unit
// by its resolved key whatever its spelling. A changed glob settles in one
// settings record beside the fence, an unchanged activation writes the fence
// alone, and dropping them leaves none.
//
// Mutations: compare settings without their path owners
// (gitlet.RepoSettings.equal) — the changed glob never reaches the log, red;
// leave them out of the reconcile's batch read — the unchanged activation
// writes a settings record, red.
func TestPathOwnersReconcileFromTheDocument(t *testing.T) {
	t.Parallel()
	sales := "  - name: Sales\n    roles:\n      - name: Manager\n        handle: mgr\n        llm: zulu\n"
	doc := func(webGlob string, owned bool) string {
		repo := "    - name: web\n      owner: {unit: Engineering}\n"
		if owned {
			repo += "      path_owners:\n" +
				"        - {paths: [\"" + webGlob + "\", \"docs/**\"], unit: Engineering}\n" +
				"        - {paths: [\"Makefile\"], seat: mgr}\n" +
				"        - {paths: [\"sales/**\"], unit: sales}\n"
		}
		return codeReposDoc(repo, sales)
	}
	ownersWith := func(webGlob string) []gitlet.PathOwner {
		return []gitlet.PathOwner{
			{Owner: gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "mgr"}, Paths: []string{"Makefile"}},
			{Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: config.MintUnitID("Engineering")},
				Paths: []string{"docs/**", webGlob}},
			{Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "sales"}, Paths: []string{"sales/**"}},
		}
	}
	e := newSandboxNode(t, parseCompany(t, doc("web/**", true)))
	servesCodeReads(t, e)
	reconciled(t, e)
	web := mustRepo(t, e, "web")
	if !reflect.DeepEqual(web.PathOwners, ownersWith("web/**")) {
		t.Fatalf("web's path owners are %+v, want %+v", web.PathOwners, ownersWith("web/**"))
	}

	// ONE GLOB CHANGED: one settings record beside the fence.
	before := codeLogEnd(t, e)
	applyReconciled(t, e, doc("web/**/*.go", true))
	if got := mustRepo(t, e, "web").PathOwners; !reflect.DeepEqual(got, ownersWith("web/**/*.go")) {
		t.Errorf("after an activation changing one glob, web's path owners are %+v, want %+v", got,
			ownersWith("web/**/*.go"))
	}
	if wrote := codeLogWrites(t, e, before); !slices.Equal(wrote,
		[]gitlet.Subject{gitlet.CompanySubject(), gitlet.RepoSubject(web.ID)}) {
		t.Errorf("an activation changing one glob wrote %v to the code log, want its fence and one "+
			"settings record", wrote)
	}

	// NOTHING CHANGED: the fence alone.
	before = codeLogEnd(t, e)
	applyReconciled(t, e, doc("web/**/*.go", true))
	if wrote := codeLogWrites(t, e, before); !slices.Equal(wrote, []gitlet.Subject{gitlet.CompanySubject()}) {
		t.Errorf("an activation changing nothing wrote %v to the code log, want its fence alone, %v", wrote,
			gitlet.CompanySubject())
	}

	// DROPPED: none.
	applyReconciled(t, e, doc("", false))
	if got := mustRepo(t, e, "web").PathOwners; len(got) != 0 {
		t.Errorf("after an activation that names no path owners, web has %+v", got)
	}
}

// AN UNCHANGED GATE WRITES NOTHING, AND A CHANGED ONE LANDS. A gate the same
// but for its lists' order is the fence alone (the gate read from the rows is a
// new pointer, so it is compared by value); one changed field settles in one
// settings record; a dropped gate leaves the rule with none.
//
// Mutations: compare two rules without their gates (gitlet.Protection.Equal) —
// the changed field never reaches the log, red; compare the rules with
// slices.Equal (gitlet.RepoSettings.equal) — the unchanged activation writes a
// settings record, red.
func TestAnUnchangedGateWritesNothing(t *testing.T) {
	t.Parallel()
	doc := func(rule string) string {
		return codeReposDoc("    - name: web\n      owner: {unit: Engineering}\n      protect:\n        - "+rule+"\n",
			"")
	}
	gated := func(checks string) string {
		return "{branch: main, merge_by: maintainers, approvals: one, checks: [" + checks + "], " +
			"human_paths: [{glob: 'deploy/**'}, {glob: '.crewlet/**', unless: [comment_only]}]}"
	}
	gateWith := func(checks ...string) *gitlet.MergeGate {
		return &gitlet.MergeGate{MergeBy: gitlet.MergeByMaintainers, Approvals: gitlet.ApprovalsOne,
			HumanPaths: []gitlet.HumanPath{{Glob: ".crewlet/**", Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}},
				{Glob: "deploy/**"}},
			Checks: checks,
			Landing: gitlet.Landing{Depth: config.CodeDefaultQueueDepth,
				Batch: config.CodeDefaultQueueBatch}, DeleteSource: gitlet.DeleteSeats}
	}
	node := newSandboxNode(t, parseCompany(t, doc(gated("unit, lint"))))
	servesCodeReads(t, node)
	reconciled(t, node)
	web := mustRepo(t, node, "web")
	read := func(stage string) *gitlet.MergeGate {
		t.Helper()
		repo := mustRepo(t, node, "web")
		if rules := repo.Protections; len(rules) != 1 || rules[0].Pattern != "refs/heads/main" {
			t.Fatalf("%s: web's rules are %+v, want main's one", stage, rules)
		}
		switch gates := repo.Gates; {
		case len(gates) == 0:
			return nil
		case len(gates) != 1 || gates[0].Pattern != "refs/heads/main":
			t.Fatalf("%s: web's gates are %+v, want main's at most", stage, gates)
		}
		return &repo.Gates[0].Gate
	}
	if got, want := read("created"), gateWith("lint", "unit"); got == nil || !got.Equal(*want) {
		t.Fatalf("web's gate is %+v, want %+v", got, *want)
	}

	// THE SAME GATE, its checks written in another order: the fence alone.
	before := codeLogEnd(t, node)
	applyReconciled(t, node, doc(gated("lint, unit")))
	if wrote := codeLogWrites(t, node, before); !slices.Equal(wrote, []gitlet.Subject{gitlet.CompanySubject()}) {
		t.Errorf("an activation whose gate is unchanged wrote %v to the code log, want its fence alone, %v",
			wrote, gitlet.CompanySubject())
	}

	// ONE FIELD CHANGED: one settings record beside the fence.
	before = codeLogEnd(t, node)
	applyReconciled(t, node, doc(gated("lint, unit, e2e")))
	if got, want := read("after a check was added"), gateWith("e2e", "lint", "unit"); got == nil ||
		!got.Equal(*want) {
		t.Errorf("after an activation adding a check, web's gate is %+v, want %+v", got, *want)
	}
	if wrote := codeLogWrites(t, node, before); !slices.Equal(wrote,
		[]gitlet.Subject{gitlet.CompanySubject(), gitlet.RepoSubject(web.ID)}) {
		t.Errorf("an activation changing one field of a gate wrote %v to the code log, want its fence and "+
			"one settings record", wrote)
	}

	// DROPPED: the rule stays, with no gate.
	applyReconciled(t, node, doc("{branch: main}"))
	if got := read("after the gate was dropped"); got != nil {
		t.Errorf("after an activation that sets no gate field, main's rule keeps the gate %+v", *got)
	}
}

// losingPublisher is a code-log write authority that, where lose says so,
// appends a name's claim on a context that cannot end and then answers
// OutcomeUnknown with no error — what statelog answers for an append whose
// context ended mid-flight, since its classifying probe runs on that dead
// context.
type losingPublisher struct {
	gitlet.Publisher
	lose func(ctx context.Context) bool
}

func (p losingPublisher) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	if gitlet.ObjectKind(req.Subject.Kind) != gitlet.KindName {
		return p.Publisher.Publish(ctx, req)
	}
	res, err := p.Publisher.Publish(context.WithoutCancel(ctx), req)
	if err != nil || !p.lose(ctx) {
		return res, err
	}
	return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: req.OpID}, nil
}

// A RECONCILE A NEWER ACTIVATION ENDS SAYS SO ONCE AND SAYS NOTHING ELSE: one
// code_repos_superseded line and no code_repo_not_applied, though statelog
// answers each append in flight unknown with no error. Here eight new
// repositories, all in flight, are ended with ErrSuperseded once all eight
// records have landed and before any answer is in.
//
// Mutation: keep the answer of a decision that failed once its run had
// ended, and have an unknown outcome on an ended context not say so, and each
// repository is logged as one the reconcile could not apply.
func TestASupersededReconcileSaysOnlyThat(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo(""), "")))
	reconciled(t, e)
	n, rt := e.native.Load(), e.core.Load()
	const inFlight = 8
	ctx, end := context.WithCancelCause(t.Context())
	var answered atomic.Int32
	store, err := gitlet.NewStore(gitlet.StoreOptions{
		Publisher: losingPublisher{
			Publisher: rt.log.Domain(gitlet.Domain{}.Name()).publisher,
			lose: func(ctx context.Context) bool {
				if answered.Add(1) == inFlight {
					end(fmt.Errorf("%w: a newer activation asked", gitlet.ErrSuperseded))
				}
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					t.Error("the reconcile's context never ended")
				}
				return true
			},
		},
		DB: rt.log.estate().Reader(), NodeID: rt.nodeID,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c := e.Company()
	repos := make([]config.CodeRepo, inFlight)
	for i := range repos {
		repos[i] = config.CodeRepo{Name: fmt.Sprintf("lost-%d", i), Owner: config.CodeOwner{Seat: "swe"}}
	}
	var buf bytes.Buffer
	if err := reconcileCodeRepos(ctx, slog.New(slog.NewJSONHandler(&buf, nil)), store, n.codeReader,
		codeOwners(c), repos, c.ActivatedAt.Add(time.Minute)); err != nil {
		t.Errorf("a superseded reconcile answered %v as left undone: the newer activation's "+
			"reconcile decides every repository, so it is not run again", err)
	}
	lines := logLines(t, &buf)
	if got := len(lines["code_repo_not_applied"]); got != 0 {
		t.Errorf("a superseded reconcile logged %d repositories it could not apply, want none: "+
			"every one was in flight when the reconcile ended", got)
	}
	if got, failed := len(lines["code_repos_superseded"]), len(lines["code_repos_not_applied"]); got != 1 || failed != 0 {
		t.Errorf("a superseded reconcile was logged superseded %d times and as a failure %d "+
			"times, want once and never", got, failed)
	}
}
