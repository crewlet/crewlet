package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// ConfigWriter removes a block from the company document: the consumer's own
// interface, implemented by the config surface every other write goes through
// (one validation, one compare-and-set), never a second path onto the document.
type ConfigWriter interface {
	// Disconnect removes kind's block and turns off every seat on through it,
	// as one revision recorded as by's, and answers the seats it turned off.
	// The block alone is not a disconnect: the document refuses an agent
	// turned on at an integration the company has not connected.
	Disconnect(ctx context.Context, kind integration.Kind, summary string,
		by iam.Actor) (seatsOff []string, err error)

	// Apply merges patch into the active revision and activates it as by's:
	// a field a pass discovered, recorded on the company's own block.
	Apply(ctx context.Context, patch []byte, summary string, by iam.Actor) error

	// Seat reads one seat's whole entity as JSON, and SetSeat writes it
	// back under the same handle. Separate from Apply because a merge patch
	// replaces an array wholesale: patching `roles` would delete every other
	// seat, while the entity route addresses a seat by its handle. The GitHub
	// pass records an installation it discovered here.
	Seat(ctx context.Context, handle string) ([]byte, error)
	SetSeat(ctx context.Context, handle string, body []byte, summary string,
		by iam.Actor) error

	// Reload re-activates the CURRENT document unchanged, advancing the epoch
	// so every node applies again: the credential-rotation gesture, and why
	// the activation pointer is append-only rather than keyed on a revision
	// id. See [Engine.rebuildForSealedSecrets] for why a pass needs it.
	Reload(ctx context.Context, summary string, by iam.Actor) error
}

// UseConfigWriter installs the surface a disconnect removes a block through.
//
// Installed rather than constructed here: the config surface is built where
// the API is, after the reconcile loop is armed. A disconnect ticking in that
// window reports [integration.ErrDisconnectUnavailable] and leaves the row
// untouched, rather than tearing down at the vendor and then failing to drop
// the block, which would leave an integration configured, live and stripped.
func (e *Engine) UseConfigWriter(w ConfigWriter) { e.configWriter.Store(&w) }

// configWriterOrNil reads what was installed.
func (e *Engine) configWriterOrNil() ConfigWriter {
	if w := e.configWriter.Load(); w != nil {
		return *w
	}
	return nil
}

// vendorDisconnect removes one surface: what it holds, and then its block.
//
// THE ORDER is why [integration.Disconnector] is one call: the block carries
// the credential the vendor teardown authenticates with, so removing it first
// strands every webhook and account with nothing to authenticate a retry.
type vendorDisconnect struct {
	engine *Engine
	kind   integration.Kind
	// pass is nil for a surface with nothing to remove anywhere.
	pass teardown
}

// teardown is the one method a disconnect calls, declared where it is called.
//
// Narrower than [setup.Teardowner], which is a [setup.Pass] too, so that
// Slack's step, which has no vendor to reconcile against, needs no Run or
// Needs that nothing would call. Every real pass still satisfies this.
type teardown interface {
	Teardown(ctx context.Context, in setup.TeardownInput) (provision.Removed, error)
}

func (d vendorDisconnect) Disconnect(
	ctx context.Context, req integration.TeardownRequest,
) (provision.Removed, error) {
	// WHOEVER ASKED records every write below; the loop only carries it out.
	// Always somebody ([integration.AskTeardown] records who asked beside
	// the intent), so the loop has no name of its own to fall back to.
	by := req.By
	var removed provision.Removed
	err := d.engine.dropBlock(ctx, d.kind, by, func(ctx context.Context) error {
		// THE SEATS FIRST, while the block still holds the credentials their
		// vendor authenticates with: turned off, revoked, and removed where
		// asked, read from the ledger rather than the document so an account
		// whose seat left the document is reached too.
		if err := d.engine.tearDownSeatAccess(ctx, d.kind, by, req.RemoveSeats); err != nil {
			return err
		}
		if d.pass == nil {
			// No kind in this build reaches this (Slack's step is
			// [slackTeardown]); kept so a kind added with a read-only pass
			// still HAS a disconnector, or its intent sits on the row for
			// ever.
			return nil
		}
		var err error
		removed, err = d.pass.Teardown(ctx, setup.TeardownInput{
			RemoveSeats: req.RemoveSeats, By: by,
		})
		if err != nil {
			return err
		}
		// Deleted here and in no vendor package: [Engine.forgetRemoved].
		return d.engine.forgetRemoved(ctx, d.kind, removed, by)
	})
	return removed, err
}

