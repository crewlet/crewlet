package artifactupload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/objstore"
)

// run is one finalize in progress: a create's or a publish's way through the
// pipeline, holding what it read and what it wrote so a refusal can undo
// exactly what it did.
type run struct {
	s         *Service
	c         artifactsdomain.Call
	principal string
	kind      artifacts.Kind
	id        artifacts.ID

	headContract string
	headCaps     []artifacts.Capability
	base         artifactsdomain.Version
	hasBase      bool

	// upload is the caller's upload, claimed by this run; nil for none.
	upload *record

	// stage is the record the objects this run writes are reserved under:
	// the upload's, or the run's own, created on first use.
	stage    *record
	stageNew bool

	// files are the new version's files this run names, by path.
	files map[string]*pending

	// removes are the base paths a patch drops.
	removes map[string]bool

	// versions are the retained versions files were carried from, by
	// number.
	versions map[int]artifactsdomain.Version

	// vendored are the copies under [VendorPrefix] the version's pages and
	// stylesheets reference, by path.
	vendored map[string]bool

	// themes are what the version's pages and stylesheets, as this run read
	// them, do about the reader's appearance: the theme warnings' input.
	themes []fileTheme

	// formatted is the version as its kind's format read it, whole, by
	// [run.checkFormat] (a deck, a doc), and nil for a kind with no format:
	// what the gallery's search text is extracted from.
	formatted formatText

	warnings []lint.Finding

	// counts are the composed manifest's files by where their bytes came
	// from, nil until it is composed.
	counts *FileCounts
}

// pending is one file of the new version, as the run assembles it.
type pending struct {
	path string
	typ  mediatype.Type

	// entry is the file's entry where its bytes are an object already: a
	// carried one, or an upload kept as it arrived.
	entry   artifacts.Entry
	hasObj  bool
	carried bool

	// text is the file's bytes where the run holds them; produced reports
	// bytes the run must store, being new (an edit's result, a linted page,
	// inline content, a copy of a CDN file) rather than an upload's object.
	text     []byte
	produced bool

	// script is a page's policy.
	script *artifacts.ScriptPolicy
}

// newRun claims the caller's upload for this run, or finds the run's own
// reservation where a retry of the same operation made one.
func (s *Service) newRun(ctx context.Context, c artifactsdomain.Call, kind artifacts.Kind,
	id artifacts.ID, upload string, p purpose) (*run, error) {

	principal, err := principalOf(c)
	if err != nil {
		return nil, err
	}
	r := &run{s: s, c: c, principal: principal, kind: kind, id: id,
		files: map[string]*pending{}, removes: map[string]bool{}, vendored: map[string]bool{},
		versions: map[int]artifactsdomain.Version{}}
	if upload != "" {
		claimed, err := s.claim(ctx, c, principal, upload, p, id) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return nil, err
		}
		r.upload, r.stage = claimed, claimed
		return r, nil
	}
	if c.OpKey == "" {
		return r, nil
	}
	// A RETRY'S OWN RESERVATION is found by the operation it retries.
	stage := uuid.NewSHA1(StageNamespace, []byte(principal+"\n"+c.OpKey+"\n"+id.String())).String()
	existing, _, err := s.readRecord(ctx, stage)
	if err != nil {
		return nil, err
	}
	switch {
	case existing == nil || existing.Principal != principal:
		r.stage = &record{ID: stage, Principal: principal, Purpose: purposeStage,
			Artifact: artifactString(id), TurnID: c.TurnID, Created: s.now(),
			Expires: s.now(), State: stateFinalizing, FinalizeKey: c.OpKey, Claimed: s.now()}
		r.stageNew = true
	case existing.State == stateClosed:
		r.stage = existing
	default:
		taken, err := s.updateRecord(ctx, stage, func(rec *record) error { return s.takeClaim(rec) })
		if err != nil {
			return nil, err
		}
		r.stage = taken
	}
	return r, nil
}

