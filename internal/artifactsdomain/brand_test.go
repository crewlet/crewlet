package artifactsdomain_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ---- brand fixtures -------------------------------------------------------- //

// kitLogo is the drawing every kit's primary logo is.
const kitLogo = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 50"><rect width="200" height="50"/></svg>`

// kitIndex is a kit's canonical brand.json, its body text in textColour on
// the kit's light ground.
func kitIndex(t *testing.T, name, textColour string) []byte {
	t.Helper()
	raw := `{"v":1,"name":"` + name + `","logos":{"primary":"logo.svg"},` +
		`"colors":{"primary":"#2450e6","text":"` + textColour + `","light_ground":"#ffffff"},` +
		`"type":{"heading":{"kit":"geist"},"body":{"stack":"system-sans"}},"voice":{}}`
	b, err := brandformat.Decode([]byte(raw))
	if err != nil {
		t.Fatalf("the fixture's brand.json does not read: %v", err)
	}
	canonical, err := brandformat.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// createKit makes a brand kit owned by the caller: its index and its logo.
func (n *node) createKit(c artifactsdomain.Call, title string, index []byte) artifacts.ID {
	n.t.Helper()
	files := []artifacts.Entry{
		entryOf(n.t, brandformat.EntryPath, string(index), keyOf(title+"/brand.json")),
		entryOf(n.t, "logo.svg", kitLogo, keyOf(title+"/logo.svg")),
	}
	got, err := n.writer.Create(n.t.Context(), c, artifactsdomain.CreateInput{
		Kind: artifacts.KindBrand, Title: title, Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind,
		First: artifactsdomain.PublishInput{
			Manifest: artifacts.Manifest{Entry: brandformat.EntryPath, Files: files},
			Uploaded: []objstore.Key{files[0].Object.Key, files[1].Object.Key},
		},
	})
	if err != nil {
		n.t.Fatalf("create kit %q: %v", title, err)
	}
	return got.Artifact
}

// adoption is an adoption of version of kit, its files copied under tag: a
// stylesheet and the logo, each a fresh upload.
func adoption(t *testing.T, kit artifacts.ID, version int, index []byte, tag string) artifactsdomain.BrandAdoptInput {
	t.Helper()
	css := ":root {\n  --brand-primary: #2450e6;\n}\n/* " + tag + " */\n"
	in := artifactsdomain.BrandAdoptInput{Kit: kit, Version: version, BrandJSON: index}
	for _, f := range []struct{ slot, contentType, content string }{
		{string(brandformat.SlotCSS), "text/css; charset=utf-8", css},
		{string(brandformat.SlotLogoPrimary), "image/svg+xml", kitLogo},
	} {
		obj := objstore.Object{Key: keyOf("adopt/" + tag + "/" + f.slot),
			Hash: objstore.HashOf([]byte(f.content)), Size: int64(len(f.content))}
		in.Files = append(in.Files, artifactsdomain.BrandFile{Slot: f.slot, ContentType: f.contentType, Object: obj})
		in.Uploaded = append(in.Uploaded, obj.Key)
	}
	return in
}

// admin is an administrator adopting the company's brand.
func admin() artifactsdomain.Call { return person("maya", artifacts.RoleViewer) }

// brandNow is the adopted brand as n holds it now.
func (n *node) brandNow() (artifactsdomain.Adoption, bool) {
	n.t.Helper()
	a, adopted, err := n.reader.Brand(n.t.Context(), statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		n.t.Fatalf("read the brand: %v", err)
	}
	return a, adopted
}

// ---- the cases ------------------------------------------------------------- //

// AN ADOPTION SNAPSHOTS ONE VERSION: its canonical index, the stylesheet and
// every file it serves, each its own object, in the order the brand serves
// them; the files are allowlisted by their hash; the version is labelled, and
// the kit's history says who labelled it.
func TestAnAdoptionSnapshotsOneVersionAndServesItsFiles(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	index := kitIndex(t, "Acme", "#111111")
	kit := n.createKit(person("jane", artifacts.RoleOwner), "Acme brand", index)
	in := adoption(t, kit, 1, index, "first")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), in); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	got, adopted := n.brandNow()
	switch {
	case !adopted:
		t.Fatal("nothing is adopted after an adoption")
	case got.Kit != kit || got.KitVersion != 1 || string(got.BrandJSON) != string(index):
		t.Fatalf("the adoption reads %s v%d %s", got.Kit, got.KitVersion, got.BrandJSON)
	case got.CSSSha != string(in.Files[0].Object.Hash) || len(got.Files) != 2 ||
		got.Files[0] != in.Files[0] || got.Files[1] != in.Files[1]:
		t.Fatalf("the adoption serves %+v, want %+v", got.Files, in.Files)
	case got.AdoptedBy.Name != "maya":
		t.Errorf("the adoption names %q as its adopter", got.AdoptedBy.Name)
	case got.KitAuthor.Name != "jane" || got.KitAuthor.Kind != iam.ActorHuman:
		// WHO WROTE THE BRAND is kept beside who adopted it, copied from the
		// version by the apply.
		t.Errorf("the adoption names %+v as the kit's author, want jane", got.KitAuthor)
	}
	for _, f := range in.Files {
		served, err := n.reader.BrandFile(t.Context(), f.Object.Hash)
		if err != nil || served.Object != f.Object || served.Slot != f.Slot {
			t.Errorf("the route's allowlist answers %+v (%v) for %s", served, err, f.Slot)
		}
	}
	if _, err := n.reader.BrandFile(t.Context(), objstore.HashOf([]byte("never adopted"))); !errors.Is(err,
		artifactsdomain.ErrNotFound) {
		t.Errorf("a hash no adoption serves answered %v", err)
	}
	v, err := n.reader.Version(t.Context(), kit, 1)
	if err != nil || v.Label != "Adopted "+monday.Format(time.DateOnly) {
		t.Errorf("the adopted version is labelled %q (%v)", v.Label, err)
	}
	history, err := n.reader.History(t.Context(), kit, "", 10)
	if err != nil || len(history.Entries) == 0 || history.Entries[0].Op != string(artifactsdomain.OpPatch) ||
		history.Entries[0].Actor.Name != "maya" {
		t.Errorf("the kit's history does not say the administrator labelled it: %+v (%v)", history.Entries, err)
	}
}

