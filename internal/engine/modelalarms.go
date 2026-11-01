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
// signs an entry in here, and its CLI's token lifetime.
//
// A READ THAT FAILS CARRIES THE LAST GOOD ONE. The lease table and the secret
// store are read on every evaluation, and a blip in either must not read as
// "no model sits out" or "no token expires": that cleared every standing model
// alarm on the blip and raised each again on the next tick, two log lines per
// entry per blip, and a health count that flickered. So each input that cannot
// be read is the last one that could, and the failure is logged. Only a node
// that has never read the leases judges nothing about models.

// modelInputs is the last good read of each fleet-wide model input.
type modelInputs struct {
	mu     sync.Mutex
	nodes  map[string][]coord.ModelStatus
	stored map[string]time.Time
}

// modelsReading fills the model half of an alarm reading.
func (e *Engine) modelsReading(ctx context.Context, now time.Time, out *statelog.Reading) {
	c := e.Company()
	if c == nil || c.Config == nil || len(c.Config.Providers.LLM) == 0 {
		return
	}
	in := &e.modelInputs
	nodes, err := e.liveModelRows(ctx)
	in.mu.Lock()
	if err != nil {
		log.WarnContext(ctx, "model_alarms_leases_unread", "error", err,
			"detail", "the model alarms are judged on the last heartbeats this node read")
		nodes = in.nodes
	} else {
		in.nodes = nodes
	}
	in.mu.Unlock()
	if nodes == nil {
		return
	}
	nodes = maps.Clone(nodes)
	if _, reported := nodes[e.selfNode()]; !reported {
		// THIS NODE'S OWN ROWS, read now where its lease did not carry
		// them yet (a node in its first beat).
		nodes[e.selfNode()] = e.modelStatus(ctx)
	}
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

// tokenLifetimes is each coding-CLI entry's headless-token lifetime, zero where
// its CLI declares none.
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
