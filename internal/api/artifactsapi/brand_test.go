package artifactsapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
)

// brandCSS is the stylesheet the rig's adoption serves.
var brandCSS = []byte(":root {\n  --brand-primary: #2450e6;\n}\n")

// adopted is an adoption of version 1 of jane's page, which the rig holds at
// version 2, serving a stylesheet and the rig's drawing as its logo.
func adopted(t *testing.T) *artifactsdomain.Adoption {
	t.Helper()
	b, err := brandformat.Decode([]byte(`{"v":1,"name":"Acme","logos":{"primary":"logo.svg"},` +
		`"colors":{"primary":"#2450e6"},"type":{"heading":{"kit":"geist"},"body":{"stack":"system-sans"}},` +
		`"voice":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	index, err := brandformat.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	css := object("brand.css", brandCSS)
	return &artifactsdomain.Adoption{Kit: page, KitVersion: 1, BrandJSON: index,
		CSSSha: string(css.Hash), AdoptedBy: iam.Actor{Name: "ada", Kind: iam.ActorHuman, OperatorID: "session:x"},
		Files: []artifactsdomain.BrandFile{
			{Slot: string(brandformat.SlotCSS), ContentType: "text/css; charset=utf-8", Object: css},
			{Slot: string(brandformat.SlotLogoPrimary), ContentType: "image/svg+xml", Object: entryLogo.Object},
		}}
}

// brandOf decodes a /brand answer.
func brandOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out struct {
		Brand map[string]any `json:"brand"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the brand answer is not JSON: %s", body)
	}
	return out.Brand
}

// THE ADOPTED BRAND IS EVERYBODY'S, and the one field that is not is said only
// to a reader who may open the kit: a newer draft and its publisher. The
// answer names no object key and no adopter's credential.
//
// Mutation: answer newer_version without deciding the kit, and the stranger
// learns a draft exists.
func TestTheAdoptedBrandIsEverybodysAndADraftIsNot(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("kim"), "GET", "/brand", ""); w.Code != http.StatusOK ||
		strings.TrimSpace(w.Body.String()) != `{"brand":null}` {
		t.Fatalf("with nothing adopted /brand answered %d %s", w.Code, w.Body)
	}
	g.estate.adoption = adopted(t)
	for _, c := range []struct {
		name   string
		p      iam.Principal
		newer  bool
		status int
	}{
		{"a stranger to the kit", person("kim"), false, http.StatusOK},
		{"a reader of the kit", person("bob"), true, http.StatusOK},
		{"an administrator", person("ada", iam.GrantAdmin), true, http.StatusOK},
	} {
		w := g.do(c.p, "GET", "/brand", "")
		if w.Code != c.status {
			t.Fatalf("%s: /brand answered %d %s", c.name, w.Code, w.Body)
		}
		b := brandOf(t, w.Body.Bytes())
		if b["css_sha"] != string(object("brand.css", brandCSS).Hash) {
			t.Errorf("%s is answered %v", c.name, b)
		}
		if _, ok := b["newer_version"]; ok != c.newer {
			t.Errorf("%s is told of a newer version: %v, want %v", c.name, ok, c.newer)
		}
		raw := w.Body.String()
		if strings.Contains(raw, `"key"`) || strings.Contains(raw, "session:x") {
			t.Errorf("%s is answered an object key or a credential: %s", c.name, raw)
		}
	}
	if len(g.marks.elevated) != 0 {
		t.Errorf("telling an administrator of a newer version recorded %d elevated reads", len(g.marks.elevated))
	}
}

// A BRAND FILE IS AN ATTACHMENT, A DRAWING PLAIN TEXT, and only a file an
// adoption serves is answered: the API origin never renders a logo, and the
// route reads no object the brand does not name.
func TestABrandFileIsAnAttachmentAndOnlyAnAdoptedOne(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.estate.adoption = adopted(t)
	w := g.do(person("kim"), "GET", "/brand/files/"+string(entryLogo.Object.Hash), "")
	if w.Code != http.StatusOK || w.Body.String() != string(logoSVG) {
		t.Fatalf("the adopted logo answered %d %q", w.Code, w.Body)
	}
	h := w.Header()
	if got := h.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("the drawing is served as %q", got)
	}
	if got := h.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("the drawing is served with disposition %q", got)
	}
	for _, path := range []string{"/brand/files/" + string(objstore.HashOf(indexHTML)), "/brand/files/not-a-hash"} {
		if w := g.do(person("kim"), "GET", path, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, w.Code)
		}
	}
}