// THE ADOPTED BRAND OUTLIVES ITS KIT: trashing the kit and publishing a new
// version change nothing a page is served, because the snapshot names its
// own objects and only an adoption changes it.
//
// Mutation: read the brand through the kit's head rather than the snapshot,
// and a draft published after the adoption restyles every page.
func TestTheAdoptedBrandOutlivesItsKitAndIgnoresItsDrafts(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	index := kitIndex(t, "Acme", "#111111")
	kit := n.createKit(jane, "Acme brand", index)
	in := adoption(t, kit, 1, index, "kept")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), in); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	draft := kitIndex(t, "Acme Draft", "#222222")
	files := []artifacts.Entry{entryOf(t, brandformat.EntryPath, string(draft), keyOf("draft/brand.json")),
		entryOf(t, "logo.svg", kitLogo, keyOf("Acme brand/logo.svg"))}
	files[1].Carried = true
	if _, err := n.writer.Publish(t.Context(), jane, artifactsdomain.PublishInput{
		Artifact: kit, Base: 1,
		Manifest: artifacts.Manifest{Entry: brandformat.EntryPath, Files: files},
		Uploaded: []objstore.Key{files[0].Object.Key},
	}); err != nil {
		t.Fatalf("publish a draft: %v", err)
	}
	if _, err := n.writer.Trash(t.Context(), jane, kit); err != nil {
		t.Fatalf("trash the kit: %v", err)
	}
	got, adopted := n.brandNow()
	if !adopted || string(got.BrandJSON) != string(index) || got.CSSSha != string(in.Files[0].Object.Hash) {
		t.Errorf("after a draft and the trash the brand reads %s (adopted %v)", got.BrandJSON, adopted)
	}
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), adoption(t, kit, 2, draft, "trashed")); !errors.Is(err,
		artifactsdomain.ErrTrashed) {
		t.Errorf("a version of a kit in the trash was adopted: %v", err)
	}
}

