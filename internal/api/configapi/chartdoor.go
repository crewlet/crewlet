package configapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
)

// THE DOOR THAT REFUSES A CHART.
//
// # What this surface writes, and what it stopped writing
//
// A stored revision is the company's SETTINGS: its providers, its
// integrations, its turn engine, its scheduling defaults. The org chart — the
// units and the seats — is a state-log domain of its own, with its own routes,
// its own arbitration per object and its own history. So a `PUT` or a `PATCH`
// carrying `roles:` or `units:` is asking this surface to write something it
// no longer owns.
//
// # Why it is refused rather than ignored
//
// Ignoring it is the shape that actually hurts. A founder sends a whole
// document with a new seat in it, the write succeeds, the revision activates —
// and the seat is nowhere, because the half that would have created it was
// dropped on the way in. Nothing failed, nothing logged, and the operator's
// own document says the seat exists. The same write refused by name takes ten
// seconds to understand, and it NAMES THE ROUTES THAT DO THE JOB — which is
// what makes the refusal a redirection rather than a dead end.
//
// # Why it is NOT in the parser
//
// The FILE still carries both halves and always will: an operator authors one
// document describing a company, and splitting the authoring surface would be
// a change to the product rather than to where the engine keeps things.
// `crewlet validate` reads that file whole, and `crewlet config import` is
// what divides it — the settings to a revision, the chart to its log. A
// refusal inside [config.ParseCompany] would break both.
//
// So it lives at this door, which is the one place a caller is writing a
// REVISION directly.
//
// # A PATCH is judged on what the CALLER sent
//
// Never on the merged document, because a merge patch can name a chart key
// and leave no trace of it in the merge: `{"units": null}` asks to remove the
// chart, merges onto a settings revision as nothing at all, and judged on the
// merge would answer 201 for a write that did nothing — the silently-dropped
// half this door exists to refuse. The caller's own keys are what it asked
// for, so they are what is judged.
//
// # And a seat or a unit is not addressed here at all
//
// There is no `/config/roles/{handle}` or `/config/units/{key}`: those
// collections are the chart's, so the entity table (entities.go) carries
// neither, and a path naming one is a route this surface does not serve.

// ChartRoutes are the chart-surface routes this package's refusals and hints
// point at, and the ONE place they are written down.
//
// EXPORTED FOR A WALK, because nothing inside this package can tell whether
// `POST /chart/batch` exists: the chart surface is a sibling, mounted by the
// same app and importable from neither direction. A hint naming a route this
// build does not serve sends an operator to a 404 with the engine's own words
// behind it, which is strictly worse than a refusal that says only no — so
// internal/api's own suite, which mounts both surfaces, holds this list
// against what the chart surface actually mounted.
//
// EVERY HINT BELOW IS BUILT FROM IT rather than spelling a pattern again in
// prose: the second copy is the one that goes stale, silently, and reads as
// authoritative for as long as nobody follows it.
var ChartRoutes = struct {
	UnitContent, SeatContent string
	Batch, Import            string
}{
	UnitContent: "PATCH /chart/units/{key}",
	SeatContent: "PATCH /chart/seats/{handle}",
	Batch:       "POST /chart/batch",
	Import:      "POST /chart/import",
}

// ChartRoutePatterns is [ChartRoutes] as a list, for the walk.
func ChartRoutePatterns() []string {
	return []string{
		ChartRoutes.UnitContent, ChartRoutes.SeatContent, ChartRoutes.Batch,
		ChartRoutes.Import,
	}
}

// chartKeysInBody are the top-level keys this surface no longer writes,
// derived from the two types rather than listed.
//
// DERIVED, on [config.CompanyKeys]'s own reasoning: a hand-written copy of a
// key list in this tree has already drifted into counting a key that appears
// nowhere and omitting five that do.
func chartKeysInBody() []string { return config.ChartKeys() }

// refuseChart answers the request when the caller's document carries a chart,
// reporting whether the request was answered.
//
// THE DOCUMENT AS SENT, which is why it takes the parsed node rather than the
// bytes: a body whose summary was lifted out is re-encoded, and a refusal that
// named a line in the re-encoded text would point an operator at a line they
// did not write.
func refuseChart(w http.ResponseWriter, doc *yaml.Node, method string) bool {
	held := chartKeysOf(doc)
	if len(held) == 0 {
		return false
	}
	httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeChartNotWritableHere, httpjson.Detail{
		"fields": held,
		"detail": fmt.Sprintf(
			"%s /config writes the company's SETTINGS, and this body carries "+
				"%s. The org chart is a domain of its own now — an ordered log "+
				"where each change is one record with its own history and its "+
				"own author — so a revision cannot hold one. It is NOT dropped "+
				"and it is NOT written: the whole request is refused, because "+
				"a write that silently kept half of what you sent is the worse "+
				"answer.",
			method, strings.Join(quoted(held), " and ")),
		// WHAT ACTUALLY WORKS TODAY, and nothing else. A hint naming a
		// route this build does not serve sends an operator to a 404 with
		// the engine's own words behind it, which is worse than saying
		// only that the write is refused.
		"hint": "send the settings alone, and write the chart through its own " +
			"routes: " + ChartRoutes.UnitContent + " and " +
			ChartRoutes.SeatContent + " for content, " + ChartRoutes.Batch +
			" for structure, " + ChartRoutes.Import + " for a whole revision's " +
			"authored placement. A node's FIRST chart is also seeded from the " +
			"company file at boot (`crewlet run -company <file>`), which seeds " +
			"only while the chart is empty",
	})
	return true
}

// chartKeysOf is every chart key this document's top level carries, in the
// order the chart declares them.
func chartKeysOf(doc *yaml.Node) []string {
	root := doc
	if root == nil {
		return nil
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var held []string
	// A MAPPING'S CONTENT IS KEY, VALUE, KEY, VALUE, so the step is two.
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if slices.Contains(chartKeysInBody(), key) {
			held = append(held, key)
		}
	}
	return held
}

// quoted renders a list of field names for a sentence.
func quoted(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = "`" + name + ":`"
	}
	return out
}

// refuseChartFromText is [refuseChart] for a body that did not parse into a
// document — which happens only when no summary was lifted out of it.
//
// IT PARSES ONCE, HERE, and swallows a failure: a body this cannot read is one
// the document parser is about to refuse in its own words and with its own
// lines, and a second opinion from here would replace a precise message with a
// vague one.
func refuseChartFromText(w http.ResponseWriter, body []byte, method string) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return false
	}
	return refuseChart(w, &doc, method)
}

// refuseChartIn answers the request when a submitted body carries a chart,
// from whichever of its two forms is available.
func refuseChartIn(w http.ResponseWriter, sent submitted, method string) bool {
	if sent.doc != nil {
		return refuseChart(w, sent.doc, method)
	}
	return refuseChartFromText(w, sent.text, method)
}
