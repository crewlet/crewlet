package workapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- the chart ---------------------------------------------------------- //

// chart is who leads what: `cto` leads the ENG project, the ENG space and the
// person in the seat `bo`, and nobody leads anything else. err makes every
// relation unanswerable, which is a node running no company yet.
type chart struct{ err error }

func (c chart) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == "cto" && subject == "bo", c.err
}

func (c chart) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	return actor == "cto" && project == "ENG", c.err
}

func (c chart) LeadsUnit(context.Context, string, string) (bool, error) { return false, c.err }

func (c chart) LeadsContainer(_ context.Context, actor, container string) (bool, error) {
	return actor == "cto" && container == "ENG", c.err
}

// LeadsAnyone is [chart.Leads] asked of every subject: `cto` leads `bo`.
func (c chart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	return actor == "cto", c.err
}

// MemberOf answers that nobody sits in any unit: no case here shares an
// artifact with a team.
func (c chart) MemberOf(context.Context, string, string) (bool, error) {
	return false, c.err
}

// ---- principals --------------------------------------------------------- //

// person is somebody signed in and bound to a seat, holding these grants.
//
// THEIR ID IS DERIVED FROM THEIR LOGIN, as a person's is stable across every
// request they make: a key is scoped by the principal that sent it, so two
// requests from one person minted two ids would be two people.
func person(seat string, grants ...iam.Grant) iam.Principal {
	login := seat + ".person"
	return iam.Principal{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(login)),
		Login: login, Seat: seat,
		Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: grants}
}

// colleague is a person holding every app and not admin: the caller a case
// about AUTHORITY acts as, because admin is checked before every relation and
// would admit anything.
func colleague(seat string) iam.Principal {
	return person(seat, iam.Apps...)
}

// admin holds admin — and so every app — and proved who they are a moment
// ago, so a verb asking for a recent sign-in (a purge) is decided on the
// permission rather than refused for the proof's age.
func admin(seat string) iam.Principal {
	p := person(seat, iam.GrantAdmin)
	p.ReauthAt = time.Now().Add(time.Hour)
	return p
}

// ---- the tracker ------------------------------------------------------- //

// reader serves ENG-1 and ENG-2 in ENG and OPS-1 in OPS. Every method but
// Task panics through the nil interface it embeds, which is what a case that
// reaches one it did not expect should do.
type reader struct {
	builtin.WorkReader
	mu    sync.Mutex
	tasks map[string]worklet.TaskDetail
	err   error

	// asks are the open asks on each task, by the seat each one names — what
	// a thread read authored by that seat infers, as the tracker's own does.
	asks map[string]map[string]string
}

func newReader() *reader {
	task := func(id, key, project string, rank worklet.Rank) worklet.TaskDetail {
		return worklet.TaskDetail{Task: worklet.Task{
			ID: id, Key: key, Project: project, Title: "work " + key,
			Status: worklet.StatusTodo, Rank: rank, Version: 3,
		}, Complete: true}
	}
	r := &reader{tasks: map[string]worklet.TaskDetail{}}
	for _, d := range []worklet.TaskDetail{
		task("t-1", "ENG-1", "ENG", "a1"),
		task("t-2", "ENG-2", "ENG", "a5"),
		task("t-9", "OPS-1", "OPS", "a3"),
	} {
		r.tasks[d.Task.ID], r.tasks[d.Task.Key] = d, d
	}
	return r
}

func (r *reader) Task(_ context.Context, ref string, want worklet.DetailWants,
	_ statelog.Freshness) (worklet.TaskDetail, error) {

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return worklet.TaskDetail{}, r.err
	}
	d, ok := r.tasks[ref]
	if !ok {
		return worklet.TaskDetail{}, worklet.ErrNoTask
	}
	if want.Comment != "" {
		d.Comments = nil
		for _, c := range r.tasks["comments:"+d.Task.ID].Comments {
			if c.ID == want.Comment {
				d.Comments = []worklet.Comment{c}
			}
		}
	}
	return d, nil
}

