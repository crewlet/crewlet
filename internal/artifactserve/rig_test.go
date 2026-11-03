package artifactserve_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE RIG: one artifact, one version of a handful of files in a real object
// store (the in-memory twin under the real checking reader), the estate and the
// identity directory as fakes a case moves by hand, and the real ticket codec,
// frame builder and handler between them.

const (
	dashboard = "https://crewlet.example.com"
	template  = "https://{artifact}.example-usercontent.net"
	owner     = "designer"
	viewer    = "reviewer"
)

// page is the entry: two script elements carrying the placeholder the linter
// writes, and an inline handler.
var page = `<!doctype html><html><head><title>Review</title>` +
	`<script nonce="` + artifacts.NoncePlaceholder + `" src="app.js"></script></head>` +
	`<body><button onclick="go()">Go</button>` +
	`<script nonce="` + artifacts.NoncePlaceholder + `">window.ready = true;</script></body></html>`

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// reads is the estate as the origin reads it, moved by hand.
type reads struct {
	mu       sync.Mutex
	head     artifactsdomain.Head
	versions map[int]artifactsdomain.Version
	assets   []artifactsdomain.Asset
	headErr  error
	asked    []statelog.Freshness
	headRead int

	// brand is every file an adoption serves, by hash: the brand route's
	// allowlist.
	brand    map[objstore.Hash]artifactsdomain.BrandFile
	brandErr error
}

func (r *reads) BrandFile(_ context.Context, hash objstore.Hash) (artifactsdomain.BrandFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.brandErr != nil {
		return artifactsdomain.BrandFile{}, r.brandErr
	}
	f, ok := r.brand[hash]
	if !ok {
		return artifactsdomain.BrandFile{}, artifactsdomain.ErrNotFound
	}
	return f, nil
}

func (r *reads) Artifact(_ context.Context, id artifacts.ID, fresh statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, fresh)
	r.headRead++
	if r.headErr != nil {
		return artifactsdomain.Head{}, statelog.Position{}, r.headErr
	}
	if id != r.head.ID {
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return r.head, statelog.Position{}, nil
}

func (r *reads) Version(_ context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.versions[number]
	if !ok || id != r.head.ID {
		return artifactsdomain.Version{}, artifactsdomain.ErrNotFound
	}
	return v, nil
}

func (r *reads) Assets(_ context.Context, id artifacts.ID) ([]artifactsdomain.Asset, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.head.ID {
		return nil, artifactsdomain.ErrNotFound
	}
	return r.assets, nil
}

func (r *reads) set(f func(*reads)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func (r *reads) reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.headRead
}

// creds is the identity directory's answer per credential name.
type creds struct {
	mu   sync.Mutex
	by   map[string]iam.Principal
	how  map[string]iam.Resolution
	read int
}

func (c *creds) Principal(_ context.Context, lineage string) (iam.Principal, iam.Resolution) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read++
	how, ok := c.how[lineage]
	if !ok {
		return iam.Principal{}, iam.Anonymous
	}
	return c.by[lineage], how
}

func (c *creds) set(lineage string, p iam.Principal, how iam.Resolution) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.by[lineage], c.how[lineage] = p, how
}

// chart answers membership by a table, and fails when told to.
type chart struct {
	mu      sync.Mutex
	members map[string]bool // "actor|unit"
	leads   map[string]bool // "actor|subject"
	err     error
}

func (c *chart) answer(table map[string]bool, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	return table[key], nil
}

func (c *chart) Leads(_ context.Context, actor, subject string) (bool, error) {
	return c.answer(c.leads, actor+"|"+subject)
}
func (c *chart) LeadsProject(context.Context, string, string) (bool, error)   { return false, nil }
func (c *chart) LeadsUnit(context.Context, string, string) (bool, error)      { return false, nil }
func (c *chart) LeadsContainer(context.Context, string, string) (bool, error) { return false, nil }
func (c *chart) LeadsAnyone(context.Context, string) (bool, error)            { return false, nil }
func (c *chart) MemberOf(_ context.Context, actor, unit string) (bool, error) {
	return c.answer(c.members, actor+"|"+unit)
}
func (c *chart) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, nil
}

// local is this node's copy health.
type local struct {
	mu     sync.Mutex
	serves bool
}

func (l *local) ArtifactsCopyServes(context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.serves
}

// recorder counts what the handler records.
type recorder struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func (r *recorder) Add(name string, n uint64, attrs metrics.Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[key(name, attrs)] += n
}

func (r *recorder) Observe(name string, _ time.Duration, attrs metrics.Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[key(name, attrs)]++
}

func (r *recorder) count(name string, attrs metrics.Attrs) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[key(name, attrs)]
}

func key(name string, attrs metrics.Attrs) string {
	var b strings.Builder
	b.WriteString(name)
	for _, k := range []string{"dest", "status", "reason", "outcome"} {
		if v, ok := attrs[k]; ok {
			b.WriteString("|" + k + "=" + v)
		}
	}
	return b.String()
}