// forgetRemoved deletes the sealed values whose accounts a teardown removed.
//
// The one writer of that deletion ([provision.TokenSink.Forget]), never a
// vendor package: a credential left sealed keeps the tracker's identity
// cache, keyed on the CREDENTIAL, on an account that is gone.
//
// Run before [Engine.dropBlock] drops the block, whose `${VAR}`s every name
// came from. A failure, an unbuildable sink included (an engine without its
// secret store), is returned rather than logged, holding the surface in
// PhaseDisconnecting for the next tick instead of dropping the block over
// credentials still sealed.
func (e *Engine) forgetRemoved(
	ctx context.Context, kind integration.Kind, removed provision.Removed,
	by iam.Actor,
) error {
	names := removed.Secrets()
	if len(names) == 0 {
		return nil
	}
	sink, err := e.SetupSink(by)
	if err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	if err := sink.Forget(ctx, names...); err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	log.InfoContext(ctx, "removed_credentials_deleted",
		"integration", kind.String(), "accounts", removed.Handles(), "secrets", names)
	return nil
}

// dropBlock runs the third-party app step and then removes the block, in that
// order, recording the removal as by's.
func (e *Engine) dropBlock(
	ctx context.Context, kind integration.Kind, by iam.Actor,
	vendor func(context.Context) error,
) error {
	// BOTH PRECONDITIONS BEFORE THE VENDOR IS TOUCHED. Each is something
	// THIS NODE lacks, so each answers "not yet": the row is left alone, no
	// attempt is counted, and a node that has it finishes the disconnect. A
	// teardown that ran and then could not drop the block would leave an
	// integration configured, live and stripped of what made it work.
	if e.Company() == nil {
		// NO ACTIVE REVISION: every pass reads its credential off
		// `Company().Config`. Guarded here, the loop's other way into a
		// pass, as [passConverger.Run] is at its own boundary, rather than
		// in each Teardown: the loop reads its rows off the COORDINATION
		// store, so a company-less node finds them, and an unguarded read
		// is a nil dereference in the loop's detached goroutine that takes
		// the whole process down.
		return fmt.Errorf("%w: this node has no active company revision",
			integration.ErrDisconnectUnavailable)
	}
	writer := e.configWriterOrNil()
	if writer == nil {
		// NO CONFIG SURFACE YET: only the window between the loop being
		// armed at construction and the writer being installed, so the
		// next tick finishes. The writer must be installed on every node,
		// a worker-only one (`api.port: 0`) included, or this is permanent
		// there on a fleet-singleton loop.
		return fmt.Errorf("%w: no config surface is wired on this node",
			integration.ErrDisconnectUnavailable)
	}
	// NO GUARD IS TAKEN HERE: the loop's worker holds it around the whole
	// teardown and its status write (the Guard wired in `startIntegrations`),
	// so ctx is the guard's, bounded by [setup.PassDeadline]. [setup.Runner]'s
	// in-process claim is not reentrant, so taking it again would answer
	// ErrDisconnectUnavailable on every tick for ever. As in
	// `passConverger.Reconcile`.
	if err := vendor(ctx); err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	// THE BLOCK AND THE AGENTS ON THROUGH IT, in one revision: the document
	// refuses agents turned on at an integration the company no longer has,
	// so dropping the block alone could never finish while one was on.
	seatsOff, err := writer.Disconnect(ctx, kind, "disconnect "+string(kind), by)
	if err != nil {
		// The vendor work is durable, so a retry re-runs a teardown with
		// nothing left to remove and tries the block again: every teardown
		// is safe to repeat.
		return fmt.Errorf("%w: remove the %s block: %w", integration.ErrBlockNotRemoved, kind, err)
	}
	if len(seatsOff) > 0 {
		log.InfoContext(ctx, "disconnect_seats_turned_off",
			"integration", kind.String(), "seats", seatsOff,
			"detail", "each keeps what it was given, so connecting again and turning it on restores it")
	}
	return nil
}