// Thread infers the one open ask a comment's author was asked, as the
// tracker's reader does, and refuses an `answers` naming somebody else's.
func (r *reader) Thread(_ context.Context, q worklet.ThreadQuery,
	_ statelog.Freshness) (worklet.ResolvedThread, error) {

	r.mu.Lock()
	defer r.mu.Unlock()
	open := r.asks[q.Task]
	out := worklet.ResolvedThread{ThreadParties: worklet.ThreadParties{Asked: q.Ask},
		Answers: q.Answers}
	if out.Answers == "" && q.Author != "" {
		out.Answers = open[q.Author]
	}
	if out.Answers != "" && open[q.Author] != out.Answers {
		return worklet.ResolvedThread{}, fmt.Errorf("%w: %s asked somebody else",
			worklet.ErrForbidden, out.Answers)
	}
	return out, nil
}

// addComment files a remark on a task, for the edit cases.
func (r *reader) addComment(taskID string, c worklet.Comment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.tasks["comments:"+taskID]
	d.Comments = append(d.Comments, c)
	r.tasks["comments:"+taskID] = d
}

// writes is what every writer below was asked to do, as operation ids and
// the actors they were asked as.
type writes struct {
	mu      sync.Mutex
	opIDs   []string
	actors  []builtin.Actor
	created []worklet.Task
	updates []worklet.TaskPatch
	placed  []worklet.Place
	targets []string
	purged  []string
	edited  []string
	inbox   []string
	pins    []string
	prios   []string
	views   []worklet.View
	authz   []worklet.PersonAuthority

	// result, caveats and err are what the next write answers.
	result  statelog.Result
	caveats []caveat.Caveat
	err     error
}

func (w *writes) answer(opID string) (worklet.WriteResult, error) {
	w.opIDs = append(w.opIDs, opID)
	if w.err != nil {
		return worklet.WriteResult{}, w.err
	}
	res := w.result
	if res.Outcome == "" {
		res.Outcome = statelog.OutcomeApplied
	}
	res.OpID = opID
	res.Version = 4
	return worklet.WriteResult{Result: res, Key: "ENG-7", Caveats: w.caveats}, nil
}

// as is every per-actor writer seam at once.
func (w *writes) as(actor builtin.Actor) *bound {
	w.mu.Lock()
	w.actors = append(w.actors, actor)
	w.mu.Unlock()
	return &bound{w: w}
}

type bound struct{ w *writes }

func (b *bound) CreateTask(_ context.Context, opID string, task worklet.Task,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.created = append(b.w.created, task)
	return b.w.answer(opID)
}

// CreateTaskAsking is a create that opens with a question to a seat: the same
// task, recorded as a create.
func (b *bound) CreateTaskAsking(ctx context.Context, opID string, task worklet.Task,
	_ worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error) {

	return b.CreateTask(ctx, opID, task, notify)
}

func (b *bound) UpdateTask(_ context.Context, opID, _, _ string, _ uint64,
	patch worklet.TaskPatch, _ worklet.ChangeKind,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.updates = append(b.w.updates, patch)
	return b.w.answer(opID)
}

func (b *bound) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	return b.w.answer(opID)
}

func (b *bound) RestoreTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	return b.w.answer(opID)
}

// MoveTaskToProject is the cross-project move. A [worklet.MoveStopped] in the
// rig's err comes WITH the root's receipt, as the tracker answers one: the
// root moved, and it is the walk that stopped.
func (b *bound) MoveTaskToProject(_ context.Context, opID, _, target string,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.targets = append(b.w.targets, target)
	if stop := (*worklet.MoveStopped)(nil); errors.As(b.w.err, &stop) {
		b.w.opIDs = append(b.w.opIDs, opID)
		return worklet.WriteResult{Result: statelog.Result{
			Outcome: statelog.OutcomeApplied, OpID: opID, Version: 4,
		}}, b.w.err
	}
	return b.w.answer(opID)
}

