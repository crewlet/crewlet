package builtin_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeArtifacts is the artifacts domain and the publish pipeline as the tools
// see them: heads, versions and object bytes in memory, every write recorded.
//
// IT HOLDS A PUBLISH TO ITS BASE exactly as the pipeline does, answering a
// moved head with [artifactsdomain.VersionMoved], so the stale answer is
// exercised against the error the tools really meet.
type fakeArtifacts struct {
	mu sync.Mutex

	heads    map[artifacts.ID]artifactsdomain.Head
	versions map[artifacts.ID]map[int]artifactsdomain.Version
	objects  map[objstore.Hash][]byte

	listed    []artifactsdomain.Viewer
	calls     []artifactsdomain.Call
	creates   []artifactupload.CreateRequest
	publishes []artifactupload.PublishRequest
	intents   []artifacts.UploadRequest
	transfers map[string][]byte
	shares    []artifacts.Share
	unshares  []artifacts.Audience
	patches   []artifactsdomain.Patch
	elevated  []int
	assets    []string

	// The folder tree, the adopted brand and the folder writes.
	folders       []artifactsdomain.Folder
	adoption      *artifactsdomain.Adoption
	folderCreates []artifactsdomain.FolderCreateInput
	moves         [][2]string
	queries       []artifactsdomain.ListQuery
	prefixes      []artifactsdomain.CommonPrefix

	// The refusals a case injects.
	publishErr  error
	elevatedErr error
	unknown     bool
}

func newFakeArtifacts() *fakeArtifacts {
	return &fakeArtifacts{
		heads:     map[artifacts.ID]artifactsdomain.Head{},
		versions:  map[artifacts.ID]map[int]artifactsdomain.Version{},
		objects:   map[objstore.Hash][]byte{},
		transfers: map[string][]byte{},
	}
}

var fakeAt = time.Unix(1_700_000_000, 0).UTC()

// seed holds an artifact owned by owner (of kind ownerKind), at version 1
// written by author, with one file per entry of files (the first is the entry
// page), shared as given.
func (f *fakeArtifacts) seed(owner string, ownerKind iam.ActorKind, author iam.Actor,
	shares []artifacts.Share, files ...[2]string) artifactsdomain.Head {

	f.mu.Lock()
	defer f.mu.Unlock()
	id := artifacts.IDFor(fmt.Sprintf("seed-%d", len(f.heads)))
	h := artifactsdomain.Head{
		ID: id, Label: id.Label(), Kind: artifacts.KindHTML, Title: "Review sheet",
		Description: "The release review.", Owner: owner, OwnerKind: ownerKind,
		HeadNumber: 1, Status: artifactsdomain.StatusLive, Shares: shares,
		CreatedBy: author, UpdatedBy: author, CreatedAt: fakeAt, UpdatedAt: fakeAt,
	}
	f.heads[id] = h
	f.versions[id] = map[int]artifactsdomain.Version{1: f.version(id, 1, author, files)}
	return h
}

// setKind makes a seeded artifact one of kind.
func (f *fakeArtifacts) setKind(id artifacts.ID, kind artifacts.Kind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.heads[id]
	h.Kind = kind
	f.heads[id] = h
}

// republish moves the head to a new version written by author, replacing the
// named files.
func (f *fakeArtifacts) republish(id artifacts.ID, author iam.Actor, files ...[2]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.heads[id]
	prev := f.versions[id][h.HeadNumber].Manifest.Files
	merged := map[string]string{}
	var order []string
	for _, e := range prev {
		merged[e.Path] = string(f.objects[e.Object.Hash])
		order = append(order, e.Path)
	}
	for _, kv := range files {
		if _, ok := merged[kv[0]]; !ok {
			order = append(order, kv[0])
		}
		merged[kv[0]] = kv[1]
	}
	var all [][2]string
	for _, p := range order {
		all = append(all, [2]string{p, merged[p]})
	}
	h.HeadNumber++
	h.UpdatedBy = author
	f.heads[id] = h
	f.versions[id][h.HeadNumber] = f.version(id, h.HeadNumber, author, all)
}

func (f *fakeArtifacts) version(id artifacts.ID, n int, author iam.Actor, files [][2]string) artifactsdomain.Version {
	m := artifacts.Manifest{}
	for i, kv := range files {
		data := []byte(kv[1])
		hash := objstore.HashOf(data)
		f.objects[hash] = data
		e, err := manifest.NewEntry(kv[0], "", objstore.Object{Key: objstore.KeyAt(fakeAt),
			Hash: hash, Size: int64(len(data))}, false)
		if err != nil {
			panic(err)
		}
		if i == 0 {
			m.Entry = e.Path
		}
		m.Files = append(m.Files, e)
	}
	slices.SortFunc(m.Files, func(a, b artifacts.Entry) int { return strings.Compare(a.Path, b.Path) })
	return artifactsdomain.Version{
		VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: n,
			Author: author, CreatedAt: fakeAt.Add(time.Duration(n) * time.Minute),
			Files: len(m.Files)},
		Manifest: m,
	}
}

