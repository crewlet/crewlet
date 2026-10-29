package artifactupload_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/artifactupload/vendor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WHOLE PATH: an upload is reserved in the fleet's coordination twin, its
// bytes stored in the object store's twin, its version decided by the real
// artifacts domain over an embedded broker, with nothing between them mocked
// but the CDN. Every property a case asserts is a claim about the pieces
// agreeing.

// testKit is the one kit release a harness serves.
const testKit = "0.20.0"

// start is every harness's first instant.
var start = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// clock is a harness's clock, shared by the store's key minting, the
// pipeline's reservations and the domain's freshness rule.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// meter records what the pipeline measured.
type meter struct {
	mu     sync.Mutex
	counts map[string]uint64
	gauges map[string]float64
}

func (m *meter) Add(name string, n uint64, attrs metrics.Attrs) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[name+attrString(attrs)] += n
}

func (m *meter) Observe(name string, _ time.Duration, attrs metrics.Attrs) {
	m.Add(name, 1, attrs)
}

func (m *meter) Set(name string, v float64, _ metrics.Attrs) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = v
}

func (m *meter) count(name string, attrs metrics.Attrs) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[name+attrString(attrs)]
}

func attrString(attrs metrics.Attrs) string {
	var b strings.Builder
	for _, k := range []string{"outcome", "reason", "transport"} {
		if v, ok := attrs[k]; ok {
			b.WriteString("|" + k + "=" + v)
		}
	}
	return b.String()
}

// harness is one node: the pipeline over its stores and a domain of its own.
type harness struct {
	t     *testing.T
	clock *clock
	// backend is what the store writes through, which [harness.stored]
	// lists.
	backend objstore.Backend
	objects *objstore.Store
	fleet   *memory.Fleet
	domain  *domainNode
	cdn     *fakeCDN
	meter   *meter
	judged  *judged
	svc     *artifactupload.Service

	// fillable is the backend a fillable harness fills, nil otherwise.
	fillable *fillableBackend
	// bucket is the in-process S3 bucket an s3 harness's store writes to,
	// which [harness.fill] fills; nil otherwise.
	bucket *s3fake.Server

	mu     sync.Mutex
	limits artifacts.Limits

	// adopted is the company's adopted brand the domain answers, nil for
	// none: what a brand copy and a deck begun in the brand read.
	adopted *artifactsdomain.Adoption
}

// brandReads is the domain's reader answering the case's adopted brand: an
// adoption is the brand change's own record, so a case about what the
// pipeline copies from one states it rather than writing it.
type brandReads struct {
	*artifactsdomain.Reader
	h *harness
}

func (r brandReads) Brand(context.Context, statelog.Freshness) (artifactsdomain.Adoption, bool, error) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if r.h.adopted == nil {
		return artifactsdomain.Adoption{}, false, nil
	}
	return *r.h.adopted, true, nil
}

// options shape a harness.
type options struct {
	// presign makes the object store's backend one that signs direct puts.
	presign bool

	// noVendor turns the copying of CDN files off.
	noVendor bool

	// fillable makes the object store's backend one a case fills
	// ([harness.fill]).
	fillable bool

	// s3 makes the object store S3, through the production backend over an
	// in-process bucket a case fills ([harness.fill]) as a provider with no
	// room refuses writes.
	s3 bool
}

