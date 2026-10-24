package artifactsdomain

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ADOPTED BRAND (ADR-0048: the adopted brand is the company's public face,
// snapshotted by an administrator and served by its bytes).
//
// A brand KIT is an ordinary artifact of kind `brand`, which anybody with the
// app may draft, agents included. The company's brand is an ADOPTION: an
// administrator, a person, at a recent proof of identity, adopts one fixed
// version of one kit on ONE subject ([BrandSubject]). The adoption SNAPSHOTS
// that version (its canonical brand.json, the stylesheet generated from it,
// and every file the brand serves) into rows of its own, so the adopted brand
// survives the kit's retention, trash and purge, and a later publish to the
// kit changes nothing until somebody adopts again.
//
// THE SNAPSHOT'S FILES ARE ITS OWN OBJECTS. The upload pipeline copies every
// logo and face out of the kit's version (sanitised again by this build's
// rules) and stores the generated stylesheet beside them, each as a fresh
// object under one reservation, and the adoption names those keys and no
// other. So no record names an object another artifact's row names: the
// collector's invariant (ADR-0027, ADR-0036: a key is named only by the write
// that uploaded it, or by a carry within one artifact) holds with no case for
// the brand.
//
// A FRAMED VERSION PINS THE STYLESHEET ADOPTED WHEN IT IS PUBLISHED
// ([VersionPayload.Brand], `artifact_versions.brand`), and the artifact origin
// places that stylesheet's link when it serves the version. So an adoption's
// files stay served while it is current or any retained version names its
// stylesheet: the applier prunes a superseded adoption's rows at the next
// adoption or clear once no retained version names it, never sooner.
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

	// Files are every file the adoption serves, the stylesheet first, then
	// the logos and each face's files in [brandFileOrder].
	Files []BrandFile `json:"files"`

	AdoptedBy iam.Actor `json:"adopted_by"`
	AdoptedAt time.Time `json:"adopted_at"`

	// Position is the packed position of the adoption record.
	Position int64 `json:"position"`
}

// File is the adopted file in slot, false where the adoption serves none.
func (a Adoption) File(slot brandformat.Slot) (BrandFile, bool) {
	i := slices.IndexFunc(a.Files, func(f BrandFile) bool { return f.Slot == string(slot) })
	if i < 0 {
		return BrandFile{}, false
	}
	return a.Files[i], true
}

// BrandAdoptInput is one version of a kit to adopt, its files already copied.
type BrandAdoptInput struct {
	Kit     artifacts.ID
	Version int

	// BrandJSON is the version's brand.json in canonical form
	// (brandformat.Canonical), which the caller read and validated from the
	// version it names.
	BrandJSON []byte

	// Files are every file the adoption serves: the generated stylesheet
	// first ([brandformat.SlotCSS], its hash the adoption's stylesheet
	// hash), then a fresh copy of each logo and face of the version, one per
	// slot the brand names ([brandformat.FontFileSlot] for a face's files).
	Files []BrandFile

	// Uploaded is every key Files names, each minted at most
	// objstore.RecordWithin before the decide.
	Uploaded []objstore.Key
}

// MaxBrandCSSBytes bounds the generated stylesheet. The generator writes at
// most fourteen custom properties and eight `@font-face` rules (two faces of
// at most [brandformat.MaxFontFilesPerFamily] files), under two kilobytes;
// sixteen leaves the generator room to grow and refuses a writer that sends
// something else as the stylesheet.
const MaxBrandCSSBytes = 16 << 10

// BrandContrast is an adoption refused because the brand's text does not
// contrast enough with its light ground to be read (422 `brand_contrast`): the
// pair that refuses, measured. It is [ErrBrandContrast].
type BrandContrast struct {
	Pair brandformat.ContrastResult `json:"pair"`
}

func (e *BrandContrast) Error() string {
	return fmt.Sprintf("artifactsdomain: the brand's %s (%s) on its %s (%s) contrasts %.2f:1, "+
		"and an adopted brand's text must reach %.1f:1: darken the text or lighten the ground",
		e.Pair.Foreground, e.Pair.ForegroundColor, e.Pair.Ground, e.Pair.GroundColor,
		e.Pair.Ratio, e.Pair.Min)
}