func (f *fakeArtifacts) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.heads[id]
	if !ok {
		return artifactsdomain.Head{}, statelog.Position{}, fmt.Errorf("%w: %s",
			artifactsdomain.ErrNotFound, id)
	}
	return h, statelog.Position{}, nil
}

func (f *fakeArtifacts) List(_ context.Context, v artifactsdomain.Viewer,
	q artifactsdomain.ListQuery) (artifactsdomain.ListPage, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, v)
	f.queries = append(f.queries, q)
	if q.Prefix != "" {
		within := []artifactsdomain.Viewer{v}
		if q.Requester != nil {
			within = append(within, *q.Requester)
		}
		if _, err := f.resolve(strings.TrimSuffix(q.Prefix, "/"), within); err != nil {
			return artifactsdomain.ListPage{}, err
		}
	}
	page := artifactsdomain.ListPage{Prefix: q.Prefix, Delimiter: q.Delimiter, Complete: true,
		CommonPrefixes: f.prefixes}
	for _, h := range f.heads {
		page.Contents = append(page.Contents, artifactsdomain.ListItem{Head: h,
			FolderPath: f.pathOf(h.Folder)})
	}
	slices.SortFunc(page.Contents, func(a, b artifactsdomain.ListItem) int {
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	return page, nil
}

func (f *fakeArtifacts) Version(_ context.Context, id artifacts.ID, n int) (artifactsdomain.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[id][n]
	if !ok {
		return artifactsdomain.Version{}, fmt.Errorf("%w: %s@%d", artifactsdomain.ErrNotFound, id, n)
	}
	return v, nil
}

func (f *fakeArtifacts) Versions(_ context.Context, id artifacts.ID, _ string, _ int) (
	artifactsdomain.VersionPage, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	page := artifactsdomain.VersionPage{}
	for n := f.heads[id].HeadNumber; n >= 1; n-- {
		if v, ok := f.versions[id][n]; ok {
			page.Versions = append(page.Versions, v.VersionSummary)
		}
	}
	return page, nil
}

func (f *fakeArtifacts) result() statelog.Result {
	if f.unknown {
		return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op-unknown"}
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied}
}

func (f *fakeArtifacts) Patch(_ context.Context, c artifactsdomain.Call, _ artifacts.ID,
	p artifactsdomain.Patch) (artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.patches = append(f.patches, p)
	return artifactsdomain.Written{Result: f.result()}, nil
}

func (f *fakeArtifacts) Share(_ context.Context, c artifactsdomain.Call, _ artifacts.ID,
	s artifacts.Share) (artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.shares = append(f.shares, s)
	return artifactsdomain.Written{Result: f.result()}, nil
}

func (f *fakeArtifacts) Unshare(_ context.Context, c artifactsdomain.Call, _ artifacts.ID,
	a artifacts.Audience) (artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.unshares = append(f.unshares, a)
	return artifactsdomain.Written{Result: f.result()}, nil
}

func (f *fakeArtifacts) ReadElevated(_ context.Context, c artifactsdomain.Call, _ artifacts.ID,
	version int) (artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.elevatedErr != nil {
		return artifactsdomain.Written{}, f.elevatedErr
	}
	f.calls = append(f.calls, c)
	f.elevated = append(f.elevated, version)
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (f *fakeArtifacts) Intend(_ context.Context, c artifactsdomain.Call, id artifacts.ID,
	req artifacts.UploadRequest) (artifacts.UploadIntent, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.intents = append(f.intents, req)
	held := map[objstore.Hash]bool{}
	if !id.IsZero() && !req.Asset {
		for _, v := range f.versions[id] {
			for _, e := range v.Manifest.Files {
				held[e.Object.Hash] = true
			}
		}
	}
	out := artifacts.UploadIntent{Upload: fmt.Sprintf("upload-%d", len(f.intents))}
	for _, e := range req.Entries {
		if held[e.SHA256] {
			out.Carried = append(out.Carried, e.Path)
			continue
		}
		out.Transfers = append(out.Transfers, artifacts.Transfer{Path: e.Path, Method: "PUT",
			URL: artifactupload.TransferPath(out.Upload, e.Path)})
	}
	return out, nil
}

func (f *fakeArtifacts) Transfer(_ context.Context, _ artifactsdomain.Call, upload, path string,
	body io.Reader, size int64) error {

	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return &artifactupload.UploadMismatch{Path: path, Reason: "length"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transfers[upload+"/"+path] = data
	f.objects[objstore.HashOf(data)] = data
	return nil
}

func (f *fakeArtifacts) Create(_ context.Context, c artifactsdomain.Call,
	req artifactupload.CreateRequest) (artifactsdomain.Published, artifactupload.Report, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.creates = append(f.creates, req)
	if f.publishErr != nil {
		return artifactsdomain.Published{}, artifactupload.Report{}, f.publishErr
	}
	id := artifacts.IDFor(c.OpKey + "/" + req.Title)
	return artifactsdomain.Published{Artifact: id, Number: 1, VersionID: uuid.New(),
		Result: f.result()}, artifactupload.Report{}, nil
}

func (f *fakeArtifacts) Publish(_ context.Context, c artifactsdomain.Call,
	req artifactupload.PublishRequest) (artifactsdomain.Published, artifactupload.Report, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.publishes = append(f.publishes, req)
	if f.publishErr != nil {
		return artifactsdomain.Published{}, artifactupload.Report{}, f.publishErr
	}
	h := f.heads[req.Artifact]
	if req.Base != h.HeadNumber {
		return artifactsdomain.Published{}, artifactupload.Report{},
			&artifactsdomain.VersionMoved{Head: h, Live: f.versions[h.ID][h.HeadNumber]}
	}
	return artifactsdomain.Published{Artifact: h.ID, Number: h.HeadNumber + 1,
		VersionID: uuid.New(), Result: f.result()}, artifactupload.Report{}, nil
}

func (f *fakeArtifacts) AddAsset(_ context.Context, c artifactsdomain.Call, id artifacts.ID,
	upload, name string) (artifactsdomain.Asset, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.assets = append(f.assets, upload+"/"+name)
	data := f.transfers[upload+"/"+name]
	return artifactsdomain.Asset{Artifact: id, ID: uuid.NewSHA1(uuid.Nil, []byte(name)),
		Name: name, ContentType: "image/png",
		Object: objstore.Object{Hash: objstore.HashOf(data), Size: int64(len(data))}}, nil
}

func (f *fakeArtifacts) SourceText(_ context.Context, e artifacts.Entry, limit int64) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[e.Object.Hash]
	if !ok {
		return nil, false, objstore.ErrNotFound
	}
	if int64(len(data)) <= limit {
		return bytes.Clone(data), true, nil
	}
	return bytes.Clone(data[:limit]), false, nil
}

// lastCall is the newest caller context a write was stamped with.
func (f *fakeArtifacts) lastCall() artifactsdomain.Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return artifactsdomain.Call{}
	}
	return f.calls[len(f.calls)-1]
}

