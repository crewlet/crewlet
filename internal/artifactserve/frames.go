package artifactserve

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE FRAME: what the dashboard embeds, minted on the API origin for the
// signed-in viewer. The engine computes the URL, the origin the bridge must
// hear from, and the sandbox and allow attributes, and the dashboard sets them
// VERBATIM, so the policy has one implementation.

// ErrNotServed reports an artifact the artifact origin does not serve: a
// doc or a native sheet, which the dashboard renders itself and
// which carry no author code, or one in the trash.
var ErrNotServed = errors.New("artifactserve: this artifact is not opened in a frame")

// ErrNoDocument reports a frame whose document is not a page of the version:
// a design's frame names no artboard, or names a path the version does not
// hold as an HTML page.
var ErrNoDocument = errors.New("artifactserve: the frame names no page of the version")

// ErrNoLineage reports a frame asked for with no credential name to bind it
// to: a principal acting through no credential (a seat's own turn, the engine
// itself) never opens a frame.
var ErrNoLineage = errors.New("artifactserve: the frame names no credential its " +
	"ticket can be bound to")

// ErrCredentialEnding reports a credential that ends before a ticket could be
// worth a second: the viewer signs in again rather than being handed a frame
// that fails on its first request.
var ErrCredentialEnding = errors.New("artifactserve: the viewer's credential ends now; " +
	"sign in again")

// ErrNotAFrame reports a URL that is not a live frame this deployment minted:
// one on an origin that is not its artifact's, one naming no ticket, or one
// whose ticket is forged or has ended. Which of them is not said.
var ErrNotAFrame = errors.New("artifactserve: that is not a live frame this deployment minted")

// ticketPath opens every ticketed URL on the artifact origin
// (`/t/<ticket>/<file>`): ONE SPELLING for the frame that mints such a URL,
// the handler that routes on it and the reader of a minted frame.
const ticketPath = "/t/"

// FrameRequest is one viewer asking for one version in a frame.
type FrameRequest struct {
	// Principal is the signed-in viewer, as the API's guard resolved them.
	Principal iam.Principal

	// Lineage is the name of the credential the viewer acts through
	// (internal/api/auth's LineageOf, whose Lineages resolves it back on
	// every document request), so ending the credential ends the frame.
	Lineage string

	// Until is the instant the viewer's credential ends on its own (the
	// session's absolute deadline, a machine token's expiry), or zero for a
	// credential with none. A ticket never outlives it.
	Until time.Time

	// Head is the artifact as the API read it.
	Head artifactsdomain.Head

	// Version is the version to open, which must be one of Head's, with
	// its manifest: the document is one of its pages.
	Version artifactsdomain.Version

	// Document is the page the frame opens: empty for the entry of a kind
	// whose frame opens on it (a page), and one artboard's path for a kind
	// that frames each page on its own (a design).
	Document string

	// Granted are the capabilities the viewer is granted in this frame, a
	// subset of what the version declares, as the API decided them.
	Granted []artifacts.Capability

	// Purpose is why the frame is minted: empty for an open, or a preview
	// ([artifacts.TicketPurposePreview]), whose grant is held to
	// [artifacts.PreviewCapabilities] and whose ticket lives at most
	// [artifacts.PreviewTicketLifetime].
	Purpose artifacts.TicketPurpose
}

// Frames mints frames.
type Frames struct {
	tickets *ticket.Codec
	origins Origins
	now     func() time.Time
}