type rig struct {
	t        *testing.T
	clock    *clock
	keyring  runtoken.Material
	mode     artifacts.OriginMode
	external string
	backend  *memobj.Backend
	store    *objstore.Store
	reads    *reads
	creds    *creds
	chart    *chart
	local    *local
	rec      *recorder
	judged   *judged
	embedded fstest.MapFS
	handler  *artifactserve.Handler
	frames   *artifactserve.Frames
	origins  artifactserve.Origins
	tickets  *ticket.Codec
	files    map[string][]byte
	objects  map[string]objstore.Object
	person   iam.Principal
	lineage  string
}

// newRig is a per-artifact deployment, a version of five files shared with the
// reviewer's seat as viewer, the reviewer signed in through one session.
func newRig(t *testing.T) *rig {
	return newRigWith(t, artifacts.OriginPerArtifact, template)
}

func newRigWith(t *testing.T, mode artifacts.OriginMode, external string) *rig {
	t.Helper()
	c := &clock{at: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	backend := memobj.NewAt(c.now)
	store, err := objstore.NewStoreAt(backend, c.now)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	r := &rig{
		t: t, clock: c, keyring: runtoken.OneKey("k1", "the-fleet-key"), mode: mode,
		external: external, backend: backend, store: store,
		reads:  &reads{versions: map[int]artifactsdomain.Version{}},
		creds:  &creds{by: map[string]iam.Principal{}, how: map[string]iam.Resolution{}},
		chart:  &chart{members: map[string]bool{}, leads: map[string]bool{}},
		local:  &local{serves: true},
		rec:    &recorder{counts: map[string]uint64{}},
		judged: &judged{},
		embedded: fstest.MapFS{
			"runtime/1.0.0/runtime.js": {Data: []byte("window.crewlet = {version: '1.0.0'};" +
				strings.Repeat(" ", 4096))},
			"runtime/1.2.0/runtime.js":  {Data: []byte("window.crewlet = {version: '1.2.0'};")},
			"runtime/1.10.0/runtime.js": {Data: []byte("window.crewlet = {version: '1.10.0'};")},
			"runtime/notes/runtime.js":  {Data: []byte("not a version")},
			"kit/0.18.0/kit.css":        {Data: []byte(":root{--x:1}")},
		},
		files:   map[string][]byte{},
		objects: map[string]objstore.Object{},
	}
	r.files["index.html"] = []byte(page)
	r.files["app.js"] = []byte("export const app = 1;\n" + strings.Repeat("// padding\n", 400))
	r.files["logo.png"] = bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, 2048)
	// LONGER THAN A MEBIBYTE, so a damaged copy is found after the first
	// unit has left and the response is visibly cut.
	r.files["clip.mp4"] = make([]byte, 5<<19)
	for i := range r.files["clip.mp4"] {
		r.files["clip.mp4"][i] = byte(i * 7)
	}
	r.files["big.css"] = bytes.Repeat([]byte(".row{margin:0 auto;padding:4px}\n"), (5<<20)/33)
	r.files["diagram.svg"] = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	var entries []artifacts.Entry
	for _, path := range []string{"app.js", "big.css", "clip.mp4", "diagram.svg", "index.html", "logo.png"} {
		obj := r.put(r.files[path])
		typ, _ := mediatype.For(path)
		e := artifacts.Entry{Path: path, ContentType: typ.Served, Object: obj}
		if path == "index.html" {
			e.Script = &artifacts.ScriptPolicy{Nonced: true, HandlerHashes: []string{handlerDigest}}
		}
		entries = append(entries, e)
		r.objects[path] = obj
	}
	id := artifacts.IDFor("create-op")
	r.reads.head = artifactsdomain.Head{
		ID: id, Label: id.Label(), Kind: artifacts.KindHTML, Title: "Review",
		Owner: owner, OwnerKind: iam.ActorHuman, HeadNumber: 2,
		Capabilities: []artifacts.Capability{artifacts.CapabilityAssets, artifacts.CapabilityDB},
		Status:       artifactsdomain.StatusLive, AccessEpoch: 3,
		Shares: []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: viewer},
			Role: artifacts.RoleViewer}},
	}
	r.reads.versions[2] = artifactsdomain.Version{
		VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: 2, Entry: "index.html",
			Contract:     "1.0.0",
			Capabilities: []artifacts.Capability{artifacts.CapabilityAssets, artifacts.CapabilityDB}},
		Manifest: artifacts.Manifest{Entry: "index.html", Files: entries},
	}
	asset := r.put([]byte("asset bytes"))
	r.reads.assets = []artifactsdomain.Asset{{Artifact: id,
		ID: uuid.MustParse("0192f00d-0000-7000-8000-0000000000a5"), Name: "notes.txt",
		ContentType: "text/plain; charset=utf-8", Object: asset}}
	r.lineage = "session:" + uuid.MustParse("0192f00d-0000-7000-8000-00000000c0de").String()
	r.person = iam.Principal{ID: uuid.MustParse("0192f00d-0000-7000-8000-0000000000aa"),
		Login: "rita.reviewer", Kind: iam.KindPerson, Seat: viewer, Stage: iam.StageActive,
		Via: r.lineage}
	r.creds.set(r.lineage, r.person, iam.Resolved)
	r.build()
	return r
}

