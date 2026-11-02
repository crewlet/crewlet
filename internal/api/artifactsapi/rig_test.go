package artifactsapi_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE RIG: the surface over fakes of everything it reads and writes, each
// recording what it was asked, so a case can say both what was answered and
// what was never touched.
//
// The company: jane owns `page` (an HTML artifact), `notes` (a doc, filed in
// the folder `campaigns`),
// `handbook` (a doc of two tabs), `deck` (slides), `design` (a canvas of
// two artboards) and `pulse` (a dashboard of one note), the last five shared
// with bob as viewer; the
// page is shared with bob as viewer, with the ENG unit as editor and with sam
// as editor. `scribe` is an agent owning `report`, led by lee. dana relates to
// nothing, and ada is an administrator who relates to nothing either. The
// tracker holds ENG-12, live, and ENG-9, removed.

var (
	page     = artifacts.IDFor("jane's page")
	notes    = artifacts.IDFor("jane's notes")
	deck     = artifacts.IDFor("jane's deck")
	handbook = artifacts.IDFor("jane's handbook")
	design   = artifacts.IDFor("jane's design")
	pulse    = artifacts.IDFor("jane's pulse")
	report   = artifacts.IDFor("scribe's report")
	binned   = artifacts.IDFor("jane's trashed page")
	unknown  = artifacts.IDFor("nothing at all")
	// legacy carries a share an earlier build wrote to a login named as a
	// seat, which reaches nobody and which its owner must still remove.
	legacy = artifacts.IDFor("jane's page shared with a login")
	// tucked is shared with bob as viewer and filed in jane's private `inner`,
	// a folder bob cannot see.
	tucked = artifacts.IDFor("jane's tucked page")
)

