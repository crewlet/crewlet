package engine

import (
	"context"
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
			// NOTHING REGISTERED AT THE VENDOR. Datadog's webhook is
			// created by a person in Datadog's own UI pointing at this
			// engine, and Slack's apps are made from the command line,
			// so neither has anything this engine put there to take
			// away. Dropping the block is the whole disconnect, and a
			// third-party app with no teardown must still HAVE a disconnector or
			// the intent sits on the row for ever.
			return nil
		}
		return d.pass.Teardown(ctx, setup.TeardownInput{RemoveSeats: removeSeats})
	})
}

// dropBlock runs the third-party app step and then removes the block, in that order.
func (e *Engine) dropBlock(
	ctx context.Context, kind integration.Kind, vendor func(context.Context) error,
) error {
	writer := e.configWriterOrNil()
	if writer == nil {
		// REFUSED BEFORE THE VENDOR IS TOUCHED, and reported as "not
		// yet" rather than as a failure: this is normally the window
		// between the loop arming and the API wiring, which resolves on
		// its own within a second. A teardown that ran and then could
		// not remove the block would leave an integration configured,
		// live, and stripped of everything that made it work.
		return fmt.Errorf("%w: no config surface is wired on this node",
			integration.ErrDisconnectUnavailable)
	}
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
// EVERY SURFACE, not only the ones with something to remove. A third-party app with no
// teardown still has a BLOCK, and a disconnect for it that no node could
// complete would leave the intent on the fleet row for ever with the screen
// reporting Disconnecting and nothing moving. Datadog and Slack are that
// case: neither has anything this engine registered at the third-party app.
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
