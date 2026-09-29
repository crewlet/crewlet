package engine

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
)

// THE VALUES THE ORG CHART SEALED, AND WHEN THIS NODE READS THEM.
//
// # What was broken
//
// A chart write seals every literal credential it carries into the company's
// secret store and puts a `${VAR}` reference on the record (internal/chart's
// seal.go). This node resolves references from a SNAPSHOT of that store, taken
// at boot, at every config apply and after a provisioning pass — and a chart
// write is none of those. So a seat hired with an address, given a token, or
// having a token rotated through the chart resolved its new reference against
// a snapshot that predated the value: empty for a new name, the OLD value for a
// rotated one, on every node, until somebody happened to re-activate a config.
// A new seat's address and its CREWLET_AGENT_EMAIL resolved empty; a rotated
// tool credential was the previous one.
//
// # The fix is the view rebuild's, on every node
//
// Every node builds its own seats from its own rows, so the re-read belongs to
// the one step every node runs when its rows move: the view rebuild, BEFORE the
// company composed from those rows is published — everything built from that
// company, a seat's tool children included, then resolves against values as
// current as the rows. It re-reads when a row names a sealed value the
// snapshot does not hold, and that is the whole of the test: a chart write
// seals under a name no other write derives and never writes over a name
// (internal/chart's seal.go), so a new value — a hire's address, a rotated
// token — is always a NEW NAME on the row, and a name this node already holds
// never means anything else. The seal lands in the store before its record is
// published, so a node that has applied the record reads a store that holds
// the value.
//
// # Only the chart's own names, and why the rest stay as they were
//
// A re-read takes the store's CHART_ names and nothing else. The operator's own
// secrets keep the propagation the secret store documents — picked up at the
// next apply, by every node together — and a re-read that took them too would
// hand one node a rotation its peers do not have until they next happened to
// move a row.

// coverChartSecrets brings this node's secret snapshot up to the sealed values
// rows name, re-reading them from the store when the rows say it must.
//
// THE VIEW REBUILD'S, called under its claim with the rows it is about to
// publish, and only there — which is what lets [Engine.chartSecretsGap] go
// unlocked.
//
// A FAILED READ IS REMEMBERED ([Engine.chartSecretsStale]), and the rebuild does
// not stand still at an unchanged cursor while it is: otherwise a re-read that
// failed once would leave a rotated credential stale until the next write.
func (e *Engine) coverChartSecrets(ctx context.Context, rows chart.Chart) {
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	view := e.env.Load()
	if view == nil {
		// NO STORE AT ALL: an engine resolving from the environment
		// alone, which holds no chart value to re-read.
		return
	}
	named, err := sealedInRows(rows)
	if err != nil {
		// A ROW WHOSE RUNTIME HALF DOES NOT DECODE names nothing this can
		// read, and the view builds the object with no runtime either; the
		// rest of the rows are still worth covering.
		log.WarnContext(ctx, "chart_secrets_unreadable_row", "error", err)
	}
	stale := e.chartSecretsStale.Load()
	for _, names := range named {
		if slices.ContainsFunc(names, func(name string) bool {
			_, held := view.values[name]
			return !held
		}) {
			stale = true
			break
		}
	}
	if !stale {
		return
	}
	sealed, err := e.chartSealedValues(ctx)
	if err != nil {
		e.chartSecretsStale.Store(true)
		log.WarnContext(ctx, "chart_secrets_unread", "error", err,
			"detail", "the org chart's sealed values could not be re-read, so "+
				"a credential or an address a chart write just sealed resolves "+
				"to what this node held before; the next rebuild retries")
		return
	}
	merged := make(map[string]string, len(view.values)+len(sealed))
	for name, value := range view.values {
		if !chart.OwnsSecret(name) {
			merged[name] = value
		}
	}
	maps.Copy(merged, sealed)
	e.installSecrets(merged)
	e.chartSecretsStale.Store(false)

	// A NAME A ROW REFERENCES AND THE STORE DOES NOT HOLD resolves to
	// nothing on every node: a value an operator removed by hand, or one a
	// build that sealed nothing never wrote. Said once per distinct set, by
	// NAME only, so the log names what to put back without repeating it on
	// every rebuild.
	var missing []string
	for _, names := range named {
		for _, name := range names {
			if _, held := merged[name]; !held && !slices.Contains(missing, name) {
				missing = append(missing, name)
			}
		}
	}
	slices.Sort(missing)
	if gap := strings.Join(missing, ","); gap != e.chartSecretsGap {
		e.chartSecretsGap = gap
		if gap != "" {
			log.WarnContext(ctx, "chart_secrets_missing", "names", missing,
				"detail", "the org chart's rows reference sealed values the "+
					"secret store does not hold, so each resolves to nothing; "+
					"write the field again through the chart, or set the name "+
					"with `crewlet secrets set`")
		}
	}
}

// chartSealedValues is every value the chart sealed, as the fleet's store holds
// it now.
func (e *Engine) chartSealedValues(ctx context.Context) (map[string]string, error) {
	if e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return map[string]string{}, nil
	}
	all, err := fleetsecrets.New(e.backends.Fleet, e.cipher).All(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, value := range all {
		if chart.OwnsSecret(name) {
			out[name] = value
		}
	}
	return out, nil
}

// sealedInRows is, per chart object, the sealed names its row references — the
// objects that reference none left out.
func sealedInRows(rows chart.Chart) (map[chart.ObjectRef][]string, error) {
	named := map[chart.ObjectRef][]string{}
	var failed error
	for _, seat := range rows.Seats {
		object := chart.ObjectRef{Kind: chart.KindSeat, ID: seat.Handle}
		names, err := seat.Sealed()
		if err != nil {
			failed = err
			continue
		}
		if len(names) > 0 {
			named[object] = names
		}
	}
	for _, unit := range rows.Units {
		object := chart.ObjectRef{Kind: chart.KindUnit, ID: unit.Key}
		names, err := unit.Sealed()
		if err != nil {
			failed = err
			continue
		}
		if len(names) > 0 {
			named[object] = names
		}
	}
	return named, failed
}