// Unwrap is the sentinel the estate and the API branch on.
func (e *BrandContrast) Unwrap() error { return ErrBrandContrast }

// CheckBrandContrast is the adoption's contrast rule, the one implementation
// the upload pipeline asks before it stores a byte and the writer asks again
// before it appends: the first pair [brandformat.ContrastPairs] marks as
// refusing that does not reach its floor.
func CheckBrandContrast(b brandformat.Brand) error {
	for _, r := range brandformat.Contrast(b) {
		if r.Refuses && !r.Passes() {
			return &BrandContrast{Pair: r}
		}
	}
	return nil
}

// ---- the writer's brand operations ---------------------------------------- //

// AdoptBrand adopts one version of a kit as the company's brand. An adoption
// whose text colour does not reach brandformat.MinTextContrast on its light
// ground is a *[BrandContrast]; a kit or version the log no longer holds is
// ErrNotFound (ErrPurged for a purged kit), a kit in the trash ErrTrashed.
//
// THE KIT'S VERSION IS NAMED IN THE SCOPE (its artifact's term), so the apply
// orders behind any record about the kit and refuses `version_not_found`
// when retention or a purge removed the version since the decide.
func (w *Writer) AdoptBrand(ctx context.Context, c Call, in BrandAdoptInput) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	at := w.now()
	if err := checkAdoption(in, at); err != nil {
		return Written{}, err
	}
	kit := in.Kit.String()
	opID, err := w.operation(c, string(OpAdopt), kit+"/"+strconv.Itoa(in.Version), in)
	if err != nil {
		return Written{}, err
	}
	subject := BrandSubject()
	scope := ScopeSet{Terms: []ScopeTerm{BrandTerm(), artifactTerm(kit)}}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := mustHead(ctx, tx, kit) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			switch {
			case head.Kind != artifacts.KindBrand:
				return statelog.Decision{}, fmt.Errorf("%w: artifact %s is a %s, and only a brand "+
					"kit is adopted as the company's brand", ErrInvalid, kit, head.Kind)
			case head.Status == StatusTrashed:
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, kit)
			}
			v, found, err := readVersionTx(ctx, tx, kit, in.Version)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !found {
				return statelog.Decision{}, fmt.Errorf("%w: kit %s keeps no version %d",
					ErrNotFound, kit, in.Version)
			}
			label, err := adoptionLabel(ctx, tx, kit, v.Label, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			payload := BrandAdoptPayload{
				V: baseRecordVersion, Kit: kit, KitVersion: in.Version, BrandJSON: in.BrandJSON,
				CSSSha: string(in.Files[0].Object.Hash), Files: in.Files, Label: label,
				Authority: c.Authority,
			}
			return w.decision(stamp, c, subject, OpAdopt, scope, opID, payload, at)
		}))
	return w.brandWritten(ctx, opID, res, err)
}