// NewFrames builds the frame minter. now is the clock; nil takes UTC wall time.
func NewFrames(tickets *ticket.Codec, origins Origins, now func() time.Time) (*Frames, error) {
	if tickets == nil {
		return nil, errors.New("artifactserve: frames need the ticket codec")
	}
	if !origins.mode.Valid() {
		return nil, errors.New("artifactserve: frames need the artifact origin")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Frames{tickets: tickets, origins: origins, now: now}, nil
}

// For mints one frame: a ticket for the viewer, the version and the grant, and
// everything the dashboard sets on the iframe.
//
// THE LIFETIME IS [artifacts.TicketLifetime] ([artifacts.PreviewTicketLifetime]
// for a preview), capped by the credential's own end, and cut to whole
// seconds (the precision a ticket carries).
//
// THE URL NAMES THE VERSION'S ENTRY under the ticket, so every relative URL in
// the page resolves under the same ticket and a page never handles it, and its
// fragment carries the bridge nonce: a fragment never reaches a server, a
// proxy log or a Referer, and the runtime reads it to say hello.
func (f *Frames) For(r FrameRequest) (artifacts.Frame, error) {
	head, v := r.Head, r.Version
	switch {
	case !head.Kind.Framed():
		return artifacts.Frame{}, fmt.Errorf("%w: a %s artifact is rendered by the dashboard",
			ErrNotServed, head.Kind)
	case head.Status != artifactsdomain.StatusLive:
		return artifacts.Frame{}, fmt.Errorf("%w: the artifact is %s", ErrNotServed, head.Status)
	case v.Artifact != head.ID || v.Number < 1 || v.Number > head.HeadNumber:
		return artifacts.Frame{}, fmt.Errorf("artifactserve: version %d is not one of "+
			"artifact %s's", v.Number, head.ID)
	case strings.TrimSpace(v.Entry) == "":
		return artifacts.Frame{}, fmt.Errorf("%w: version %d names no entry", ErrNotServed,
			v.Number)
	}
	doc, err := frameDocument(head.Kind, v, r.Document)
	if err != nil {
		return artifacts.Frame{}, err
	}
	if !r.Purpose.Valid() {
		return artifacts.Frame{}, fmt.Errorf("artifactserve: %q is not a purpose a frame is "+
			"minted for", r.Purpose)
	}
	preview := r.Purpose == artifacts.TicketPurposePreview
	for _, c := range r.Granted {
		if !c.Valid() || !slices.Contains(v.Capabilities, c) {
			return artifacts.Frame{}, fmt.Errorf("artifactserve: capability %q is not one "+
				"version %d declares", c, v.Number)
		}
		if preview && !slices.Contains(artifacts.PreviewCapabilities, c) {
			return artifacts.Frame{}, fmt.Errorf("artifactserve: capability %q is not one a "+
				"preview is granted", c)
		}
	}
	owner := iam.RecordOwner(r.Principal)
	if owner == "" || strings.TrimSpace(r.Lineage) == "" {
		return artifacts.Frame{}, fmt.Errorf("%w (record %q)", ErrNoLineage, owner)
	}
	now := f.now()
	lifetime := artifacts.TicketLifetime
	if preview {
		lifetime = artifacts.PreviewTicketLifetime
	}
	expires := now.Add(lifetime)
	if !r.Until.IsZero() && r.Until.Before(expires) {
		expires = r.Until
	}
	expires = time.Unix(expires.Unix(), 0).UTC()
	if !expires.After(now) {
		return artifacts.Frame{}, ErrCredentialEnding
	}
	nonce, err := randomToken()
	if err != nil {
		return artifacts.Frame{}, err
	}
	token, err := f.tickets.Mint(artifacts.TicketClaims{
		Artifact:     head.ID,
		Version:      v.Number,
		Principal:    owner,
		Lineage:      r.Lineage,
		Capabilities: r.Granted,
		AccessEpoch:  head.AccessEpoch,
		Document:     doc,
		Nonce:        nonce,
		Expires:      expires,
		Purpose:      r.Purpose,
	})
	if err != nil {
		return artifacts.Frame{}, fmt.Errorf("artifactserve: mint the ticket: %w", err)
	}
	origin := f.origins.Origin(head.ID)
	return artifacts.Frame{
		URL:       origin + ticketPath + token + "/" + escapePath(doc) + "#n=" + nonce,
		Origin:    origin,
		Version:   v.Number,
		ExpiresAt: expires,
		Sandbox:   strings.Join(f.origins.Sandbox(), " "),
		// EMPTY IN THIS RELEASE: no device capability is served, so the
		// frame delegates no feature at all.
		Allow:    "",
		Mode:     f.origins.Mode(),
		Nonce:    nonce,
		Document: doc,
		// The grant as the ticket carries it, never nil, so a frame granted
		// nothing says so rather than leaving its host to guess.
		Capabilities: canonicalGrant(r.Granted),
	}, nil
}

// canonicalGrant is a grant in the canonical order, each capability once, and
// an empty list rather than nil for none: the order the runtime element
// carries it in ([runtimeCapabilities]), so the frame and the page agree.
func canonicalGrant(granted []artifacts.Capability) []artifacts.Capability {
	out := make([]artifacts.Capability, 0, len(granted))
	for _, c := range artifacts.Capabilities {
		if slices.Contains(granted, c) {
			out = append(out, c)
		}
	}
	return out
}

// frameDocument is the page a frame of kind opens in version v: the entry
// where the kind's frame opens on it and none is named, else the page named,
// which must be an HTML page of the version. A design names its artboard
// always, since its entry is the canvas the dashboard draws.
func frameDocument(kind artifacts.Kind, v artifactsdomain.Version, named string) (string, error) {
	spec, _ := kind.Spec()
	doc := named
	if doc == "" {
		if !spec.OpensEntry() {
			return "", fmt.Errorf("%w: a %s frame names the page it opens", ErrNoDocument, kind)
		}
		doc = v.Entry
	}
	e, ok := manifest.EntryAt(v.Manifest, doc)
	if !ok || essenceOf(e.ContentType) != "text/html" {
		return "", fmt.Errorf("%w: version %d holds no HTML page at %q", ErrNoDocument,
			v.Number, doc)
	}
	return doc, nil
}

// Claims reads back the ticket of a live frame this deployment minted, from the
// frame's URL as [Frames.For] answered it: what a viewer's report about that
// frame is bound to (internal/api/artifactsapi's ready report). The ticket is
// verified exactly as the origin verifies it, and the URL must be on the origin
// its artifact is served from.
func (f *Frames) Claims(frameURL string) (artifacts.TicketClaims, error) {
	u, err := url.Parse(strings.TrimSpace(frameURL))
	if err != nil {
		return artifacts.TicketClaims{}, fmt.Errorf("%w: %w", ErrNotAFrame, err)
	}
	rest, ok := strings.CutPrefix(u.Path, ticketPath)
	token, _, named := strings.Cut(rest, "/")
	if !ok || !named || token == "" {
		return artifacts.TicketClaims{}, fmt.Errorf("%w: it names no ticket", ErrNotAFrame)
	}
	claims, verdict, err := f.tickets.Open(token)
	switch {
	case verdict == ticket.Expired:
		return artifacts.TicketClaims{}, fmt.Errorf("%w: its ticket has ended", ErrNotAFrame)
	case verdict != ticket.Valid:
		return artifacts.TicketClaims{}, fmt.Errorf("%w: %w", ErrNotAFrame, err)
	case !strings.EqualFold(u.Scheme+"://"+u.Host, f.origins.Origin(claims.Artifact)):
		return artifacts.TicketClaims{}, fmt.Errorf("%w: it is not on its artifact's origin",
			ErrNotAFrame)
	}
	return claims, nil
}

// randomToken is 128 random bits in unpadded base64url: a bridge nonce, or a
// script nonce, unguessable and a valid source expression either way.
func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("artifactserve: read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// escapePath escapes each segment of a published path for a URL.
func escapePath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
