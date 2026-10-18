package artifacts

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
)

// ---- identity ------------------------------------------------------------ //

// ID names one artifact for the life of the deployment.
//
// DERIVED, NEVER RANDOM: an artifact's id is a function of the operation that
// created it ([IDNamespace]), so the retry an unknown outcome asks for names
// the artifact its first attempt created rather than one nothing will ever
// find. The derivation and the text codec are the domain's
// (internal/artifactsdomain adds them in id.go).
type ID uuid.UUID

// IDNamespace scopes the derivation of an artifact's id from its creating
// operation. FIXED for the life of the format: a new namespace would give a
// retry that straddles an upgrade a second artifact.
var IDNamespace = uuid.MustParse("d7a1cd11-0607-42f6-82b4-dada4cbe981e")

// Label is an artifact's DNS host label: the sixteen bytes of its [ID] in
// lowercase Crockford base32, unpadded, exactly [LabelLen] characters.
//
// THE ID ITSELF, NOT A KEYED DIGEST OF IT. A label derived under a keyring key
// changes when the keyring rotates, and the label is the artifact's origin, so
// a rotation would wipe every viewer's per-artifact browser storage unless the
// key were pinned for ever. The label is not a secret; the view ticket is.
//
// Crockford's alphabet because a DNS label is case-insensitive and is read
// aloud and retyped: it has no I, L, O or U to confuse with 1, 0 or V.
type Label string

// LabelLen is the length of every [Label]: 128 bits at 5 bits a character.
const LabelLen = 26

// LabelAlphabet is the lowercase Crockford base32 alphabet a [Label] is
// spelled in.
const LabelAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// ---- kinds and capabilities ------------------------------------------------ //

// Kind is what an artifact is. Everything decided by kind is the kind's row of
// the kind table ([KindSpec], kind.go), never a switch on the name.
type Kind string

const (
	// KindHTML is a page that runs author code, served from the artifact
	// origin inside a sandboxed frame.
	KindHTML Kind = "html"

	// KindMarkdown is a document the dashboard renders with its own
	// element-constructing renderer. It carries no author code.
	KindMarkdown Kind = "markdown"

	// KindSlides is a deck: an index and one slide file each, in the closed
	// slide format (internal/artifacts/slideformat), which the dashboard
	// draws, presents and prints with its own element-constructing renderer.
	// It carries no author code.
	KindSlides Kind = "slides"

	// KindSheet is a native sheet: its content is its datastore and the
	// dashboard draws it from the schema alone, so there is nothing for a
	// model to get wrong and no page script at all.
	KindSheet Kind = "sheet"
)

// Capability is a runtime capability a version declares and a viewer's ticket
// may be granted. A version declaring one this build does not know is refused
// at publish rather than silently served without it.
type Capability string

const (
	// CapabilityAssets lets a page reference the artifact's asset store.
	CapabilityAssets Capability = "assets"

	// CapabilityDB lets a page read and write the artifact's shared
	// datastore through the host bridge, under the collection rules.
	CapabilityDB Capability = "db"

	// CapabilityState lets a page keep per-viewer state that follows the
	// viewer across devices, in a private collection, which is what stands
	// in for browser storage on an opaque origin.
	CapabilityState Capability = "state"

	// CapabilityIdentity lets a page learn who the viewer is.
	CapabilityIdentity Capability = "identity"

	// CapabilityPresence lets a page see who else is present and announce
	// the viewer's focus.
	CapabilityPresence Capability = "presence"
)

// Capabilities are every capability this build serves.
var Capabilities = []Capability{
	CapabilityAssets, CapabilityDB, CapabilityState, CapabilityIdentity, CapabilityPresence,
}

// Valid reports whether c is a capability this build serves.
func (c Capability) Valid() bool { return slices.Contains(Capabilities, c) }

// ---- the role ladder and shares ------------------------------------------ //

// Role is a principal's relation to one artifact. A ROLE IS A RELATION, NEVER
// A PERMISSION: the artifacts app gates authoring, and a share alone admits its
// holder to read, comment and write data.
//
// Each role includes every role below it in [Roles]. The ordering methods are
// the authority table's (internal/authz's owner adds role.go).
type Role string

const (
	// RoleViewer reads the artifact, its versions and its history, and is
	// present.
	RoleViewer Role = "viewer"

	// RoleCommenter also comments.
	RoleCommenter Role = "commenter"

	// RoleContributor also writes data the collection rules admit, and
	// never the page itself: the role a review sheet's reviewers hold.
	RoleContributor Role = "contributor"

	// RoleEditor also publishes versions and changes the schema, beside the
	// artifacts app.
	RoleEditor Role = "editor"

	// RoleOwner also shares, trashes and transfers. Held by the owner, by
	// the lead of an agent owner, and through the admin path. Never shared:
	// ownership moves only by transfer.
	RoleOwner Role = "owner"
)

