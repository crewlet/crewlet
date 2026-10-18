package artifactupload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/artifactupload/vendor"
	"github.com/crewlet/crewlet/internal/objstore"
)

// stageProduced stores every file the run produced (an edit's result, a linted
// page, a copy of a CDN file, inline content) under its reservation.
//
// THE SAME RULE AS AN UPLOAD'S: the bytes are reserved before they move. A
// file whose bytes the artifact already holds is carried instead (a page linted
// to what its last version held, a CDN file it copied before); one a retry of
// this very operation stored already is named again; the rest are minted a
// key, held in the ledger and recorded, and only then put.
func (r *run) stageProduced(ctx context.Context) error {
	var produced []*pending
	for _, p := range r.sortedPaths() {
		if f := r.files[p]; f.produced && !f.hasObj {
			produced = append(produced, f)
		}
	}
	if len(produced) == 0 {
		return nil
	}
	now := r.s.now()
	hashes := make([]objstore.Hash, len(produced))
	for i, f := range produced {
		hashes[i] = objstore.HashOf(f.text)
		// THE FILE'S BOUNDS before a byte moves, through the one entry
		// constructor the version is validated by.
		probe := objstore.Object{Key: objstore.KeyAt(now), Hash: hashes[i], Size: int64(len(f.text))}
		if _, err := manifest.NewEntry(f.path, f.typ.Served, probe, false); err != nil {
			return classify(err)
		}
	}
	held := map[objstore.Hash]objstore.Object{}
	if !r.id.IsZero() {
		found, err := r.s.reads.HeldObjects(ctx, r.id, hashes)
		if err != nil {
			return fmt.Errorf("artifactupload: read what artifact %s holds: %w", r.id, err)
		}
		held = found
	}
	var fresh []file
	var total, largest int64
	for i, f := range produced {
		size := int64(len(f.text))
		if obj, ok := held[hashes[i]]; ok && obj.Size == size {
			if err := f.name(obj, true); err != nil {
				return err
			}
			continue
		}
		if r.stage != nil && !r.stageNew {
			if j := slices.IndexFunc(r.stage.Files, func(g file) bool {
				return g.Transport == transportGenerated && g.Path == f.path &&
					g.SHA256 == hashes[i] && g.State == fileStored
			}); j >= 0 {
				if err := f.name(r.stage.Files[j].object(), false); err != nil {
					return err
				}
				continue
			}
		}
		fresh = append(fresh, file{Path: f.path, ContentType: f.typ.Served, SHA256: hashes[i],
			Size: size, Key: r.s.objects.Mint(), Transport: transportGenerated, State: filePending})
		total += size
		largest = max(largest, size)
	}
	if len(fresh) == 0 {
		return nil
	}
	if r.stage == nil {
		// A PUBLISH WITH NO OPERATION KEY AND NO UPLOAD is never retried as
		// itself, so its reservation is its own, minted fresh.
		r.stage = &record{ID: uuid.NewString(), Principal: r.principal, Purpose: purposeStage,
			Artifact: artifactString(r.id), TurnID: r.c.TurnID, Created: now,
			State: stateFinalizing, FinalizeKey: r.c.OpKey}
		r.stageNew = true
	}
	giveBack, err := r.s.turns.spend(r.c.TurnID, total)
	if err != nil {
		return err
	}
	usage, err := r.s.reads.Usage(ctx)
	if err != nil {
		giveBack()
		return fmt.Errorf("artifactupload: read the company's usage: %w", err)
	}
	until := holdUntil(now.Add(objstore.PaceFor(largest)))
	if err := r.s.grow(ctx, r.stage.ID, r.principal, total, until, usage.Bytes); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		giveBack()
		return err
	}
	if err := r.recordFresh(ctx, fresh, until); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		giveBack()
		r.s.release(ctx, r.stage.ID)
		return err
	}
	for _, g := range fresh {
		f := r.files[g.Path]
		stored, err := r.s.objects.PutMinted(ctx, g.Key, bytes.NewReader(f.text), g.Size, //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			objstore.PutMeta{ContentType: g.ContentType})
		if err != nil {
			return fmt.Errorf("artifactupload: store %q: %w", g.Path, err)
		}
		if stored != g.object() {
			return fmt.Errorf("artifactupload: the store answered %+v for %q and the pipeline wrote %+v",
				stored, g.Path, g.object())
		}
		r.s.countBytes(g.Size, "engine")
		if err := f.name(stored, false); err != nil {
			return err
		}
	}
	updated, err := r.s.updateRecord(ctx, r.stage.ID, func(rec *record) error {
		for i := range rec.Files {
			if slices.ContainsFunc(fresh, func(g file) bool { return g.Key == rec.Files[i].Key }) {
				rec.Files[i].State = fileStored
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.stage = updated
	return nil
}

// recordFresh writes the files about to be stored into the stage's record,
// creating it on first use.
func (r *run) recordFresh(ctx context.Context, fresh []file, until time.Time) error {
	if r.stageNew {
		r.stage.Files = append(r.stage.Files, fresh...)
		if until.After(r.stage.Expires) {
			r.stage.Expires = until
		}
		wrote, err := r.s.writeRecord(ctx, r.stage, 0)
		if err != nil {
			return err
		}
		if wrote == 0 {
			return fmt.Errorf("%w: another attempt of this publish is running", ErrUploadConsumed)
		}
		r.stageNew = false
		return nil
	}
	updated, err := r.s.updateRecord(ctx, r.stage.ID, func(rec *record) error {
		rec.Files = append(rec.Files, fresh...)
		return nil
	})
	if err != nil {
		return err
	}
	r.stage = updated
	return nil
}

// name makes a file's entry the object holding its bytes.
func (f *pending) name(obj objstore.Object, carried bool) error {
	e, err := manifest.NewEntry(f.path, f.typ.Served, obj, carried)
	if err != nil {
		return classify(err)
	}
	f.entry, f.hasObj, f.carried = e, true, carried
	return nil
}

// manifest composes the next version and the keys it names that this publish
// uploaded.
func (r *run) manifest(mode manifest.Mode, entry string) (artifacts.Manifest, []objstore.Key, error) {
	changes := make([]manifest.Change, 0, len(r.files)+len(r.removes))
	for _, p := range r.sortedPaths() {
		f := r.files[p]
		if !f.hasObj {
			return artifacts.Manifest{}, nil, fmt.Errorf("artifactupload: %q reached the manifest with "+
				"no object", p)
		}
		e := f.entry
		e.Script = nil
		if f.typ.Essence() == "text/html" {
			e.Script = f.script
		}
		changes = append(changes, manifest.Change{Path: p, Entry: e})
	}
	if mode == manifest.ModePatch {
		for p := range r.removes {
			if _, held := manifest.EntryAt(r.base.Manifest, p); held {
				changes = append(changes, manifest.Change{Path: p, Remove: true})
			}
		}
	}
	var base artifacts.Manifest
	if r.hasBase {
		base = r.base.Manifest
	}
	m, err := manifest.Next(mode, base, entry, changes)
	if err != nil {
		return artifacts.Manifest{}, nil, classify(err)
	}
	if err := manifest.Validate(r.kind, m); err != nil {
		return artifacts.Manifest{}, nil, classify(err)
	}
	var uploaded []objstore.Key
	for _, e := range m.Files {
		if !e.Carried && !slices.Contains(uploaded, e.Object.Key) {
			uploaded = append(uploaded, e.Object.Key)
		}
	}
	return m, uploaded, nil
}

// searchText is the text the gallery indexes the version by: its entry's
// readable text, at most [artifacts.MaxSearchTextBytes].
func (r *run) searchText(ctx context.Context, m artifacts.Manifest) (string, error) {
	if m.Entry == "" {
		return "", nil
	}
	e, _ := manifest.EntryAt(m, m.Entry)
	typ, err := typeOf(e.Path, e.ContentType)
	if err != nil {
		return "", err
	}
	if !typ.Text {
		return "", nil
	}
	var text []byte
	if f := r.files[e.Path]; f != nil && f.text != nil && !f.carried {
		text = f.text
	} else {
		if text, err = readObject(ctx, r.s.objects, e.Object, true); err != nil {
			return "", fmt.Errorf("artifactupload: read the entry %q: %w", e.Path, err)
		}
	}
	limit := int(artifacts.MaxSearchTextBytes)
	if typ.Essence() == "text/html" {
		return lint.Text(lint.Source(text), limit), nil
	}
	return truncateText(string(text), limit), nil
}

// truncateText cuts s to at most n bytes at a character boundary.
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// fileTheme is what one page or stylesheet of the version does about the
// reader's appearance, and whether this publish brought its bytes.
type fileTheme struct {
	path string

	// fresh is a file this publish brings new bytes for; a carried one was
	// judged when it was published.
	fresh bool

	lint.Theme
}

// themeWarnings warns a version that ignores its reader's appearance, and every
// link a fresh file makes to a kit release this build does not serve.
//
// THE VERSION IS THE UNIT, as a page and the rules that theme it are often
// different files. It follows the reader when any page or stylesheet carries a
// dark-mode rule of its own, or when its pages link BOTH of the kit's theming
// files at a release the build serves: the kit's stylesheet alone follows the
// device rather than the reader (only the bootstrap writes the reader's chosen
// mode onto the page), and a link to a release the origin does not hold loads
// nothing at all. The kit's own rules are not in the version, which is why its
// links are read rather than its text.
func (r *run) themeWarnings(m artifacts.Manifest) {
	if r.kind != artifacts.KindHTML || m.Entry == "" {
		return
	}
	rules, sheet, bootstrap := false, "", ""
	for _, f := range r.themes {
		rules = rules || f.Rules
		for _, k := range f.Kit {
			if !r.s.kits.Serves(k.Version) {
				if f.fresh {
					r.warnings = append(r.warnings, lint.Finding{Path: f.path, Line: k.Line,
						Code: lint.CodeKitUnserved, Message: fmt.Sprintf("%s is a kit release "+
							"this engine does not serve, so the page loads no kit; %s",
							kitFile(k.Version, k.File), r.kitHint(k.File))})
				}
				continue
			}
			switch k.File {
			case artifacts.KitStylesheet:
				sheet = k.Version
			case artifacts.KitTheme:
				bootstrap = k.Version
			}
		}
	}
	switch {
	case rules || sheet != "" && bootstrap != "":
	case sheet != "":
		r.warnings = append(r.warnings, lint.Finding{Path: m.Entry, Code: lint.CodeNoDarkMode,
			Message: fmt.Sprintf("the version links the kit's %s without its %s, so it follows "+
				"the device's appearance rather than the reader's; add <script src=\"%s\"> "+
				"to the page's head", artifacts.KitStylesheet, artifacts.KitTheme,
				kitFile(sheet, artifacts.KitTheme))})
	case bootstrap != "":
		r.warnings = append(r.warnings, lint.Finding{Path: m.Entry, Code: lint.CodeNoDarkMode,
			Message: fmt.Sprintf("the version links the kit's %s without its %s, so nothing "+
				"styles the mode it applies; add <link rel=\"stylesheet\" href=\"%s\"> to the "+
				"page's head", artifacts.KitTheme, artifacts.KitStylesheet,
				kitFile(bootstrap, artifacts.KitStylesheet))})
	default:
		r.warnings = append(r.warnings, lint.Finding{Path: m.Entry, Code: lint.CodeNoDarkMode,
			Message: fmt.Sprintf("no page or stylesheet of the version carries a "+
				"prefers-color-scheme rule or links the kit's %s and %s, so it ignores the "+
				"reader's theme", artifacts.KitStylesheet, artifacts.KitTheme)})
	}
}

// kitHint is what to link instead of a kit release the build does not serve.
func (r *run) kitHint(file string) string {
	latest := r.s.kits.Latest()
	if latest == "" {
		return "this engine serves no kit, so style the page with rules of its own"
	}
	return "link " + kitFile(latest, file) + ", the newest it serves"
}

// kitFile is the path a page links one file of a kit release at.
func kitFile(version, file string) string { return artifacts.KitPrefix + version + "/" + file }

// fail answers a publish the domain or the pipeline refused, undoing what the
// run wrote: every object it produced is deleted and its reservation released,
// and the caller's upload is open again for another try. An outcome nobody
// could establish leaves everything for the retry of the same operation.
func (r *run) fail(ctx context.Context, cause error) error {
	if r.stage == nil || r.stageNew {
		return cause
	}
	ctx = context.WithoutCancel(ctx)
	if !refused(cause) {
		// AN OUTCOME NOBODY COULD ESTABLISH keeps everything for the retry,
		// and gives the lease back so the retry need not wait for it.
		if _, err := r.s.updateRecord(ctx, r.stage.ID, func(rec *record) error {
			rec.Claimed = time.Time{}
			return nil
		}); err != nil {
			log.WarnContext(ctx, "artifact_upload_record_failed", "upload", r.stage.ID, "error", err.Error())
		}
		return cause
	}
	var deleted []objstore.Key
	for _, f := range r.stage.Files {
		if f.Transport == transportGenerated && f.held() {
			if err := r.s.objects.Delete(ctx, f.Key); err != nil {
				log.WarnContext(ctx, "artifact_upload_delete_failed", "key", f.Key.String(),
					"error", err.Error())
				continue
			}
			deleted = append(deleted, f.Key)
		}
	}
	updated, err := r.s.updateRecord(ctx, r.stage.ID, func(rec *record) error {
		for i := range rec.Files {
			if slices.Contains(deleted, rec.Files[i].Key) {
				rec.Files[i].State = fileDeleted
			}
		}
		if rec.Purpose == purposeStage {
			rec.State = stateClosed
		} else {
			rec.State, rec.FinalizeKey = stateOpen, ""
		}
		rec.Claimed = time.Time{}
		return nil
	})
	if err != nil {
		return errors.Join(cause, fmt.Errorf("artifactupload: give the upload back after the "+
			"refusal: %w", err))
	}
	r.s.release(ctx, updated.ID)
	return cause
}

// close records a publish that landed: the files the version names are named,
// the outcome kept for a retry of the same operation, and then every object the
// engine wrote or streamed that the version does not name is deleted, its
// reservation released. A file the client put straight to the bucket and the
// version does not name stays held until the hold lapses: its put may still be
// in flight on a URL the engine cannot recall.
func (r *run) close(ctx context.Context, published artifactsdomain.Published, m artifacts.Manifest) {
	if r.stage == nil || r.stageNew {
		return
	}
	ctx = context.WithoutCancel(ctx)
	named := map[objstore.Key]bool{}
	for _, e := range m.Files {
		if !e.Carried {
			named[e.Object.Key] = true
		}
	}
	closed, err := r.s.updateRecord(ctx, r.stage.ID, func(rec *record) error {
		for i := range rec.Files {
			if named[rec.Files[i].Key] {
				rec.Files[i].State = fileNamed
			}
		}
		rec.State, rec.Claimed = stateClosed, time.Time{}
		rec.Outcome = &outcome{Published: published, Warnings: r.warnings}
		return nil
	})
	if err != nil {
		// THE VERSION IS RECORDED; only the account of it is not. Its bytes
		// stay held until the hold lapses, and a retry of the operation is
		// answered by the domain from its own record.
		log.WarnContext(ctx, "artifact_upload_record_failed", "upload", r.stage.ID, "error", err.Error())
		return
	}
	var deleted []objstore.Key
	for _, f := range closed.Files {
		if !f.held() || f.Transport == transportDirect {
			continue
		}
		if err := r.s.objects.Delete(ctx, f.Key); err != nil {
			log.WarnContext(ctx, "artifact_upload_delete_failed", "key", f.Key.String(), "error", err.Error())
			continue
		}
		deleted = append(deleted, f.Key)
	}
	if len(deleted) > 0 {
		if updated, err := r.s.updateRecord(ctx, closed.ID, func(rec *record) error {
			for i := range rec.Files {
				if slices.Contains(deleted, rec.Files[i].Key) {
					rec.Files[i].State = fileDeleted
				}
			}
			return nil
		}); err == nil {
			closed = updated
		} else {
			log.WarnContext(ctx, "artifact_upload_record_failed", "upload", closed.ID, "error", err.Error())
		}
	}
	r.s.release(ctx, closed.ID)
}

// typeOf is the media table's type for a path, refused as [ErrMediaType].
func typeOf(p, declared string) (mediatype.Type, error) {
	typ, err := mediatype.Resolve(p, declared)
	if err != nil {
		return mediatype.Type{}, fmt.Errorf("%w: %w", ErrMediaType, err)
	}
	return typ, nil
}

// readObject reads an object through the store's verifying reader: all of it
// where keep is set, and otherwise its first [sniffBytes], the rest read and
// verified all the same. A damaged object is an error, never short bytes.
func readObject(ctx context.Context, objects *objstore.Store, o objstore.Object, keep bool) ([]byte, error) {
	rc, err := objects.Open(ctx, o)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	if keep {
		return io.ReadAll(rc)
	}
	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return nil, err
	}
	return head[:n], nil
}

// headOf is what a file's format is judged from: all of a text file, the
// first bytes of a binary one.
func headOf(b []byte, text bool) []byte {
	if text || len(b) <= sniffBytes {
		return b
	}
	return b[:sniffBytes]
}

// vendorPath is where the version holds its copy of a CDN file: under
// [VendorPrefix], by host and path, so a copied stylesheet's relative
// references land on the copies of what they name. A query becomes a digest
// in the name, and a name the media table does not serve as the reference's
// kind gains the extension that kind is served as.
func vendorPath(ref lint.Ref) (string, error) {
	local := VendorPrefix + strings.ToLower(ref.URL.Hostname()) + path.Clean("/"+ref.URL.Path)
	if ref.URL.RawQuery != "" {
		sum := sha256.Sum256([]byte(ref.URL.RawQuery))
		ext := path.Ext(local)
		local = strings.TrimSuffix(local, ext) + "-" + hex.EncodeToString(sum[:4]) + ext
	}
	want := map[lint.RefKind]mediatype.Class{
		lint.RefScript: mediatype.ClassScript, lint.RefModule: mediatype.ClassScript,
		lint.RefStylesheet: mediatype.ClassStyle, lint.RefFont: mediatype.ClassFont,
	}[ref.Kind]
	typ, known := mediatype.For(local)
	switch {
	case known && (want == "" || typ.Class == want):
	case want == mediatype.ClassScript:
		local += ".js"
	case want == mediatype.ClassStyle:
		local += ".css"
	default:
		return "", fmt.Errorf("%s names no file type an artifact serves", ref.URL.Redacted())
	}
	p, err := manifest.CleanPath(local)
	if err != nil {
		return "", fmt.Errorf("%s cannot be held as a file of the version: %w", ref.URL.Redacted(), err)
	}
	return p, nil
}

// relativePath is target as a URL reference from a file in dir, both paths of
// the version, each segment escaped.
func relativePath(dir, target string) string {
	var from []string
	if dir != "." && dir != "" {
		from = strings.Split(dir, "/")
	}
	to := strings.Split(target, "/")
	common := 0
	for common < len(from) && common < len(to)-1 && from[common] == to[common] {
		common++
	}
	segments := make([]string, 0, len(from)-common+len(to)-common)
	for range len(from) - common {
		segments = append(segments, "..")
	}
	for _, seg := range to[common:] {
		segments = append(segments, url.PathEscape(seg))
	}
	return strings.Join(segments, "/")
}

// vendorRequest is the fetch a reference asks for.
func vendorRequest(ref lint.Ref, immutable bool) vendor.Request {
	kind := map[lint.RefKind]vendor.Kind{
		lint.RefScript: vendor.KindScript, lint.RefModule: vendor.KindModule,
		lint.RefStylesheet: vendor.KindStylesheet, lint.RefFont: vendor.KindFont,
		lint.RefAsset: vendor.KindAsset,
	}[ref.Kind]
	return vendor.Request{URL: ref.URL, Kind: kind, Integrity: ref.Integrity, Immutable: immutable}
}