// claimLease is how long one finalize holds its upload against a second run of
// the same operation beside it: the second is refused while the first may still
// be writing the objects both would name, so two attempts never compose two
// versions from one operation. A run that ends, however it ends, gives the
// lease back; only a node that died mid-run leaves one to lapse.
//
// FIFTEEN MINUTES: a finalize reads back and writes at most a publish's bytes
// and copies its CDN files, each copy bounded at vendor.FetchTimeout, which is
// minutes at the slowest; a retry within the lease of a run that died is told
// to wait rather than allowed to race a run that did not.
const claimLease = 15 * time.Minute

// takeClaim takes the claim on a record this operation already claimed, unless
// a run of it is still within its lease.
func (s *Service) takeClaim(rec *record) error {
	if now := s.now(); !rec.Claimed.IsZero() && now.Sub(rec.Claimed) < claimLease {
		return fmt.Errorf("%w: a publish under this operation key is still running; try again "+
			"when it has answered", ErrUploadConsumed)
	}
	rec.Claimed = s.now()
	return nil
}

// claim takes the caller's upload for one finalize: open and in its window, or
// already claimed by this very operation (its retry).
func (s *Service) claim(ctx context.Context, c artifactsdomain.Call, principal, upload string,
	p purpose, id artifacts.ID) (*record, error) {

	if _, err := s.ownRecord(ctx, principal, upload); err != nil {
		return nil, err
	}
	return s.updateRecord(ctx, upload, func(r *record) error {
		switch {
		case r.Purpose != p || r.Artifact != artifactString(id):
			return fmt.Errorf("%w: upload %s is not one for this %s", ErrInvalid, upload, p)
		case r.State == stateClosed && c.OpKey != "" && r.FinalizeKey == c.OpKey:
			return nil
		case r.State == stateFinalizing && c.OpKey != "" && r.FinalizeKey == c.OpKey:
			return s.takeClaim(r)
		case r.State != stateOpen:
			return fmt.Errorf("%w: upload %s is claimed by another publish", ErrUploadConsumed, upload)
		case !r.Expires.After(s.now()):
			return fmt.Errorf("%w: %s", ErrUploadExpired, upload)
		}
		r.State, r.FinalizeKey, r.Claimed = stateFinalizing, c.OpKey, s.now()
		return nil
	})
}

// answered is the outcome a retry of a publish that already closed its upload
// is answered with.
func (r *run) answered() (*outcome, bool) {
	if r.stage != nil && r.stage.State == stateClosed && r.stage.Outcome != nil {
		return r.stage.Outcome, true
	}
	return nil, false
}

func artifactString(id artifacts.ID) string {
	if id.IsZero() {
		return ""
	}
	return id.String()
}

// report is what the run's linter found and where the version's files came
// from.
func (r *run) report() Report { return Report{Warnings: r.warnings, Files: r.counts} }

