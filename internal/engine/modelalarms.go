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
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MODEL ALARMS' INPUTS (M15 of the Models design): each entry's condition
// across the fleet, which the alarm table judges at the owner's fixed
// thresholds ([statelog.KindLLMCredentialExpiring] and the four beside it).
//
// MOSTLY FLEET-WIDE: the live leases' model rows (a limit met on one node, a
// refusal on another) and the secret store's write instants are the fleet's,
// as `backup_age` reads the fleet's one backup register, so a refusal, a limit,
// a wait and a stored credential's age read the same whichever node a person
// asks. Two inputs are this node's own reading, and say so: whether anything
// signs an entry in here, and its harness's token lifetime.
//
// A READ THAT FAILS CARRIES THE LAST GOOD ONE. The lease table and the secret
// store are read on every evaluation, and a blip in either must not read as
// "no model sits out" or "no token expires": that cleared every standing model
// alarm on the blip and raised each again on the next tick, two log lines per
// entry per blip, and a health count that flickered. So each input that cannot
// be read is the last one that could, and the failure is logged. Only a node
// that has never read the leases judges nothing about models.
//
// ONE LISTING OF THE LEASES FEEDS THE MCP ALARMS TOO ([mcpConditions]), read
// here once per evaluation, so a model's and a server's conditions are judged
// on the same beat of every node, and a blip carries both on the last good
// listing.

// entryInputs is the last good read of each fleet-wide input the per-entry
// alarms judge.
type entryInputs struct {
	mu sync.Mutex
	// statuses is every live node's heartbeat, by node id.
	statuses map[string]coord.NodeStatus
	// stored is when the secret store last wrote each secret, by name.
	stored map[string]time.Time
	// grants are the MCP servers' OAuth grants, by server.
	grants map[string]coord.MCPGrant
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
	statuses, err := e.liveStatuses(ctx)
	in.mu.Lock()
	if err != nil {
		log.WarnContext(ctx, "entry_alarms_leases_unread", "error", err,
			"detail", "the model and MCP alarms are judged on the last heartbeats this node read")
		statuses = in.statuses
	} else {
		in.statuses = statuses
	}
	in.mu.Unlock()
	if statuses == nil {
		return
	}
	statuses = maps.Clone(statuses)
	if _, reported := statuses[e.selfNode()]; !reported {
		// THIS NODE'S OWN ROWS, read now where its lease did not carry
		// them yet (a node in its first beat).
		statuses[e.selfNode()] = coord.NodeStatus{Models: e.modelStatus(ctx), MCP: e.mcpStatus(ctx)}
	}
	if len(c.Config.Providers.LLM) > 0 {
		e.modelsReading(ctx, c, statuses, now, out)
	}
	if len(c.Config.MCPServers) > 0 {
		out.MCPServers = mcpConditions(c, statuses, e.grantsReading(ctx, c))
		heldConditions(out.MCPServers, e.heldRuns(ctx))
		e.changedConditions(ctx, c, out.MCPServers)
		out.MCPServersAt = now
	}
}

// heldRuns is the fleet's active runs, for the calls they hold for a person's
// approval: the last good read where this one fails, as the heartbeats are,
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

// modelsReading fills the model half of an alarm reading from the heartbeats
// [Engine.entriesReading] read.
func (e *Engine) modelsReading(ctx context.Context, c *Company, statuses map[string]coord.NodeStatus,
	now time.Time, out *statelog.Reading) {

	nodes := make(map[string][]coord.ModelStatus, len(statuses))
	for node, status := range statuses {
		nodes[node] = status.Models
	}
	in := &e.entryInputs
	stored, err := e.storedAt(ctx)
	in.mu.Lock()
	if err != nil {
		log.WarnContext(ctx, "model_alarms_secrets_unread", "error", err,
			"detail", "the credential expiries are judged on the last write instants this node read")
		stored = in.stored
	} else {
		in.stored = stored
	}
	in.mu.Unlock()
	out.Models = modelConditions(c, nodes, e.selfNode(), stored, e.namedEntries(c),
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

// storedAt is when the secret store last wrote each secret, by name: empty on a
// node with no store, an error where the store could not be listed.
func (e *Engine) storedAt(ctx context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return out, nil
	}
	records, err := fleetsecrets.New(e.backends.Fleet, e.cipher).List(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		out[r.Name] = r.UpdatedAt
	}
	return out, nil
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
	if keys, err := c.Config.AssistantChain(); err == nil {
		for _, key := range keys {
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