func newHarness(t *testing.T, o options) *harness {
	t.Helper()
	h := &harness{t: t, clock: &clock{t: start}, fleet: memory.NewFleet(),
		meter:  &meter{counts: map[string]uint64{}, gauges: map[string]float64{}},
		judged: &judged{}, limits: defaultLimits()}
	mem := memobj.NewAt(h.clock.Now)
	h.backend = mem
	switch {
	case o.presign:
		h.backend = &signingBackend{Backend: mem}
	case o.fillable:
		h.fillable = &fillableBackend{Backend: mem}
		h.backend = h.fillable
	case o.s3:
		h.bucket = s3fake.Start(t, "files")
		b, err := s3obj.Open(t.Context(), s3obj.Config{
			Endpoint: h.bucket.URL, Region: "us-east-1", Bucket: "files", PathStyle: true,
			AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
		})
		if err != nil {
			t.Fatalf("opening the S3 fake: %v", err)
		}
		h.backend = b
	}
	store, err := objstore.NewStoreAt(h.backend, h.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	h.objects = store
	h.domain = newDomainNode(t, h.clock)
	h.cdn = newFakeCDN(t)
	runtimes, err := artifactupload.ReleasesIn(fstest.MapFS{"1.0.0/runtime.js": {Data: []byte("//")}})
	if err != nil {
		t.Fatal(err)
	}
	// The harness's kit release ships the guide's three files and not the
	// content stylesheet, as the releases before it did not.
	kits, err := artifactupload.ReleasesIn(fstest.MapFS{
		testKit + "/kit.css":  {Data: []byte(":root{}")},
		testKit + "/theme.js": {Data: []byte("//")},
		testKit + "/kit.js":   {Data: []byte("//")},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := artifactupload.Deps{
		Objects: store, Reservations: h.fleet, Writes: h.domain.writer,
		Reads: brandReads{Reader: h.domain.reader, h: h},
		Limits: func() artifacts.Limits {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.limits
		},
		Hosts: lint.DefaultAllowlist(), Runtimes: runtimes, Kits: kits, Meter: h.meter, Objectives: h.judged,
		Now: h.clock.Now,
	}
	if o.presign {
		deps.Presign = store
	}
	if !o.noVendor {
		cache, err := vendor.New(vendor.Options{Hosts: deps.Hosts.Names(),
			Client: &http.Client{Transport: httpxtest.Rewrite(t, h.cdn.srv)}, Now: h.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		deps.Vendor = cache
	}
	if h.svc, err = artifactupload.New(deps); err != nil {
		t.Fatal(err)
	}
	return h
}

func defaultLimits() artifacts.Limits {
	return artifacts.Limits{
		CompanyBytes: artifacts.DefaultCompanyBytes, Artifacts: artifacts.DefaultArtifacts,
		DataBytes: artifacts.DefaultDataBytes, DataStoreBytes: artifacts.DefaultDataStoreBytes,
		PendingBytes: artifacts.DefaultPendingBytes,
	}
}

// fill makes a fillable or an s3 harness's bucket full, or makes room in it
// again: an s3 bucket refuses every write `507 XMinioStorageFull`, as MinIO
// does at its drives' free-space floor.
func (h *harness) fill(full bool) {
	h.t.Helper()
	switch {
	case h.fillable != nil:
		h.fillable.full.Store(full)
	case h.bucket != nil && full:
		h.bucket.Fill(http.StatusInsufficientStorage, "XMinioStorageFull")
	case h.bucket != nil:
		h.bucket.Fill(0, "")
	default:
		h.t.Fatal("fill on a harness whose store is not fillable")
	}
}

// setLimits changes the quotas the pipeline and its calls are held to.
func (h *harness) setLimits(change func(*artifacts.Limits)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(&h.limits)
}

// client is a call by a person through the API with a bearer credential: the
// CLI, a script. The one caller a presigned put is handed to.
func (h *harness) client(handle, key string) artifactsdomain.Call {
	c := h.person(handle, key)
	c.Authority.Origin = artifacts.OriginAPI
	return c
}

// person is a call by a person through the dashboard.
func (h *harness) person(handle, key string) artifactsdomain.Call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return artifactsdomain.Call{OpKey: key, Authority: artifacts.Authority{
		Actor: iam.Actor{Name: handle, Kind: iam.ActorHuman, OperatorID: "session:" + handle},
		Role:  artifacts.RoleOwner, Limits: h.limits, Origin: artifacts.OriginDashboard,
	}}
}

// agent is a call by an agent seat's tool in a turn.
func (h *harness) agent(handle, turn, key string) artifactsdomain.Call {
	c := h.person(handle, key)
	c.Authority.Actor = iam.Actor{Name: handle, Kind: iam.ActorAgent, OperatorID: handle}
	c.Authority.Origin = artifacts.OriginTool
	c.TurnID = turn
	return c
}

// opKey is an operation key minted at the harness's clock, as a surface
// mints one.
func (h *harness) opKey(verb string) string { return statelog.NewOpID(h.clock.Now(), verb) }

// declare is an upload request for files.
func declare(files map[string]string) artifacts.UploadRequest {
	var req artifacts.UploadRequest
	for p, body := range files {
		req.Entries = append(req.Entries, artifacts.UploadEntry{Path: p,
			SHA256: objstore.HashOf([]byte(body)), Size: int64(len(body))})
	}
	return req
}

// upload reserves and transfers files through the engine, answering the
// upload's id.
func (h *harness) upload(c artifactsdomain.Call, id artifacts.ID, files map[string]string) string {
	h.t.Helper()
	intent, err := h.svc.Intend(h.t.Context(), c, id, declare(files))
	if err != nil {
		h.t.Fatalf("Intend: %v", err)
	}
	for _, tr := range intent.Transfers {
		if tr.Direct {
			h.t.Fatalf("a harness without a presigner was handed a direct transfer for %s", tr.Path)
		}
		body := files[tr.Path]
		if err := h.svc.Transfer(h.t.Context(), c, intent.Upload, tr.Path, strings.NewReader(body),
			int64(len(body))); err != nil {
			h.t.Fatalf("Transfer %s: %v", tr.Path, err)
		}
	}
	return intent.Upload
}

// specs names every file of an upload.
func specs(files map[string]string) []artifactupload.FileSpec {
	var out []artifactupload.FileSpec
	for p := range files {
		out = append(out, artifactupload.FileSpec{Path: p, Upload: true})
	}
	return out
}

// create makes an HTML artifact from files, entry index.html.
func (h *harness) create(c artifactsdomain.Call, files map[string]string) artifactsdomain.Published {
	h.t.Helper()
	up := h.upload(c, artifacts.ID{}, files)
	got, _, err := h.svc.Create(h.t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindHTML, Title: "A page", Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(files), Entry: "index.html",
	})
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return got
}

// version reads one version back.
func (h *harness) version(id artifacts.ID, n int) artifactsdomain.Version {
	h.t.Helper()
	v, err := h.domain.reader.Version(h.t.Context(), id, n)
	if err != nil {
		h.t.Fatalf("Version(%s, %d): %v", id, n, err)
	}
	return v
}

// file reads one file of a version back, verified.
func (h *harness) file(v artifactsdomain.Version, p string) string {
	h.t.Helper()
	for _, e := range v.Manifest.Files {
		if e.Path != p {
			continue
		}
		rc, err := h.objects.Open(h.t.Context(), e.Object)
		if err != nil {
			h.t.Fatalf("open %s: %v", p, err)
		}
		defer func() { _ = rc.Close() }()
		var b bytes.Buffer
		if _, err := b.ReadFrom(rc); err != nil {
			h.t.Fatalf("read %s: %v", p, err)
		}
		return b.String()
	}
	h.t.Fatalf("version %d holds no %s", v.Number, p)
	return ""
}

// stored is every object the backend holds, by key, with its size.
func (h *harness) stored() map[objstore.Key]int64 {
	h.t.Helper()
	out := map[objstore.Key]int64{}
	if err := h.backend.List(h.t.Context(), func(info objstore.Info) error {
		k, err := objstore.KeyOfName(info.Name)
		if err != nil {
			return err
		}
		out[k] = info.Size
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// named is every key a row of the artifacts estate names.
func (h *harness) named() map[objstore.Key]bool {
	h.t.Helper()
	out := map[objstore.Key]bool{}
	if err := h.domain.db.Read(h.t.Context(), func(tx *sql.Tx) error {
		for _, q := range []string{`SELECT object FROM artifact_objects`,
			`SELECT object FROM artifact_assets WHERE object IS NOT NULL`,
			`SELECT object FROM artifact_brand_files`} {
			rows, err := tx.QueryContext(h.t.Context(), q)
			if err != nil {
				return err
			}
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					_ = rows.Close()
					return err
				}
				k, err := objstore.ParseKey(raw)
				if err != nil {
					_ = rows.Close()
					return err
				}
				out[k] = true
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// unnamedBytes is every stored byte no row names.
func (h *harness) unnamedBytes() int64 {
	named := h.named()
	var n int64
	for k, size := range h.stored() {
		if !named[k] {
			n += size
		}
	}
	return n
}

// held is every byte the reservation ledger holds now.
func (h *harness) held() int64 {
	h.t.Helper()
	n, err := artifactupload.HeldBytes(h.t.Context(), h.svc)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// ---- the domain --------------------------------------------------------- //

// domainNode is the artifacts domain over an embedded broker: the shipped
// writer, applier and reader, applying every record before a write returns.
type domainNode struct {
	t       *testing.T
	db      store.ReplicatedHandle
	log     *js.DomainLog
	writer  *artifactsdomain.Writer
	reader  *artifactsdomain.Reader
	applier *artifactsdomain.Applier
	waiter  *waiter
	verify  *statelog.Verifier

	mu       sync.Mutex
	consumed uint64
}

func newDomainNode(t *testing.T, clock *clock) *domainNode {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	d := artifactsdomain.Domain{}
	spec := d.Stream()
	// A SMALL CEILING: the broker reserves a stream's whole ceiling on disk
	// when it creates it, and a case writes a few records; the gate reserve
	// above the soft ceiling is a few of the log's largest appends, far
	// inside it.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: config.ArtifactsLogMaxBytesFloor / 32,
		MaxMessageBytes: spec.MaxAppendBytes(), Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = dbNode.Close() })
	rows, err := artifactsdomain.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fence := artifactsdomain.NewFence(db.Reader(), "node-a")
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	w := &waiter{}
	fence.Committed = w.Committed
	reserve, err := statelog.NewReserve(spec, func(ctx context.Context) (statelog.Usage, error) {
		stats, err := log.Stats(ctx)
		return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := statelog.NewSigner(d.Name(), statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := statelog.NewVerifier(d.Name(), statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: d, Spec: spec, Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Rows:    rows, Fence: fence, Signer: signer,
		Gates: artifactsdomain.NewGates(db.Reader()), Waiter: w, Voids: w,
		Identity: w, NodeID: "node-a", Admission: reserve,
		Generation: func() uint32 { return 0 }, ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := artifactsdomain.NewWriter(artifactsdomain.WriterDeps{
		Publisher: publisher, DB: db.Reader(), Now: clock.Now,
		SeatKind: func(context.Context, string) (iam.ActorKind, error) { return iam.ActorHuman, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := statelogtest.LocalReaderOver(d, db, w)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := artifactsdomain.NewReader(artifactsdomain.ReaderOptions{DB: db.Reader(), Log: authority})
	if err != nil {
		t.Fatal(err)
	}
	n := &domainNode{t: t, db: db, log: log, writer: writer, reader: reader, waiter: w, verify: verifier,
		applier: artifactsdomain.NewApplier("node-a", func([]artifactsdomain.Moved) {})}
	w.advance = n.drain
	return n
}

// drain applies every record the broker holds beyond what this node applied,
// as the framework's loop does.
func (n *domainNode) drain() {
	n.mu.Lock()
	defer n.mu.Unlock()
	ctx := context.Background()
	last, err := n.log.End(ctx)
	if err != nil {
		n.t.Errorf("read the log's end: %v", err)
		return
	}
	spec := artifactsdomain.Domain{}.Stream()
	for seq := n.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := n.log.At(ctx, seq)
		if err != nil || !ok {
			if err != nil {
				n.t.Errorf("read record %d: %v", seq, err)
				return
			}
			continue
		}
		body, verdict := n.verify.Open(payload)
		if verdict != statelog.Verified {
			n.t.Errorf("record %d did not verify: %s", seq, verdict)
			return
		}
		envelope, err := artifactsdomain.Domain{}.Envelope(body)
		if err != nil {
			n.t.Errorf("decode record %d: %v", seq, err)
			return
		}
		record := statelog.Record{Envelope: envelope,
			Position: statelog.Position{Stream: spec.Name, Generation: envelope.Gen, Seq: seq},
			Payload:  body, StoredAt: storedAt}
		wire := spec.SubjectPrefix + "." + envelope.Subject.String()
		if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
			_, gated, err := n.applier.Gated(ctx, tx, record)
			if err != nil {
				return err
			}
			if !gated {
				if _, err := n.applier.Apply(ctx, tx, record, statelog.ApplyOptions{
					Now: storedAt, StoredAt: storedAt, MaxVariables: 999,
				}); err != nil {
					return err
				}
				if envelope.OpID != "" {
					if _, err := tx.ExecContext(ctx, `
						INSERT INTO artifacts_ops (op_id, subject, position, applied_at)
						VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
						envelope.OpID, wire, record.Position.Packed(), store.EncodeTime(storedAt)); err != nil {
						return err
					}
				}
			}
			if envelope.Kind != statelog.BarrierKind {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?,?,?)
					ON CONFLICT (stream, subject) DO UPDATE SET anchor = MAX(anchor, excluded.anchor)`,
					record.Position.Stream, wire, record.Position.Packed()); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO statelog_cursor
					(stream, generation, seq, stream_created_at, updated_at, applied_version)
				VALUES (?,?,?,0,0,0)
				ON CONFLICT (stream) DO UPDATE SET generation = excluded.generation, seq = excluded.seq`,
				record.Position.Stream, int64(record.Position.Generation), int64(seq))
			return err
		}); err != nil {
			n.t.Errorf("apply record %d: %v", seq, err)
			return
		}
		n.applier.Committed(ctx)
		n.consumed = seq
		n.waiter.reach(record.Position)
	}
}

// waiter is the node's applier as the publisher and the reader see it: it
// applies while a write or a read waits, as a running node's does.
type waiter struct {
	mu      sync.Mutex
	at      statelog.Position
	advance func()
}

func (w *waiter) reach(p statelog.Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Packed() > w.at.Packed() {
		w.at = p
	}
}

func (w *waiter) Committed() statelog.Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

// Reached is Committed: this rig moves its position when it applies a record,
// in one step, so it reaches nothing its commit has not.
func (w *waiter) Reached() statelog.Position { return w.Committed() }

func (w *waiter) StreamIdentity() error                         { return nil }
func (w *waiter) Truncated() error                              { return nil }
func (w *waiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

func (w *waiter) WaitReached(ctx context.Context, p statelog.Position) error {
	for {
		if w.Committed().Packed() >= p.Packed() {
			return nil
		}
		w.mu.Lock()
		advance := w.advance
		w.mu.Unlock()
		advance()
		if w.Committed().Packed() >= p.Packed() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (w *waiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet, p statelog.Position) error {
	return w.WaitReached(ctx, p)
}

// ---- the CDN and the bucket ---------------------------------------------- //

// fakeCDN answers every path as JavaScript but for the files a case sets.
type fakeCDN struct {
	srv *httptest.Server

	mu    sync.Mutex
	files map[string]cdnFile
	hits  int
}

type cdnFile struct {
	contentType string
	body        string
}

func newFakeCDN(t *testing.T) *fakeCDN {
	c := &fakeCDN{files: map[string]cdnFile{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.hits++
		f, ok := c.files[r.URL.Path]
		c.mu.Unlock()
		if !ok {
			f = cdnFile{contentType: "application/javascript", body: "/* " + r.URL.Path + " */"}
		}
		w.Header().Set("Content-Type", f.contentType)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCDN) set(path, contentType, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[path] = cdnFile{contentType: contentType, body: body}
}

// fillableBackend is the object store's twin as the fleet's own bucket, which
// a case fills: once full, every put reads its content and is refused as a
// backend refuses one for room ([objstore.Full]), storing nothing and naming
// [caseRemedy].
type fillableBackend struct {
	*memobj.Backend
	full atomic.Bool
}

// caseRemedy is the room a full [fillableBackend] names: a case's own, so a
// sentence naming it spliced the store's remedy rather than a spelling of the
// pipeline's own.
const caseRemedy = "an operator makes room in this case's own bucket"

func (f *fillableBackend) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	if !f.full.Load() {
		return f.Backend.Put(ctx, name, r, m)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return &objstore.Full{Remedy: caseRemedy, Err: fmt.Errorf(
		"natsobj: put %s: OBJ_crewlet_files holds as many bytes as it reserves", name)}
}

// signingBackend is the object store's twin as a bucket whose provider proved
// direct puts: it signs a put, and a test plays the client making it.
type signingBackend struct {
	*memobj.Backend
}

func (s *signingBackend) DirectUpload() (bool, string) { return true, "" }

func (s *signingBackend) PresignPut(_ context.Context, name string, r objstore.PresignRequest) (objstore.Presigned, error) {
	return objstore.Presigned{Method: http.MethodPut, URL: "https://bucket.example/" + name,
		Headers: map[string]string{"Content-Type": r.ContentType,
			"x-amz-checksum-sha256": string(r.SHA256), "If-None-Match": "*"}}, nil
}