// compose assembles the next version from the request: every file's bytes
// found, read back and checked, the edits applied, every page and stylesheet
// linted and its CDN references copied in, the objects the run wrote stored
// under its reservation, the contract resolved, and the manifest composed.
func (r *run) compose(ctx context.Context, mode manifest.Mode, specs []FileSpec, list []edits.Edit,
	entry, contract string, caps []artifacts.Capability) (artifactsdomain.PublishInput, error) {

	if caps == nil {
		caps = r.headCaps
	}
	if err := checkCapabilities(caps); err != nil {
		return artifactsdomain.PublishInput{}, err
	}
	if err := r.nameFiles(ctx, mode, specs); err != nil {
		return artifactsdomain.PublishInput{}, err
	}
	if err := r.applyEdits(ctx, list, entry); err != nil {
		return artifactsdomain.PublishInput{}, err
	}
	if mode == manifest.ModePatch {
		if err := r.keepBase(); err != nil {
			return artifactsdomain.PublishInput{}, err
		}
	}
	if err := r.checkNewText(); err != nil {
		return artifactsdomain.PublishInput{}, err
	}
	// ONLY A FRAMED KIND'S FILES ARE PAGES: a native kind serves no document
	// on the artifact origin, so its HTML (a deck's slides) is its format's
	// to judge, never linted, nonce-marked or given CDN copies as a page.
	if r.kind.Framed() {
		if err := r.lintFiles(ctx); err != nil {
			return artifactsdomain.PublishInput{}, err
		}
		if err := r.carryVendored(ctx); err != nil {
			return artifactsdomain.PublishInput{}, err
		}
	}
	resolved, err := r.s.resolveContract(r.kind, contract, r.headContract, caps)
	if err != nil {
		return artifactsdomain.PublishInput{}, err
	}
	if err := r.stageProduced(ctx); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return artifactsdomain.PublishInput{}, err
	}
	m, uploaded, err := r.manifest(mode, entry)
	if err != nil {
		return artifactsdomain.PublishInput{}, err
	}
	if err := r.checkFormat(ctx, m); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return artifactsdomain.PublishInput{}, err
	}
	search, err := r.searchText(ctx, m)
	if err != nil {
		return artifactsdomain.PublishInput{}, err
	}
	r.themeWarnings(m)
	r.counts = &FileCounts{}
	for _, e := range m.Files {
		if e.Carried {
			r.counts.Carried++
		} else {
			r.counts.Stored++
		}
	}
	return artifactsdomain.PublishInput{
		Contract: resolved, Capabilities: caps, Manifest: m, Uploaded: uploaded, SearchText: search,
	}, nil
}

