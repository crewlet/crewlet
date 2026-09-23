package engine

import (
	"context"
	"slices"
)

// WHAT FOLLOWS A PUBLISHED COMPANY, AND WHY IT IS ONE FUNCTION.
//
// # A company is published by two different gestures now
//
// A config apply publishes one — an operator activated a revision. A chart
// write publishes one too — somebody was hired, moved, renamed or given a
// schedule — and that one arrives on a different path, on a different rhythm,
// from a log rather than from a document.
//
// Everything derived from a company has to follow BOTH. Written as two lists
// it followed one: the party registry, the seat tool surfaces, the mailboxes,
// the tracker's projects and the scheduler were all rebuilt by the apply and
// by nothing else, so a seat hired this morning was unaddressable, had no
// mailbox, appeared on no dashboard and fired no schedule until somebody
// happened to change a provider — at which point all five corrected
// themselves at once, which is the shape that makes the cause impossible to
// find.
//
// So there is ONE list, here, and both gestures run it. A step added to it is
// a step both paths get; a step added to one path only is the bug this file
// exists to make unavailable.
//
// # Every step is idempotent, and that is what makes calling it cheap
//
// This runs on every committed chart record and on the view's own timer, so
// the ordinary call has nothing to do. Each step is written to notice that
// for itself — the registry compares the company it was built from, the
// mailbox pass compares its own set against the broker's, the tracker and the
// knowledge containers compare the row against what the chart says, the
// scheduler arms or disarms rather than rebuilding — and the gate below skips
// the whole list when this exact company has already been through it.
//
// # The order is not arbitrary
//
// A released inbox runs a turn immediately, and that turn resolves parties,
// loads its tool surface and files work into a project. So everything a turn
// reads is converged BEFORE the mailboxes are ensured and the held mail is
// let through. The socket publish is last, because it tells every open
// dashboard what this node now serves.

