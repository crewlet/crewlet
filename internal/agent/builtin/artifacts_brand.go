package builtin

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE COMPANY'S BRAND, AS AN AGENT APPLIES IT (ADR-0048: the adopted brand is
// the company's public face).
//
// Most of the brand is applied FOR the agent: the artifact origin links the
// adopted brand's stylesheet beside the kit on every page and design, and the
// dashboard draws docs and dashboards in it, so a page written with the kit's
// tokens is on brand with no step of the agent's. What only a model can apply
// is what it writes literally: a slide's inline colours and faces, a logo
// placed in a page, a typeface copied into a version. get_company_brand is
// that, read once per authoring session rather than carried in every turn's
// prompt (the artifacts log is never read on a turn's own path).
//
// EVERYBODY READS IT, under every ceiling, a stranger's included: what an
// administrator adopted is the face the company shows on everything it
// publishes, so nothing in it is anybody's to withhold. Only the ADOPTED
// version is answered, never a kit's head: a draft an agent or a colleague
// published reaches nobody until a person adopts it.
//
// THE VOICE IS THE ONE FREE TEXT, and whoever drafted it need not be an
// administrator, so it arrives fenced under the `company` trust: adopted for
// everybody, governing tone and wording and never the task, the tools, the
// recipients or access. Colours, faces and logos are structured values the
// engine validated, answered plain (every string still neutralised).

type companyBrand struct{ deps ArtifactDeps }

var _ tools.SeatCallable = (*companyBrand)(nil)

func (t *companyBrand) Name() string { return CompanyBrandTool }

func (t *companyBrand) Description() string {
	return "Read your company's adopted brand: its colours by role (light and dark, each with " +
		"the kit token that carries it on a page), its typefaces, its logos (each as the kit's " +
		"`crewlet-logo` markup and ready to copy into a version with `from_brand`), the colours " +
		"of the brand slide theme for a slide you write by hand, the artifact kit version to " +
		"link, and how the company writes. Read it before you write a slide deck, place a logo " +
		"or choose a typeface. Pages, designs, docs and dashboards take the brand without it. " +
		"`brand` is null when the company has adopted none."
}

func (t *companyBrand) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *companyBrand) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *companyBrand) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, false)
	if refusal != nil {
		return *refusal, nil
	}
	out := map[string]any{}
	if t.deps.KitVersion != nil {
		if v := t.deps.KitVersion(); v != "" {
			out["kit_version"] = v
		}
	}
	adoption, adopted, err := t.deps.Reader.Brand(ctx, seatRead)
	if err != nil {
		if why, ok := Condition(err); ok {
			return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf(CompanyBrandTool+" could "+
				"not read the company's brand right now (%s). This is NOT \"no brand\": do not "+
				"conclude the company has none. Try again shortly.", why)), nil
		}
		return faulted(ctx, t.Name(), err, CompanyBrandTool+" could not read the company's "+
			"brand: "+faultSaid+". This is NOT \"no brand\": say you could not check."), nil
	}
	if !adopted {
		out["brand"] = nil
		out["note"] = "Your company has adopted no brand: colour pages with the kit's tokens, " +
			"and begin decks from the engine's themes."
		return jsonResult(out)
	}
	kit, err := brandformat.DecodeAdopted(adoption.BrandJSON)
	if err != nil {
		return faulted(ctx, t.Name(), err, CompanyBrandTool+" could not read the adopted brand's "+
			"record: "+faultSaid+"."), nil
	}
	read := newForeignRead(turn, c.self())
	brand, err := brandAnswer(adoption, kit)
	b := kit.Brand()
	if err != nil {
		return faulted(ctx, t.Name(), err, CompanyBrandTool+" could not name where the brand's "+
			"files go: "+faultSaid+"."), nil
	}
	if voice := voiceText(b.Voice); voice != "" {
		brand["voice"] = voice
		read.fenceAt(fieldPath{}.key("brand").key("voice"), provenance.SourceBrandVoice,
			provenance.TrustCompany, voiceAuthors(read, adoption), voice)
	}
	out["brand"] = brand
	return read.answer(ctx, t.Name(), out, "")
}

// voiceAuthors are who the adopted voice's fence names: who published the
// adopted version, who WROTE it (possibly an agent), and the administrator who
// adopted it (the fence names each once). The trust is `company` either way;
// what the fence must not say is that the administrator wrote words an agent
// drafted. An adoption recorded before its author was kept names its adopter.
func voiceAuthors(read *foreignRead, adoption artifactsdomain.Adoption) []provenance.Author {
	var out []provenance.Author
	if a := adoption.KitAuthor; a.Name != "" {
		out = append(out, read.writer(a.Name, a.Kind))
	}
	return append(out, read.writer(adoption.AdoptedBy.Name, adoption.AdoptedBy.Kind))
}

// The kit tokens the adopted brand restyles, by the role that fills each: what
// a page colours and sets type with to follow the brand in both modes. They
// are the public names the artifact kit's brand layer declares from the
// `--brand-*` inputs the brand stylesheet sets (@crewlethq/artifact-kit,
// brand.css), so a page written with them follows a later adoption too.
var brandTokens = map[string]string{
	"primary":   "--color-brand-accent",
	"secondary": "--color-brand-highlight",
	"text":      "--color-text-primary",
	"ground":    "--color-surface-background",
	"heading":   "--font-family-display",
	"body":      "--font-family-sans",
}