// nameFiles finds where each named file's bytes are: the upload, an object
// the artifact holds, a retained version, or the request itself.
func (r *run) nameFiles(ctx context.Context, mode manifest.Mode, specs []FileSpec) error {
	var (
		inline  int64
		carries []objstore.Hash
		missing []string
	)
	versions := r.versions
	if r.hasBase {
		versions[r.base.Number] = r.base
	}
	for _, spec := range specs {
		p, err := manifest.CleanPath(spec.Path)
		if err != nil {
			return classify(err)
		}
		if err := authorPath(p); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return err
		}
		if spec.sources() != 1 {
			return fmt.Errorf("%w: %q names %d sources for its bytes and a file names exactly one "+
				"(upload, carry, from, remove or inline)", ErrInvalid, p, spec.sources())
		}
		if r.files[p] != nil || r.removes[p] {
			return fmt.Errorf("%w: %q is named twice in one publish", ErrInvalid, p)
		}
		if spec.Remove {
			if mode != manifest.ModePatch {
				return fmt.Errorf("%w: %q is removed in a whole publish, which states the full set; "+
					"leave it out instead", ErrInvalid, p)
			}
			r.removes[p] = true
			continue
		}
		typ, err := typeOf(p, "")
		if err != nil {
			return err
		}
		f := &pending{path: p, typ: typ}
		switch {
		case spec.Upload:
			if r.upload == nil {
				return fmt.Errorf("%w: %q names an upload and the publish names none", ErrInvalid, p)
			}
			i := r.upload.fileAt(p)
			if i < 0 {
				return fmt.Errorf("%w: upload %s declares no file %q", ErrInvalid, r.upload.ID, p)
			}
			up := r.upload.Files[i]
			if up.Transport == transportEngine && up.State != fileStored ||
				up.State == fileDeleted || up.State == fileStreaming {
				missing = append(missing, p)
				continue
			}
			upType, err := typeOf(p, up.ContentType) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return err
			}
			body, err := r.s.readBack(ctx, up)
			var gone *UploadMissing
			if errors.As(err, &gone) {
				missing = append(missing, p)
				continue
			}
			if err != nil {
				return err
			}
			e, err := manifest.NewEntry(p, upType.Served, up.object(), false)
			if err != nil {
				return classify(err)
			}
			f.typ, f.entry, f.hasObj = upType, e, true
			if upType.Text {
				f.text = body
			}
		case spec.Carry != "":
			carries = append(carries, spec.Carry)
			f.entry.Object.Hash = spec.Carry
			f.carried = true
		case spec.From != 0:
			v, ok := versions[spec.From]
			if !ok {
				if r.id.IsZero() {
					return fmt.Errorf("%w: %q carries from version %d of an artifact that has none",
						ErrInvalid, p, spec.From)
				}
				if v, err = r.s.reads.Version(ctx, r.id, spec.From); err != nil {
					return err
				}
				versions[spec.From] = v
			}
			e, held := manifest.EntryAt(v.Manifest, p)
			if !held {
				return fmt.Errorf("%w: version %d holds no file %q to carry", ErrInvalid, spec.From, p)
			}
			e.Carried = true
			f.entry, f.hasObj, f.carried, f.script = e, true, true, e.Script
		default:
			inline += int64(len(spec.Inline))
			if inline > artifacts.MaxInlineBytes {
				return fmt.Errorf("%w: the call carries more than the %d inline bytes a tool call "+
					"may; write the files into the sandbox and publish them from there", ErrInvalid,
					artifacts.MaxInlineBytes)
			}
			if err := checkContent(p, typ, headOf(spec.Inline, typ.Text), int64(len(spec.Inline))); err != nil {
				return err
			}
			f.text, f.produced = spec.Inline, true
		}
		r.files[p] = f
	}
	if len(missing) > 0 {
		return &UploadMissing{Paths: missing}
	}
	if len(carries) == 0 {
		return nil
	}
	held, err := r.s.reads.HeldObjects(ctx, r.id, carries)
	if err != nil {
		return fmt.Errorf("artifactupload: read what artifact %s holds: %w", r.id, err)
	}
	var gone []string
	for _, f := range r.files {
		if !f.carried || f.hasObj {
			continue
		}
		obj, ok := held[f.entry.Object.Hash]
		if !ok {
			gone = append(gone, f.path)
			continue
		}
		// THE BASE'S OWN ENTRY FOR THE OBJECT, where it names one, keeps the
		// type it was published as (an `.mp4` of audio) and a page's policy.
		served, script := f.typ.Served, (*artifacts.ScriptPolicy)(nil)
		if base, ok := r.baseEntry(obj.Key); ok && base.Path != "" {
			if typ, err := mediatype.Resolve(f.path, base.ContentType); err == nil {
				served, f.typ = typ.Served, typ
			}
			script = base.Script
		}
		e, err := manifest.NewEntry(f.path, served, obj, true)
		if err != nil {
			return classify(err)
		}
		f.entry, f.hasObj, f.script = e, true, script
	}
	if len(gone) > 0 {
		slices.Sort(gone)
		return &artifactsdomain.CarriedObjectGone{Paths: gone}
	}
	return nil
}

// sourceEntry is the entry at p of a version this run carries from: the base,
// or a version a file was carried from by number. A copy of a CDN file a
// carried page references is found in the version the page came from.
func (r *run) sourceEntry(p string) (artifacts.Entry, bool) {
	if r.hasBase {
		if e, ok := manifest.EntryAt(r.base.Manifest, p); ok {
			return e, true
		}
	}
	for _, v := range r.versions {
		if e, ok := manifest.EntryAt(v.Manifest, p); ok {
			return e, true
		}
	}
	return artifacts.Entry{}, false
}

// baseEntry is the base version's entry naming an object, false where none
// does.
func (r *run) baseEntry(k objstore.Key) (artifacts.Entry, bool) {
	for _, e := range r.base.Manifest.Files {
		if e.Object.Key == k {
			return e, true
		}
	}
	return artifacts.Entry{}, false
}