// AN ADOPTION'S TEXT MUST BE READABLE ON ITS GROUND, refused before anything
// is appended and answered with the pair that refused it.
//
// Mutation: drop the contrast check from CheckBrandAdoptable and the grey text is
// adopted.
func TestAnUnreadableBrandIsNeverAdopted(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	grey := kitIndex(t, "Acme", "#cccccc")
	kit := n.createKit(person("jane", artifacts.RoleOwner), "Grey brand", grey)
	_, err := n.writer.AdoptBrand(t.Context(), admin(), adoption(t, kit, 1, grey, "grey"))
	var contrast *artifactsdomain.BrandContrast
	switch {
	case !errors.As(err, &contrast) || !errors.Is(err, artifactsdomain.ErrBrandContrast):
		t.Fatalf("a grey text on white was answered %v", err)
	case contrast.Pair.Foreground != "text" || contrast.Pair.Ratio >= brandformat.MinTextContrast:
		t.Errorf("the refusal names %+v", contrast.Pair)
	}
	if _, adopted := n.brandNow(); adopted {
		t.Error("an unreadable brand was adopted anyway")
	}
}

// AN ADOPTION NAMES ONLY WHAT IT UPLOADED, AND ONLY WHAT THE BRAND SERVES: a
// file another write uploaded, a key past the window a write may name, a slot
// the brand has no file for and an index not in its canonical form are each
// refused before the append.
func TestAnAdoptionNamesOnlyTheFreshCopiesItsBrandServes(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	index := kitIndex(t, "Acme", "#111111")
	kit := n.createKit(person("jane", artifacts.RoleOwner), "Acme brand", index)
	for name, mutate := range map[string]func(*artifactsdomain.BrandAdoptInput){
		"a file it did not upload": func(in *artifactsdomain.BrandAdoptInput) { in.Uploaded = in.Uploaded[:1] },
		"a stale key": func(in *artifactsdomain.BrandAdoptInput) {
			old := keyAt("stale", monday.Add(-objstore.RecordWithin-time.Minute))
			in.Files[1].Object.Key, in.Uploaded[1] = old, old
		},
		"a slot the brand has no file for": func(in *artifactsdomain.BrandAdoptInput) {
			in.Files[1].Slot = string(brandformat.SlotLogoMark)
		},
		"a missing stylesheet": func(in *artifactsdomain.BrandAdoptInput) { in.Files = in.Files[1:] },
		"an index not canonical": func(in *artifactsdomain.BrandAdoptInput) {
			in.BrandJSON = []byte(strings.Replace(string(in.BrandJSON), "{", "{ ", 1))
		},
		"a logo of the wrong type": func(in *artifactsdomain.BrandAdoptInput) {
			in.Files[1].ContentType = "text/html"
		},
	} {
		in := adoption(t, kit, 1, index, "bad")
		mutate(&in)
		if _, err := n.writer.AdoptBrand(t.Context(), admin(), in); err == nil {
			t.Errorf("an adoption naming %s was written", name)
		}
	}
	if _, adopted := n.brandNow(); adopted {
		t.Error("a refused adoption left a brand adopted")
	}
}