// convergeOn brings everything derived from a company up to the one that is
// now published, and reports the steps it ran, in order.
//
// The report is what an apply records as the subsystems it GOT THROUGH — see
// [Engine.Apply] — so a step that is skipped because it was already current is
// still named: it ran, and its answer was that there was nothing to do.
func (e *Engine) convergeOn(ctx context.Context, c *Company) []string {
	if c == nil {
		return nil
	}
	e.converging.Lock()
	defer e.converging.Unlock()
	if e.convergedFor == c {
		// ALREADY DONE FOR EXACTLY THIS COMPANY. By identity, for
		// [Engine.indexes]'s reason: a company equal in every field is
		// still a different publish with its own derived state. Two
		// callers reaching here for one company is ordinary — the
		// rebuild that published it and a waiter woken behind it — and
		// this is what stops the second from repeating the first.
		//
		// IT REPORTS THE SAME STEPS ANYWAY, because the report says what
		// is TRUE of this company rather than what this caller did: an
		// apply whose epoch the view's own rebuild converged on first
		// would otherwise record a trail that stops at the swap, and the
		// one thing that trail is read for is where an apply got to.
		return slices.Clone(e.convergedSteps)
	}
	e.convergedFor, e.convergedSteps = c, nil

	steps := make([]string, 0, 8)
	if !e.indexes(c) {
		// THE PARTY REGISTRY is derived from one org and answers for it
		// permanently. An apply indexes it BEFORE it publishes, so that
		// call is usually the one that has already happened; a chart
		// write has no such moment and this is where it gets one.
		e.refreshParties(ctx, c)
	}
	steps = append(steps, "parties")

	// AND THE VENDOR ACCOUNTS THOSE PARTIES HOLD. A code host or a tracker
	// names a seat by an ACCOUNT, and which account a seat holds is read
	// from the seat's own credential — which rides the chart. So a seat
	// hired with a token, or one whose token was rotated, needs a lookup
	// that only an apply ever made: until the next activation the engine
	// went on routing the previous account's deliveries to that seat and
	// knew nothing about the one it actually holds.
	//
	// The lookups are keyed on the TOKEN and cached, so a company whose
	// credentials did not move spends no requests at all — which is what
	// makes this safe to run on every published company rather than only
	// on the ones that changed a credential. Each of the three refuses
	// itself when its surface is not configured.
	e.rewireJira(ctx, c)
	e.rewireGitLab(ctx, c)
	e.rewireGitHub(ctx, c)
	steps = append(steps, "seat_identities")

	e.refileSeatTools(ctx, c)
	steps = append(steps, "seat_tools")

	// THE TRACKER'S PROJECTS AND THE KNOWLEDGE CONTAINERS, before any mail
	// is released: a unit is a CHART object, so the publish that first
	// names a project or a space is usually a chart write, and the first
	// thing the seats in a new unit do is file work into it.
	e.applyChart(ctx, c)
	steps = append(steps, "tracker_projects")
	e.applyContainers(ctx, c)
	steps = append(steps, "knowledge_containers")

	if e.node != nil {
		// A CONVERGENCE RATHER THAN A WALK, which is what makes it right
		// to call unconditionally: it asks the broker what is missing and
		// writes only that, so a company whose mailboxes all exist costs a
		// comparison and no consumer proposals at all. And it must be
		// unconditional, because "nothing changed the roster" is a
		// comparison of two companies while a missing mailbox is a fact
		// about the broker — the two disagree exactly when it matters,
		// after a mailbox was lost under a company nobody edited.
		//
		// Nil on an engine built without a node: `crewlet validate`
		// applies to nothing.
		e.node.EnsureMailboxes(ctx)
		// AND THE MAIL A COMPANY WITH NO MODEL HELD BACK is let through
		// once this company has one. Here, after the seat tools are
		// refiled, because the first thing a released inbox does is run a
		// turn and that turn must find everything it reads already
		// current.
		if c.Models != nil {
			e.releaseModelHolds(ctx)
		}
		steps = append(steps, "mailboxes")
	}

	// THE SCHEDULER reads its schedules off the CURRENT company, so it is
	// armed after the pointer moves rather than before — arming from a
	// company that is not published yet opens a window in which the loop
	// fires the outgoing one's crons. A seat's `schedules:` ride the chart,
	// so a founder giving somebody their first standup is a chart write,
	// and a loop armed only on an apply fires nothing until the next one —
	// which on a company nobody is reconfiguring is never.
	e.reconcileScheduler(ctx, c)
	steps = append(steps, "scheduler")

	// LAST, after everything derived from this company has been rebuilt, so
	// a surface that reads the company on this signal reads the one now
	// serving rather than the one being replaced.
	e.notifyCompanyPublished(ctx)
	steps = append(steps, "published")
	e.convergedSteps = steps
	return slices.Clone(steps)
}

// DeclaresIntegration reports whether this company uses the named external
// surface at all — the SETTINGS and this node's chart view together.
//
// # Why the composition is here
//
// Seven of the eight surfaces are declared entirely by the `integrations:`
// block, which a stored revision holds. Slack is the eighth: every agent
// carries its own app, and a seat is org chart content. So
// [config.Company.DeclaresIntegration] answers the half a document can answer
// and this adds the half only a running company has.
//
// The alternative — walking the settings document's own `roles:` — is what it
// used to do, and it went silently false for every company on earth the moment
// a revision stopped carrying a chart. Its one caller DELETES a surface's
// fleet status row on false, and Slack's row is the only record of the public
// base its Request URLs were set against, so the wrong answer here removes the
// one warning an operator gets about a moved address.
func (c *Company) DeclaresIntegration(surface string) bool {
	if c == nil {
		return false
	}
	if c.Config.DeclaresIntegration(surface) {
		return true
	}
	if surface != "slack" {
		// EVERY OTHER SURFACE IS ANSWERED BY THE SETTINGS ALONE,
		// including one this build does not know: that arm answers TRUE
		// above, because an older node must not erase a newer one's row.
		return false
	}
	for role := range c.Org.AllRoles() {
		if !role.Slack.IsZero() {
			return true
		}
	}
	return false
}