// applyEdits applies the request's edits to the base version's files, read
// back verified and, for a page, in its author's source form.
func (r *run) applyEdits(ctx context.Context, list []edits.Edit, entry string) error {
	if len(list) == 0 {
		return nil
	}
	if !r.hasBase {
		return fmt.Errorf("%w: a new artifact has no base version to edit", ErrInvalid)
	}
	def := entry
	if def == "" {
		def = r.base.Manifest.Entry
	}
	normalized, err := edits.Normalize(list, def)
	if err != nil {
		return err
	}
	texts := map[string][]byte{}
	types := map[string]mediatype.Type{}
	for i, p := range edits.Paths(normalized) {
		clean, err := manifest.CleanPath(p) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return classify(err)
		}
		if clean != p {
			return fmt.Errorf("%w: edit %d names %q, whose stored form is %q", ErrInvalid, i, p, clean)
		}
		if r.files[p] != nil || r.removes[p] {
			return fmt.Errorf("%w: %q is edited and named by a file of the same publish; do one "+
				"or the other", ErrInvalid, p)
		}
		e, held := manifest.EntryAt(r.base.Manifest, p)
		if !held {
			continue // edits.Apply refuses it, naming the edit
		}
		typ, err := typeOf(p, e.ContentType)
		if err != nil {
			return err
		}
		if !typ.Text {
			return fmt.Errorf("%w: %q is a %s file, and only text is edited", ErrInvalid, p, typ.Essence())
		}
		body, err := readObject(ctx, r.s.objects, e.Object, true)
		if err != nil {
			return fmt.Errorf("artifactupload: read %q of version %d: %w", p, r.base.Number, err)
		}
		if typ.Essence() == "text/html" {
			body = lint.Source(body)
		}
		texts[p], types[p] = body, typ
	}
	out, err := edits.Apply(texts, normalized, func(p string) int64 { return textLimit(p, r.base.Manifest) })
	if err != nil {
		return err
	}
	for p, text := range out {
		r.files[p] = &pending{path: p, typ: types[p], text: text, produced: true}
	}
	return nil
}

// textLimit is the bound an edited file is held to: the entry's, or any text
// file's.
func textLimit(p string, m artifacts.Manifest) int64 {
	if p == m.Entry {
		return artifacts.MaxEntryBytes
	}
	return artifacts.MaxTextFileBytes
}

// keepBase adds, in patch mode, every base file the request neither names nor
// removes, carried: the pages among them are linted already, but their copies
// of CDN files are recomputed with the rest ([run.carryVendored]).
func (r *run) keepBase() error {
	for _, e := range r.base.Manifest.Files {
		if r.files[e.Path] != nil || r.removes[e.Path] || strings.HasPrefix(e.Path, VendorPrefix) {
			continue
		}
		typ, err := typeOf(e.Path, e.ContentType)
		if err != nil {
			return err
		}
		e.Carried = true
		r.files[e.Path] = &pending{path: e.Path, typ: typ, entry: e, hasObj: true, carried: true, script: e.Script}
	}
	return nil
}

// checkNewText refuses, for an agent, a credential in any new text the
// publish carries, and in any path it names.
func (r *run) checkNewText() error {
	kind := r.c.Authority.Actor.Kind
	for _, p := range r.sortedPaths() {
		f := r.files[p]
		if err := checkCredentials(kind, "the path "+p, []byte(p)); err != nil {
			return err
		}
		if f.carried || f.text == nil {
			continue
		}
		if err := checkCredentials(kind, p, f.text); err != nil {
			return err
		}
	}
	return nil
}

