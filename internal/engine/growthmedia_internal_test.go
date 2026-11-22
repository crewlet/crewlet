package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mediaWorld is one artifact owned by the seat `designer`, holding a version
// file and an asset, and whatever shares a case gives it.
type mediaWorld struct {
	head    artifactsdomain.Head
	version artifactsdomain.Version
	assets  []artifactsdomain.Asset
	bytes   map[objstore.Hash][]byte
}

func (w *mediaWorld) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (artifactsdomain.Head,
	statelog.Position, error) {
	if id != w.head.ID {
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return w.head, statelog.Position{}, nil
}

func (w *mediaWorld) Version(_ context.Context, _ artifacts.ID, number int) (artifactsdomain.Version, error) {
	if number != w.head.HeadNumber {
		return artifactsdomain.Version{}, artifactsdomain.ErrNotFound
	}
	return w.version, nil
}

func (w *mediaWorld) Assets(context.Context, artifacts.ID) ([]artifactsdomain.Asset, error) {
	return w.assets, nil
}

func (w *mediaWorld) Open(_ context.Context, o objstore.Object) (io.ReadCloser, error) {
	b, ok := w.bytes[o.Hash]
	if !ok {
		return nil, errors.New("no such object")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// object stores b and answers its row.
func (w *mediaWorld) object(b []byte) objstore.Object {
	h := objstore.HashOf(b)
	w.bytes[h] = b
	return objstore.Object{Hash: h, Size: int64(len(b))}
}

// noLeads is a chart where nobody leads anybody: an artifact is read by its
// owner and its shares alone.
type noLeads struct{}

func (noLeads) Leads(context.Context, string, string) (bool, error)          { return false, nil }
func (noLeads) LeadsProject(context.Context, string, string) (bool, error)   { return false, nil }
func (noLeads) LeadsUnit(context.Context, string, string) (bool, error)      { return false, nil }
func (noLeads) LeadsContainer(context.Context, string, string) (bool, error) { return false, nil }
func (noLeads) LeadsAnyone(context.Context, string) (bool, error)            { return false, nil }
func (noLeads) MemberOf(context.Context, string, string) (bool, error)       { return false, nil }
func (noLeads) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, nil
}

func newMediaReader(t *testing.T) (mediaReader, *mediaWorld) {
	t.Helper()
	company, err := config.ParseCompanyDocument([]byte(`
name: Acme
roles:
  - name: Designer
    handle: designer
  - name: Mobile lead
    handle: mobile-lead
`))
	if err != nil {
		t.Fatal(err)
	}
	o, err := company.Organization()
	if err != nil {
		t.Fatal(err)
	}
	w := &mediaWorld{bytes: map[objstore.Hash][]byte{}}
	w.head = artifactsdomain.Head{ID: artifacts.ID(uuid.New()), Owner: "designer", OwnerKind: iam.ActorAgent,
		HeadNumber: 1, Status: artifactsdomain.StatusLive}
	w.version = artifactsdomain.Version{Manifest: artifacts.Manifest{Files: []artifacts.Entry{
		{Path: "shots/phone-1.png", ContentType: "image/png", Object: w.object([]byte("\x89PNG phone one"))},
	}}}
	w.assets = []artifactsdomain.Asset{{Name: "icon.png", ContentType: "image/png",
		Object: w.object([]byte("\x89PNG icon"))}}
	return mediaReader{artifacts: w, objects: w, chart: noLeads{}, org: o}, w
}

// shareWith shares the artifact with seat at viewer.
func (w *mediaWorld) shareWith(seat string) {
	w.head.Shares = []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: seat},
		Role: artifacts.RoleViewer}}
}

func refusedAs(err error, kind growth.RefusalKind) bool {
	var r *growth.Refusal
	return errors.As(err, &r) && r.Kind == kind
}