// handlerDigest is the digest of the page's one inline handler, as the linter
// records it.
var handlerDigest = func() string {
	sum := sha256.Sum256([]byte("go()"))
	return base64.StdEncoding.EncodeToString(sum[:])
}()

func (r *rig) put(data []byte) objstore.Object {
	r.t.Helper()
	obj, err := r.store.Put(r.t.Context(), bytes.NewReader(data), int64(len(data))+1, objstore.PutMeta{})
	if err != nil {
		r.t.Fatalf("Put: %v", err)
	}
	return obj
}

// build (re)builds the codec, origin, frames and handler over the rig.
func (r *rig) build() {
	r.t.Helper()
	var err error
	r.tickets, err = ticket.New(r.keyring, r.clock.now)
	if err != nil {
		r.t.Fatalf("ticket.New: %v", err)
	}
	r.origins, err = artifactserve.NewOrigins(artifactserve.OriginsConfig{
		External: r.external, Mode: r.mode, Dashboard: []string{dashboard}})
	if err != nil {
		r.t.Fatalf("NewOrigins: %v", err)
	}
	r.frames, err = artifactserve.NewFrames(r.tickets, r.origins, r.clock.now)
	if err != nil {
		r.t.Fatalf("NewFrames: %v", err)
	}
	r.handler, err = artifactserve.New(artifactserve.Deps{
		Tickets: r.tickets, Origins: r.origins, Artifacts: r.reads, Objects: r.store,
		Credentials: r.creds, Chart: r.chart, Local: r.local, Metrics: r.rec,
		Objectives: r.judged, Embedded: r.embedded, Now: r.clock.now,
	})
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
}

// frame mints the reviewer's frame for version 2.
func (r *rig) frame() artifacts.Frame {
	r.t.Helper()
	return r.frameFor(r.person, []artifacts.Capability{artifacts.CapabilityAssets})
}

func (r *rig) frameFor(p iam.Principal, granted []artifacts.Capability) artifacts.Frame {
	r.t.Helper()
	f, err := r.frames.For(artifactserve.FrameRequest{
		Principal: p, Lineage: p.Via, Head: r.reads.head,
		Version: r.reads.versions[2], Granted: granted,
	})
	if err != nil {
		r.t.Fatalf("Frames.For: %v", err)
	}
	return f
}

// base is the frame's URL up to and including the ticket's trailing slash.
func base(f artifacts.Frame) string {
	u := strings.SplitN(f.URL, "#", 2)[0]
	return u[:strings.LastIndex(u, "/")+1]
}

// request is one request to the handler, as a browser in the frame sends it.
type request struct {
	url     string
	dest    string
	method  string
	headers map[string]string
}

// reply is one answer as the handler wrote it, read whole, so no response body
// outlives the request that made it.
type reply struct {
	StatusCode int
	Header     http.Header
	data       []byte
}

func (r *rig) do(req request) *reply {
	r.t.Helper()
	method := req.method
	if method == "" {
		method = http.MethodGet
	}
	hr := httptest.NewRequestWithContext(r.t.Context(), method, req.url, nil)
	if req.dest != "" {
		hr.Header.Set("Sec-Fetch-Dest", req.dest)
	}
	for k, v := range req.headers {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, hr)
	res := rec.Result()
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		r.t.Fatalf("read the body: %v", err)
	}
	return &reply{StatusCode: res.StatusCode, Header: res.Header, data: data}
}

func (r *rig) document(f artifacts.Frame) *reply {
	return r.do(request{url: strings.SplitN(f.URL, "#", 2)[0], dest: "iframe"})
}

// body is a reply's body.
func body(t *testing.T, res *reply) []byte {
	t.Helper()
	return res.data
}

// empty fails unless res is an empty refusal of status.
func empty(t *testing.T, res *reply, status int) {
	t.Helper()
	got := body(t, res)
	if res.StatusCode != status || len(got) != 0 {
		t.Fatalf("status %d with %d bytes (%q), want an empty %d", res.StatusCode, len(got),
			got, status)
	}
	if res.Header.Get("Set-Cookie") != "" {
		t.Fatalf("a refusal set a cookie: %q", res.Header.Get("Set-Cookie"))
	}
}

var errStale = &statelog.Refused{Code: statelog.RefuseTooStale, Level: statelog.ReadStale,
	Detail: "this node is about 2m0s behind and this read accepts 1m0s"}

var errUnreachable = errors.New("estate: no data node serves the estate right now")