// artifactChart answers the relations a share asks: who leads whom, and who
// sits in which unit.
type artifactChart struct {
	leads   map[string][]string
	members map[string][]string
	err     error
}

func (c artifactChart) Leads(_ context.Context, actor, subject string) (bool, error) {
	return slices.Contains(c.leads[actor], subject), c.err
}
func (c artifactChart) LeadsProject(context.Context, string, string) (bool, error) {
	return false, c.err
}
func (c artifactChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, c.err
}
func (c artifactChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, c.err
}
func (c artifactChart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	return len(c.leads[actor]) > 0, c.err
}
func (c artifactChart) MemberOf(_ context.Context, actor, unit string) (bool, error) {
	return slices.Contains(c.members[actor], unit), c.err
}

// SeatGranted answers that no role carries a grant: no artifact verb names one.
func (c artifactChart) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, c.err
}

var _ authz.Chart = artifactChart{}

// requesters composes the principals a turn may be acting for.
type requesters map[string]iam.Principal

func (r requesters) Requester(_ context.Context, handle string) (iam.Principal, error) {
	p, ok := r[handle]
	if !ok {
		return iam.Principal{}, fmt.Errorf("no seat %q", handle)
	}
	return p, nil
}

// founderPrincipal is the person who holds the Nimbus founder's seat.
func founderPrincipal(grants ...iam.Grant) iam.Principal {
	return iam.Principal{Kind: iam.KindPerson, Login: "founder@example.com", Seat: "founder",
		Stage: iam.StageActive, Grants: grants}
}

// fakeBox is a coding run's box: files by workspace path.
type fakeBox struct {
	files  map[string]string
	listed []string // overrides the listing, for a box that lies
}

func (b *fakeBox) Files(_ context.Context, dir string, max int) ([]string, error) {
	if b.listed != nil {
		return b.listed, nil
	}
	var out []string
	for p := range b.files {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (b *fakeBox) Open(_ context.Context, p string) (io.ReadCloser, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("no file %q", p)
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

// boxes hands every turn the one box, or none.
type boxes struct{ box *fakeBox }

func (b boxes) Box(context.Context, *turnctx.Turn) (builtin.ArtifactBox, error) {
	if b.box == nil {
		return nil, builtin.ErrNoBox
	}
	return b.box, nil
}

// artifactDeps wires the artifacts tools over fakes.
func artifactDeps(f *fakeArtifacts, chart artifactChart, req requesters) builtin.ArtifactDeps {
	return builtin.ArtifactDeps{
		Reader: f, Writer: f, Publisher: f, Chart: chart, Requesters: req,
		Limits: func() artifacts.Limits { return artifacts.Limits{CompanyBytes: artifacts.DefaultCompanyBytes} },
		Link: func(id artifacts.ID) string {
			return "https://crewlet.example.com/dashboard#/artifacts/" + id.String()
		},
		Now:         func() time.Time { return fakeAt },
		KitVersion:  func() string { return "0.47.0" },
		FolderTurns: builtin.NewFolderTurns(func() time.Time { return fakeAt }),
	}
}