// AN UPLOAD READS EXACTLY THE ARTIFACT'S FILE: a version file and an asset an
// agent may read come back byte for byte with their digest, which an
// approval binds to.
func TestGrowthMediaReadsTheArtifactsFileExactly(t *testing.T) {
	t.Parallel()
	r, w := newMediaReader(t)
	w.shareWith("mobile-lead")
	for file, want := range map[string]string{"shots/phone-1.png": "\x89PNG phone one", "icon.png": "\x89PNG icon"} {
		ref := "artifact:" + w.head.ID.String() + "/" + file
		f, err := r.open(t.Context(), "mobile-lead", ref, 1<<20, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		sum := sha256.Sum256([]byte(want))
		if string(f.Data) != want || f.SHA256 != hex.EncodeToString(sum[:]) || f.ContentType != "image/png" ||
			f.Ref != ref {
			t.Errorf("%s read as %+v", file, f)
		}
	}
}

// AN AGENT UPLOADS ONLY WHAT IT MAY READ: an artifact not shared with the
// seat is refused in the words of one nothing holds, before any byte is read.
//
// Mutation: skip mayRead, and the seat uploads another agent's artifact.
func TestGrowthMediaRefusesAnArtifactTheSeatMayNotRead(t *testing.T) {
	t.Parallel()
	r, w := newMediaReader(t)
	ref := "artifact:" + w.head.ID.String() + "/shots/phone-1.png"
	_, err := r.open(t.Context(), "mobile-lead", ref, 1<<20, time.Now())
	if !refusedAs(err, growth.RefusedNotFound) {
		t.Fatalf("an artifact not shared with the seat answered %v", err)
	}
	other := "artifact:" + artifacts.ID(uuid.New()).String() + "/shots/phone-1.png"
	_, unknown := r.open(t.Context(), "mobile-lead", other, 1<<20, time.Now())
	if err.Error() != noArtifact(ref).Error() || unknown.Error() != noArtifact(other).Error() {
		t.Errorf("a hidden artifact (%v) is told apart from a missing one (%v)", err, unknown)
	}
	if _, err := r.open(t.Context(), "nobody", ref, 1<<20, time.Now()); !refusedAs(err, growth.RefusedNotFound) {
		t.Errorf("a handle that is no agent seat answered %v", err)
	}
}

// A FILE THE ARTIFACT DOES NOT HOLD, A MALFORMED REFERENCE AND A FILE OVER
// THE UPLOAD'S LIMIT ARE REFUSED, nothing read.
//
// Mutation: drop the size check, and a file over the limit is read whole.
func TestGrowthMediaRefusesWhatItCannotUpload(t *testing.T) {
	t.Parallel()
	r, w := newMediaReader(t)
	w.shareWith("mobile-lead")
	id := w.head.ID.String()
	for _, tc := range []struct {
		ref   string
		limit int64
		kind  growth.RefusalKind
	}{
		{"artifact:" + id + "/shots/missing.png", 1 << 20, growth.RefusedNotFound},
		{"artifact:" + id, 1 << 20, growth.RefusedInvalid},
		{"shots/phone-1.png", 1 << 20, growth.RefusedInvalid},
		{"artifact:" + id + "/../secret", 1 << 20, growth.RefusedInvalid},
		{"artifact:" + id + "/shots/phone-1.png", 4, growth.RefusedInvalid},
	} {
		if _, err := r.open(t.Context(), "mobile-lead", tc.ref, tc.limit, time.Now()); !refusedAs(err, tc.kind) {
			t.Errorf("%s (limit %d) answered %v, want a %s refusal", tc.ref, tc.limit, err, tc.kind)
		}
	}
}

// A TRASHED ARTIFACT IS NOT UPLOADED until its owner restores it.
func TestGrowthMediaRefusesATrashedArtifact(t *testing.T) {
	t.Parallel()
	r, w := newMediaReader(t)
	w.shareWith("mobile-lead")
	w.head.Status = artifactsdomain.StatusTrashed
	_, err := r.open(t.Context(), "mobile-lead", "artifact:"+w.head.ID.String()+"/shots/phone-1.png", 1<<20,
		time.Now())
	if !refusedAs(err, growth.RefusedConflict) {
		t.Fatalf("a trashed artifact answered %v", err)
	}
}

// AN UPLOAD IS HELD TO WHOEVER THE TURN WORKS FOR: a ceiling bound to the
// call that may not open the artifact refuses it, though the seat may; one
// that may passes; a ceiling that cannot be read refuses; and the gate's
// "outside any turn" is the seat alone.
//
// Mutation: decide with CeilingNone whatever is bound, and the stranger's
// turn uploads the artifact.
func TestGrowthMediaIsHeldToWhoeverTheTurnWorksFor(t *testing.T) {
	t.Parallel()
	r, w := newMediaReader(t)
	w.shareWith("mobile-lead")
	ref := "artifact:" + w.head.ID.String() + "/shots/phone-1.png"
	bind := func(c authz.Ceiling, err error) mediaReader {
		out := r
		out.ceiling = func(context.Context) (authz.Ceiling, bool, error) { return c, true, err }
		return out
	}
	stranger := bind(authz.Ceiling{Kind: authz.CeilingStranger}, nil)
	if _, err := stranger.open(t.Context(), "mobile-lead", ref, 1<<20, time.Now()); !refusedAs(err,
		growth.RefusedNotFound) {
		t.Fatalf("a stranger's turn answered %v", err)
	}
	colleague := bind(authz.Ceiling{Kind: authz.CeilingRequester, Principal: iam.Principal{Kind: iam.KindSeat,
		Seat: "mobile-lead", Stage: iam.StageActive, Grants: iam.Apps}}, nil)
	if _, err := colleague.open(t.Context(), "mobile-lead", ref, 1<<20, time.Now()); err != nil {
		t.Fatalf("a requester who may open the artifact answered %v", err)
	}
	unreadable := bind(authz.Ceiling{}, errors.New("directory down"))
	if _, err := unreadable.open(t.Context(), "mobile-lead", ref, 1<<20, time.Now()); !refusedAs(err,
		growth.RefusedUnavailable) {
		t.Fatalf("an unreadable ceiling answered %v", err)
	}
	gated := r
	gated.ceiling = builtin.CallCeiling
	if _, err := gated.open(builtin.OutsideAnyTurn(t.Context()), "mobile-lead", ref, 1<<20, time.Now()); err != nil {
		t.Fatalf("a call outside any turn answered %v", err)
	}
}
