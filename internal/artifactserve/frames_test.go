package artifactserve_test

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/iam"
)

// TestAFrameIsWhatTheDashboardSetsVerbatim: the URL names the entry under the
// ticket on the artifact's own host with the bridge nonce in its fragment, the
// ticket says exactly who, which version, through what and until when, and
// the attributes are the engine's.
func TestAFrameIsWhatTheDashboardSetsVerbatim(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	u, err := url.Parse(f.URL)
	if err != nil {
		t.Fatalf("the frame URL %q does not parse: %v", f.URL, err)
	}
	host := r.reads.head.ID.Label().String() + ".example-usercontent.net"
	if u.Scheme != "https" || u.Host != host || f.Origin != "https://"+host {
		t.Errorf("the frame is on %s://%s (origin %s), want the artifact's own host %s", u.Scheme,
			u.Host, f.Origin, host)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "t" || parts[2] != "index.html" {
		t.Fatalf("the frame path is %q, want /t/<ticket>/index.html", u.Path)
	}
	if u.Fragment != "n="+f.Nonce || f.Nonce == "" {
		t.Errorf("the fragment is %q, want the bridge nonce %q", u.Fragment, f.Nonce)
	}
	claims, verdict, err := r.tickets.Open(parts[1])
	if err != nil || verdict != ticket.Valid {
		t.Fatalf("the frame's ticket opened %v: %v", verdict, err)
	}
	want := artifacts.TicketClaims{
		Artifact: r.reads.head.ID, Version: 2, Principal: viewer, Lineage: r.lineage,
		Capabilities: []artifacts.Capability{artifacts.CapabilityAssets}, AccessEpoch: 3,
		Nonce: f.Nonce, Expires: r.clock.now().Add(artifacts.TicketLifetime),
	}
	if claims.Artifact != want.Artifact || claims.Version != want.Version ||
		claims.Principal != want.Principal || claims.Lineage != want.Lineage ||
		len(claims.Capabilities) != 1 || claims.Capabilities[0] != artifacts.CapabilityAssets ||
		claims.AccessEpoch != want.AccessEpoch || claims.Nonce != want.Nonce ||
		!claims.Expires.Equal(want.Expires) || !f.ExpiresAt.Equal(want.Expires) {
		t.Errorf("the ticket says %+v, want %+v", claims, want)
	}
	if f.Sandbox != "allow-scripts allow-forms allow-same-origin" || f.Allow != "" ||
		f.Mode != artifacts.OriginPerArtifact || f.Version != 2 {
		t.Errorf("frame attributes: %+v", f)
	}
	for _, token := range []string{"allow-popups", "allow-top-navigation", "allow-modals",
		"allow-pointer-lock", "allow-popups-to-escape-sandbox", "allow-downloads"} {
		if strings.Contains(f.Sandbox, token) {
			t.Errorf("the frame is handed %s", token)
		}
	}
}

// TestAFrameNeverOutlivesItsCredential: a ticket is an hour at most, the
// credential's own end where that is sooner, and no frame at all for one ending
// now.
func TestAFrameNeverOutlivesItsCredential(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	soon := r.clock.now().Add(10*time.Minute + 700*time.Millisecond)
	f, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person, Lineage: r.lineage,
		Until: soon, Head: r.reads.head, Version: r.reads.versions[2]})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if !f.ExpiresAt.Equal(r.clock.now().Add(10 * time.Minute)) {
		t.Errorf("the frame expires %s, want the credential's end cut to the second", f.ExpiresAt)
	}
	_, err = r.frames.For(artifactserve.FrameRequest{Principal: r.person, Lineage: r.lineage,
		Until: r.clock.now().Add(500 * time.Millisecond), Head: r.reads.head,
		Version: r.reads.versions[2]})
	if !errors.Is(err, artifactserve.ErrCredentialEnding) {
		t.Errorf("a credential ending within the second was handed a frame: %v", err)
	}
}