// A FRAMED VERSION PINS THE STYLESHEET ADOPTED WHEN IT IS PUBLISHED: none
// before an adoption, the adopted one after it, none for a page that opts
// out, and a native kind never pins one (the dashboard draws it live).
//
// Mutation: leave Brand off the version payload and the framed page pins
// nothing.
func TestAFramedVersionPinsTheStylesheetAdoptedWhenItIsPublished(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	before := n.create(jane, "Before", upload(t, "index.html", "<p>before</p>")).Artifact
	index := kitIndex(t, "Acme", "#111111")
	kit := n.createKit(jane, "Acme brand", index)
	in := adoption(t, kit, 1, index, "pinned")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), in); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	after := n.create(jane, "After", upload(t, "index.html", "<p>after</p>")).Artifact
	off := page(upload(t, "index.html", `<html data-crewlet-brand="off"><p>own</p>`))
	off.BrandOff = true
	opted, err := n.writer.Create(t.Context(), jane, artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: "Own look", Owner: "jane", OwnerKind: jane.Authority.Actor.Kind,
		First: off,
	})
	if err != nil {
		t.Fatalf("create the opted-out page: %v", err)
	}
	for _, c := range []struct {
		name string
		id   artifacts.ID
		want string
	}{
		{"a page published before any adoption", before, ""},
		{"a page published after it", after, string(in.Files[0].Object.Hash)},
		{"a page that opts out", opted.Artifact, ""},
		{"the native kit itself", kit, ""},
	} {
		v, err := n.reader.Version(t.Context(), c.id, 1)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if v.Brand != c.want {
			t.Errorf("%s pins %q, want %q", c.name, v.Brand, c.want)
		}
	}
}

// A SUPERSEDED ADOPTION IS SERVED WHILE A RETAINED VERSION PINS IT, and no
// longer: the next adoption keeps the files of the one a page pins, drops the
// files nothing pins, and a clear withdraws the brand while a pinned page keeps
// its stylesheet.
//
// Mutation: prune every non-current adoption's rows, and the pinned page's
// stylesheet answers not found.
func TestASupersededAdoptionIsServedWhileAVersionPinsIt(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	index := kitIndex(t, "Acme", "#111111")
	kit := n.createKit(jane, "Acme brand", index)

	unpinned := adoption(t, kit, 1, index, "unpinned")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), unpinned); err != nil {
		t.Fatalf("adopt the first: %v", err)
	}
	pinned := adoption(t, kit, 1, index, "pinned")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), pinned); err != nil {
		t.Fatalf("adopt the second: %v", err)
	}
	if _, err := n.reader.BrandFile(t.Context(), unpinned.Files[0].Object.Hash); !errors.Is(err,
		artifactsdomain.ErrNotFound) {
		t.Errorf("a superseded stylesheet nothing pins is still served: %v", err)
	}
	n.create(jane, "Pins the second", upload(t, "index.html", "<p>pinned</p>"))
	third := adoption(t, kit, 1, index, "third")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), third); err != nil {
		t.Fatalf("adopt the third: %v", err)
	}
	if _, err := n.writer.ClearBrand(t.Context(), admin()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, adopted := n.brandNow(); adopted {
		t.Error("a brand is adopted after a clear")
	}
	for _, f := range pinned.Files {
		if _, err := n.reader.BrandFile(t.Context(), f.Object.Hash); err != nil {
			t.Errorf("the pinned adoption's %s is not served: %v", f.Slot, err)
		}
	}
	if _, err := n.reader.BrandFile(t.Context(), third.Files[0].Object.Hash); !errors.Is(err,
		artifactsdomain.ErrNotFound) {
		t.Errorf("the cleared adoption's stylesheet, which nothing pins, is still served: %v", err)
	}
	// A CLEAR WHEN NOTHING IS ADOPTED appends nothing and answers success.
	if _, err := n.writer.ClearBrand(t.Context(), admin()); err != nil {
		t.Errorf("a second clear: %v", err)
	}
}