// sortedPaths is every named path, in bytewise order, so a refusal names the
// same file on every attempt.
func (r *run) sortedPaths() []string {
	out := make([]string, 0, len(r.files))
	for p := range r.files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// lintFiles lints every page and stylesheet the publish brings new bytes for,
// and holds every new script to the import rule. Every file's refusals are
// gathered into one answer.
func (r *run) lintFiles(ctx context.Context) error {
	var refusals []lint.Finding
	hasWasm := false
	for p := range r.files {
		if strings.HasSuffix(strings.ToLower(p), ".wasm") {
			hasWasm = true
		}
	}
	for _, p := range r.sortedPaths() {
		f := r.files[p]
		if f.carried || f.text == nil {
			continue
		}
		opts := lint.Options{Hosts: r.s.hosts, Resolve: r.resolver(ctx, p, 0), HasWasm: hasWasm}
		var err error
		switch {
		case f.typ.Essence() == "text/html":
			var page lint.Page
			if page, err = lint.HTML(p, f.text, opts); err == nil {
				r.warnings = append(r.warnings, page.Warnings...)
				r.themes = append(r.themes, fileTheme{path: p, fresh: true,
					Theme: lint.ThemeOf(f.text, true)})
				policy := page.Policy
				f.script = &policy
				f.produce(page.Text)
			}
		case f.typ.Class == mediatype.ClassStyle:
			var (
				css      []byte
				warnings []lint.Finding
			)
			if css, warnings, err = lint.CSS(p, f.text, opts, nil); err == nil {
				r.warnings = append(r.warnings, warnings...)
				r.themes = append(r.themes, fileTheme{path: p, fresh: true,
					Theme: lint.ThemeOf(f.text, false)})
				f.produce(css)
			}
		case f.typ.Class == mediatype.ClassScript:
			for _, im := range lint.RemoteImports(lint.Imports(f.text)) {
				refusals = append(refusals, lint.Finding{Path: p, Line: im.Line, Code: lint.CodeRemoteImport,
					Message: fmt.Sprintf("the script imports %s, another origin, which a page loads at "+
						"view time; load it with a classic <script src> from an allowlisted CDN at an "+
						"exact version, or upload the module into the artifact", im.Specifier)})
			}
		}
		var refusal *lint.Refusal
		switch {
		case errors.As(err, &refusal):
			refusals = append(refusals, refusal.Findings...)
		case err != nil:
			return err
		}
	}
	if len(refusals) > 0 {
		return &lint.Refusal{Findings: refusals}
	}
	return nil
}

// produce records the bytes a file will hold: new bytes to store where they
// differ from the object it arrived as.
func (f *pending) produce(out []byte) {
	if f.hasObj && bytes.Equal(out, f.text) {
		return
	}
	f.text, f.produced, f.hasObj = out, true, false
}

// carryVendored completes the version's copies of CDN files: every copy a page
// or stylesheet of the version references (one it brings new bytes for, one it
// carries) is carried from the base version where this publish did not copy it
// again, and a base copy nothing references any more is dropped.
func (r *run) carryVendored(ctx context.Context) error {
	type ref struct {
		path, from string
		// fresh is a reference a file this publish brings new bytes for
		// makes: one the version must hold.
		fresh bool
	}
	queue := []ref{}
	for _, p := range r.sortedPaths() {
		f := r.files[p]
		isHTML := f.typ.Essence() == "text/html"
		isCSS := f.typ.Class == mediatype.ClassStyle
		if (!isHTML && !isCSS) || strings.HasPrefix(p, VendorPrefix) {
			continue
		}
		body, fresh := f.text, !f.carried && f.text != nil
		if !fresh {
			read, err := readObject(ctx, r.s.objects, f.entry.Object, true)
			if err != nil {
				return fmt.Errorf("artifactupload: read carried %q: %w", p, err)
			}
			body = read
			r.themes = append(r.themes, fileTheme{path: p, Theme: lint.ThemeOf(body, isHTML)})
			if isHTML && f.script == nil {
				policy, err := lint.Stored(p, body, lint.Options{Hosts: r.s.hosts})
				if err != nil {
					return fmt.Errorf("artifactupload: carried %q: %w", p, err)
				}
				f.script = &policy
			}
		}
		for _, local := range lint.LocalRefs(p, body, isHTML) {
			if strings.HasPrefix(local, VendorPrefix) {
				queue = append(queue, ref{path: local, from: p, fresh: fresh})
			}
		}
	}
	var missing []lint.Finding
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		p := next.path
		if r.files[p] != nil {
			r.vendored[p] = true
			continue
		}
		e, held := r.sourceEntry(p)
		if !held {
			// A COPY THE VERSION DOES NOT HOLD, named by bytes this publish
			// brings: an author pointing at the engine's folder by hand,
			// or an edit that changed a copy's path. A page carried as it
			// was is not refused for what it already referenced.
			if next.fresh {
				missing = append(missing, lint.Finding{Path: next.from, Code: lint.CodeVendorFailed,
					Message: fmt.Sprintf("the file references %s, a copy of a CDN file this version "+
						"does not hold; reference the CDN file's own URL and the engine copies it in", p)})
			}
			continue
		}
		if r.vendored[p] {
			continue
		}
		r.vendored[p] = true
		typ, err := typeOf(p, e.ContentType)
		if err != nil {
			return err
		}
		e.Carried = true
		r.files[p] = &pending{path: p, typ: typ, entry: e, hasObj: true, carried: true}
		if typ.Class == mediatype.ClassStyle {
			body, err := readObject(ctx, r.s.objects, e.Object, true)
			if err != nil {
				return fmt.Errorf("artifactupload: read carried %q: %w", p, err)
			}
			for _, local := range lint.LocalRefs(p, body, false) {
				if strings.HasPrefix(local, VendorPrefix) {
					queue = append(queue, ref{path: local, from: p})
				}
			}
		}
	}
	if len(missing) > 0 {
		return &lint.Refusal{Findings: missing}
	}
	if r.hasBase {
		for _, e := range r.base.Manifest.Files {
			if strings.HasPrefix(e.Path, VendorPrefix) && !r.vendored[e.Path] && r.files[e.Path] == nil {
				r.removes[e.Path] = true
			}
		}
	}
	return nil
}

