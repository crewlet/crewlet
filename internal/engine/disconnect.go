package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// ConfigWriter removes a block from the company document.
//
// The consumer's own interface, one method wide, because that is all a
// disconnect needs. The implementation is the same `PATCH /config` surface
// every other write goes through (one merge, one validation, one
// compare-and-set) rather than a second path onto the document.
type ConfigWriter interface {
	// Apply merges patch into the active revision and activates the
	// result.
	Apply(ctx context.Context, patch []byte, summary, operator string) error

	// Seat reads one seat's whole entity as JSON, and SetSeat writes it
	// back under the same handle.
	//
	// A SEPARATE PATH FROM Apply, and it has to be: a merge patch replaces
	// an array wholesale, so patching `roles` to change one seat would
	// delete every other one. The entity route addresses a seat by its
	// handle, which is its identity rather than its position.
	//
	// The GitHub pass writes here: an operator installs an agent's app in
	// a browser, which tells the engine nothing, so the loop discovers the
	// installation and records it against that one seat.
	Seat(ctx context.Context, handle string) ([]byte, error)
	SetSeat(ctx context.Context, handle string, body []byte, summary, operator string) error
}

// UseConfigWriter installs the surface a disconnect removes a block through.
//
// Installed rather than constructed here because the config surface is built
// where the API is, and the reconcile loop is armed when the engine itself is
// CONSTRUCTED, several hundred milliseconds earlier, measured. A disconnect
// ticking in that window reports [integration.ErrDisconnectUnavailable] and
// the loop leaves the row untouched, rather than running the third-party app teardown
// and finding it cannot remove the block: that would leave an integration
// configured, live, and stripped of everything that made it work.
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
// THE ORDER IS THE WHOLE POINT and is why [integration.Disconnector] is one
// call rather than two. The block carries the credential the third-party app teardown
// authenticates with, so removing it first strands every webhook and account
// the integration still has, with nothing left to authenticate a second
// attempt.
type vendorDisconnect struct {
	engine *Engine
	kind   integration.Kind
	// pass is nil for a third-party app that registers nothing at all.
	pass setup.Teardowner
}

func (d vendorDisconnect) Disconnect(ctx context.Context, removeSeats bool) error {
	return d.engine.dropBlock(ctx, d.kind, func(ctx context.Context) error {
		if d.pass == nil {
			// NOTHING REGISTERED AT THE VENDOR. Slack is the only
			// surface here with no pass at all — its apps are made from
			// the command line — so it is the only one that reaches
			// this branch, and there is nothing this engine put at
			// Slack for a teardown to take away. Dropping the block is
			// the whole disconnect, and a third-party app with no
			// teardown must still HAVE a disconnector or the intent
			// sits on the row for ever.
			return nil
		}
		return d.pass.Teardown(ctx, setup.TeardownInput{RemoveSeats: removeSeats})
	})
}

// dropBlock runs the third-party app step and then removes the block, in that order.
func (e *Engine) dropBlock(
	ctx context.Context, kind integration.Kind, vendor func(context.Context) error,
) error {
	// BOTH PRECONDITIONS BEFORE THE VENDOR IS TOUCHED. Each is a thing
	// THIS NODE lacks rather than anything wrong with the surface, so each
	// is reported as "not yet": the row is left alone, no attempt is
	// counted, and a node that has what is missing finishes the disconnect.
	// A teardown that ran and then could not remove the block would leave
	// an integration configured, live, and stripped of everything that
	// made it work.
	if e.Company() == nil {
		// NO ACTIVE REVISION. Every pass reads the credential it
		// authenticates with off `Company().Config`, so without one
		// there is nothing to authenticate as and no block to drop.
		//
		// GUARDED HERE rather than in each of the seven Teardown methods
		// for the reason [passConverger.Run] is guarded at its own
		// boundary: this is the loop's OTHER way into a pass, and the
		// two have to answer a company-less node the same way. Left to
		// the passes it was seven chances to forget, and the loop reads
		// its rows off the COORDINATION store — so a disconnect asked
		// for on a node that holds the document is found by one that
		// does not, and an unguarded read there is not an error the loop
		// records but a nil dereference in its detached goroutine,
		// taking down a process whose seats were running perfectly.
		return fmt.Errorf("%w: this node has no active company revision",
			integration.ErrDisconnectUnavailable)
	}
	writer := e.configWriterOrNil()
	if writer == nil {
		// NO CONFIG SURFACE YET. Normally the window between the loop
		// arming and the API wiring, which resolves on its own within a
		// second.
		return fmt.Errorf("%w: no config surface is wired on this node",
			integration.ErrDisconnectUnavailable)
	}
	// AND NOT WHILE SOMETHING ELSE IS WRITING AT THIS SURFACE. A teardown
	// and a provisioning pass are the two operations that write at the
	// third-party app, and letting them overlap is how a disconnect deletes
	// the webhook the pass beside it is registering. [setup.Runner.Hold] is
	// the one guard all three writers take, so a teardown reached through
	// the Disconnector takes it here rather than inventing a second one.
	release, held, err := e.holdSurface(ctx, kind)
	switch {
	case err != nil:
		return fmt.Errorf("%w: this node could not check whether %s is "+
			"being provisioned right now: %w",
			integration.ErrDisconnectUnavailable, kind, err)
	case !held:
		return fmt.Errorf("%w: a provisioning pass for %s is running",
			integration.ErrDisconnectUnavailable, kind)
	}
	defer release()
	if err := vendor(ctx); err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	patch := []byte(`{"integrations":{"` + string(kind) + `":null}}`)
	if err := writer.Apply(ctx, patch, "disconnect "+string(kind), "reconcile loop"); err != nil {
		// The third-party app work IS done and is durable, so a retry re-runs a
		// teardown with nothing left to remove and then tries the block
		// again. That is why every teardown is safe to repeat.
		return fmt.Errorf("engine: remove the %s block: %w", kind, err)
	}
	return nil
}