// TestAPreviewsFrameIsShortLivedAndDrawsOnly: a preview's frame carries its
// purpose, lives at most a preview's lifetime and is granted nothing past what
// drawing needs, so its page offers no datastore, viewer or room.
func TestAPreviewsFrameIsShortLivedAndDrawsOnly(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person, Lineage: r.lineage,
		Head: r.reads.head, Version: r.reads.versions[2],
		Granted: []artifacts.Capability{artifacts.CapabilityAssets},
		Purpose: artifacts.TicketPurposePreview})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if !f.ExpiresAt.Equal(r.clock.now().Add(artifacts.PreviewTicketLifetime)) {
		t.Errorf("the preview expires %s, want a preview's lifetime from now", f.ExpiresAt)
	}
	u, err := url.Parse(f.URL)
	if err != nil {
		t.Fatalf("the frame URL %q does not parse: %v", f.URL, err)
	}
	claims, verdict, err := r.tickets.Open(strings.Split(strings.TrimPrefix(u.Path, "/"), "/")[1])
	if err != nil || verdict != ticket.Valid || claims.Purpose != artifacts.TicketPurposePreview {
		t.Errorf("the preview's ticket opened %v %+v: %v", verdict, claims, err)
	}
	for _, more := range []artifacts.Capability{artifacts.CapabilityDB, artifacts.CapabilityPresence,
		artifacts.CapabilityIdentity, artifacts.CapabilityState} {
		if _, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person,
			Lineage: r.lineage, Head: r.reads.head, Version: r.reads.versions[2],
			Granted: []artifacts.Capability{more}, Purpose: artifacts.TicketPurposePreview}); err == nil {
			t.Errorf("a preview was granted %q", more)
		}
	}
	// The version declares the datastore; a preview is refused it all the same.
	_, err = r.frames.For(artifactserve.FrameRequest{Principal: r.person, Lineage: r.lineage,
		Head: r.reads.head, Version: r.reads.versions[2],
		Granted: []artifacts.Capability{artifacts.CapabilityDB}, Purpose: artifacts.TicketPurposePreview})
	if err == nil || !strings.Contains(err.Error(), "preview") {
		t.Errorf("a preview of a version declaring the datastore was answered %v", err)
	}
	if _, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person, Lineage: r.lineage,
		Head: r.reads.head, Version: r.reads.versions[2], Purpose: "embed"}); err == nil {
		t.Error("a frame was minted for a purpose this build does not know")
	}
}

// TestOnlyAnHTMLArtifactIsOpenedInAFrame, and only a version of its own, for a
// grant the version declares.
func TestOnlyAnHTMLArtifactIsOpenedInAFrame(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	v := r.reads.versions[2]
	for name, tc := range map[string]struct {
		head    func(*artifactsdomain.Head)
		version func(*artifactsdomain.Version)
		granted []artifacts.Capability
		want    error
	}{
		"a doc": {head: func(h *artifactsdomain.Head) { h.Kind = artifacts.KindDoc },
			want: artifactserve.ErrNotServed},
		"a native sheet": {head: func(h *artifactsdomain.Head) { h.Kind = artifacts.KindSheet },
			want: artifactserve.ErrNotServed},
		"a trashed page": {head: func(h *artifactsdomain.Head) { h.Status = artifactsdomain.StatusTrashed },
			want: artifactserve.ErrNotServed},
		"another artifact's version": {version: func(v *artifactsdomain.Version) {
			v.Artifact = artifacts.IDFor("other")
		}},
		"a version past the head":              {version: func(v *artifactsdomain.Version) { v.Number = 3 }},
		"a grant the version does not declare": {granted: []artifacts.Capability{artifacts.CapabilityPresence}},
		"a grant this build does not know":     {granted: []artifacts.Capability{"camera"}},
	} {
		head, version := r.reads.head, v
		if tc.head != nil {
			tc.head(&head)
		}
		if tc.version != nil {
			tc.version(&version)
		}
		_, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person, Lineage: r.lineage,
			Head: head, Version: version, Granted: tc.granted})
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: For = %v, want a refusal (%v)", name, err, tc.want)
		}
	}
	if _, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person,
		Head: r.reads.head, Version: v}); !errors.Is(err, artifactserve.ErrNoLineage) {
		t.Errorf("a frame with no credential name was minted: %v", err)
	}
	seat := iam.Principal{Kind: iam.KindSeat, Seat: "writer-agent"}
	if _, err := r.frames.For(artifactserve.FrameRequest{Principal: seat, Lineage: r.lineage,
		Head: r.reads.head, Version: v}); err != nil {
		t.Errorf("a seat with a lineage was refused: %v", err)
	}
	nested := r.reads.versions[2]
	nested.Entry = "my app/index page.html"
	page, _ := manifest.EntryAt(nested.Manifest, "index.html")
	page.Path = nested.Entry
	nested.Manifest.Files = append([]artifacts.Entry{page}, nested.Manifest.Files...)
	slices.SortFunc(nested.Manifest.Files, func(a, b artifacts.Entry) int {
		return strings.Compare(a.Path, b.Path)
	})
	f, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person, Lineage: r.lineage,
		Head: r.reads.head, Version: nested})
	if err != nil || !strings.Contains(f.URL, "/my%20app/index%20page.html#n=") {
		t.Errorf("a nested entry's frame is %q (%v)", f.URL, err)
	}
}
