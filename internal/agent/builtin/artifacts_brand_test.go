package builtin_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// adoptBrand holds an adopted brand in the fake: Acme, with a primary and an
// on-dark logo, an uploaded heading family, the kit's body face, and a voice
// whose text tries to close the fence it arrives in.
func adoptBrand(t *testing.T, f *fakeArtifacts, voice string) brandformat.Brand {
	t.Helper()
	b := brandformat.Brand{V: brandformat.Version, Name: "Acme", Tagline: "Split the bill, not the friendship",
		Logos: brandformat.Logos{Primary: "logo.svg", OnDark: "logo-dark.svg"},
		Colors: brandformat.Colors{Primary: "#0f7b5f", Secondary: "#1d2433", Text: "#121625",
			LightGround: "#f6f7fb", DarkGround: "#0c0e16", PrimaryOnDark: "#3ccb9c"},
		Type: brandformat.Type{
			Heading: brandformat.Face{Family: "Acme Sans", Licensed: true, Files: []brandformat.FontFile{
				{Path: "fonts/bold.woff2", Weight: 700, Style: "normal"},
				{Path: "fonts/regular.woff2", Weight: 400, Style: "normal"}}},
			Body: brandformat.Face{Kit: "geist"}},
		Voice: brandformat.Voice{Summary: voice, WriteLike: []string{"Plain words, short sentences"},
			Avoid: []string{"Jargon"}},
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	file := func(slot brandformat.Slot, ct string) artifactsdomain.BrandFile {
		return artifactsdomain.BrandFile{Slot: string(slot), ContentType: ct,
			Object: objstore.Object{Hash: objstore.HashOf([]byte(slot)), Size: 4}}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adoption = &artifactsdomain.Adoption{Kit: artifacts.IDFor("kit"), KitVersion: 7, BrandJSON: raw,
		CSSSha: "css", AdoptedBy: founder, AdoptedAt: fakeAt,
		Files: []artifactsdomain.BrandFile{file(brandformat.SlotCSS, "text/css"),
			file(brandformat.SlotLogoPrimary, "image/svg+xml"), file(brandformat.SlotLogoOnDark, "image/svg+xml"),
			file(brandformat.FontFileSlot(brandformat.SlotFontHeading, 0), "font/woff2"),
			file(brandformat.FontFileSlot(brandformat.SlotFontHeading, 1), "font/woff2")}}
	return b
}

// THE ADOPTED BRAND IS EVERYBODY'S TO READ, under every ceiling: a turn nobody
// woke, one a person woke and one a stranger woke all read the same brand,
// because it is the face the company shows on everything it publishes.
func TestTheCompanyBrandIsReadableUnderEveryCeiling(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{"founder": founderPrincipal(iam.Apps...)}, nil)
	adoptBrand(t, r.f, "Warm and direct.")
	for name, turn := range map[string]*turnctx.Turn{
		"nobody woke it": turnFor(t, "agent-cto"), "a person woke it": asked(t),
		"a stranger woke it": strangerTurn(t),
	} {
		got := r.call(t, turn, builtin.CompanyBrandTool, map[string]any{})
		brand, _ := decode(t, got)["brand"].(map[string]any)
		if got.Failed || brand["name"] != "Acme" {
			t.Errorf("%s: answered %s", name, got.Output)
		}
	}
}

// THE BRAND ANSWERS WHAT A MODEL WRITES LITERALLY: each colour role in both
// modes with the kit token that carries it, the logos as the kit's markup and
// ready-to-paste `from_brand` copies, the uploaded face's regular file for a
// deck, the slide theme's colours, the version adopted and by whom, and the
// kit version to link.
func TestTheCompanyBrandAnswersWhatAModelWritesLiterally(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	b := adoptBrand(t, r.f, "Warm and direct.")
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CompanyBrandTool, map[string]any{})
	out := decode(t, got)
	if out["kit_version"] != "0.47.0" {
		t.Errorf("kit_version %v", out["kit_version"])
	}
	brand := out["brand"].(map[string]any)
	if brand["version"] != float64(7) || brand["adopted_by"].(map[string]any)["seat"] != "founder" {
		t.Errorf("the adoption is answered as %v by %v", brand["version"], brand["adopted_by"])
	}
	colors := map[string]map[string]any{}
	for _, c := range brand["colors"].([]any) {
		m := c.(map[string]any)
		colors[m["role"].(string)] = m
	}
	if p := colors["primary"]; p["light"] != "#0f7b5f" || p["dark"] != "#3ccb9c" || p["token"] != "--color-brand-accent" {
		t.Errorf("primary %v", p)
	}
	if g := colors["ground"]; g["light"] != "#f6f7fb" || g["dark"] != "#0c0e16" {
		t.Errorf("ground %v", g)
	}
	logos := brand["logos"].([]any)
	if len(logos) != 2 {
		t.Fatalf("logos %v, want the two the brand holds", logos)
	}
	primary := logos[0].(map[string]any)
	if !strings.Contains(primary["markup"].(string), `class="crewlet-logo"`) ||
		strings.Contains(primary["markup"].(string), "data-variant") {
		t.Errorf("the primary logo's markup %v", primary["markup"])
	}
	if dark := logos[1].(map[string]any); !strings.Contains(dark["markup"].(string), `data-variant="on-dark"`) {
		t.Errorf("the on-dark logo's markup %v", dark["markup"])
	}
	if cp := primary["copy"].(map[string]any)["brand/logo-primary.svg"]; cp == nil ||
		cp.(map[string]any)["from_brand"] != "logo.primary" {
		t.Errorf("the primary logo's copy %v", primary["copy"])
	}
	heading := brand["type"].(map[string]any)["heading"].(map[string]any)
	face := heading["deck_face"].(map[string]any)
	// THE BRAND THEME SETS HEADINGS BOLD, so the bold file is the one a deck
	// declares, at the path the copy puts it.
	adopted, ok := brandformat.AsAdoptable(b)
	if !ok {
		t.Fatalf("the adopted fixture leaves out %v", brandformat.MissingToAdopt(b))
	}
	theme := brandformat.SlideTheme(adopted)
	wantFile := "brand/fonts/heading-0.woff2"
	if theme.DisplayWeight < 550 {
		wantFile = "brand/fonts/heading-1.woff2"
	}
	if face["family"] != "Acme Sans" || face["src"] != wantFile || heading["copy"].(map[string]any)[wantFile] == nil {
		t.Errorf("the heading face %v", heading)
	}
	if body := brand["type"].(map[string]any)["body"].(map[string]any); body["source"] != "kit" || body["copy"] != nil {
		t.Errorf("the kit body face %v", body)
	}
	slides := brand["slides"].(map[string]any)
	if slides["theme"] != slideformat.BrandTheme || slides["accent"] != theme.Accent || slides["stage"] != theme.Stage {
		t.Errorf("the slide colours %v, want the brand theme's", slides)
	}
}