// checkAdoption refuses, before anything is read, an adoption the domain would
// never write: a kit index that is not canonical or not the format, a brand
// whose text does not contrast with its ground, and files that are not exactly
// the ones the brand serves, each uploaded by this adoption.
func checkAdoption(in BrandAdoptInput, at time.Time) error {
	switch {
	case in.Kit.IsZero():
		return fmt.Errorf("%w: an adoption names its kit", ErrInvalid)
	case in.Version <= 0:
		return fmt.Errorf("%w: an adoption names the kit's version", ErrInvalid)
	case len(in.BrandJSON) > brandformat.MaxIndexBytes:
		return fmt.Errorf("%w: the kit's brand.json is %d bytes and may be at most %d",
			ErrInvalid, len(in.BrandJSON), brandformat.MaxIndexBytes)
	}
	b, err := brandformat.Decode(in.BrandJSON)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	canonical, err := brandformat.Canonical(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, in.BrandJSON) {
		return fmt.Errorf("%w: the kit's brand.json is not in its canonical form "+
			"(brandformat.Canonical), which is what an adoption snapshots", ErrInvalid)
	}
	if err := CheckBrandContrast(b); err != nil {
		return err
	}
	want := brandSlots(b)
	if len(in.Files) != len(want) {
		return fmt.Errorf("%w: the brand serves %d files and the adoption names %d",
			ErrInvalid, len(want), len(in.Files))
	}
	for i, f := range in.Files {
		if f.Slot != string(want[i]) {
			return fmt.Errorf("%w: file %d of the adoption fills slot %q, and the brand's "+
				"file %d is %q", ErrInvalid, i, f.Slot, i, want[i])
		}
		if err := checkBrandFile(f); err != nil {
			return err
		}
		if !slices.Contains(in.Uploaded, f.Object.Key) {
			return fmt.Errorf("%w: slot %q names object %s, which this adoption did not "+
				"upload: an adoption copies every file it serves", ErrInvalid, f.Slot, f.Object.Key)
		}
	}
	for _, k := range in.Uploaded {
		if age := at.Sub(k.Minted()); age > objstore.RecordWithin {
			return fmt.Errorf("%w: object %s was minted %s ago and an adoption names an "+
				"upload at most %s after it", ErrUploadStale, k, age.Truncate(time.Second),
				objstore.RecordWithin)
		}
	}
	return nil
}

// brandSlots are the slots an adoption of b serves, in [brandFileOrder]: the
// stylesheet, each logo the brand names, and each file of an uploaded face.
func brandSlots(b brandformat.Brand) []brandformat.Slot {
	out := []brandformat.Slot{brandformat.SlotCSS}
	for _, l := range []struct {
		slot brandformat.Slot
		path string
	}{{brandformat.SlotLogoPrimary, b.Logos.Primary}, {brandformat.SlotLogoOnDark, b.Logos.OnDark},
		{brandformat.SlotLogoMark, b.Logos.Mark}} {
		if l.path != "" {
			out = append(out, l.slot)
		}
	}
	for _, f := range []struct {
		slot brandformat.Slot
		face brandformat.Face
	}{{brandformat.SlotFontHeading, b.Type.Heading}, {brandformat.SlotFontBody, b.Type.Body}} {
		for i := range f.face.Files {
			out = append(out, brandformat.FontFileSlot(f.slot, i))
		}
	}
	return out
}

// checkBrandFile refuses a file whose type is not its slot's, or whose object
// is not a stored object within its slot's bound.
func checkBrandFile(f BrandFile) error {
	slot := brandformat.Slot(f.Slot)
	essence, _, _ := strings.Cut(f.ContentType, ";")
	limit := int64(0)
	ok := false
	switch {
	case slot == brandformat.SlotCSS:
		ok, limit = essence == "text/css", MaxBrandCSSBytes
	case slot == brandformat.SlotLogoPrimary || slot == brandformat.SlotLogoOnDark ||
		slot == brandformat.SlotLogoMark:
		switch essence {
		case "image/svg+xml":
			ok, limit = true, brandformat.MaxSVGBytes
		case "image/png", "image/webp":
			ok, limit = true, brandformat.MaxImageBytes
		}
	case strings.HasPrefix(f.Slot, string(brandformat.SlotFontHeading)+".") ||
		strings.HasPrefix(f.Slot, string(brandformat.SlotFontBody)+"."):
		ok, limit = essence == "font/woff2", brandformat.MaxFontFileBytes
	}
	switch {
	case !ok:
		return fmt.Errorf("%w: slot %q holds %q, which is not a type that slot serves",
			ErrInvalid, f.Slot, f.ContentType)
	case f.Object.Key.IsZero() || !f.Object.Hash.Valid() || f.Object.Size <= 0:
		return fmt.Errorf("%w: slot %q names no stored object", ErrInvalid, f.Slot)
	case f.Object.Size > limit:
		return fmt.Errorf("%w: slot %q is %d bytes and may be at most %d", ErrInvalid,
			f.Slot, f.Object.Size, limit)
	}
	return nil
}