// The logo variants the kit's `crewlet-logo` draws, by the slot that fills
// each, and what each is for.
var brandLogos = []struct {
	slot    brandformat.Slot
	variant string
	use     string
}{
	{brandformat.SlotLogoPrimary, "primary", "on a light ground (the kit swaps in the logo for " +
		"dark grounds in dark mode where the brand has one)"},
	{brandformat.SlotLogoOnDark, "on-dark", "on a dark ground, whatever the reader's mode"},
	{brandformat.SlotLogoMark, "mark", "a square space: a chip, a corner, an icon"},
}

// brandAnswer is the adopted brand as get_company_brand states it.
func brandAnswer(a artifactsdomain.Adoption, adopted brandformat.Adoptable) (map[string]any, error) {
	b := adopted.Brand()
	out := map[string]any{
		"name": b.Name, "artifact": a.Kit.String(), "version": a.KitVersion,
		"adopted_by": actorJSON(a.AdoptedBy), "adopted_at": a.AdoptedAt.UTC().Format(time.RFC3339),
		"colors": brandColors(b.Colors),
	}
	if b.Tagline != "" {
		out["tagline"] = b.Tagline
	}
	held := map[string]artifactsdomain.BrandFile{}
	for _, f := range a.Files {
		held[f.Slot] = f
	}
	logos := make([]map[string]any, 0, len(brandLogos))
	for _, l := range brandLogos {
		f, ok := held[string(l.slot)]
		if !ok {
			continue
		}
		p, err := artifactupload.BrandPath(l.slot, f.ContentType)
		if err != nil {
			return nil, err
		}
		markup := `<span class="crewlet-logo" role="img" aria-label="` + html.EscapeString(b.Name) + `"></span>`
		if l.variant != "primary" {
			markup = `<span class="crewlet-logo" data-variant="` + l.variant + `" role="img" aria-label="` +
				html.EscapeString(b.Name) + `"></span>`
		}
		logos = append(logos, map[string]any{"slot": string(l.slot), "use": l.use,
			"content_type": f.ContentType, "markup": markup,
			"copy": map[string]any{p: map[string]any{"from_brand": string(l.slot)}}})
	}
	out["logos"] = logos
	typ := map[string]any{}
	for _, face := range []struct {
		key  string
		slot brandformat.Slot
		f    brandformat.Face
	}{{"heading", brandformat.SlotFontHeading, b.Type.Heading}, {"body", brandformat.SlotFontBody, b.Type.Body}} {
		entry := map[string]any{"font_family": brandformat.FontStack(face.f), "token": brandTokens[face.key]}
		switch {
		case face.f.Kit != "":
			entry["source"] = "kit"
		case face.f.Stack != "":
			entry["source"] = "system"
		default:
			entry["source"] = "uploaded"
			entry["family"] = face.f.Family
			if file, ok := artifactupload.FaceFile(b, face.slot); ok {
				f, held := held[string(file)]
				if !held {
					return nil, fmt.Errorf("builtin: the adopted brand holds no %s, which its "+
						"brand.json names", file)
				}
				p, err := artifactupload.BrandPath(file, f.ContentType)
				if err != nil {
					return nil, err
				}
				// A DECK'S FACE IS A FAMILY AND ONE FILE: the copy and the
				// `faces` entry that declares it, ready to paste.
				entry["copy"] = map[string]any{p: map[string]any{"from_brand": string(face.slot)}}
				entry["deck_face"] = map[string]any{"family": face.f.Family, "src": p}
			}
		}
		typ[face.key] = entry
	}
	out["type"] = typ
	out["slides"] = slideColors(brandformat.SlideTheme(adopted))
	return out, nil
}

// brandColors are the brand's colour roles as a page and a slide use them: each
// role the brand sets, its value on the light and on the dark ground, and the
// kit token that carries it. A role the brand leaves out is not answered: the
// kit's own value paints it, and a page using the token follows either way.
func brandColors(c brandformat.Colors) []map[string]any {
	var out []map[string]any
	role := func(name, light, dark string) {
		if light == "" && dark == "" {
			return
		}
		r := map[string]any{"role": name, "token": brandTokens[name]}
		if light != "" {
			r["light"] = light
		}
		if dark != "" {
			r["dark"] = dark
		}
		out = append(out, r)
	}
	dark := c.PrimaryOnDark
	if dark == "" {
		dark = c.Primary
	}
	role("primary", c.Primary, dark)
	role("secondary", c.Secondary, c.Secondary)
	role("text", c.Text, "")
	role("ground", c.LightGround, c.DarkGround)
	return out
}

// slideColors is the brand slide theme's roles as a slide writes them inline:
// the same values a deck begun in the brand is drawn with.
func slideColors(t slideformat.Theme) map[string]any {
	return map[string]any{
		"theme": slideformat.BrandTheme, "feature": t.Feature, "feature_ink": t.FeatureInk,
		"feature_muted": t.FeatureMuted, "stage": t.Stage, "ink": t.Ink, "muted": t.Muted,
		"accent": t.Accent, "card": t.Card, "edge": t.Edge, "display": t.Display, "body": t.Body,
		"display_weight": t.DisplayWeight,
	}
}

// voiceText is the brand's voice as one text a model reads: the summary, then
// the lines to write like and the lines to avoid; empty for a brand with none.
func voiceText(v brandformat.Voice) string {
	var b strings.Builder
	if v.Summary != "" {
		b.WriteString(v.Summary)
		b.WriteString("\n")
	}
	list := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString(title)
		b.WriteString(":\n")
		for _, l := range lines {
			b.WriteString("- ")
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	list("Write like this", v.WriteLike)
	list("Avoid", v.Avoid)
	return strings.TrimSuffix(b.String(), "\n")
}