// THE VOICE IS FENCED UNDER THE COMPANY TRUST: it is the one free text, written
// by whoever drafted the kit and adopted by an administrator, so it arrives
// labelled as adopted for everybody and confined to tone, and a voice that
// writes a closing marker cannot step outside its fence.
func TestTheBrandVoiceArrivesFencedAsTheCompanys(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	turn, b := readerTurn(t)
	adoptBrand(t, r.f, attack(b, "voice"))
	got := r.call(t, turn, builtin.CompanyBrandTool, map[string]any{})
	seen, answer := fencesIn(t, b, got.Output)
	assertFences(t, seen, []wantFence{
		{"source=brand_voice trust=company authors=human:founder", "Plain words, short sentences"},
	})
	assertEnvelope(t, answer, provenance.TrustCompany, 1)
	if len(seen) != 1 {
		t.Fatalf("the brand answer carries %d fences, want the voice's alone", len(seen))
	}

	// WHO WROTE IT IS NAMED BESIDE WHO ADOPTED IT: a kit an agent drafted is
	// never fenced as words the administrator wrote.
	r.f.mu.Lock()
	r.f.adoption.KitAuthor = iam.Actor{Name: "agent-designer", Kind: iam.ActorAgent}
	r.f.mu.Unlock()
	got = r.call(t, turn, builtin.CompanyBrandTool, map[string]any{})
	seen, _ = fencesIn(t, b, got.Output)
	assertFences(t, seen, []wantFence{
		{"source=brand_voice trust=company authors=agent:agent-designer,human:founder",
			"Plain words, short sentences"},
	})
}

// NO BRAND IS AN ANSWER, NOT A REFUSAL: `brand` is null with the kit version
// still named, so an agent colours with the kit's tokens and links the kit.
func TestNoAdoptedBrandIsANullBrand(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CompanyBrandTool, map[string]any{})
	out := decode(t, got)
	if got.Failed || out["brand"] != nil || out["kit_version"] != "0.47.0" {
		t.Fatalf("no brand answered %s", got.Output)
	}
	if _, present := out["brand"]; !present {
		t.Fatalf("the answer has no brand field: %s", got.Output)
	}
}

