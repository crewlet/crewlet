package engine

import "context"

// followCompany brings everything derived from a company up to the one now
// published, and reports the steps it ran, in order. A boot and a config apply
// both publish a company and both run this ONE list: a step on one path only
// leaves a seat the other path added unaddressable, with no mailbox, on no
// dashboard and firing no schedule until something unrelated runs the step.
//
// Every step notices for itself that it has nothing to do, so re-applying an
// unchanged revision (the credential-rotation gesture) costs a comparison per
// step. Order matters: a released inbox runs a turn at once, so everything a
// turn reads is current BEFORE mailboxes are ensured and held mail let
// through. The report is what an apply records as the subsystems it got
// through ([Engine.Apply]), so a step that found nothing to do is still named.
func (e *Engine) followCompany(ctx context.Context, c *Company) []string {
	if c == nil {
		return nil
	}
	steps := make([]string, 0, 8)
	if !e.indexes(c) {
		// The party registry answers for one org permanently. An apply
		// indexes it before publishing; a boot has only this moment.
		e.refreshParties(ctx, c)
	}
	steps = append(steps, "parties")

	// The vendor account a seat holds is read from its credential, so an
	// added or rotated token needs a lookup or the previous account's
	// deliveries keep routing to the seat. Lookups are cached by TOKEN, so
	// an unchanged company spends no requests; each refuses itself when its
	// surface is not configured.
	e.rewireJira(ctx, c)
	e.rewireGitLab(ctx, c)
	e.rewireGitHub(ctx, c)
	steps = append(steps, "seat_identities")

	// Each held seat's tool registry is a clone of the company's surface,
	// refiled at once or a turn runs with the previous builtins and knobs;
	// its children then converge on this company and the secret snapshot
	// on the seat's own runner, NOT WAITED FOR: a start can take a whole
	// startup timeout and every step below would wait on a vendor's
	// handshake. See seatservers.go.
	e.reconcileSeatServers(ctx)
	steps = append(steps, "seat_tools")

	// Projects, containers, chart-staffed rooms and declared repositories
	// before any mail is released: a new unit's seats first file work into
	// its project and talk in its room.
	e.applyChart(ctx, c)
	steps = append(steps, "tracker_projects")
	e.applyContainers(ctx, c)
	steps = append(steps, "knowledge_containers")
	e.applyUnitRooms(ctx, c)
	e.applyAutoJoinRooms(ctx, c)
	steps = append(steps, "chat_rooms")
	// Stamped with the company's activation and decided beside the apply,
	// not inside it: see [Engine.applyCodeRepos].
	e.applyCodeRepos(ctx, c, c.ActivatedAt)
	steps = append(steps, "code_repos")

	if e.node != nil {
		// Unconditional because it converges: it asks the broker what is
		// missing and writes only that. "Nothing changed the roster" compares
		// two companies, while a missing mailbox is a fact about the broker,
		// lost under a company nobody edited. Nil node: `crewlet validate`.
		e.node.EnsureMailboxes(ctx)
		// Mail a company with no model held back, released only now that
		// the seat tools are refiled: a released inbox runs a turn at once.
		if c.Models != nil {
			e.releaseModelHolds(ctx)
		}
		// Likewise a budget park whose ceilings changed: a released inbox
		// first judges a delivery against the ceilings now current.
		e.reconcileBudgetParks(ctx, c)
		// A park on the models' limit decided under another registry is
		// released. An entry still calling as the same account finds its
		// sign-in's sit-out ([harness.Plans]) and refuses before any harness
		// starts; a seat whose run holds its mail is parked again on the
		// carried limit when the readmit below re-fires the completion
		// ([Engine.collectedRefusal]).
		e.reconcileProviderParks(ctx, c)
		// A published company can bring a model, move a ceiling or rebuild
		// every entry (each unchanged one keeping its sit-out), so resumes
		// waiting on any of them are re-checked; one still refused waits.
		if c.Models != nil {
			e.readmitResumes(waitTurnEngine)
			e.readmitResumes(waitProvider)
		}
		e.readmitResumes(waitBudget)
		// State keyed on a seat, model or server this company removed goes
		// with it: seatpause.go, accessreview.go (so a reused handle starts
		// with no review), modelretry.go, mcprestart.go, mcpcheck.go and
		// modelcheckrun.go.
		e.clearRemovedSeatPauses(ctx, c)
		e.liftChangedFailurePauses(ctx, c)
		e.clearRemovedConversationHolds(ctx, c)
		e.clearRemovedBoxReadings(ctx, c)
		e.rereadConversationHolds()
		e.clearRemovedSeatReviews(ctx, c)
		e.clearRemovedModelRetries(ctx, c)
		e.clearRemovedMCPRestarts(ctx, c)
		e.clearRemovedMCPChecks(ctx, c)
		e.clearRemovedModelChecks(ctx, c)
		steps = append(steps, "mailboxes")
	}

	// Armed after the pointer moves, never before: the scheduler reads the
	// CURRENT company, so arming early fires the outgoing one's crons. A
	// first schedule starts its loop; the last one removed stops it.
	e.reconcileScheduler(ctx, c)
	steps = append(steps, "scheduler")

	// Last, so a surface reading the company on this signal reads the one
	// now serving.
	e.notifyCompanyPublished(ctx)
	steps = append(steps, "published")
	return steps
}
