package engine

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MODEL ALARMS' INPUTS (M15 of the Models design): each entry's condition
// across the fleet, judged at the owner's fixed thresholds
// ([statelog.KindLLMCredentialExpiring] and the four beside it), read off the
// live nodes' status records, save whether anything signs an entry here, its
// harness's token lifetime and when its token was written, which are this
// node's own (the last its secret snapshot, [Engine.storedAt]). A
// read that fails carries the last good one, so a blip neither clears nor
// raises an alarm; a live node whose record is not counted keeps its last one
// at the same epoch, all but this node, whose rows are its memory; and one
// listing feeds the MCP alarms too ([mcpConditions]). The package doc's "The
// model alarms' inputs" holds the why of each.

// entryInputs is the last good read of each fleet-wide input the per-entry
// alarms judge.
type entryInputs struct {
	mu sync.Mutex
	// statuses is every live node's status record, by node id, a live node
	// whose record is not counted keeping the one it last had.
	statuses map[string]coord.NodeStatusRecord
	// grants are the MCP servers' OAuth grants, by server.
	grants map[string]coord.OAuthGrant
	// runs is the fleet's active runs, read for the calls they hold for a
	// person's approval ([heldConditions]).
	runs []sandbox.PendingRun
}

// entriesReading fills the per-entry half of an alarm reading: each model
// entry's condition and each MCP server's, across the fleet.
func (e *Engine) entriesReading(ctx context.Context, now time.Time, out *statelog.Reading) {
	c := e.Company()
	if c == nil || c.Config == nil || (len(c.Config.Providers.LLM) == 0 && len(c.Config.MCPServers) == 0) {
		return
	}
	in := &e.entryInputs
	leases, records, err := e.statusListings(ctx)
	in.mu.Lock()
	var statuses map[string]coord.NodeStatusRecord
	if err != nil {
		log.WarnContext(ctx, "entry_alarms_status_unread", "error", err,
			"detail", "the model and MCP alarms are judged on the last status records this node read")
		statuses = in.statuses
	} else {
		// NEVER THIS NODE'S OWN, which its memory answers below.
		last := maps.Clone(in.statuses)
		delete(last, e.selfNode())
		statuses = coord.LiveNodeStatusesKept(records, leases, last)
		in.statuses = statuses
	}
	in.mu.Unlock()
	if statuses == nil {
		return
	}
	statuses = maps.Clone(statuses)
	if _, reported := statuses[e.selfNode()]; !reported {
		// THIS NODE'S OWN ROWS, read now where no record of its counts in
		// this listing: a node before its first write at its presence
		// epoch, or one whose record aged out while its writes failed.
		statuses[e.selfNode()] = coord.NodeStatusRecord{Node: e.selfNode(),
			Models: e.modelStatus(ctx), MCP: e.mcpStatus(ctx)}
	}
	if len(c.Config.Providers.LLM) > 0 {
		e.modelsReading(c, statuses, now, out)
	}
	if len(c.Config.MCPServers) > 0 {
		out.MCPServers = mcpConditions(c, statuses, e.grantsReading(ctx, c))
		heldConditions(out.MCPServers, e.heldRuns(ctx))
		e.changedConditions(ctx, c, out.MCPServers)
		out.MCPServersAt = now
	}
}

// heldRuns is the fleet's active runs, for the calls they hold for a person's
// approval: the last good read where this one fails, as the status records are,
// so a blip neither clears a standing `mcp_approval_waiting` nor raises it
// again on the next tick.
func (e *Engine) heldRuns(ctx context.Context) []sandbox.PendingRun {
	in := &e.entryInputs
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	runs, err := sandbox.NewCoordStore(e.backends.Fleet).ListActive(ctx)
	in.mu.Lock()
	defer in.mu.Unlock()
	if err != nil {
		log.WarnContext(ctx, "entry_alarms_runs_unread", "error", err,
			"detail", "the calls waiting for approval are judged on the runs this node last read")
		return in.runs
	}
	in.runs = runs
	return runs
}

// modelsReading fills the model half of an alarm reading from the status
// records [Engine.entriesReading] read.
func (e *Engine) modelsReading(c *Company, statuses map[string]coord.NodeStatusRecord,
	now time.Time, out *statelog.Reading) {

	nodes := make(map[string][]coord.ModelStatus, len(statuses))
	for node, status := range statuses {
		nodes[node] = status.Models
	}
	out.Models = modelConditions(c, nodes, e.selfNode(), e.storedAt(), e.namedEntries(c),
		e.tokenLifetimes(c), now)
	out.ModelsAt = now
}