// adoptionLabel is the label an adoption sets on the version it adopts, so the
// kit's history says which version is the company's brand: `Adopted
// <date>`, numbered when that label is already held, and empty when the
// version carries a label already or the kit keeps as many labels as it may
// (the snapshot holds its own copy of every file, so the label is provenance,
// never what keeps the adopted bytes).
func adoptionLabel(ctx context.Context, tx *sql.Tx, kit, current string, at time.Time) (string, error) {
	if current != "" {
		return "", nil
	}
	count, err := labelled(ctx, tx, kit)
	if err != nil || count >= artifacts.MaxLabels {
		return "", err
	}
	base := "Adopted " + at.UTC().Format(time.DateOnly)
	for n := 1; n <= artifacts.MaxLabels+1; n++ {
		label := base
		if n > 1 {
			label = fmt.Sprintf("%s (%d)", base, n)
		}
		holder, err := labelHolder(ctx, tx, kit, label)
		if err != nil {
			return "", err
		}
		if holder == 0 {
			return label, nil
		}
	}
	return "", nil
}

// ClearBrand withdraws the company's adopted brand. Clearing when none is
// adopted publishes nothing and answers success. Pages pinned to an earlier
// stylesheet keep it while a retained version names it.
func (w *Writer) ClearBrand(ctx context.Context, c Call) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	at := w.now()
	opID, err := w.operation(c, string(OpClear), "company", struct{}{})
	if err != nil {
		return Written{}, err
	}
	subject, scope := BrandSubject(), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			_, adopted, err := readAdoptionTx(ctx, tx) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil || !adopted {
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, subject, OpClear, scope, opID,
				BrandClearPayload{V: baseRecordVersion, Authority: c.Authority}, at)
		}))
	return w.brandWritten(ctx, opID, res, err)
}

// brandWritten is a brand write's outcome: the verdict its apply wrote on this
// node, read back from the brand's history.
func (w *Writer) brandWritten(ctx context.Context, opID string, res statelog.Result, err error) (Written, error) {
	if err != nil {
		return Written{Result: res}, err
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
		return Written{Result: res}, nil
	}
	var outcome, reason, kit string
	err = w.db.Read(ctx, func(tx *sql.Tx) error {
		read := tx.QueryRowContext(ctx, `
			SELECT outcome, reason, kit_id FROM artifact_brand_history WHERE op_id = ?
			ORDER BY position LIMIT 1`, opID).Scan(&outcome, &reason, &kit)
		if errors.Is(read, sql.ErrNoRows) {
			return nil
		}
		return read
	})
	switch {
	case err != nil:
		return Written{Result: res}, fmt.Errorf("artifactsdomain: read what operation %s did "+
			"to the brand: %w", opID, err)
	case Outcome(outcome) != OutcomeRefused:
		return Written{Result: res}, nil
	}
	switch Refusal(reason) {
	case RefusedVersionNotFound, RefusedNotFound:
		return Written{Result: res}, fmt.Errorf("%w: kit %s no longer keeps the version adopted",
			ErrNotFound, kit)
	case RefusedTrashed:
		return Written{Result: res}, fmt.Errorf("%w: %s", ErrTrashed, kit)
	}
	return Written{Result: res}, fmt.Errorf("%w: the log refused this adoption as %s", ErrInvalid, reason)
}

// ---- the reader's brand questions ----------------------------------------- //

// Brand answers the adopted brand, and false when none is adopted.
func (r *Reader) Brand(ctx context.Context, fresh statelog.Freshness) (Adoption, bool, error) {
	if fresh.Level == "" {
		return Adoption{}, false, errors.New("artifactsdomain: this read names no level: a " +
			"surface resolves an absent read_level to its own default before it reads")
	}
	var (
		out     Adoption
		adopted bool
	)
	_, err := r.log.Read(ctx, fresh.Query(BrandScope(), false), func(tx *sql.Tx) error {
		var err error
		out, adopted, err = readAdoptionTx(ctx, tx)
		return err
	})
	return out, adopted, err
}