// files of the page's version 2.
var (
	indexHTML = []byte(`<!doctype html><title>x</title><script>alert(1)</script>`)
	logoSVG   = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(2)</script></svg>`)
	chartPNG  = []byte("\x89PNG\r\n\x1a\nnot really")
	notesMD   = []byte("# Notes\n\nSee CRW-412.\n")
	deckJSON  = []byte(`{"title":"Plan","order":["cover","next"],"cover":"next"}`)
	coverHTML = []byte(`<section id="cover" style="background:#fff"><h1>Plan</h1><aside>Hello.</aside></section>`)
	nextHTML  = []byte(`<section id="next" style="background:#000;color:#fff"><h2>Next steps</h2></section>`)
	docJSON   = []byte(`{"v":1,"tabs":[{"path":"tabs/start.md","title":"Start"},{"path":"tabs/oncall.md","title":"On call"}]}`)
	startMD   = []byte("# Welcome\n\nRead this first.\n")
	oncallMD  = []byte("# On call\n\nPage the lead.\n")

	canvasJSON = []byte(`{"v":1,"title":"Launch","boards":{"hero.html":{"x":0,"y":0,"w":1440,"h":900,` +
		`"title":"Hero"},"mobile.html":{"x":1540,"y":0,"w":390,"h":844}},"order":["hero.html",` +
		`"mobile.html"],"notes":[{"id":"n1","kind":"title","x":0,"y":-100,"w":600,"h":60,"text":"Launch"}],` +
		`"cover":"mobile.html"}`)
	heroHTML  = []byte(`<!doctype html><title>Hero</title><h1>Ship</h1>`)
	boardJSON = []byte(`{"v":1,"title":"Pulse","datasets":[{"id":"deals","title":"Pipeline","source":"store",` +
		`"store":{"collection":"deals"},"columns":[{"key":"name","type":"text"}]}],` +
		`"widgets":[{"id":"hello","type":"note","w":12,"text":"Hello."}]}`)
	mobileHTML = []byte(`<!doctype html><title>Mobile</title><h1>Go</h1>`)
)

func object(name string, body []byte) objstore.Object {
	return objstore.Object{Key: objstore.KeyAt(time.Unix(1_700_000_000, 0)), Hash: objstore.HashOf(body),
		Size: int64(len(body))}
}

func entry(path, served string, body []byte) artifacts.Entry {
	return artifacts.Entry{Path: path, ContentType: served, Object: object(path, body)}
}

var (
	entryIndex = entry("index.html", "text/html; charset=utf-8", indexHTML)
	entryLogo  = entry("img/logo.svg", "image/svg+xml", logoSVG)
	entryChart = entry("chart.png", "image/png", chartPNG)
	entryNotes = entry("notes.md", "text/markdown; charset=utf-8", notesMD)
	entryDeck  = entry("deck.json", "application/json", deckJSON)
	entryCover = entry("slides/cover.html", "text/html; charset=utf-8", coverHTML)
	entryNext  = entry("slides/next.html", "text/html; charset=utf-8", nextHTML)
	entryDoc   = entry("doc.json", "application/json", docJSON)
	entryStart = entry("tabs/start.md", "text/markdown; charset=utf-8", startMD)
	entryOncal = entry("tabs/oncall.md", "text/markdown; charset=utf-8", oncallMD)

	entryCanvas = entry("canvas.json", "application/json", canvasJSON)
	entryHero   = entry("hero.html", "text/html; charset=utf-8", heroHTML)
	entryMobile = entry("mobile.html", "text/html; charset=utf-8", mobileHTML)
	entryBoard  = entry("dashboard.json", "application/json", boardJSON)
)

func share(kind artifacts.AudienceKind, id string, role artifacts.Role) artifacts.Share {
	return artifacts.Share{Audience: artifacts.Audience{Kind: kind, ID: id}, Role: role}
}

// estate is the artifacts estate: heads, versions and listings.
type estate struct {
	mu        sync.Mutex
	versions  int
	listed    []artifactsdomain.Viewer
	listItems []artifactsdomain.ListItem

	// adoption is the company's adopted brand, nil for none.
	adoption *artifactsdomain.Adoption
}

func (e *estate) Brand(context.Context, statelog.Freshness) (artifactsdomain.Adoption, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.adoption == nil {
		return artifactsdomain.Adoption{}, false, nil
	}
	return *e.adoption, true, nil
}

func (e *estate) BrandFile(_ context.Context, hash objstore.Hash) (artifactsdomain.BrandFile, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.adoption != nil {
		for _, f := range e.adoption.Files {
			if f.Object.Hash == hash {
				return f, nil
			}
		}
	}
	return artifactsdomain.BrandFile{}, artifactsdomain.ErrNotFound
}

func heads() map[artifacts.ID]artifactsdomain.Head {
	pageShares := []artifacts.Share{
		share(artifacts.AudienceSeat, "bob", artifacts.RoleViewer),
		share(artifacts.AudienceUnit, "ENG", artifacts.RoleEditor),
		share(artifacts.AudienceSeat, "sam", artifacts.RoleEditor),
	}
	return map[artifacts.ID]artifactsdomain.Head{
		page: {ID: page, Label: page.Label(), Kind: artifacts.KindHTML, Title: "Page", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 2, Status: artifactsdomain.StatusLive,
			Shares: pageShares, AccessEpoch: 4},
		notes: {ID: notes, Kind: artifacts.KindDoc, Title: "Notes", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{share(artifacts.AudienceSeat, "bob", artifacts.RoleViewer)},
			Folder: campaigns},
		deck: {ID: deck, Kind: artifacts.KindSlides, Title: "Plan", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{share(artifacts.AudienceSeat, "bob", artifacts.RoleViewer)}},
		handbook: {ID: handbook, Kind: artifacts.KindDoc, Title: "Handbook", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{share(artifacts.AudienceSeat, "bob", artifacts.RoleViewer)}},
		design: {ID: design, Kind: artifacts.KindDesign, Title: "Launch", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{share(artifacts.AudienceSeat, "bob", artifacts.RoleViewer)}},
		pulse: {ID: pulse, Kind: artifacts.KindDashboard, Title: "Pulse", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{share(artifacts.AudienceSeat, "bob", artifacts.RoleViewer)}},
		report: {ID: report, Kind: artifacts.KindHTML, Title: "Report", Owner: "scribe",
			OwnerKind: iam.ActorAgent, HeadNumber: 1, Status: artifactsdomain.StatusLive},
		binned: {ID: binned, Kind: artifacts.KindHTML, Title: "Binned", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusTrashed},
		tucked: {ID: tucked, Kind: artifacts.KindDoc, Title: "Tucked away", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{share(artifacts.AudienceSeat, "bob", artifacts.RoleViewer)},
			Folder: inner},
		legacy: {ID: legacy, Kind: artifacts.KindHTML, Title: "Old share", Owner: "jane",
			OwnerKind: iam.ActorHuman, HeadNumber: 1, Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{share(artifacts.AudienceSeat, "token:maya", artifacts.RoleViewer)}},
	}
}

func (e *estate) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if h, ok := heads()[id]; ok {
		return h, statelog.Position{}, nil
	}
	return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
}

func (e *estate) Version(_ context.Context, id artifacts.ID, n int) (artifactsdomain.Version, error) {
	e.mu.Lock()
	e.versions++
	e.mu.Unlock()
	h, ok := heads()[id]
	if !ok || n < 1 || n > h.HeadNumber {
		return artifactsdomain.Version{}, artifactsdomain.ErrNotFound
	}
	v := artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: n,
		Capabilities: []artifacts.Capability{artifacts.CapabilityDB}}}
	switch id {
	case notes:
		v.Manifest = artifacts.Manifest{Entry: "notes.md", Files: []artifacts.Entry{entryNotes}}
	case deck:
		v.Manifest = artifacts.Manifest{Entry: "deck.json",
			Files: []artifacts.Entry{entryDeck, entryCover, entryNext}}
	case handbook:
		v.Manifest = artifacts.Manifest{Entry: "doc.json",
			Files: []artifacts.Entry{entryDoc, entryOncal, entryStart}}
	case design:
		v.Manifest = artifacts.Manifest{Entry: "canvas.json",
			Files: []artifacts.Entry{entryCanvas, entryHero, entryMobile}}
	case pulse:
		v.Manifest = artifacts.Manifest{Entry: "dashboard.json", Files: []artifacts.Entry{entryBoard}}
	default:
		v.Manifest = artifacts.Manifest{Entry: "index.html",
			Files: []artifacts.Entry{entryChart, entryLogo, entryIndex}}
	}
	v.Entry = v.Manifest.Entry
	return v, nil
}

func (e *estate) List(_ context.Context, v artifactsdomain.Viewer, _ artifactsdomain.ListQuery) (
	artifactsdomain.ListPage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.listed = append(e.listed, v)
	return artifactsdomain.ListPage{Contents: e.listItems, Total: len(e.listItems)}, nil
}

func (e *estate) SeenBy(context.Context, artifacts.ID) ([]artifactsdomain.Read, error) {
	return []artifactsdomain.Read{{Seat: "bob"}}, nil
}

func (e *estate) versionReads() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.versions
}

// writes records every write call.
type writes struct {
	mu    sync.Mutex
	calls []call
	err   error
}

type call struct {
	verb string
	c    artifactsdomain.Call
	id   artifacts.ID
	arg  any
}

func (w *writes) record(verb string, c artifactsdomain.Call, id artifacts.ID, arg any) (
	artifactsdomain.Written, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, call{verb: verb, c: c, id: id, arg: arg})
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, w.err
}

func (w *writes) Patch(_ context.Context, c artifactsdomain.Call, id artifacts.ID, p artifactsdomain.Patch) (
	artifactsdomain.Written, error) {
	return w.record("patch", c, id, p)
}
func (w *writes) Share(_ context.Context, c artifactsdomain.Call, id artifacts.ID, s artifacts.Share) (
	artifactsdomain.Written, error) {
	return w.record("share", c, id, s)
}
func (w *writes) Unshare(_ context.Context, c artifactsdomain.Call, id artifacts.ID, a artifacts.Audience) (
	artifactsdomain.Written, error) {
	return w.record("unshare", c, id, a)
}
func (w *writes) Transfer(_ context.Context, c artifactsdomain.Call, id artifacts.ID, to string) (
	artifactsdomain.Written, error) {
	return w.record("transfer", c, id, to)
}
func (w *writes) LinkWorkItem(_ context.Context, c artifactsdomain.Call, id artifacts.ID, item string) (
	artifactsdomain.Written, error) {
	return w.record("link", c, id, item)
}
func (w *writes) Trash(_ context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error) {
	return w.record("trash", c, id, nil)
}
func (w *writes) Restore(_ context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error) {
	return w.record("restore", c, id, nil)
}
func (w *writes) Purge(_ context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error) {
	return w.record("purge", c, id, nil)
}
func (w *writes) ClearBrand(_ context.Context, c artifactsdomain.Call) (artifactsdomain.Written, error) {
	return w.record("clear_brand", c, artifacts.ID{}, nil)
}
func (w *writes) RemoveAsset(_ context.Context, c artifactsdomain.Call, id artifacts.ID, a uuid.UUID) (
	artifactsdomain.Written, error) {
	return w.record("remove_asset", c, id, a)
}
func (w *writes) Pin(_ context.Context, c artifactsdomain.Call, id artifacts.ID, pinned bool) (
	artifactsdomain.Written, error) {
	return w.record("pin", c, id, pinned)
}

func (w *writes) all() []call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]call(nil), w.calls...)
}

// workItems is the company's tracker: ENG-12 (formerly OPS-3, and by its id
// `task-12`) is live, and ENG-9 was removed. down, when set, is what asking
// for the tracker answers.
type workItems struct {
	mu    sync.Mutex
	asked []string
	fresh []statelog.Freshness
	down  error

	// log, when a case installs one ([workItems.behind]), is the tracker's
	// own read authority every read goes through first, as the tracker's
	// reader does: the framework's reader over a node whose applied
	// position the case moves, so a floor it names is waited for and
	// refused exactly as on a node, rather than as this fake would guess.
	log *statelog.Reader
}

func (t *workItems) Task(ctx context.Context, ref string, _ worklet.DetailWants, fresh statelog.Freshness) (
	worklet.TaskDetail, error) {
	t.mu.Lock()
	t.asked = append(t.asked, ref)
	t.fresh = append(t.fresh, fresh)
	log := t.log
	t.mu.Unlock()
	// THE WAIT HOLDS NO LOCK, as the reader's own holds nothing.
	if log != nil {
		if _, err := log.Read(ctx, fresh.Query(statelog.ScopeSet{}, false),
			func(*sql.Tx) error { return nil }); err != nil {
			return worklet.TaskDetail{}, err
		}
	}
	switch ref {
	case "ENG-12", "OPS-3", "task-12":
		return worklet.TaskDetail{Task: worklet.Task{ID: "task-12", Key: "ENG-12",
			FormerKeys: []string{"OPS-3"}}}, nil
	case "ENG-9":
		return worklet.TaskDetail{Task: worklet.Task{ID: "task-9", Key: "ENG-9",
			Removed: &worklet.Tombstone{By: "jane", At: time.Unix(1_700_000_000, 0)}}}, nil
	}
	return worklet.TaskDetail{}, worklet.ErrNoTask
}

func (t *workItems) tracker() (artifactsapi.WorkItems, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.down != nil {
		return nil, t.down
	}
	return t, nil
}

func (t *workItems) reads() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.asked...)
}

// floors are the freshness every read of the tracker was asked at, in order.
func (t *workItems) floors() []statelog.Freshness {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]statelog.Freshness(nil), t.fresh...)
}

// behind puts the tracker on a node that has applied its log through at, and
// answers that node's applied position for the case to move.
func (t *workItems) behind(tb testing.TB, at statelog.Position) *appliedLog {
	tb.Helper()
	node, db := storetest.OpenEstate(tb, filepath.Join(tb.TempDir(), "node.db"), store.Options{}, 1)
	tb.Cleanup(func() { _ = node.Close() })
	applied := &appliedLog{at: at, moved: make(chan struct{}), waiting: make(chan statelog.Position, 8)}
	log, err := statelogtest.LocalReaderOver(worklet.Domain{}, db, applied)
	if err != nil {
		tb.Fatal(err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.log = log
	return applied
}

// appliedLog is a node's applied position on the tracker's log, which only a
// case moves: a wait for a position past it blocks until the case advances it
// there, or the wait's own deadline passes.
type appliedLog struct {
	mu    sync.Mutex
	at    statelog.Position
	moved chan struct{}

	// waiting receives every position a wait began blocking on, so a case
	// knows a read is held rather than guessing with a sleep.
	waiting chan statelog.Position
}

func (l *appliedLog) Reached() statelog.Position {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.at
}

func (l *appliedLog) WaitReached(ctx context.Context, p statelog.Position) error {
	announced := false
	for {
		l.mu.Lock()
		at, moved := l.at, l.moved
		l.mu.Unlock()
		if p.Packed() <= at.Packed() {
			return nil
		}
		if !announced {
			announced = true
			select {
			case l.waiting <- p:
			default:
			}
		}
		select {
		case <-moved:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (l *appliedLog) WaitApplied(ctx context.Context, _ statelog.ScopeSet, p statelog.Position) error {
	return l.WaitReached(ctx, p)
}

// advance applies the log through p.
func (l *appliedLog) advance(p statelog.Position) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at = p
	close(l.moved)
	l.moved = make(chan struct{})
}

// marks records the reader records a read leaves.
type marks struct {
	mu       sync.Mutex
	elevated []artifactsdomain.Call
	versions []int
	opened   []artifactsdomain.Call
	err      error
}

func (m *marks) ReadElevated(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, version int) (
	artifactsdomain.Written, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return artifactsdomain.Written{}, m.err
	}
	m.elevated = append(m.elevated, c)
	m.versions = append(m.versions, version)
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (m *marks) Opened(_ context.Context, c artifactsdomain.Call, _ artifacts.ID) (artifactsdomain.Written, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opened = append(m.opened, c)
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

// pipeline records what the upload pipeline was asked and answers what a case
// set.
type pipeline struct {
	mu        sync.Mutex
	calls     []string
	creates   []artifactupload.CreateRequest
	publishes []artifactupload.PublishRequest
	calls2    []artifactsdomain.Call
	transfers []string
	upload    artifactupload.Upload
	checks    [][]artifactupload.CheckFile
	report    artifactupload.CheckReport
	err       error
}

func (p *pipeline) note(verb string, c artifactsdomain.Call) {
	p.calls = append(p.calls, verb)
	p.calls2 = append(p.calls2, c)
}

func (p *pipeline) Intend(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, _ artifacts.UploadRequest) (
	artifacts.UploadIntent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("intend", c)
	return artifacts.UploadIntent{Upload: "u-1"}, p.err
}

func (p *pipeline) Transfer(_ context.Context, c artifactsdomain.Call, upload, path string, body io.Reader,
	_ int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("transfer", c)
	raw, _ := io.ReadAll(body)
	p.transfers = append(p.transfers, upload+"/"+path+"="+string(raw))
	return p.err
}

func (p *pipeline) Lookup(context.Context, artifactsdomain.Call, string) (artifactupload.Upload, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.upload, nil
}

func (p *pipeline) Create(_ context.Context, c artifactsdomain.Call, req artifactupload.CreateRequest) (
	artifactsdomain.Published, artifactupload.Report, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("create", c)
	p.creates = append(p.creates, req)
	id := artifacts.IDFor("created")
	return artifactsdomain.Published{Artifact: id, Number: 1,
		Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, artifactupload.Report{}, p.err
}

func (p *pipeline) Publish(_ context.Context, c artifactsdomain.Call, req artifactupload.PublishRequest) (
	artifactsdomain.Published, artifactupload.Report, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("publish", c)
	p.publishes = append(p.publishes, req)
	return artifactsdomain.Published{Artifact: req.Artifact, Number: req.Base + 1,
		Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, artifactupload.Report{}, p.err
}

func (p *pipeline) AddAsset(_ context.Context, c artifactsdomain.Call, id artifacts.ID, _, name string) (
	artifactsdomain.Asset, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("asset", c)
	return artifactsdomain.Asset{Artifact: id, Name: name}, p.err
}

func (p *pipeline) Check(_ context.Context, c artifactsdomain.Call, _ artifacts.Kind, _ string,
	files []artifactupload.CheckFile) (artifactupload.CheckReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note("check", c)
	p.checks = append(p.checks, files)
	return p.report, p.err
}

func (p *pipeline) AdoptBrand(_ context.Context, c artifactsdomain.Call, kit artifacts.ID, version int) (
	artifactsdomain.Written, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.note(fmt.Sprintf("adopt_brand:%s:%d", kit, version), c)
	if p.err != nil {
		return artifactsdomain.Written{}, p.err
	}
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (p *pipeline) verbs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *pipeline) lastCall() artifactsdomain.Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls2[len(p.calls2)-1]
}

// objects serves the files' bytes by digest and counts every open.
type objects struct {
	mu     sync.Mutex
	opened int
}

func (o *objects) Open(_ context.Context, obj objstore.Object) (io.ReadCloser, error) {
	o.mu.Lock()
	o.opened++
	o.mu.Unlock()
	for _, body := range [][]byte{indexHTML, logoSVG, chartPNG, notesMD, deckJSON, coverHTML, nextHTML,
		docJSON, startMD, oncallMD, canvasJSON, heroHTML, mobileHTML, boardJSON} {
		if objstore.HashOf(body) == obj.Hash {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	return nil, objstore.ErrNotFound
}

func (o *objects) opens() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened
}

// framer records the frame requests it was asked.
type framer struct {
	mu       sync.Mutex
	requests []artifactserve.FrameRequest

	// minted are the frames [framer.Claims] reads back, by URL.
	minted map[string]artifacts.TicketClaims

	// refuse is what every mint answers, where a case sets it.
	refuse error
}

func (f *framer) For(r artifactserve.FrameRequest) (artifacts.Frame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.refuse != nil {
		return artifacts.Frame{}, f.refuse
	}
	return artifacts.Frame{URL: "https://" + string(r.Head.Label) + ".artifacts.example.net/t/x/index.html",
		Version: r.Version.Number, Mode: artifacts.OriginPerArtifact}, nil
}

// Claims reads a frame back: the frame the rig's claims name, at its own URL,
// and nothing else.
func (f *framer) Claims(frameURL string) (artifacts.TicketClaims, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for url, c := range f.minted {
		if url == frameURL {
			return c, nil
		}
	}
	return artifacts.TicketClaims{}, artifactserve.ErrNotAFrame
}

// chart is the company's relations: who sits in ENG, and who leads whom.
type chart struct{}

func (chart) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == "lee" && subject == "scribe", nil
}
func (chart) LeadsProject(context.Context, string, string) (bool, error)   { return false, nil }
func (chart) LeadsUnit(context.Context, string, string) (bool, error)      { return false, nil }
func (chart) LeadsContainer(context.Context, string, string) (bool, error) { return false, nil }
func (chart) LeadsAnyone(_ context.Context, actor string) (bool, error)    { return actor == "lee", nil }
func (chart) MemberOf(_ context.Context, actor, unit string) (bool, error) {
	return unit == "ENG" && (actor == "eve" || actor == "lee"), nil
}
func (chart) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, nil
}

// roster is the company's units and seats.
type roster struct{}

func (roster) Units(context.Context) ([]string, error) { return []string{"ENG", "OPS"}, nil }
func (roster) Seats(context.Context) ([]artifactsapi.RosterSeat, error) {
	return []artifactsapi.RosterSeat{{Handle: "jane"}, {Handle: "bob"}, {Handle: "eve"},
		{Handle: "lee"}, {Handle: "sam"}, {Handle: "scribe", Agent: true}}, nil
}

func limits() artifacts.Limits {
	return artifacts.Limits{CompanyBytes: artifacts.DefaultCompanyBytes, Artifacts: artifacts.DefaultArtifacts,
		DataBytes: artifacts.DefaultDataBytes, DataStoreBytes: artifacts.DefaultDataStoreBytes,
		PendingBytes: artifacts.DefaultPendingBytes}
}

// rig is the surface over the fakes, mounted.
type rig struct {
	t        *testing.T
	mux      *http.ServeMux
	svc      *artifactsapi.Service
	estate   *estate
	writes   *writes
	marks    *marks
	pipeline *pipeline
	objects  *objects
	framer   *framer
	ready    *ready
	work     *workItems
	company  config.CompanyArtifacts
}

func newRig(t *testing.T, framed bool) *rig {
	t.Helper()
	g := &rig{t: t, estate: &estate{}, writes: &writes{}, marks: &marks{}, pipeline: &pipeline{},
		objects: &objects{}, framer: &framer{minted: map[string]artifacts.TicketClaims{}}, ready: &ready{},
		work: &workItems{}}
	opts := artifactsapi.Options{
		Gate: artifactsapi.Gate{Heads: g.estate, Chart: chart{}, Limits: limits, Marks: g.marks,
			Roster: roster{}, Company: func() config.CompanyArtifacts { return g.company }},
		Reads: g.estate, Writes: g.writes, Objects: g.objects,
		Uploads:   func() (artifactsapi.Pipeline, error) { return g.pipeline, nil },
		WorkItems: g.work.tracker,
		Ready:     g.ready,
	}
	if framed {
		opts.Frames = g.framer
	}
	svc, err := artifactsapi.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	g.svc = svc
	g.mux = http.NewServeMux()
	if err := svc.Routes(g.mux); err != nil {
		t.Fatal(err)
	}
	return g
}

// person is a signed-in person bound to seat through a browser session,
// holding the artifacts app and a fresh proof of identity.
func person(seat string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.NewSHA1(uuid.Nil, []byte(seat)), Login: seat + ".person",
		Kind: iam.KindPerson, Seat: seat, Grants: append([]iam.Grant{iam.GrantArtifacts}, grants...),
		Stage: iam.StageActive, Via: iam.SessionName(uuid.NewSHA1(uuid.Nil, []byte("session "+seat)).String()),
		ReauthAt: time.Now().Add(time.Hour)}
}

// token is seat's person acting through a machine token.
func token(seat string) iam.Principal {
	p := person(seat)
	p.Via = iam.MachineTokenName(uuid.NewSHA1(uuid.Nil, []byte("token "+seat)).String())
	return p
}

// do sends one request as p and answers the recorder.
func (g *rig) do(p iam.Principal, method, path, body string, header ...string) *httptest.ResponseRecorder {
	g.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	r = r.WithContext(iam.WithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	g.mux.ServeHTTP(w, r)
	return w
}

// keyed is the Idempotency-Key header pair, freshly minted.
func keyed() []string {
	return []string{"Idempotency-Key", statelog.NewOpID(time.Now(), "")}
}

// errorOf is a refusal's code.
func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("a refusal that is not JSON (%d): %s", w.Code, w.Body.String())
	}
	code, _ := body["error"].(string)
	return code
}

// fields is a JSON answer's top-level fields.
func fields(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("an answer that is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return body
}

var errBoom = errors.New("the store blinked")