// PlaceTask is a board drag: the card, the neighbour it was dropped beside
// and the lane, as the tool resolved them. The order step answers as every
// write here does; the lane step wrote nothing unless the drag named one.
func (b *bound) PlaceTask(_ context.Context, opID string, place worklet.Place,
	_ *worklet.Notify) (worklet.PlaceResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.placed = append(b.w.placed, place)
	order, err := b.w.answer(opID)
	if err != nil {
		return worklet.PlaceResult{}, err
	}
	return worklet.PlaceResult{Order: order, Rank: "a7", Version: order.Version}, nil
}

func (b *bound) PurgeTask(_ context.Context, opID, id, project, reason string) (
	worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.purged = append(b.w.purged, id+"|"+project+"|"+reason)
	return b.w.answer(opID)
}

func (b *bound) EditComment(_ context.Context, opID, _, _, commentID, body string,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.edited = append(b.w.edited, commentID+"|"+body)
	return b.w.answer(opID)
}

func (b *bound) MarkInbox(_ context.Context, opID, handle string,
	_ worklet.InboxGesture, authority worklet.PersonAuthority) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.inbox = append(b.w.inbox, handle)
	b.w.authz = append(b.w.authz, authority)
	return b.w.answer(opID)
}

func (b *bound) WritePins(_ context.Context, opID, handle string, _ worklet.PinGesture,
	authority worklet.PersonAuthority) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.pins = append(b.w.pins, handle)
	b.w.authz = append(b.w.authz, authority)
	return b.w.answer(opID)
}

func (b *bound) WritePriorities(_ context.Context, opID, handle string, _ []string,
	_ *uint64, authority worklet.PersonAuthority) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.prios = append(b.w.prios, handle)
	b.w.authz = append(b.w.authz, authority)
	return b.w.answer(opID)
}

// ---- the identity directory -------------------------------------------- //

// directory is whose record a login names: `bo.person` holds the seat `bo`,
// `jane.doe` holds none, and nobody else holds anything. err makes every
// lookup one this node cannot answer.
type directory struct{ err error }

func (d directory) HolderRecord(_ context.Context, login string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	switch login {
	case "bo.person":
		return "bo", nil
	case "jane.doe":
		return "jane.doe", nil
	}
	return "", fmt.Errorf("%w: %s", iam.ErrNoHolder, login)
}

func (b *bound) ViewPrior(context.Context, string) (worklet.ViewPrior, error) {
	return worklet.ViewPrior{}, nil
}

func (b *bound) WriteView(_ context.Context, opID string, view worklet.View,
	_ worklet.ViewPrior) (worklet.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.views = append(b.w.views, view)
	return b.w.answer(opID)
}

// ---- the knowledge base ------------------------------------------------ //

// kb serves one page, ENG/Runbook, with one remark by `ana`. Its PageReader
// half embeds a nil interface for the reason [reader] does.
type kb struct {
	builtin.PageReader
	mu      sync.Mutex
	page    doclet.Detail
	actors  []doclet.Actor
	did     []string
	moderat []bool

	err error
}

func newKB() *kb {
	return &kb{page: doclet.Detail{
		Page:     doclet.Page{ID: "p-1", Container: "ENG", Title: "Runbook", Version: 2},
		Comments: []doclet.Comment{{ID: "c-1", Author: "ana", Body: "stale step"}},
	}}
}

func (k *kb) Get(_ context.Context, ref string, _ statelog.Freshness) (doclet.Detail, error) {
	if ref != k.page.Page.ID {
		return doclet.Detail{}, doclet.ErrNotFound
	}
	return k.page, nil
}