// resolver is the linter's resolver for references in the file at from:
// each allowlisted CDN file copied into the version under [VendorPrefix] and
// answered as a path relative to from's directory.
func (r *run) resolver(ctx context.Context, from string, depth int) func(lint.Ref) (string, error) {
	if r.s.vendor == nil {
		return nil
	}
	return func(ref lint.Ref) (string, error) {
		local, err := vendorPath(ref)
		if err != nil {
			return "", err
		}
		if !r.vendored[local] {
			if err := r.vendorFile(ctx, ref, local, depth); err != nil {
				return "", err
			}
		}
		return relativePath(path.Dir(from), local), nil
	}
}

// maxVendorDepth bounds how deep a copied stylesheet's own references are
// followed: a page's stylesheet, the stylesheet it imports, and the fonts
// that one names.
const maxVendorDepth = 3

// vendorFile copies one CDN file into the version at local.
func (r *run) vendorFile(ctx context.Context, ref lint.Ref, local string, depth int) error {
	if depth >= maxVendorDepth {
		return fmt.Errorf("the file is %d stylesheets deep, and copying follows at most %d", depth, maxVendorDepth)
	}
	host := strings.ToLower(ref.URL.Hostname())
	immutable := ref.Kind != lint.RefStylesheet || host != "fonts.googleapis.com"
	got, err := r.s.vendor.Fetch(ctx, vendorRequest(ref, immutable))
	if err != nil {
		return err
	}
	typ, err := typeOf(local, "")
	if err != nil {
		return err
	}
	body := got.Body
	switch ref.Kind {
	case lint.RefModule:
		if imports := lint.Imports(body); len(imports) > 0 {
			return fmt.Errorf("the module imports %q; copying a module graph is a bundler's job, so "+
				"load the library's classic build, or upload the module and what it imports",
				imports[0].Specifier)
		}
	case lint.RefStylesheet:
		var warnings []lint.Finding
		body, warnings, err = lint.CSS(local, body,
			lint.Options{Hosts: r.s.hosts, Resolve: r.resolver(ctx, local, depth+1)}, ref.URL)
		if err != nil {
			return err
		}
		r.warnings = append(r.warnings, warnings...)
	}
	if err := checkContent(local, typ, headOf(body, typ.Text), int64(len(body))); err != nil {
		return err
	}
	r.vendored[local] = true
	r.files[local] = &pending{path: local, typ: typ, text: body, produced: true}
	return nil
}