// BrandFile answers one file a current or retained adoption serves, by its
// hash (SHA-256 in hex): the public brand route's allowlist. A hash no
// adoption serves is ErrNotFound.
//
// READ AT `stale`: a hash names the same bytes for ever, so the only question
// is whether this node's rows allow it, and a node that has not applied the
// adoption yet answers not found until it has.
func (r *Reader) BrandFile(ctx context.Context, hash objstore.Hash) (BrandFile, error) {
	if !hash.Valid() {
		return BrandFile{}, fmt.Errorf("%w: %q is not a file's hash", ErrNotFound, hash)
	}
	var out BrandFile
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(BrandScope(), false),
		func(tx *sql.Tx) error {
			var object, h string
			var size int64
			err := tx.QueryRowContext(ctx, `
				SELECT slot, content_type, object, hash, size FROM artifact_brand_files
				WHERE hash = ? ORDER BY position DESC LIMIT 1`, string(hash)).Scan(
				&out.Slot, &out.ContentType, &object, &h, &size)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no adopted brand serves %s", ErrNotFound, hash)
			}
			if err != nil {
				return fmt.Errorf("artifactsdomain: read the brand file %s: %w", hash, err)
			}
			key, err := objstore.ParseKey(object)
			if err != nil {
				return fmt.Errorf("artifactsdomain: the brand file %s names object %q: %w",
					hash, object, err)
			}
			out.Object = objstore.Object{Key: key, Hash: objstore.Hash(h), Size: size}
			return nil
		})
	return out, err
}

// readAdoptionTx is the adopted brand as tx holds it, false for none.
func readAdoptionTx(ctx context.Context, tx *sql.Tx) (Adoption, bool, error) {
	var (
		a                 Adoption
		kit, by, kind, op string
		at                int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT kit_id, kit_version, brand_json, css_sha, adopted_by, adopted_by_kind,
		       operator_id, adopted_at, position
		FROM artifact_brand WHERE key = ?`, brandKey).Scan(&kit, &a.KitVersion, &a.BrandJSON,
		&a.CSSSha, &by, &kind, &op, &at, &a.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return Adoption{}, false, nil
	}
	if err != nil {
		return Adoption{}, false, fmt.Errorf("artifactsdomain: read the adopted brand: %w", err)
	}
	if a.Kit, err = artifacts.ParseID(kit); err != nil {
		return Adoption{}, false, fmt.Errorf("artifactsdomain: the adopted brand names kit %q: %w", kit, err)
	}
	a.AdoptedBy = iam.Actor{Name: by, Kind: iam.ActorKind(kind), OperatorID: op}
	a.AdoptedAt = store.DecodeTime(at)
	rows, err := tx.QueryContext(ctx, `
		SELECT slot, content_type, object, hash, size FROM artifact_brand_files
		WHERE css_sha = ?`, a.CSSSha)
	if err != nil {
		return Adoption{}, false, fmt.Errorf("artifactsdomain: read the adopted brand's files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f BrandFile
		var object, hash string
		var size int64
		if err := rows.Scan(&f.Slot, &f.ContentType, &object, &hash, &size); err != nil {
			return Adoption{}, false, fmt.Errorf("artifactsdomain: read the adopted brand's files: %w", err)
		}
		key, err := objstore.ParseKey(object)
		if err != nil {
			return Adoption{}, false, fmt.Errorf("artifactsdomain: the adopted brand's %s names "+
				"object %q: %w", f.Slot, object, err)
		}
		f.Object = objstore.Object{Key: key, Hash: objstore.Hash(hash), Size: size}
		a.Files = append(a.Files, f)
	}
	if err := rows.Err(); err != nil {
		return Adoption{}, false, err
	}
	slices.SortStableFunc(a.Files, func(x, y BrandFile) int {
		return brandFileOrder(x.Slot) - brandFileOrder(y.Slot)
	})
	return a, true, nil
}

// brandFileOrder is a slot's place in an adoption's files: the stylesheet, the
// three logos, then the heading's files and the body's, each by index.
func brandFileOrder(slot string) int {
	switch brandformat.Slot(slot) {
	case brandformat.SlotCSS:
		return 0
	case brandformat.SlotLogoPrimary:
		return 1
	case brandformat.SlotLogoOnDark:
		return 2
	case brandformat.SlotLogoMark:
		return 3
	}
	for base, face := range []brandformat.Slot{brandformat.SlotFontHeading, brandformat.SlotFontBody} {
		if rest, ok := strings.CutPrefix(slot, string(face)+"."); ok {
			if i, err := strconv.Atoi(rest); err == nil {
				return 100*(base+1) + i
			}
		}
	}
	return 1000
}

// brandKey is the one row `artifact_brand` holds.
const brandKey = "company"

// currentCSSSha is the stylesheet the adopted brand serves, empty for none.
func currentCSSSha(ctx context.Context, tx *sql.Tx) (string, error) {
	var sha string
	err := tx.QueryRowContext(ctx, `SELECT css_sha FROM artifact_brand WHERE key = ?`,
		brandKey).Scan(&sha)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("artifactsdomain: read the adopted brand's stylesheet: %w", err)
	}
	return sha, nil
}

// pinnedBrand is the stylesheet a framed version is stored as pinning: the one
// its record names while an adoption still serves it, else (an adoption pruned
// it between the decide and this apply) the stylesheet adopted now, or none.
// A record naming a stylesheet names [BrandTerm] in its scope, so every node
// reads these rows at the same point of the log.
func pinnedBrand(ctx context.Context, tx *sql.Tx, named string) (string, error) {
	if named == "" {
		return "", nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_brand_files WHERE css_sha = ? AND slot = ?`,
		named, string(brandformat.SlotCSS)).Scan(&n); err != nil {
		return "", fmt.Errorf("artifactsdomain: read whether stylesheet %s is served: %w", named, err)
	}
	if n > 0 {
		return named, nil
	}
	return currentCSSSha(ctx, tx)
}