// ADOPTING IS AN ADMINISTRATOR'S, AND READS THE KIT AS A CONTENT READ: a
// person without admin is refused, an administrator with no share on the kit
// is admitted with the override recorded, and the pipeline is asked the kit
// and the version.
func TestAdoptingIsAnAdministratorsAndRecordsTheOverride(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := `{"kit":"` + page.String() + `","version":1}`
	if w := g.do(person("jane"), "POST", "/brand/adoptions", body, keyed()...); w.Code != http.StatusForbidden {
		t.Errorf("a person without admin adopting answered %d %s", w.Code, w.Body)
	}
	w := g.do(person("ada", iam.GrantAdmin), "POST", "/brand/adoptions", body, keyed()...)
	if w.Code != http.StatusOK {
		t.Fatalf("an administrator's adoption answered %d %s", w.Code, w.Body)
	}
	if verbs := g.pipeline.verbs(); len(verbs) != 1 || verbs[0] != "adopt_brand:"+page.String()+":1" {
		t.Errorf("the pipeline was asked %v", verbs)
	}
	if len(g.marks.elevated) != 1 {
		t.Errorf("an administrator's read of a kit nobody shared with them recorded %d overrides",
			len(g.marks.elevated))
	}
	if w := g.do(person("ada", iam.GrantAdmin), "DELETE", "/brand/adoptions/current", "", keyed()...); w.Code != http.StatusOK {
		t.Errorf("a clear answered %d %s", w.Code, w.Body)
	}
}

// AN UNREADABLE BRAND IS ITS OWN ANSWER, naming the pair that refused it, so
// the adopt dialog says which colours to change.
func TestAnUnreadableBrandIsAnsweredWithItsPair(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	pair := brandformat.Contrast(brandformat.Brand{Colors: brandformat.Colors{Primary: "#2450e6",
		Text: "#cccccc", LightGround: "#ffffff"}})[0]
	g.pipeline.err = &artifactsdomain.BrandContrast{Pair: pair}
	w := g.do(person("ada", iam.GrantAdmin), "POST", "/brand/adoptions",
		`{"kit":"`+page.String()+`","version":1}`, keyed()...)
	if w.Code != http.StatusUnprocessableEntity || errorOf(t, w) != "brand_contrast" {
		t.Fatalf("an unreadable brand answered %d %s", w.Code, w.Body)
	}
	var body struct {
		Contrast brandformat.ContrastResult `json:"contrast"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Contrast.Ratio != pair.Ratio ||
		body.Contrast.ForegroundColor != "#cccccc" {
		t.Errorf("the refusal carries %+v (%v)", body.Contrast, err)
	}
}

// AN INCOMPLETE BRAND IS REFUSED NAMING WHAT IT LEAVES OUT, as findings the
// adopt dialog lists beside the fields to fill and as the bare field names.
//
// Mutation: answer BrandIncomplete through the default failure and the
// dialog gets no findings and no missing list.
func TestAnIncompleteBrandIsAnsweredWithWhatItLeavesOut(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.pipeline.err = &artifactsdomain.BrandIncomplete{Missing: []string{"logos.primary", "colors.primary"}}
	w := g.do(person("ada", iam.GrantAdmin), "POST", "/brand/adoptions",
		`{"kit":"`+page.String()+`","version":1}`, keyed()...)
	if w.Code != http.StatusUnprocessableEntity || errorOf(t, w) != "policy_refused" {
		t.Fatalf("an incomplete brand answered %d %s", w.Code, w.Body)
	}
	var body struct {
		Missing  []string `json:"missing"`
		Findings []struct {
			Path    string `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal does not decode: %v", err)
	}
	if strings.Join(body.Missing, ",") != "logos.primary,colors.primary" || len(body.Findings) != 2 {
		t.Fatalf("the refusal carries missing %v and %d findings", body.Missing, len(body.Findings))
	}
	for i, field := range body.Missing {
		f := body.Findings[i]
		if f.Path != brandformat.EntryPath || f.Code != "brand_format" ||
			!strings.HasPrefix(f.Message, "field: "+field+": is required to adopt the brand") {
			t.Errorf("finding %d is %+v, want it to name %s", i, f, field)
		}
	}
}