// lastBrandRecord is the body of the newest brand record on n's log, as a
// backup opens it before handing it to a declaration's reader: the signed
// frame verified (ADR-0031, ADR-0075).
func (n *node) lastBrandRecord(op artifactsdomain.OpKind) []byte {
	n.t.Helper()
	records := statelog.VerifiedLog{Log: n.log, Verifier: n.verifier}
	last, err := n.log.End(n.t.Context())
	if err != nil {
		n.t.Fatalf("read the log's end: %v", err)
	}
	for seq := last; seq > 0; seq-- {
		_, payload, _, ok, err := records.At(n.t.Context(), seq)
		if err != nil || !ok {
			n.t.Fatalf("read the log at %d: ok %v, %v", seq, ok, err)
		}
		env, err := artifactsdomain.DecodeEnvelope(payload)
		if err == nil && env.Subject.Kind == artifactsdomain.KindBrand && env.Op == op {
			return payload
		}
	}
	n.t.Fatalf("the log holds no brand %s record", op)
	return nil
}

// brandFileRows is every artifact_brand_files row n holds, read by the walk
// the audit and the backup share and spelled as it spells them.
func (n *node) brandFileRows() map[string]objstore.Object {
	n.t.Helper()
	query, err := artifactsdomain.BrandFileReferences.ReferencesAfter(100)
	if err != nil {
		n.t.Fatalf("the brand files' walk: %v", err)
	}
	out := map[string]objstore.Object{}
	err = n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), query, "")
		if err != nil {
			return err
		}
		defer rows.Close()
		return artifactsdomain.BrandFileReferences.ScanReferences(rows, func(obj objstore.Object, namedBy string) error {
			out[namedBy] = obj
			return nil
		})
	})
	if err != nil {
		n.t.Fatalf("walk the brand files: %v", err)
	}
	return out
}

