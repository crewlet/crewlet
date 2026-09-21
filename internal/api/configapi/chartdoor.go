package configapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

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
// seconds to understand and points at the route that does the job.
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
// Never on the merged document. A patch is applied over the stored revision,
// and a revision written before the split still carries a chart inside it — so
// judging the merge would refuse an operator patching a mission for a chart
// they did not send and cannot see.

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
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":  "chart_not_writable_here",
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
		"hint": "send the settings alone. A node's FIRST chart is seeded from " +
			"the company file at boot (`crewlet run -company <file>`, which " +
			"seeds only while the chart is empty); after that the chart is " +
			"written through its own log and not through this document",
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

// refuseChartEntity answers a per-entity write to a collection the chart owns,
// reporting whether the request was answered.
//
// # Why the entity routes are refused too, and not just PUT /config
//
// `PUT /config/roles/{handle}` never sent a chart key at all — it sends one
// seat, and the route splices it into the stored revision. That splice is
// exactly what the door above exists to stop: it writes a document carrying
// `roles:`, and the next node to read that revision refuses it as a settings
// document. The failure would move from the write, where the caller is
// standing, to every node's next restart.
//
// So the two doors are one rule stated at both of its entrances, and this one
// is the entrance where the chart arrives WITHOUT naming itself.
//
// # Why it is at the HTTP door and not inside the draft
//
// [Service.ApplyEntity] is the same write one layer down, and it is NOT
// refused. That looks like a hole and is not, because of the rule the entity
// write already had: it never CREATES. A settings revision carries no seats,
// so an entity write naming one finds nothing and is refused as absent — and
// the only document a splice can land on is one that already held a chart,
// which is already a revision no node will apply. Refusing there would change
// which error the engine's own setup flow gets while changing no outcome.
//
// What it would change is the message a PERSON gets, and that is the whole
// reason this exists: `no roles called ceo in the active revision`, answered
// to a founder whose company plainly has a CEO, reads as the engine having
// lost their org chart.
func refuseChartEntity(w http.ResponseWriter, kind, id string) bool {
	if kind != EntityRoles && kind != EntityUnits {
		return false
	}
	// A SEAT AND A UNIT ARE SAID SEPARATELY, because the two arrive in
	// different gestures: a seat is hired and moved, a unit is opened and
	// reparented, and one sentence covering both tells somebody holding
	// either of them nothing they can act on.
	noun, gesture := "seat", "hire, move or edit a seat"
	if kind == EntityUnits {
		noun, gesture = "unit", "open, move or edit a unit"
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":  "chart_not_writable_here",
		"fields": []string{kind},
		"detail": fmt.Sprintf(
			"a %s is not part of the company's settings any more, so this "+
				"route cannot write %s/%s. Writing it here would store a "+
				"revision carrying a chart, which every node refuses to read "+
				"as settings — so the failure would arrive at the next "+
				"restart rather than here.", noun, kind, id),
		"hint": "to " + gesture + ", write the org chart's own log; this " +
			"route writes the company's settings. A node's first chart is " +
			"seeded from the company file at boot (`crewlet run -company " +
			"<file>`), and reading " + kind + "/" + id + " here still works",
	})
	return true
}
