package artifacts

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// ---- uploads ----------------------------------------------------------- //

// UploadEntry is one file a client intends to upload, declared before any byte
// moves so the engine can reserve its bytes against the quota first.
type UploadEntry struct {
	Path   string        `json:"path"`
	SHA256 objstore.Hash `json:"sha256"`
	Size   int64         `json:"size"`

	// ContentType is optional and only disambiguates an extension the media
	// table already knows; the served type is always the table's.
	ContentType string `json:"content_type,omitempty"`
}

// UploadRequest asks for an upload intent.
type UploadRequest struct {
	Entries []UploadEntry `json:"entries"`

	// Asset is true for an upload into the asset store rather than a
	// version.
	Asset bool `json:"asset,omitempty"`
}

// Transfer is how one declared file's bytes reach the store.
type Transfer struct {
	Path    string            `json:"path"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`

	// Direct is true for a presigned transfer straight to the bucket, false
	// for one streamed through the engine.
	Direct bool `json:"direct"`
}

// UploadIntent is the engine's answer to an [UploadRequest]: which files the
// artifact already holds (carried, no bytes move), and how each of the rest is
// transferred. The reservation behind it expires at ExpiresAt.
type UploadIntent struct {
	Upload    string     `json:"upload"`
	ExpiresAt time.Time  `json:"expires_at"`
	Carried   []string   `json:"carried,omitempty"`
	Transfers []Transfer `json:"transfers,omitempty"`
}

// ---- view tickets and frames ---------------------------------------------- //

// TicketClaims are what a view ticket says, minted on the dashboard origin for
// the signed-in principal and opened on the artifact origin, which holds no
// credential of its own.
type TicketClaims struct {
	// Artifact and Version name one immutable version: an open page never
	// changes under its reader.
	Artifact ID  `json:"a"`
	Version  int `json:"v"`

	// Principal is the minting principal's record owner (iam.RecordOwner),
	// re-decided on every document request.
	Principal string `json:"p"`

	// Lineage is the credential the ticket was minted through
	// (`session:<lineage>`, `pat:<id>` or `token:<id>`), so ending the
	// credential ends the ticket.
	Lineage string `json:"k"`

	// Capabilities are the capabilities granted to this viewer.
	Capabilities []Capability `json:"c,omitempty"`

	// AccessEpoch is the artifact's access epoch at mint; any narrowing
	// moves it.
	AccessEpoch int64 `json:"x"`

	// Document is the page the frame was minted for: the version's entry
	// for a page, one artboard for a design. A kind that frames each page
	// on its own ([DocumentsEach]) serves no other document under this
	// ticket, so the frames a viewer holds are the tickets minted for them.
	Document string `json:"d"`

	// Nonce binds the frame's bridge handshake to this ticket.
	Nonce string `json:"n"`

	// Expires is the claims' own expiry.
	Expires time.Time `json:"e"`

	// Purpose is why the frame was minted: empty for a viewer opening the
	// artifact, [TicketPurposePreview] for a gallery card drawing it small.
	// A preview carries only [PreviewCapabilities], lives at most
	// [PreviewTicketLifetime] and files no ready report.
	Purpose TicketPurpose `json:"u,omitempty"`
}

// TicketPurpose is why a frame was minted.
type TicketPurpose string

// TicketPurposePreview is a gallery card's live preview: the same read, under
// the same authorization, of a page drawn small and inert. It records no
// open, joins no room, writes nothing and reports no ready time.
const TicketPurposePreview TicketPurpose = "preview"

// Valid reports whether p is a purpose this build mints: none, or a preview.
func (p TicketPurpose) Valid() bool { return p == "" || p == TicketPurposePreview }

// PreviewCapabilities are the only capabilities a preview's frame is granted:
// what drawing the page needs, its asset store's pictures and its datastore
// READ, and nothing that names the viewer, keeps their state or shows the
// room. The datastore is answered by the dashboard's preview host, which
// answers reads (a document, a page, a query, the schema, a subscription
// once) and refuses every write, so a page draws its real content and can
// change none of it; the engine decides every read again as the reader.
var PreviewCapabilities = []Capability{CapabilityAssets, CapabilityDB}

// OriginMode is how the artifact origin isolates one artifact.
type OriginMode string

const (
	// OriginPerArtifact gives every artifact a real origin of its own (a
	// host per label), on a site that is not the dashboard's.
	OriginPerArtifact OriginMode = "per_artifact"

	// OriginShared serves every document with an opaque origin from one
	// host or port, through the policy's sandbox directive.
	OriginShared OriginMode = "shared"
)

// OriginModes are every mode this build serves.
var OriginModes = []OriginMode{OriginPerArtifact, OriginShared}

// Valid reports whether m is a mode this build serves.
func (m OriginMode) Valid() bool { return slices.Contains(OriginModes, m) }

// Frame is what the dashboard embeds: the ticketed URL, the origin it must see
// messages from, and the sandbox and allow attributes it sets VERBATIM. The
// engine computes the policy so no client re-implements it.
type Frame struct {
	URL       string     `json:"frame_url"`
	Origin    string     `json:"origin"`
	Version   int        `json:"version"`
	ExpiresAt time.Time  `json:"expires_at"`
	Sandbox   string     `json:"sandbox"`
	Allow     string     `json:"allow"`
	Mode      OriginMode `json:"mode"`
	Nonce     string     `json:"nonce"`

	// Document is the page the frame opens, a path of the version.
	Document string `json:"document"`
}