// withBrand is a framed write's scope with the adopted brand named beside what
// it already names: its apply reads the brand's rows ([pinnedBrand]).
func withBrand(subject Subject, s ScopeSet) ScopeSet {
	terms := slices.Clone(s.Terms)
	if s.Subject || len(terms) == 0 {
		terms = []ScopeTerm{artifactTerm(subject.ID)}
	}
	return ScopeSet{Terms: append(terms, BrandTerm())}
}

// ---- the applier's brand records ------------------------------------------ //

// applyBrand applies one record on the brand subject.
func (a *Applier) applyBrand(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	var n int
	if read := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_brand_history WHERE position = ?`, c.packed).Scan(&n); read != nil {
		return 0, fmt.Errorf("artifactsdomain: read whether the brand record at %s applied: %w",
			c.position, read)
	}
	if n > 0 {
		return 0, nil
	}
	var rows int
	switch p := payload.(type) {
	case BrandAdoptPayload:
		rows, err = a.applyAdopt(ctx, tx, c, p)
	case BrandClearPayload:
		rows, err = a.applyClear(ctx, tx, c, p)
	default:
		return 0, fmt.Errorf("artifactsdomain: the brand record at %s carries a %T", c.position, payload)
	}
	if err != nil {
		return 0, err
	}
	pruned, err := pruneBrandHistory(ctx, tx, c)
	if err != nil {
		return 0, err
	}
	return rows + pruned, nil
}

// brandHistory is one brand history row, before it is written.
type brandHistory struct {
	op         OpKind
	outcome    Outcome
	reason     Refusal
	kit        string
	kitVersion int
	summary    BrandHistorySummary
}

// writeBrandHistory writes the record's brand history row.
func writeBrandHistory(ctx context.Context, tx *sql.Tx, c applyContext, h brandHistory) (int, error) {
	summary, err := json.Marshal(h.summary)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: encode the brand history summary at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_brand_history
			(position, op_id, op, outcome, reason, kit_id, kit_version, actor, actor_kind,
			 operator_id, at, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (position) DO NOTHING`,
		c.packed, c.record.OpID, string(h.op), string(h.outcome), string(h.reason), h.kit,
		h.kitVersion, c.record.Actor, string(c.record.ActorKind), c.record.OperatorID, c.at(),
		string(summary))
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: write the brand history row at %s: %w", c.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyAdopt replaces the adopted brand with the record's snapshot.
//
// IT REFUSES ONLY WHAT CAN MOVE BETWEEN THE DECIDE AND THE APPLY: the kit's
// version retired or purged (`version_not_found`), the kit trashed. Every
// other rule (the format, the contrast, the files) is the writer's, decided
// from the record's own bytes, which no later record changes.
func (a *Applier) applyAdopt(ctx context.Context, tx *sql.Tx, c applyContext, p BrandAdoptPayload) (int, error) {
	refuse := func(reason Refusal) (int, error) {
		return writeBrandHistory(ctx, tx, c, brandHistory{op: OpAdopt, outcome: OutcomeRefused,
			reason: reason, kit: p.Kit, kitVersion: p.KitVersion})
	}
	head, found, err := readHead(ctx, tx, p.Kit)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return refuse(RefusedVersionNotFound)
	case head.status == string(StatusTrashed):
		return refuse(RefusedTrashed)
	}
	var label string
	err = tx.QueryRowContext(ctx, `
		SELECT label FROM artifact_versions WHERE artifact_id = ? AND number = ?`,
		p.Kit, p.KitVersion).Scan(&label)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return refuse(RefusedVersionNotFound)
	case err != nil:
		return 0, fmt.Errorf("artifactsdomain: read kit %s version %d: %w", p.Kit, p.KitVersion, err)
	}
	if len(p.Files) == 0 || p.Files[0].Slot != string(brandformat.SlotCSS) ||
		string(p.Files[0].Object.Hash) != p.CSSSha {
		return refuse(RefusedInvalid)
	}
	actor := c.record.actor()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO artifact_brand
			(key, kit_id, kit_version, brand_json, css_sha, adopted_by, adopted_by_kind,
			 operator_id, adopted_at, position, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			kit_id = excluded.kit_id, kit_version = excluded.kit_version,
			brand_json = excluded.brand_json, css_sha = excluded.css_sha,
			adopted_by = excluded.adopted_by, adopted_by_kind = excluded.adopted_by_kind,
			operator_id = excluded.operator_id, adopted_at = excluded.adopted_at,
			position = excluded.position, version = excluded.version
		WHERE excluded.version > artifact_brand.version`,
		brandKey, p.Kit, p.KitVersion, p.BrandJSON, p.CSSSha, actor.Name, string(actor.Kind),
		actor.OperatorID, c.at(), c.packed, c.packed); err != nil {
		return 0, fmt.Errorf("artifactsdomain: adopt kit %s version %d at %s: %w", p.Kit,
			p.KitVersion, c.position, err)
	}
	rows := 1
	for _, f := range p.Files {
		// THE SAME STYLESHEET ADOPTED AGAIN names the copies its own record
		// uploaded: the row is the latest adoption's, and the earlier
		// copies, now named by nothing, are the collector's.
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO artifact_brand_files
				(css_sha, slot, content_type, object, hash, size, position)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (css_sha, slot) DO UPDATE SET
				content_type = excluded.content_type, object = excluded.object,
				hash = excluded.hash, size = excluded.size, position = excluded.position`,
			p.CSSSha, f.Slot, f.ContentType, f.Object.Key.String(), string(f.Object.Hash),
			f.Object.Size, c.packed); err != nil {
			return 0, fmt.Errorf("artifactsdomain: record the brand's %s at %s: %w", f.Slot,
				c.position, err)
		}
		rows++
	}
	pruned, err := pruneBrandFiles(ctx, tx, p.CSSSha)
	if err != nil {
		return 0, err
	}
	rows += pruned
	set := ""
	if p.Label != "" && label == "" {
		n, ok, labelErr := a.labelAdopted(ctx, tx, c, p, head)
		if labelErr != nil {
			return 0, labelErr
		}
		if ok {
			set = p.Label
		}
		rows += n
	}
	n, err := writeBrandHistory(ctx, tx, c, brandHistory{op: OpAdopt, outcome: OutcomeApplied,
		kit: p.Kit, kitVersion: p.KitVersion,
		summary: BrandHistorySummary{CSSSha: p.CSSSha, Files: len(p.Files), Label: set}})
	return rows + n, err
}

