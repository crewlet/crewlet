package slideformat

import (
	"encoding/json"
	"html"
	"strings"
)

// ---- the starter deck ----------------------------------------------------- //
//
// What a person starts a deck from in the dashboard: four slides that show the
// format's layouts (a cover, an agenda, two columns, a closing statement), each
// with notes saying what to put there. It is the ENGINE'S, not the browser's,
// because a create from the dashboard carries no upload: the pipeline publishes
// these files inline, held to this format like any deck, so a starter that drifts
// from the format fails its own test rather than a person's first save.

// starterSlides are the starter's slides in order, each a section's markup
// with `{{title}}` where the deck's title goes.
var starterSlides = []struct{ id, markup string }{
	{"cover", `<section id="cover" style="background:#1d2433;color:#f5f3ee;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;justify-content:center;gap:32px">
<h1 style="font-size:112px;font-weight:600;line-height:1.05;letter-spacing:-2px">{{title}}</h1>
<p style="font-size:40px;color:#c7cdd8">A subtitle: who it is for, and when</p>
<aside>The cover. Say what the deck is for in one sentence before the first slide.</aside>
</section>
`},
	{"agenda", `<section id="agenda" style="background:#f7f6f2;color:#1d2433;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;gap:48px">
<h2 style="font-size:72px;font-weight:400;line-height:1.1">Agenda</h2>
<ol style="font-size:40px;line-height:1.5">
<li>Where we are</li>
<li>What we will do</li>
<li>What we need</li>
</ol>
<aside>Three parts at most. Each one becomes a section of the outline.</aside>
</section>
`},
	{"points", `<section id="points" style="background:#f7f6f2;color:#1d2433;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;gap:48px">
<h2 style="font-size:72px;font-weight:400;line-height:1.1">One idea per slide</h2>
<div style="display:flex;gap:32px">
<div style="flex:1;display:flex;flex-direction:column;gap:12px;background:#ffffff;padding:40px;border:1px solid #e2e4e8;border-radius:16px">
<h3 style="font-size:36px;font-weight:600">The point</h3>
<p style="font-size:28px">Say it in one sentence a person remembers.</p>
</div>
<div style="flex:1;display:flex;flex-direction:column;gap:12px;background:#ffffff;padding:40px;border:1px solid #e2e4e8;border-radius:16px">
<h3 style="font-size:36px;font-weight:600">The evidence</h3>
<p style="font-size:28px">A number, a quote or a picture that backs it.</p>
</div>
</div>
<aside>Two cards side by side. Click any text to change it.</aside>
</section>
`},
	{"close", `<section id="close" style="background:#1d2433;color:#f5f3ee;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;justify-content:center;gap:32px">
<h2 style="font-size:88px;font-weight:600;line-height:1.1">What we need</h2>
<p style="font-size:40px;color:#c7cdd8">The one decision or the one ask, said plainly</p>
<aside>End on what the room should do next.</aside>
</section>
`},
}

// Starter is the starter deck's files for a deck titled title, by path, its
// index first in [IndexPath]. The title is written escaped, as JSON in the
// index and as text on the cover.
func Starter(title string) map[string][]byte {
	order := make([]string, len(starterSlides))
	files := make(map[string][]byte, len(starterSlides)+1)
	for i, s := range starterSlides {
		order[i] = s.id
		files[SlidePath(s.id)] = []byte(strings.ReplaceAll(s.markup, "{{title}}", html.EscapeString(title)))
	}
	index, err := json.MarshalIndent(Index{
		V: 4, Title: title, Order: order, Cover: "cover",
		Sections: map[string]IndexSection{
			"s1": {Description: "Where we are", Start: "cover"},
			"s2": {Description: "What we will do", Start: "points"},
			"s3": {Description: "What we need", Start: "close"},
		},
	}, "", "  ")
	if err != nil {
		// AN INDEX OF STRINGS ALWAYS ENCODES; a failure here is a broken
		// build, not an input.
		panic("slideformat: encode the starter index: " + err.Error())
	}
	files[IndexPath] = append(index, '\n')
	return files
}
