package engine

import "context"

// WHAT FOLLOWS A PUBLISHED COMPANY, AND WHY IT IS ONE FUNCTION.
//
// # A company is published in two places
//
// A node's boot publishes the company it starts on, and a config apply
// publishes the revision an operator activated. Everything derived from a
// company has to follow BOTH. Written as two lists it followed one: the party
// registry, the seat tool surfaces, the mailboxes, the tracker's projects and
// the scheduler were each rebuilt on one path and forgotten on the other, so a
// seat a revision added was unaddressable, had no mailbox, appeared on no
// dashboard and fired no schedule until something unrelated happened to run
// the missing step — at which point all of them corrected themselves at once,
// which is the shape that makes the cause impossible to find.
//
// So there is ONE list, here, and both paths run it. A step added to it is a
// step both paths get; a step added to one path only is the bug this file
// exists to make unavailable.
//
// # Every step is idempotent, and that is what makes calling it cheap
//
// Each step is written to notice for itself that it has nothing to do — the
// registry compares the company it was built from, the mailbox pass compares
// its own set against the broker's, the tracker and the knowledge containers
// compare the row against what the company says, the scheduler arms or
// disarms rather than rebuilding — so re-applying an unchanged revision, which
// is the credential-rotation gesture, costs a comparison per step.
//
// # The order is not arbitrary
//
// A released inbox runs a turn immediately, and that turn resolves parties,
// loads its tool surface and files work into a project. So everything a turn
// reads is brought up to date BEFORE the mailboxes are ensured and the held
// mail is let through. The socket publish is last, because it tells every open
// dashboard what this node now serves.

// followCompany brings everything derived from a company up to the one that is
// now published, and reports the steps it ran, in order.
//
// The report is what an apply records as the subsystems it GOT THROUGH — see
// [Engine.Apply] — so a step that found nothing to do is still named: it ran,
// and its answer was that there was nothing to do.
func (e *Engine) followCompany(ctx context.Context, c *Company) []string {
	if c == nil {
		return nil
	}
	steps := make([]string, 0, 8)
	if !e.indexes(c) {
		// THE PARTY REGISTRY is derived from one org and answers for it
		// permanently. An apply indexes it BEFORE it publishes, so that
		// call is usually the one that has already happened; a boot has
		// no such moment and this is where it gets one.
		e.refreshParties(ctx, c)
	}
	steps = append(steps, "parties")

	// AND THE VENDOR ACCOUNTS THOSE PARTIES HOLD. A code host or a tracker
	// names a seat by an ACCOUNT, and which account a seat holds is read
	// from the seat's own credential. So a seat added with a token, or one
	// whose token was rotated, needs a lookup: until it is made the engine
	// goes on routing the previous account's deliveries to that seat and
	// knows nothing about the one it actually holds.
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
	// is released: the first thing the seats in a new unit do is file work
	// into its project.
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
		// AND A SEAT PARKED ON ITS BUDGET whose ceilings this company
		// changed, for the same reason and in the same place: the first
		// thing a released inbox does is judge a delivery against the
		// counters, and it must do so under the ceilings now current.
		e.reconcileBudgetParks(ctx, c)
		// AND A PAUSE WHOSE SEAT THIS COMPANY REMOVED goes with the seat.
		// See seatpause.go.
		e.clearRemovedSeatPauses(ctx, c)
		steps = append(steps, "mailboxes")
	}

	// THE SCHEDULER reads its schedules off the CURRENT company, so it is
	// armed after the pointer moves rather than before — arming from a
	// company that is not published yet opens a window in which the loop
	// fires the outgoing one's crons. A founder's first schedule starts the
	// loop here; their last one removed stops it.
	e.reconcileScheduler(ctx, c)
	steps = append(steps, "scheduler")

	// LAST, after everything derived from this company has been rebuilt, so
	// a surface that reads the company on this signal reads the one now
	// serving rather than the one being replaced.
	e.notifyCompanyPublished(ctx)
	steps = append(steps, "published")
	return steps
}