// modelConditions joins every node's rows into one condition per entry.
func modelConditions(c *Company, nodes map[string][]coord.ModelStatus, self string,
	stored map[string]time.Time, named map[string]bool, lifetimes map[string]time.Duration,
	now time.Time) []statelog.ModelCondition {

	keys := slices.Sorted(maps.Keys(c.Config.Providers.LLM))
	out := make([]statelog.ModelCondition, 0, len(keys))
	for _, key := range keys {
		m := statelog.ModelCondition{Key: key}
		spec := c.Config.Providers.LLM[key]
		if lifetime := lifetimes[key]; lifetime > 0 && spec.ResolvedAuthMode() == config.AuthSubscription {
			// THE TOKEN'S EXPIRY, computed from when the store last
			// wrote it: the token is opaque. A value read from the
			// environment has no write instant, so no expiry.
			if variable := variableOf(spec.CredentialSources(key).Token); variable != "" {
				m.Variable = variable
				if at, ok := stored[variable]; ok {
					m.ExpiresAt = at.Add(lifetime)
				}
			}
		}
		var answered time.Time
		var refused *coord.ModelFailure
		for _, node := range slices.Sorted(maps.Keys(nodes)) {
			for _, row := range nodes[node] {
				if row.Key != key {
					continue
				}
				if row.LastAnswer.After(answered) {
					answered = row.LastAnswer
				}
				if f := row.LastFailure; f != nil && f.Kind == "auth" && (refused == nil || f.At.After(refused.At)) {
					failure := *f
					refused = &failure
				}
				if window := row.SittingOut; window != nil && now.Before(window.Until) &&
					(m.SittingOut == nil || window.Until.After(m.SittingOut.Until)) {
					latest := *window
					m.SittingOut = &latest
				}
				// THE FIRST WAIT, which travels whole beside the
				// bounded list of seats ([coord.MaxModelParkedSeats]).
				waiting := row.ParkedSince
				for _, parked := range row.Parked {
					if waiting.IsZero() || parked.Since.Before(waiting) {
						waiting = parked.Since
					}
				}
				if !waiting.IsZero() && (m.WaitingSince.IsZero() || waiting.Before(m.WaitingSince)) {
					m.WaitingSince = waiting
				}
				if node == self && !row.Resolves && named[key] {
					m.NoSignIn = true
				}
			}
		}
		if refused != nil && refused.At.After(answered) {
			m.RefusedAt, m.RefusedSaid = refused.At, refused.Said
		}
		out = append(out, m)
	}
	return out
}

// variableOf is the variable a credential is read from: the ${VAR} the entry
// names, else the profile's default.
func variableOf(source config.CredentialSource) string {
	if name, whole := envref.Whole(source.Value); whole {
		return name
	}
	if source.Value != "" {
		// A literal written into the document is no variable.
		return ""
	}
	return source.Fallback
}

// storedRecords is the row of each secret this node resolves from, by name,
// as its snapshot holds it ([secretView]): empty before one is installed. THE
// SNAPSHOT, never the store, for the alarm beat ([Engine.storedAt]) and
// Settings › Models & Fleets ([Engine.LocalModels]) alike: what both judge is the value
// this node sends, and a listing every beat on every node would carry every
// sealed value across its link (ADR-0175, "The secrets bucket declares its
// class"). A value written with its reload deferred keeps its earlier row
// until the next apply, as it keeps its earlier value.
func (e *Engine) storedRecords() map[string]secrets.Record {
	if v := e.env.Load(); v != nil && v.records != nil {
		return v.records
	}
	return map[string]secrets.Record{}
}

// storedAt is when the store wrote each value this node resolves, by name
// ([Engine.storedRecords]).
func (e *Engine) storedAt() map[string]time.Time {
	records := e.storedRecords()
	out := make(map[string]time.Time, len(records))
	for name, rec := range records {
		out[name] = rec.UpdatedAt
	}
	return out
}

// tokenLifetimes is each harness entry's headless-token lifetime, zero where
// its harness declares none.
func (e *Engine) tokenLifetimes(c *Company) map[string]time.Duration {
	out := map[string]time.Duration{}
	for key := range c.Config.Providers.LLM {
		if agent, ok := e.harnessEntry(key); ok {
			out[key] = agent.TokenLifetime()
		}
	}
	return out
}

// namedEntries is every entry a seat's job or the assistant runs on: each agent
// seat's resolved chain for every phase, and the assistant's chain while it is
// on. An entry nothing runs on is no alarm when nothing signs it in.
func (e *Engine) namedEntries(c *Company) map[string]bool {
	out := map[string]bool{}
	if c.Models != nil && c.Org != nil {
		for role := range c.Org.AllRoles() {
			if !role.IsAgent() {
				continue
			}
			for _, ph := range phase.All {
				members, err := c.Models.Chain(role, ph)
				if err != nil {
					continue
				}
				for _, m := range members {
					out[m.Key] = true
				}
			}
		}
	}
	if ref, err := c.Config.AssistantModel(); err == nil {
		for _, key := range c.Config.FleetMembers(ref) {
			out[key] = true
		}
	}
	return out
}

// LatestAlarms is this node's latest alarm evaluation, every kind it found
// standing with what it measured, nil before one has run: what the Models page
// shows of the model kinds, so the page and the health count cannot disagree.
func (e *Engine) LatestAlarms() []statelog.Alarm {
	tracker := e.alarms.Load()
	if tracker == nil {
		return nil
	}
	return tracker.Latest()
}

// AlarmsEvaluated reports whether this node's alarm table has been evaluated,
// which an empty [Engine.LatestAlarms] cannot tell from none standing.
func (e *Engine) AlarmsEvaluated() bool {
	tracker := e.alarms.Load()
	if tracker == nil {
		return false
	}
	_, evaluated := tracker.Standing()
	return evaluated
}