// AN ADOPTION NAMES THE FILES ITS ROWS NAME (ADR-0075). A backup reads the
// artifacts log past its store copy and carries every object a record names,
// because a restore replays those records into the rows — so what the
// declaration's reader says an adoption writes has to be what the applier
// wrote: one row per file it serves, spelled as the walk spells it, each the
// fresh copy the adoption uploaded. A clear removes rows it does not list and
// answers none; a record of another kind answers none; and one a newer build
// wrote, whose scope reaches the brand, is the refusal a backup recognises.
//
// Mutation: answer an adoption's files under the kit's id rather than the
// stylesheet's hash, and the walk and the record disagree on every row.
func TestAnAdoptionNamesTheFilesItsRowsName(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	index := kitIndex(t, "Acme", "#111111")
	kit := n.createKit(person("jane", artifacts.RoleOwner), "Acme brand", index)
	in := adoption(t, kit, 1, index, "named")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), in); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	records := artifactsdomain.BrandFileReferences.Records
	adopted := n.lastBrandRecord(artifactsdomain.OpAdopt)
	got, err := records(adopted)
	if err != nil || len(got) != len(in.Files) {
		t.Fatalf("an adoption: Records = %+v, %v; want one row per file it serves", got, err)
	}
	walked := n.brandFileRows()
	if len(walked) != len(in.Files) {
		t.Fatalf("the walk finds %d rows after one adoption of %d files: %+v", len(walked), len(in.Files), walked)
	}
	for i, f := range in.Files {
		want := objstore.RecordReference{
			NamedBy: objstore.JoinOwner(string(in.Files[0].Object.Hash), f.Slot), Object: f.Object, Names: true,
		}
		if got[i] != want {
			t.Errorf("the record says it writes %+v, want %+v", got[i], want)
		}
		if obj, ok := walked[want.NamedBy]; !ok || obj != f.Object {
			t.Errorf("the applied row %s reads as %+v (found %v); the record said %+v",
				want.NamedBy, obj, ok, f.Object)
		}
	}

	if _, err := n.writer.ClearBrand(t.Context(), admin()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, err := records(n.lastBrandRecord(artifactsdomain.OpClear)); err != nil || got != nil {
		t.Errorf("a clear: Records = %+v, %v; want none — it removes rows it does not list", got, err)
	}
	if rows := n.brandFileRows(); len(rows) != 0 {
		t.Errorf("a cleared adoption nothing pins leaves rows %+v", rows)
	}

	// THE KIT'S CREATE is the log's first record: it names files, in
	// artifact_objects, and writes no brand file.
	_, create, _, ok, err := statelog.VerifiedLog{Log: n.log, Verifier: n.verifier}.At(t.Context(), 1)
	if err != nil || !ok {
		t.Fatalf("read the log's first record: ok %v, %v", ok, err)
	}
	if got, err := records(create); err != nil || got != nil {
		t.Errorf("the kit's create: Records = %+v, %v; want none", got, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(adopted, &doc); err != nil {
		t.Fatal(err)
	}
	doc["v"] = artifactsdomain.RecordVersion + 1
	newer, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := records(newer); !errors.Is(err, objstore.ErrRecordUnreadable) {
		t.Errorf("an adoption a newer build wrote: %v; want ErrRecordUnreadable", err)
	}
	if _, err := records([]byte("not a record")); err == nil {
		t.Error("bytes no build reads were answered as naming nothing")
	}
}

// A SUPERSEDED ADOPTION GOES WITH THE LAST VERSION THAT PINS IT, in the apply
// that retires or purges that version, not at some later adoption: a page's
// retention retiring one pinned version keeps the brand served while another
// page still pins it, and purging that page withdraws it.
//
// Mutation: prune only in the adoption and clear applies, and the purged
// page's brand is still served.
func TestASupersededAdoptionGoesWithTheLastVersionThatPinsIt(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	index := kitIndex(t, "Acme", "#111111")
	kit := n.createKit(jane, "Acme brand", index)
	old := adoption(t, kit, 1, index, "old")
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), old); err != nil {
		t.Fatalf("adopt the old brand: %v", err)
	}
	saves := n.create(jane, "Saves itself", upload(t, "index.html", "<p>1</p>")).Artifact
	kept := n.create(jane, "Kept as it was", upload(t, "index.html", "<p>kept</p>")).Artifact
	if _, err := n.writer.AdoptBrand(t.Context(), admin(), adoption(t, kit, 1, index, "new")); err != nil {
		t.Fatalf("adopt the new brand: %v", err)
	}
	served := func() bool {
		t.Helper()
		_, err := n.reader.BrandFile(t.Context(), old.Files[0].Object.Hash)
		if err != nil && !errors.Is(err, artifactsdomain.ErrNotFound) {
			t.Fatalf("read the old stylesheet: %v", err)
		}
		return err == nil
	}

	// RETENTION RETIRES THE FIRST PAGE'S PINNED VERSION, while the second
	// page still pins the old brand.
	for number := 2; number <= artifacts.VersionsKeptUnlabelled+1; number++ {
		in := page(upload(t, "index.html", fmt.Sprintf("<p>%d</p>", number)))
		in.Artifact, in.Base = saves, number-1
		if _, err := n.writer.Publish(t.Context(), jane, in); err != nil {
			t.Fatalf("publish %d: %v", number, err)
		}
	}
	if slices.Contains(n.versionNumbers(saves), 1) {
		t.Fatal("retention kept the first version")
	}
	if !served() {
		t.Error("the old brand went while a retained version still pins it")
	}

	// PURGING THE LAST PAGE THAT PINS IT withdraws it in the same apply.
	if _, err := n.writer.Purge(t.Context(), jane, kept); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if served() {
		t.Error("the old brand is served after the last version pinning it was purged")
	}
}

