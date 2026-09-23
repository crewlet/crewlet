package workapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ---- the chart ---------------------------------------------------------- //

// chart is who leads what: `cto` leads the ENG project and the ENG space,
// and nobody leads anything else. err makes every relation unanswerable,
// which is a node behind the chart log.
type chart struct{ err error }

func (c chart) Leads(context.Context, string, string) (bool, error) { return false, c.err }

func (c chart) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	return actor == "cto" && project == "ENG", c.err
}

func (c chart) LeadsUnit(context.Context, string, string) (bool, error) { return false, c.err }

func (c chart) LeadsContainer(_ context.Context, actor, container string) (bool, error) {
	return actor == "cto" && container == "ENG", c.err
}

// ---- principals --------------------------------------------------------- //

// person is somebody signed in and bound to a seat, holding these grants.
func person(seat string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: seat + ".person", Seat: seat,
		Kind: iam.KindPerson, Stage: iam.StageActive,
		Colleague: iam.ColleagueWrite, Grants: grants}
}

// colleague is a person holding the ordinary grants and no deployment grant:
// the caller a case about AUTHORITY acts as, because the admin grant is
// checked before every relation and would admit anything.
func colleague(seat string) iam.Principal {
	return person(seat, iam.GrantStateRead, iam.GrantWorkWrite, iam.GrantKnowledgeWrite)
}

// admin holds every grant.
func admin(seat string) iam.Principal { return person(seat, iam.AllGrants...) }

// ---- the tracker ------------------------------------------------------- //

// reader serves ENG-1 and ENG-2 in ENG and OPS-1 in OPS. Every method but
// Task panics through the nil interface it embeds, which is what a case that
// reaches one it did not expect should do.
type reader struct {
	builtin.WorkReader
	mu    sync.Mutex
	tasks map[string]tracker.TaskDetail
	err   error
}

func newReader() *reader {
	task := func(id, key, project string, rank tracker.Rank) tracker.TaskDetail {
		return tracker.TaskDetail{Task: tracker.Task{
			ID: id, Key: key, Project: project, Title: "work " + key,
			Status: tracker.StatusTodo, Rank: rank, Version: 3,
		}, Complete: true}
	}
	r := &reader{tasks: map[string]tracker.TaskDetail{}}
	for _, d := range []tracker.TaskDetail{
		task("t-1", "ENG-1", "ENG", "a1"),
		task("t-2", "ENG-2", "ENG", "a5"),
		task("t-9", "OPS-1", "OPS", "a3"),
	} {
		r.tasks[d.Task.ID], r.tasks[d.Task.Key] = d, d
	}
	return r
}

func (r *reader) Task(_ context.Context, ref string, want tracker.DetailWants,
	_ statelog.Freshness) (tracker.TaskDetail, error) {

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return tracker.TaskDetail{}, r.err
	}
	d, ok := r.tasks[ref]
	if !ok {
		return tracker.TaskDetail{}, tracker.ErrNoTask
	}
	if want.Comment != "" {
		d.Comments = nil
		for _, c := range r.tasks["comments:"+d.Task.ID].Comments {
			if c.ID == want.Comment {
				d.Comments = []tracker.Comment{c}
			}
		}
	}
	return d, nil
}

// addComment files a remark on a task, for the edit cases.
func (r *reader) addComment(taskID string, c tracker.Comment) {
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
	created []tracker.Task
	updates []tracker.TaskPatch
	moved   [][2]tracker.Rank
	purged  []string
	edited  []string
	inbox   []string
	pins    []string
	views   []tracker.View
	authz   []tracker.PersonAuthority

	// result and err are what the next write answers.
	result statelog.Result
	err    error
}

func (w *writes) answer(opID string) (tracker.WriteResult, error) {
	w.opIDs = append(w.opIDs, opID)
	if w.err != nil {
		return tracker.WriteResult{}, w.err
	}
	res := w.result
	if res.Outcome == "" {
		res.Outcome = statelog.OutcomeApplied
	}
	res.OpID = opID
	res.Version = 4
	return tracker.WriteResult{Result: res, Key: "ENG-7"}, nil
}

// as is every per-actor writer seam at once.
func (w *writes) as(actor builtin.Actor) *bound {
	w.mu.Lock()
	w.actors = append(w.actors, actor)
	w.mu.Unlock()
	return &bound{w: w}
}

type bound struct{ w *writes }

func (b *bound) CreateTask(_ context.Context, opID string, task tracker.Task,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.created = append(b.w.created, task)
	return b.w.answer(opID)
}

func (b *bound) UpdateTask(_ context.Context, opID, _, _ string, _ uint64,
	patch tracker.TaskPatch, _ tracker.ChangeKind,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.updates = append(b.w.updates, patch)
	return b.w.answer(opID)
}

func (b *bound) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	return b.w.answer(opID)
}

func (b *bound) RestoreTask(_ context.Context, opID, _, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	return b.w.answer(opID)
}

func (b *bound) MoveTask(_ context.Context, opID, _, _ string,
	after, before tracker.Rank) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.moved = append(b.w.moved, [2]tracker.Rank{after, before})
	return b.w.answer(opID)
}

func (b *bound) PurgeTask(_ context.Context, opID, id, project, reason string) (
	tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.purged = append(b.w.purged, id+"|"+project+"|"+reason)
	return b.w.answer(opID)
}