// Roles are the ladder in ascending order.
var Roles = []Role{RoleViewer, RoleCommenter, RoleContributor, RoleEditor, RoleOwner}

// ShareableRoles are the roles a share may grant: every role but owner.
var ShareableRoles = []Role{RoleViewer, RoleCommenter, RoleContributor, RoleEditor}

// Valid reports whether r is a role on the ladder.
func (r Role) Valid() bool { return slices.Contains(Roles, r) }

// AudienceKind is who a share names.
//
// A CLOSED SET of company principals: never a login, never an address outside
// the company. A later build adds a link kind for public links; this build
// refuses one as it refuses any unknown kind.
type AudienceKind string

const (
	// AudienceSeat is one seat, by handle: whoever holds it, so the access
	// follows the seat as an inbox does and a successor inherits it.
	AudienceSeat AudienceKind = "seat"

	// AudienceUnit is every seat in a unit or anywhere below it, decided
	// live against the running org.
	AudienceUnit AudienceKind = "unit"

	// AudienceCompany is every principal the guard resolves whose stage may
	// act, agents included. Only a person widens a share to it.
	AudienceCompany AudienceKind = "company"
)

// AudienceKinds are every audience kind this build knows.
var AudienceKinds = []AudienceKind{AudienceSeat, AudienceUnit, AudienceCompany}

// Valid reports whether k is an audience kind this build knows.
func (k AudienceKind) Valid() bool { return slices.Contains(AudienceKinds, k) }

// Audience is the principal set one share names. ID is the seat handle for
// [AudienceSeat], the unit key for [AudienceUnit], and empty for
// [AudienceCompany]. Its text form and parser are the domain's (audience.go).
type Audience struct {
	Kind AudienceKind `json:"kind"`
	ID   string       `json:"id,omitempty"`
}

// Share is one audience's role on one artifact.
type Share struct {
	Audience  Audience  `json:"audience"`
	Role      Role      `json:"role"`
	GrantedBy iam.Actor `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
}

// ---- manifests --------------------------------------------------------- //

// Entry is one published file of one version.
type Entry struct {
	// Path is the published path, relative, with no leading slash.
	Path string `json:"path"`

	// ContentType is the media table's served type for the path. NEVER the
	// client's declaration: a client may only disambiguate an extension the
	// table already knows.
	ContentType string `json:"content_type"`

	// Object is the stored bytes: the key, the SHA-256 of the whole content
	// and the size, verified on every read the engine serves.
	Object objstore.Object `json:"object"`

	// Carried is true when the object was found in this artifact rather than
	// uploaded by the record that names it.
	Carried bool `json:"carried,omitempty"`

	// Script is what the publish-time linter derived for an HTML entry; nil
	// for every other type.
	Script *ScriptPolicy `json:"script,omitempty"`
}

// ScriptPolicy is what the serving origin needs to build one HTML document's
// script policy without parsing it again.
type ScriptPolicy struct {
	// Nonced reports that every script element carries [NoncePlaceholder],
	// which the serving path replaces with a fresh nonce per response.
	Nonced bool `json:"nonced"`

	// HandlerHashes are the base64 SHA-256 digests of the inline event
	// handlers the page publishes, admitted by hash and nothing wider.
	HandlerHashes []string `json:"handler_hashes,omitempty"`

	// UnsafeEval reports that the page needs string evaluation (an
	// in-browser transform the linter detected or the author declared).
	UnsafeEval bool `json:"unsafe_eval,omitempty"`

	// WasmEval reports that the page compiles WebAssembly.
	WasmEval bool `json:"wasm_eval,omitempty"`
}

// Manifest is every file of one version.
type Manifest struct {
	// Entry is the path served at the version's root.
	Entry string `json:"entry"`

	// Files are sorted bytewise by path, at most [MaxFilesPerVersion].
	Files []Entry `json:"files"`
}

// NoncePlaceholder is the token the linter writes into every script element of
// a published HTML file, and the serving path replaces with a fresh nonce per
// response: a byte substitution on a cached template rather than a parse per
// request. The linter refuses a source file that already contains it, so an
// author cannot place it themselves, and the serving path substitutes it only
// in a page whose script facts the linter wrote ([ScriptPolicy.Nonced]): in
// any other document (an SVG or XML file, an asset) it is inert text, since
// the constant is public.
const NoncePlaceholder = "__CREWLET_NONCE_03252d69b9fa46508ca1f9ade71be8ee__"