// AN ADOPTION OF A VERSION GONE BY ITS APPLY IS REFUSED THERE, on every node:
// retention or a purge can remove the version between the decide and the
// apply, and the record is refused `version_not_found` with nothing adopted.
//
// Mutation: skip the version check in applyAdopt and the record adopts a
// version no node holds.
func TestAnAdoptionOfAVersionGoneByItsApplyIsRefusedThere(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	id := newID()
	e.apply(handCreate(t, id, "op-create", suiteObject("kit", "<p>kit</p>"), limits()), monday)
	in := adoption(t, artifacts.ID{}, 7, nil, "gone")
	e.apply(handRecord(t, artifactsdomain.BrandSubject(), artifactsdomain.OpAdopt, "op-adopt",
		artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{artifactsdomain.BrandTerm(),
			{Kind: artifactsdomain.TermArtifact, ID: id}}},
		artifactsdomain.BrandAdoptPayload{V: 1, Kit: id, KitVersion: 7, BrandJSON: []byte(`{"v":1}`),
			CSSSha: string(in.Files[0].Object.Hash), Files: in.Files, Authority: suiteAuthority()}),
		monday.Add(time.Minute))
	if got := e.count(`SELECT COUNT(*) FROM artifact_brand_history WHERE op_id = 'op-adopt' AND
		outcome = 'refused' AND reason = 'version_not_found'`); got != 1 {
		t.Errorf("the adoption of a version the kit does not keep was not refused version_not_found")
	}
	if got := e.count(`SELECT COUNT(*) FROM artifact_brand`) + e.count(`SELECT COUNT(*) FROM artifact_brand_files`); got != 0 {
		t.Errorf("a refused adoption wrote %d brand rows", got)
	}
}

// A PIN OF A STYLESHEET PRUNED BEFORE ITS APPLY FOLLOWS THE ADOPTION AT THE
// APPLY: a framed create decided against an adoption that a later adoption
// superseded (and pruned, nothing pinning it yet) is stored pinning the
// stylesheet adopted when it applies, never one the route no longer serves.
//
// Mutation: store the record's pin unchecked and the version pins a
// stylesheet nothing serves.
func TestAPinOfAPrunedStylesheetFollowsTheAdoptionAtTheApply(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	kit := newID()
	e.apply(handCreate(t, kit, "op-kit", suiteObject("kit", "<p>kit</p>"), limits()), monday)
	adopt := func(opID, tag string) artifactsdomain.BrandAdoptInput {
		in := adoption(t, artifacts.ID{}, 1, nil, tag)
		e.apply(handRecord(t, artifactsdomain.BrandSubject(), artifactsdomain.OpAdopt, opID,
			artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{artifactsdomain.BrandTerm(),
				{Kind: artifactsdomain.TermArtifact, ID: kit}}},
			artifactsdomain.BrandAdoptPayload{V: 1, Kit: kit, KitVersion: 1, BrandJSON: []byte(`{"v":1}`),
				CSSSha: string(in.Files[0].Object.Hash), Files: in.Files, Authority: suiteAuthority()}),
			monday.Add(time.Minute))
		return in
	}
	first := adopt("op-first", "first")
	second := adopt("op-second", "second")
	if got := e.count(`SELECT COUNT(*) FROM artifact_brand_files WHERE css_sha = ?`,
		string(first.Files[0].Object.Hash)); got != 0 {
		t.Fatalf("the superseded adoption nothing pins kept %d file rows", got)
	}

	page := newID()
	payload := createPayload(page)
	payload.Version.Brand = string(first.Files[0].Object.Hash)
	payload.Authority = suiteAuthority()
	e.apply(handRecord(t, artifactsdomain.ArtifactSubject(mustID(page)), artifactsdomain.OpCreate,
		"op-stale-pin", artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{
			{Kind: artifactsdomain.TermArtifact, ID: page}, artifactsdomain.BrandTerm()}}, payload),
		monday.Add(2*time.Minute))
	if got := e.count(`SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = ? AND brand = ?`,
		page, string(second.Files[0].Object.Hash)); got != 1 {
		t.Errorf("the version does not pin the stylesheet adopted at its apply")
	}
}