// disconnectors pairs every pass this build can tear down with the seam the
// loop removes it through.
//
// EVERY SURFACE, not only the ones with something to remove. A third-party app
// with no teardown still has a BLOCK, and a disconnect for it that no node
// could complete would leave the intent on the fleet row for ever with the
// screen reporting Disconnecting and nothing moving. Slack is that case, and
// the only one: it is the single kind with no pass, so it is the single kind
// with nothing this engine registered to take away. Datadog was in this
// sentence and is not any more — it registers its own webhook and tears it
// down again.
func (e *Engine) disconnectors() map[integration.Kind]integration.Disconnector {
	tearers := map[integration.Kind]setup.Teardowner{}
	for _, pass := range e.setupPasses() {
		if tearer, ok := pass.(setup.Teardowner); ok {
			tearers[pass.Kind()] = tearer
		}
	}
	out := map[integration.Kind]integration.Disconnector{}
	for _, kind := range integration.Kinds {
		out[kind] = vendorDisconnect{engine: e, kind: kind, pass: tearers[kind]}
	}
	return out
}

// RecordGitHubInstallation writes an installation the loop discovered onto
// one seat, or clears one it found gone.
//
// THROUGH THE ENTITY ROUTE, for the reason [ConfigWriter] gives: a merge
// patch on `roles` would replace the whole list. Refused rather than skipped
// when no writer is installed, because a pass that silently failed to record
// an adoption would rediscover the same installation on every tick and never
// say why the seat stays unready.
func (e *Engine) RecordGitHubInstallation(ctx context.Context, handle string, id int64) error {
	summary := "record " + handle + "'s GitHub installation"
	if id == 0 {
		// A CLEARED ID IS AN UNINSTALL SOMEBODY PERFORMED AT GITHUB, and
		// the summary says so: an operator reading the revision list
		// should not have to work out why the engine removed something.
		summary = "clear " + handle + "'s GitHub installation, which is gone at GitHub"
	}
	return e.editGitHubSeat(ctx, handle, summary, func(block map[string]any) {
		block["installation_id"] = id
	})
}

// ForgetGitHubApp clears the app a seat names, because GitHub no longer has
// it.
//
// AN APP IS DELETED BY A PERSON AT GITHUB, and GitHub tells the engine
// nothing. What is left behind is a record naming an app id that answers 404
// to every call: the seat mints no token, the install link is one GitHub
// itself 404s, and every surface reports a step nobody can take. Clearing it
// puts the seat back to the one act that is available, which is creating
// another app.
//
// THE SEALED KEY IS LEFT WHERE IT IS. It is named per seat, so the next app's
// conversion overwrites it, and deleting a credential on the strength of one
// remote 404 is a destructive answer to a question only GitHub can settle.
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
// THROUGH THE ENTITY ROUTE, for the reason [ConfigWriter] gives: a merge
// patch on `roles` would replace the whole list. Refused rather than skipped
// when no writer is installed, because a pass that silently failed to record
// what it found would rediscover the same thing on every tick and never say
// why the seat stays unready.
func (e *Engine) editGitHubSeat(
	ctx context.Context, handle, summary string, edit func(block map[string]any),
) error {
	writer := e.configWriter.Load()
	if writer == nil {
		// THE SENTINEL, WRAPPED WITH WHAT IT IS ACTUALLY REFUSING. Bare, it
		// reads "this node cannot complete a disconnect yet" — which is the
		// sentence its doc scopes it to and is not what happened: this is a
		// pass recording an app it just discovered, and no disconnect is in
		// flight. It stays comparable because the loop's own
		// [integration.Worker.tearDown] arm keys on errors.Is, and it is
		// the right sentinel: the cause is identical, the config surface
		// this node has not wired yet.
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
	return (*writer).SetSeat(ctx, handle, updated, summary, "reconcile loop")
}