// A LOGO OR A FACE IS COPIED BY ITS SLOT, never typed: `from_brand` reaches the
// pipeline as the slot to copy from the adopted snapshot with no byte in the
// call, and a slot the brand format has no name for is refused before anything
// is staged.
func TestFromBrandHandsThePipelineTheSlotToCopy(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, map[string]any{
		"kind": "html", "title": "Launch", "content": "<p>x</p>",
		"files": map[string]any{"brand/logo.svg": map[string]any{"from_brand": "logo.primary"},
			"brand/fonts/heading.woff2": map[string]any{"from_brand": "font.heading.1"}}})
	if got.Failed {
		t.Fatalf("create with brand copies: %s", got.Output)
	}
	req := r.f.creates[len(r.f.creates)-1]
	brands := map[string]brandformat.Slot{}
	for _, f := range req.Files {
		if f.Brand != "" {
			brands[f.Path] = f.Brand
			if f.Inline != nil || f.Upload {
				t.Errorf("%s carries bytes beside its slot: %+v", f.Path, f)
			}
		}
	}
	if brands["brand/logo.svg"] != brandformat.SlotLogoPrimary ||
		brands["brand/fonts/heading.woff2"] != "font.heading.1" {
		t.Fatalf("the pipeline was handed %v", brands)
	}
	if len(r.f.intents) != 0 {
		t.Errorf("a brand copy reserved an upload: %+v", r.f.intents)
	}
	for _, slot := range []string{"css", "logo.secondary", "font.heading.9", ""} {
		before := len(r.f.creates)
		got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, map[string]any{
			"kind": "html", "title": "Launch", "content": "<p>x</p>",
			"files": map[string]any{"brand/x.svg": map[string]any{"from_brand": slot}}})
		if tools.RefusalOf(got) != tools.RefusalInvalid || len(r.f.creates) != before {
			t.Errorf("slot %q answered %v: %s", slot, tools.RefusalOf(got), got.Output)
		}
	}
}

// A DECK BEGINS FROM A PLAN: `starter` hands the pipeline the theme and the
// slides to compose, the company's brand among the themes, and is refused
// beside files of its own or on any kind but a deck.
func TestADeckBeginsFromAStarterPlan(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, map[string]any{
		"kind": "slides", "title": "Board review",
		"starter": map[string]any{"theme": "brand", "slides": []any{
			map[string]any{"layout": "cover"}, map[string]any{"layout": "points", "title": "Risks"}}}})
	if got.Failed {
		t.Fatalf("a deck from a starter: %s", got.Output)
	}
	req := r.f.creates[len(r.f.creates)-1]
	if !req.Starter || req.Deck == nil || req.Deck.Theme != slideformat.BrandTheme ||
		len(req.Deck.Slides) != 2 || req.Deck.Slides[1] != (slideformat.PlanSlide{Layout: "points", Title: "Risks"}) ||
		len(req.Files) != 0 || req.Entry != "" {
		t.Fatalf("the pipeline was asked %+v (deck %+v)", req, req.Deck)
	}
	for name, args := range map[string]map[string]any{
		"on a page":        {"kind": "html", "title": "x", "starter": map[string]any{}},
		"beside content":   {"kind": "slides", "title": "x", "content": "x", "starter": map[string]any{}},
		"an unknown field": {"kind": "slides", "title": "x", "starter": map[string]any{"look": "dark"}},
	} {
		before := len(r.f.creates)
		got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, args)
		if tools.RefusalOf(got) != tools.RefusalInvalid || len(r.f.creates) != before {
			t.Errorf("%s answered %v: %s", name, tools.RefusalOf(got), got.Output)
		}
	}
}

// A DRAFT KIT IS OUTLINED AS IT STANDS: a kit holding its name alone answers
// get_artifact's outline with its name and its faces as unset, and says
// nothing of a logo or a colour it does not hold.
//
// Mutation: print every logo and colour role unconditionally and the draft's
// outline names an empty primary.
func TestADraftKitIsOutlinedAsItStands(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{"founder": founderPrincipal(iam.Apps...)}, nil)
	turn := asked(t)
	h := r.f.seed("founder", iam.ActorHuman, founder,
		[]artifacts.Share{shareWith("agent-cto", artifacts.RoleEditor)},
		[2]string{brandformat.EntryPath, `{"v":1,"name":"Acme"}`})
	r.f.setKind(h.ID, artifacts.KindBrand)

	got := r.call(t, turn, builtin.GetArtifactTool, map[string]any{"artifact": h.ID.String(), "view": "outline"})
	if got.Failed {
		t.Fatalf("the draft's outline was refused: %s", got.Output)
	}
	outline, _ := decode(t, got)["outline"].(map[string]any)
	text, _ := outline["text"].(string)
	for _, want := range []string{"Brand kit: Acme", "Heading face: not set", "Body face: not set"} {
		if !strings.Contains(text, want) {
			t.Errorf("the outline lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Logo") || strings.Contains(text, "Colour") || outline["name"] != "Acme" {
		t.Errorf("the draft's outline answered %v:\n%s", outline["name"], text)
	}
}
