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
// every other write goes through — one merge, one validation, one
// compare-and-set — rather than a second path onto the document.
type ConfigWriter interface {
	// Apply merges patch into the active revision and activates the
	// result.
	Apply(ctx context.Context, patch []byte, summary, operator string) error
}

// UseConfigWriter installs the surface a disconnect removes a block through.
//
// Installed rather than constructed here because the config surface is built
// where the API is, and the engine starts before it. Until it is set, a
// disconnect is refused rather than half-done: the vendor teardown would run
// and the block would stay, which is the one outcome worse than not starting.
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
// call rather than two. The block carries the credential the vendor teardown
// authenticates with, so removing it first strands every webhook and account
// the integration still has, with nothing left to authenticate a second
// attempt.
type vendorDisconnect struct {
	engine *Engine
	pass   setup.Teardowner
}

func (d vendorDisconnect) Disconnect(ctx context.Context, removeSeats bool) error {
	writer := d.engine.configWriterOrNil()
	if writer == nil {
		// REFUSED BEFORE THE VENDOR IS TOUCHED. A teardown that ran and
		// then could not remove the block would leave an integration
		// configured, live, and stripped of everything that made it
		// work — worse than one that has not started, and not visible
		// as either.
		return fmt.Errorf(
			"engine: this node cannot remove a company block, so the vendor was " +
				"left alone; a node serving the config API will finish the disconnect")
	}
	kind := d.pass.Kind()
	if err := d.pass.Teardown(ctx, setup.TeardownInput{RemoveSeats: removeSeats}); err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	patch := []byte(`{"integrations":{"` + string(kind) + `":null}}`)
	if err := writer.Apply(ctx, patch, "disconnect "+string(kind), "reconcile loop"); err != nil {
		// The vendor work IS done and is durable, so a retry re-runs a
		// teardown with nothing left to remove and then tries the block
		// again. That is why every teardown is safe to repeat.
		return fmt.Errorf("engine: remove the %s block: %w", kind, err)
	}
	return nil
}

// disconnectors pairs every pass this build can tear down with the seam the
// loop removes it through.
func (e *Engine) disconnectors() map[integration.Kind]integration.Disconnector {
	out := map[integration.Kind]integration.Disconnector{}
	for _, pass := range e.setupPasses() {
		tearer, ok := pass.(setup.Teardowner)
		if !ok {
			continue
		}
		out[pass.Kind()] = vendorDisconnect{engine: e, pass: tearer}
	}
	return out
}
