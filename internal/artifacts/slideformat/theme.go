package slideformat

// ---- themes --------------------------------------------------------------- //
//
// A THEME IS A DECK'S LOOK AS DATA: the grounds, inks, accent and typefaces
// every layout is drawn with. A deck begun from the engine's starter names its
// theme in `deck.json` (`theme`), so a slide added later is drawn in the same
// look as the slides it joins. The theme is only a name the index carries: the
// slides themselves hold their styles inline, as every deck's do, so a deck
// reads the same whether or not anything knows its theme, and a deck written
// by hand or by an agent, which names none, is no less a deck.
//
// Every colour here is a slide's own content, painted on the 1920 by 1080
// stage, never the dashboard's chrome: a slide sets its own background so it
// reads the same in every theme of the product around it.

// Theme is one look a starter deck is drawn in.
type Theme struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Tag is a few words on what sets it apart, said with its name.
	Tag string `json:"tag"`

	// Feature is the ground of a cover, a section and a closing slide, with
	// its ink and its quieter ink.
	Feature      string `json:"-"`
	FeatureInk   string `json:"-"`
	FeatureMuted string `json:"-"`

	// Stage is the ground of every other slide, with its ink and its quieter
	// ink.
	Stage string `json:"-"`
	Ink   string `json:"-"`
	Muted string `json:"-"`

	// Accent marks the one thing a slide leads with: a cover's rule, a big
	// number, a card's edge.
	Accent string `json:"-"`

	// Card and Edge are a card's ground and its hairline on the stage.
	Card string `json:"-"`
	Edge string `json:"-"`

	// Display is the family of headings, Body of everything else, and
	// DisplayWeight the weight a heading is set in.
	Display       string `json:"-"`
	Body          string `json:"-"`
	DisplayWeight int    `json:"-"`

	// Tracking is a heading's letter spacing, in px (negative tightens).
	Tracking int `json:"-"`
}

// DefaultTheme is the theme a starter names when its plan names none: the look
// the engine's starter deck has always had.
const DefaultTheme = "ink"

// themes are the themes a starter can be drawn in, in the order a picker
// offers them. Each one is held to the format with every layout by a test, so
// a theme cannot ship a colour or a family the format refuses.
var themes = []Theme{
	{
		ID: "nightshade", Name: "Nightshade", Tag: "Periwinkle on blue-black",
		Feature: "linear-gradient(160deg, #0d0f16 0%, #1b1640 100%)", FeatureInk: "#eef0f8",
		FeatureMuted: "#a9aec4",
		Stage:        "#0d0f16", Ink: "#e8ebf4", Muted: "#9aa0b8", Accent: "#9584ff",
		Card: "#151925", Edge: "#262c3e",
		Display: "Inter, system-ui, sans-serif", Body: "Inter, system-ui, sans-serif",
		DisplayWeight: 600, Tracking: -2,
	},
	{
		ID: "ink", Name: "Ink", Tag: "Navy and paper, set in a serif",
		Feature: "#1d2433", FeatureInk: "#f5f3ee", FeatureMuted: "#c7cdd8",
		Stage: "#f7f6f2", Ink: "#1d2433", Muted: "#5b6472", Accent: "#2f5d8c",
		Card: "#ffffff", Edge: "#e2e4e8",
		Display: "Georgia, serif", Body: "Georgia, serif",
		DisplayWeight: 600, Tracking: -1,
	},
	{
		ID: "paper", Name: "Paper", Tag: "White, quiet and exact",
		Feature: "#f2f2ee", FeatureInk: "#111111", FeatureMuted: "#5f5f5a",
		Stage: "#ffffff", Ink: "#111111", Muted: "#62625d", Accent: "#2450e6",
		Card: "#f5f5f1", Edge: "#e4e4de",
		Display: "Helvetica Neue, Arial, sans-serif", Body: "Helvetica Neue, Arial, sans-serif",
		DisplayWeight: 700, Tracking: -2,
	},
	{
		ID: "signal", Name: "Signal", Tag: "Yellow on black, for a big room",
		Feature: "#0a0a0a", FeatureInk: "#ffffff", FeatureMuted: "#b5b5b5",
		Stage: "#141414", Ink: "#f5f5f5", Muted: "#a8a8a8", Accent: "#ffd400",
		Card: "#1f1f1f", Edge: "#333333",
		Display: "Helvetica Neue, Arial, sans-serif", Body: "Helvetica Neue, Arial, sans-serif",
		DisplayWeight: 800, Tracking: -2,
	},
	{
		ID: "studio", Name: "Studio", Tag: "Warm cream and terracotta",
		Feature: "#2b1d17", FeatureInk: "#f6efe6", FeatureMuted: "#d6c6b3",
		Stage: "#f6efe6", Ink: "#2b1d17", Muted: "#76604f", Accent: "#c4562b",
		Card: "#fffaf3", Edge: "#e7d9c6",
		Display:       "Palatino, Palatino Linotype, Georgia, serif",
		Body:          "Avenir Next, Avenir, Helvetica Neue, Arial, sans-serif",
		DisplayWeight: 500, Tracking: -1,
	},
	{
		ID: "grove", Name: "Grove", Tag: "Deep green and mint",
		Feature: "#10261d", FeatureInk: "#eef5ef", FeatureMuted: "#a9c3b3",
		Stage: "#f3f6f2", Ink: "#10261d", Muted: "#566a5e", Accent: "#1f8a5b",
		Card: "#ffffff", Edge: "#d9e3db",
		Display: "system-ui, sans-serif", Body: "system-ui, sans-serif",
		DisplayWeight: 600, Tracking: -1,
	},
}

// Themes are the themes a starter can be drawn in, in a picker's order.
func Themes() []Theme { return append([]Theme(nil), themes...) }

// ThemeByID is the theme with that id, false where there is none.
func ThemeByID(id string) (Theme, bool) {
	for _, t := range themes {
		if t.ID == id {
			return t, true
		}
	}
	return Theme{}, false
}

// themeIDs are the themes' ids, for a refusal that names them.
func themeIDs() []string {
	out := make([]string, len(themes))
	for i, t := range themes {
		out[i] = t.ID
	}
	return out
}