func (b *bound) EditComment(_ context.Context, opID, _, _, commentID, body string,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.edited = append(b.w.edited, commentID+"|"+body)
	return b.w.answer(opID)
}

func (b *bound) WriteInbox(_ context.Context, opID, handle string,
	_, _, _ []tracker.InboxEntry, _ []tracker.Reason, _ tracker.Position,
	authority tracker.PersonAuthority) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.inbox = append(b.w.inbox, handle)
	b.w.authz = append(b.w.authz, authority)
	return b.w.answer(opID)
}

func (b *bound) WritePins(_ context.Context, opID, handle string, _ []string,
	_ []tracker.Favorite, authority tracker.PersonAuthority) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	b.w.pins = append(b.w.pins, handle)
	b.w.authz = append(b.w.authz, authority)
	return b.w.answer(opID)
}

func (b *bound) WritePriorities(_ context.Context, opID, _ string, _ []string,
	_ tracker.PersonAuthority) (tracker.WriteResult, error) {

	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	return b.w.answer(opID)
}

func (b *bound) ViewPrior(context.Context, string) (tracker.ViewPrior, error) {
	return tracker.ViewPrior{}, nil
}

func (b *bound) WriteView(_ context.Context, opID string, view tracker.View,
	_ tracker.ViewPrior) (tracker.WriteResult, error) {

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
	page    pages.Detail
	actors  []pages.Actor
	did     []string
	moderat []bool

	err error
}

func newKB() *kb {
	return &kb{page: pages.Detail{
		Page:     pages.Page{ID: "p-1", Container: "ENG", Title: "Runbook", Version: 2},
		Comments: []pages.Comment{{ID: "c-1", Author: "ana", Body: "stale step"}},
	}}
}

func (k *kb) Get(_ context.Context, ref string, _ statelog.Freshness) (pages.Detail, error) {
	if ref != k.page.Page.ID {
		return pages.Detail{}, pages.ErrNotFound
	}
	return k.page, nil
}

func (k *kb) record(actor pages.Actor, what string) (pages.Written, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.actors = append(k.actors, actor)
	k.did = append(k.did, what)
	if k.err != nil {
		return pages.Written{}, k.err
	}
	return pages.Written{Page: k.page.Page, Revision: 9, Outcome: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

func (k *kb) Create(_ context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error) {
	return k.record(actor, "create "+in.Title)
}

func (k *kb) SavePage(_ context.Context, actor pages.Actor, id string, _ pages.Save) (pages.Written, error) {
	return k.record(actor, "save "+id)
}

func (k *kb) Rename(_ context.Context, actor pages.Actor, id, title string, _ bool) (pages.Written, error) {
	return k.record(actor, "rename "+id+" "+title)
}

func (k *kb) Comment(_ context.Context, actor pages.Actor, id string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {

	written, err := k.record(actor, "comment "+id)
	return pages.Comment{ID: "c-new", Body: in.Body}, written, err
}

func (k *kb) EditComment(_ context.Context, actor pages.Actor, id, cid, body string) (
	pages.Comment, pages.Written, error) {

	written, err := k.record(actor, "edit "+cid+" "+body)
	return pages.Comment{ID: cid, Body: body}, written, err
}

func (k *kb) Trash(_ context.Context, actor pages.Actor, id string) (pages.Written, error) {
	return k.record(actor, "trash "+id)
}

func (k *kb) Restore(_ context.Context, actor pages.Actor, id string) (pages.Written, error) {
	return k.record(actor, "restore "+id)
}

func (k *kb) Purge(_ context.Context, actor pages.Actor, id, reason string) (pages.Written, error) {
	return k.record(actor, "purge "+id+" "+reason)
}

func (k *kb) RemoveComment(_ context.Context, actor pages.Actor, id, cid string,
	authority pages.CommentAuthority) (pages.Written, error) {

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
}

func newRig(t *testing.T, c authz.Chart) *rig {
	t.Helper()
	r := &rig{t: t, mux: http.NewServeMux(), reader: newReader(),
		writes: &writes{}, kb: newKB()}
	svc, err := workapi.New(r.options(c))
	if err != nil || svc == nil {
		t.Fatalf("New: %v (%v)", err, svc)
	}
	if err := svc.Routes(r.mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return r
}

// options are the surface's options over this rig's fakes.
func (r *rig) options(c authz.Chart) workapi.Options {
	return workapi.Options{
		Work: builtin.WorkDeps{
			Reader: r.reader,
			Writer: func(a builtin.Actor) builtin.WorkWriter { return r.writes.as(a) },
			TrashWriter: func(a builtin.Actor) builtin.TrashWriter {
				return r.writes.as(a)
			},
			PersonWriter: func(a builtin.Actor) builtin.PersonWriter {
				return r.writes.as(a)
			},
			ViewWriter: func(a builtin.Actor) builtin.ViewWriter {
				return r.writes.as(a)
			},
		},
		Pages: builtin.PageDeps{Reader: r.kb, Writer: r.kb},
		Tracker: func(a builtin.Actor) workapi.TrackerWriter {
			return r.writes.as(a)
		},
		PageStore: r.kb,
		Chart:     c,
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
