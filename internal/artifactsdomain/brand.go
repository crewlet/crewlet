package artifactsdomain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ADOPTED BRAND (ADR-0048: the adopted brand is the company's public face,
// snapshotted by an administrator and served by its bytes).
//
// A brand KIT is an ordinary artifact of kind `brand`, which anybody with the
// app may draft, agents included. The company's brand is an ADOPTION: an
// administrator, a person, at a recent proof of identity, adopts one fixed
// version of one kit on ONE subject ([BrandSubject]). The adoption SNAPSHOTS
// that version (its canonical brand.json, the stylesheet generated from it,
// and every file the brand serves) into rows of its own, which name the
// objects, so the adopted brand survives the kit's retention, trash and purge,
// and a later publish to the kit changes nothing until somebody adopts again.
//
// What the adoption serves is the company's public face: every artifact,
// every agent and the public brand route on the artifact listener read it, so
// it is everybody's to read and only an administrator's to change.

// Adoption is the company's adopted brand.
type Adoption struct {
	// Kit and KitVersion are the brand artifact and version it was adopted
	// from. The kit may since have been trashed or purged: the snapshot is
	// what is served.
	Kit        artifacts.ID `json:"kit"`
	KitVersion int          `json:"kit_version"`

	// BrandJSON is the adopted version's canonical brand.json, byte for
	// byte (internal/artifacts/brandformat reads it).
	BrandJSON []byte `json:"brand_json"`

	// CSSSha is the generated stylesheet's SHA-256 in hex: what a framed
	// version published now pins, and the `css` file's hash.
	CSSSha string `json:"css_sha"`

	// Files are every file the adoption serves, the stylesheet first.
	Files []BrandFile `json:"files"`

	AdoptedBy iam.Actor `json:"adopted_by"`
	AdoptedAt time.Time `json:"adopted_at"`

	// Position is the packed position of the adoption record.
	Position int64 `json:"position"`
}

// BrandFileInput is one logo or face of the kit's version an adoption serves,
// named by its path in that version: the writer resolves it to the object the
// version names in its own snapshot, so the adoption names only bytes the
// version still holds.
type BrandFileInput struct {
	// Slot is the file's place in the brand (brandformat.Slot, a face's
	// files under brandformat.FontFileSlot).
	Slot string

	// Path is the file of the kit's version.
	Path string
}

// BrandAdoptInput is one version of a kit to adopt.
type BrandAdoptInput struct {
	Kit     artifacts.ID
	Version int

	// BrandJSON is the version's canonical brand.json, which the caller
	// read and validated (brandformat) from the version it names.
	BrandJSON []byte

	// CSS is the stylesheet generated from BrandJSON, uploaded under a
	// reservation (ADR-0036), and CSSSha its SHA-256 in hex.
	CSS    objstore.Object
	CSSSha string

	// Files are the logos and faces the brand serves.
	Files []BrandFileInput

	// Uploaded is the stylesheet's key, minted at most
	// objstore.RecordWithin before the decide.
	Uploaded []objstore.Key
}

// ---- the writer's brand operations ---------------------------------------- //

// AdoptBrand adopts one version of a kit as the company's brand. An adoption
// whose text colour does not reach brandformat.MinTextContrast on its light
// ground is ErrBrandContrast; a version the kit no longer holds is ErrNotFound.
func (w *Writer) AdoptBrand(_ context.Context, _ Call, _ BrandAdoptInput) (Written, error) {
	return Written{}, fmt.Errorf("%w: adopt a brand", ErrNotBuilt)
}

// ClearBrand withdraws the company's adopted brand. Clearing when none is
// adopted applies and changes nothing.
func (w *Writer) ClearBrand(_ context.Context, _ Call) (Written, error) {
	return Written{}, fmt.Errorf("%w: clear the brand", ErrNotBuilt)
}

// ---- the reader's brand questions ----------------------------------------- //

// Brand answers the adopted brand, and false when none is adopted.
func (r *Reader) Brand(_ context.Context, _ statelog.Freshness) (Adoption, bool, error) {
	return Adoption{}, false, fmt.Errorf("%w: read the brand", ErrNotBuilt)
}

// BrandFile answers one file a current or retained adoption serves, by its
// hash (SHA-256 in hex): the public brand route's allowlist. A hash no
// adoption serves is ErrNotFound.
func (r *Reader) BrandFile(_ context.Context, _ objstore.Hash) (BrandFile, error) {
	return BrandFile{}, fmt.Errorf("%w: read a brand file", ErrNotBuilt)
}

// ---- the applier's brand records ------------------------------------------ //

// applyBrand applies one record on the brand subject.
func (a *Applier) applyBrand(_ context.Context, _ *sql.Tx, c applyContext) (int, error) {
	return 0, fmt.Errorf("%w: apply the brand record at %s", ErrNotBuilt, c.position)
}