func (k *kb) record(actor doclet.Actor, what string) (doclet.Written, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.actors = append(k.actors, actor)
	k.did = append(k.did, what)
	if k.err != nil {
		return doclet.Written{}, k.err
	}
	return doclet.Written{Page: k.page.Page, Revision: 9, Outcome: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

func (k *kb) Create(_ context.Context, actor doclet.Actor, in doclet.NewPage) (doclet.Written, error) {
	return k.record(actor, "create "+in.Title)
}

func (k *kb) SavePage(_ context.Context, actor doclet.Actor, id string, _ doclet.Save) (doclet.Written, error) {
	return k.record(actor, "save "+id)
}

func (k *kb) Rename(_ context.Context, actor doclet.Actor, id, title string, _ bool) (doclet.Written, error) {
	return k.record(actor, "rename "+id+" "+title)
}

func (k *kb) Comment(_ context.Context, actor doclet.Actor, id string,
	in doclet.NewComment) (doclet.Comment, doclet.Written, error) {

	written, err := k.record(actor, "comment "+id)
	return doclet.Comment{ID: "c-new", Body: in.Body}, written, err
}

func (k *kb) EditComment(_ context.Context, actor doclet.Actor, id, cid, body string) (
	doclet.Comment, doclet.Written, error) {

	written, err := k.record(actor, "edit "+cid+" "+body)
	return doclet.Comment{ID: cid, Body: body}, written, err
}

func (k *kb) Trash(_ context.Context, actor doclet.Actor, id string) (doclet.Written, error) {
	return k.record(actor, "trash "+id)
}

func (k *kb) Restore(_ context.Context, actor doclet.Actor, id string) (doclet.Written, error) {
	return k.record(actor, "restore "+id)
}

func (k *kb) Purge(_ context.Context, actor doclet.Actor, id, reason string) (doclet.Written, error) {
	return k.record(actor, "purge "+id+" "+reason)
}

func (k *kb) RemoveComment(_ context.Context, actor doclet.Actor, id, cid string,
	authority doclet.CommentAuthority) (doclet.Written, error) {

	k.mu.Lock()
	k.moderat = append(k.moderat, authority.Moderate)
	k.mu.Unlock()
	return k.record(actor, "uncomment "+cid)
}

// ---- the rig ------------------------------------------------------------ //

type rig struct {
	t      *testing.T
	mux    *http.ServeMux
	reader *reader
	writes *writes
	kb     *kb
	dir    directory
	audit  *auditLog
}

func newRig(t *testing.T, c authz.Chart) *rig {
	t.Helper()
	return newRigWith(t, c, directory{})
}

// newRigWith is [newRig] over an identity directory a case states.
func newRigWith(t *testing.T, c authz.Chart, dir directory) *rig {
	t.Helper()
	r := &rig{t: t, mux: http.NewServeMux(), reader: newReader(),
		writes: &writes{}, kb: newKB(), dir: dir, audit: &auditLog{}}
	svc, err := workapi.New(r.options(c))
	if err != nil || svc == nil {
		t.Fatalf("New: %v (%v)", err, svc)
	}
	if err := svc.Routes(r.mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return r
}

// options are the surface's options over this rig's fakes: its halves, served
// on every request, and the operator dispatch over the same halves and chart.
func (r *rig) options(c authz.Chart) workapi.Options {
	halves := serving(r.halves())
	return workapi.Options{Halves: halves, Chart: c,
		Operator: r.operator(c, halves), Audit: r.audit}
}

// operator is the operator surface's dispatch over the halves a source finds
// and the chart the routes decide on — what a node builds, where both read
// one engine's halves and one chart value — auditing into the rig's log.
func (r *rig) operator(c authz.Chart, halves func() (workapi.Halves, bool)) *operator.Server {
	r.t.Helper()
	// THE SEAT CONTROLS operator.New requires, over an in-memory fleet and
	// queue nobody else reads, a company with no chart, a steer channel under
	// a key no node holds and run records no turn has: no case here calls one.
	fleet, queue := coordmemory.NewFleet(), queuememory.New()
	wire, err := signedask.New[steer.Request, steer.Reply](steer.Channel,
		statelog.OneKey("k1", "the work surface cases' seat-control key material"))
	if err != nil {
		r.t.Fatalf("the steer channel: %v", err)
	}
	s, err := operator.New(operator.Options{
		Halves: func() (operator.Halves, bool) {
			h, up := halves()
			return operator.Halves{Work: h.Work, Pages: h.Pages}, up
		},
		Authorize: builtin.Decide(c),
		Audit:     r.audit,
		Runs: builtin.RunDeps{Desk: sandbox.AnswerDesk{
			Pending: sandbox.NewCoordStore(fleet), Queue: queue}},
		Pauses: builtin.SeatPauseDeps{Pauses: fleet, Announce: queue,
			Org: func() *org.Organization { return nil }},
		Steer: builtin.SteerDeps{Asker: queue, Wire: wire, Runs: noRunRecords{}},
	})
	if err != nil {
		r.t.Fatalf("operator.New: %v", err)
	}
	return s
}

// noRunRecords is the run records the rig's steer control files a note on: a
// fleet in which no turn has launched its coding run.
type noRunRecords struct{}

func (noRunRecords) Seat(context.Context, string) (builtin.RunSeat, bool, error) {
	return builtin.RunSeat{}, false, nil
}

func (noRunRecords) File(context.Context, string, steer.Note) (builtin.RunFiled, error) {
	return builtin.RunFiled{}, errors.New("no turn here has a run record to file a note on")
}

// auditLog records every runtime audit record the surface publishes.
type auditLog struct {
	mu     sync.Mutex
	events []*events.Event
}

func (a *auditLog) Publish(_ context.Context, _ string, ev *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

// acted is every `operator_acted` record published, in order.
func (a *auditLog) acted() []types.OperatorActed {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []types.OperatorActed
	for _, ev := range a.events {
		if p, ok := events.DataAs[*types.OperatorActed](ev); ok {
			out = append(out, *p)
		}
	}
	return out
}

// serving is a halves source that always answers with h — a node that has met
// its company.
func serving(h workapi.Halves) func() (workapi.Halves, bool) {
	return func() (workapi.Halves, bool) { return h, true }
}

// halves are the two halves this rig's fakes make.
func (r *rig) halves() workapi.Halves {
	return workapi.Halves{
		Work: builtin.WorkDeps{
			Reader: r.reader,
			Writer: func(a builtin.Actor) builtin.WorkWriter { return r.writes.as(a) },
			TrashWriter: func(a builtin.Actor) builtin.TrashWriter {
				return r.writes.as(a)
			},
			Moves: func(a builtin.Actor) builtin.WorkMover {
				return r.writes.as(a)
			},
			PersonWriter: func(a builtin.Actor) builtin.PersonWriter {
				return r.writes.as(a)
			},
			ViewWriter: func(a builtin.Actor) builtin.ViewWriter {
				return r.writes.as(a)
			},
			Placer: func(a builtin.Actor) builtin.WorkPlacer {
				return r.writes.as(a)
			},
			Holders: r.dir,
		},
		Pages: builtin.PageDeps{Reader: r.kb, Writer: r.kb},
		Tracker: func(a builtin.Actor) workapi.TrackerWriter {
			return r.writes.as(a)
		},
		PageStore: r.kb,
	}
}

// answered is one response, decoded.
type answered struct {
	status int
	header http.Header
	body   map[string]any
}

func (a answered) detail() string { s, _ := a.body["detail"].(string); return s }

// caller is who a request comes from: a principal, nobody, or a node that
// could not tell.
type caller func(context.Context) context.Context

func as(p iam.Principal) caller {
	return func(ctx context.Context) context.Context { return iam.WithPrincipal(ctx, p) }
}

var (
	anonymous caller = iam.WithAnonymous
	unknown   caller = func(ctx context.Context) context.Context {
		return iam.WithUnresolved(ctx, errors.New("the identity estate is behind"))
	}
)

// do runs one request through the surface.
func (r *rig) do(who caller, method, target string, body any,
	headers ...string) answered {

	r.t.Helper()
	var payload *strings.Reader
	switch b := body.(type) {
	case nil:
		payload = strings.NewReader("")
	case string:
		payload = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			r.t.Fatalf("encode: %v", err)
		}
		payload = strings.NewReader(string(encoded))
	}
	req := httptest.NewRequest(method, target, payload)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	req = req.WithContext(who(req.Context()))
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	out := answered{status: rec.Code, header: rec.Header()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	return out
}