// disconnectors pairs every surface with the seam the loop removes it
// through, and with the step that removes what it holds.
//
// EVERY SURFACE: one with no teardown still has a BLOCK, and a disconnect no
// node could complete would sit on the fleet row for ever, Disconnecting.
func (e *Engine) disconnectors() map[integration.Kind]integration.Disconnector {
	tearers := map[integration.Kind]teardown{}
	for _, pass := range e.setupPasses() {
		if tearer, ok := pass.(setup.Teardowner); ok {
			tearers[pass.Kind()] = tearer
		}
	}
	// THE ONE STEP THAT IS NOT A PASS'S. Slack's pass removes nothing at
	// Slack (a person deletes an app), so it is no Teardowner, yet every
	// seat holds a sealed bot token and signing secret on the company
	// document for a disconnect to remove. Registered by name, the only
	// such entry.
	tearers[integration.KindSlack] = slackTeardown{engine: e}
	out := map[integration.Kind]integration.Disconnector{}
	for _, kind := range integration.Kinds {
		out[kind] = vendorDisconnect{engine: e, kind: kind, pass: tearers[kind]}
	}
	return out
}

// RecordGitHubInstallation writes an installation the loop discovered onto
// one seat, or clears one it found gone.
//
// Written through [Engine.editGitHubSeat].
func (e *Engine) RecordGitHubInstallation(ctx context.Context, handle string, id int64) error {
	summary := "record " + handle + "'s GitHub installation"
	if id == 0 {
		// A cleared id is an uninstall somebody performed at GitHub, and the
		// revision's summary says so for whoever reads the list.
		summary = "clear " + handle + "'s GitHub installation, which is gone at GitHub"
	}
	return e.editGitHubSeat(ctx, handle, summary, func(block map[string]any) {
		block["installation_id"] = id
	})
}

// ForgetGitHubApp clears the app a seat names, because GitHub no longer has
// it.
//
// A person deletes an app at GitHub, which tells the engine nothing, leaving
// an app id that 404s on every call and a step nobody can take; clearing it
// puts the seat back to the one available act, creating another app.
//
// THE SEALED KEY IS LEFT: it is named per seat, so the next app overwrites
// it, and deleting a credential on one remote 404 is too destructive.
func (e *Engine) ForgetGitHubApp(ctx context.Context, handle string) error {
	return e.editGitHubSeat(ctx, handle,
		"clear "+handle+"'s GitHub App, which no longer exists at GitHub",
		func(block map[string]any) {
			for _, field := range []string{
				"app_id", "app_slug", "installation_id", "private_key", "webhook_secret",
			} {
				delete(block, field)
			}
		})
}

// editGitHubSeat reads one seat's GitHub block, applies an edit and writes it
// back through the entity route.
//
// Through the entity route, for the reason [ConfigWriter.Seat] gives. Refused
// rather than skipped when no writer is installed: a pass that silently failed
// to record what it found would rediscover it every tick and never say why the
// seat stays unready.
func (e *Engine) editGitHubSeat(
	ctx context.Context, handle, summary string, edit func(block map[string]any),
) error {
	writer := e.configWriter.Load()
	if writer == nil {
		// The same cause as a disconnect's, so the same sentinel, which
		// [integration.Worker.tearDown] keys on with errors.Is; wrapped to
		// say what it refuses, since no disconnect is in flight.
		return fmt.Errorf(
			"%w: no config surface is wired on this node, so %s's GitHub app "+
				"cannot be recorded yet", integration.ErrDisconnectUnavailable, handle)
	}
	body, err := (*writer).Seat(ctx, handle)
	if err != nil {
		return fmt.Errorf("engine: read the seat %s: %w", handle, err)
	}
	var role map[string]any
	if decodeErr := json.Unmarshal(body, &role); decodeErr != nil {
		return fmt.Errorf("engine: decode the seat %s: %w", handle, decodeErr)
	}
	integrations, _ := role["integrations"].(map[string]any)
	if integrations == nil {
		return fmt.Errorf("engine: the seat %s has no integrations block", handle)
	}
	block, _ := integrations["github"].(map[string]any)
	if block == nil {
		return fmt.Errorf("engine: the seat %s has no github app to record against", handle)
	}
	edit(block)
	integrations["github"] = block
	role["integrations"] = integrations

	updated, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("engine: encode the seat %s: %w", handle, err)
	}
	return (*writer).SetSeat(ctx, handle, updated, summary, loopActor())
}