// TWO NODES ADOPTING AND PUBLISHING ON ONE LOG CONVERGE, and a replay rebuilds
// them: the adoption's snapshot, its file rows and their prune, a framed
// version's pin, the kit's label and every history row are the same bytes on
// every node, because each is a function of the record and the rows at its
// position alone.
//
// Mutation: stamp the adoption with the applying node's clock rather than the
// broker's, and the brand rows differ between the nodes.
func TestTwoNodesConvergeOnTheAdoptedBrand(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	jane := person("jane", artifacts.RoleOwner)
	index := kitIndex(t, "Acme", "#111111")
	kit := a.createKit(jane, "Acme brand", index)
	if _, err := a.writer.AdoptBrand(t.Context(), admin(), adoption(t, kit, 1, index, "a")); err != nil {
		t.Fatalf("adopt on a: %v", err)
	}
	b.drain()
	b.create(jane, "Pinned on b", upload(t, "index.html", "<p>b</p>"))
	if _, err := b.writer.AdoptBrand(t.Context(), admin(), adoption(t, kit, 1, index, "b")); err != nil {
		t.Fatalf("adopt on b: %v", err)
	}
	a.drain()
	if _, err := a.writer.ClearBrand(t.Context(), admin()); err != nil {
		t.Fatalf("clear on a: %v", err)
	}
	a.drain()
	b.drain()
	replay := newNode(t, log, openStore(t, "replay.db"), "node-c")
	replay.drain()
	for _, table := range []string{"artifact_brand", "artifact_brand_files", "artifact_brand_history",
		"artifact_versions", "artifact_history"} {
		want := a.table(table)
		if got := b.table(table); got != want {
			t.Errorf("%s differs between the two writing nodes:\n a: %s\n b: %s", table, want, got)
		}
		if got := replay.table(table); got != want {
			t.Errorf("%s differs on the node that replayed the log:\n a: %s\n c: %s", table, want, got)
		}
	}
	if a.table("artifact_brand_files") == "" || a.table("artifact_brand_history") == "" {
		t.Fatal("the compared brand rows are empty, so the comparison proves nothing")
	}
}

// A DRAFT IS NEVER ADOPTED: a kit saved with its name alone, or with its name
// and its logo, is refused at the adoption as a *BrandIncomplete naming each
// field it leaves out, before anything is appended, and nothing is adopted.
//
// Mutation: drop the completeness check from CheckBrandAdoptable and the
// name-only draft is adopted.
func TestADraftIsNeverAdopted(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	for name, c := range map[string]struct {
		raw     string
		missing []string
	}{
		"the name alone":    {`{"v":1,"name":"Acme"}`, []string{"logos.primary", "colors.primary"}},
		"a name and a logo": {`{"v":1,"name":"Acme","logos":{"primary":"logo.svg"}}`, []string{"colors.primary"}},
		"a name and a colour": {`{"v":1,"name":"Acme","colors":{"primary":"#2450e6"}}`,
			[]string{"logos.primary"}},
	} {
		b, err := brandformat.Decode([]byte(c.raw))
		if err != nil {
			t.Fatalf("%s: the draft does not read: %v", name, err)
		}
		index, err := brandformat.Canonical(b)
		if err != nil {
			t.Fatal(err)
		}
		kit := n.createKit(person("jane", artifacts.RoleOwner), "Draft "+name, index)
		in := adoption(t, kit, 1, index, "draft-"+name)
		if b.Logos.Primary == "" {
			in.Files, in.Uploaded = in.Files[:1], in.Uploaded[:1]
		}
		_, err = n.writer.AdoptBrand(t.Context(), admin(), in)
		var incomplete *artifactsdomain.BrandIncomplete
		if !errors.As(err, &incomplete) || !errors.Is(err, artifactsdomain.ErrBrandIncomplete) {
			t.Errorf("%s: adopting the draft answered %v", name, err)
			continue
		}
		if !slices.Equal(incomplete.Missing, c.missing) {
			t.Errorf("%s: the refusal names %v, want %v", name, incomplete.Missing, c.missing)
		}
	}
	if _, adopted := n.brandNow(); adopted {
		t.Error("a draft was adopted")
	}
}