// labelAdopted sets the adoption's label on the adopted version, where it is
// still free and the kit may keep another label, with a history row on the kit
// saying who labelled it. A label taken since the decide is left off: the
// snapshot does not depend on it.
func (a *Applier) labelAdopted(ctx context.Context, tx *sql.Tx, c applyContext, p BrandAdoptPayload,
	head headRow) (int, bool, error) {

	holder, err := labelHolder(ctx, tx, p.Kit, p.Label)
	if err != nil || holder != 0 {
		return 0, false, err
	}
	count, err := labelled(ctx, tx, p.Kit)
	if err != nil || count >= artifacts.MaxLabels {
		return 0, false, err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE artifact_versions SET label = ? WHERE artifact_id = ? AND number = ?`,
		p.Label, p.Kit, p.KitVersion); err != nil {
		return 0, false, fmt.Errorf("artifactsdomain: label kit %s version %d: %w", p.Kit, p.KitVersion, err)
	}
	authority := p.Authority
	n, err := writeHistory(ctx, tx, c, historyEntry{
		artifact: p.Kit, op: OpPatch, outcome: OutcomeApplied, number: p.KitVersion,
		summary: HistorySummary{Label: p.Label, Fields: []string{"label"}},
	}, &authority)
	if err != nil {
		return 0, false, err
	}
	a.move(p.Kit, MoveMeta, head.epoch, head.head, c.position)
	return n + 1, true, nil
}

// applyClear withdraws the adopted brand.
func (a *Applier) applyClear(ctx context.Context, tx *sql.Tx, c applyContext, _ BrandClearPayload) (int, error) {
	var kit string
	var kitVersion int
	err := tx.QueryRowContext(ctx, `SELECT kit_id, kit_version FROM artifact_brand WHERE key = ?`,
		brandKey).Scan(&kit, &kitVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("artifactsdomain: read the adopted brand at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM artifact_brand WHERE key = ?`, brandKey)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: clear the brand at %s: %w", c.position, err)
	}
	rows, _ := res.RowsAffected()
	pruned, err := pruneBrandFiles(ctx, tx, "")
	if err != nil {
		return 0, err
	}
	n, err := writeBrandHistory(ctx, tx, c, brandHistory{op: OpClear, outcome: OutcomeApplied,
		kit: kit, kitVersion: kitVersion})
	return int(rows) + pruned + n, err
}

// pruneBrandFiles removes the file rows of every adoption but the current one
// (current, empty after a clear) that no retained version still names, so
// their objects become the collector's. A version naming a stylesheet keeps
// every file of that adoption served for as long as it is retained.
func pruneBrandFiles(ctx context.Context, tx *sql.Tx, current string) (int, error) {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_brand_files
		WHERE css_sha != ?
		  AND NOT EXISTS (SELECT 1 FROM artifact_versions v WHERE v.brand = artifact_brand_files.css_sha)`,
		current)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: prune the files of superseded adoptions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// pruneBrandHistory removes brand history rows past [artifacts.HistoryRetention]
// of broker time, in the bounded batch every history prune takes.
func pruneBrandHistory(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	if c.brokerAt.IsZero() {
		return 0, nil
	}
	cutoff := store.EncodeTime(c.brokerAt.Add(-artifacts.HistoryRetention))
	res, err := tx.ExecContext(ctx, `
		DELETE FROM artifact_brand_history WHERE position IN (
			SELECT position FROM artifact_brand_history WHERE at < ?
			ORDER BY at, position LIMIT ?)`, cutoff, historyPruneBatch)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: prune the brand history: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
