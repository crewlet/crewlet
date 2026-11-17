package engine

import (
	"context"
	"log/slog"
	"runtime"
	"slices"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// reportCompanyAdvisories logs every advisory the applied revision raises
// ([config.Company.AdvisoryWarnings], [BoxCredentialWarnings]) as a
// config_advisory line at WARN, in the document's order.
//
// AT APPLY, on every node ([reportDanglingRefs] has why): a company seeded
// from `-company` or `-import-company` is answered to nobody, so this is its
// only report. Only the company's own advisories: dangling references,
// admission rules and Tier A warnings each log under their own name
// ([warnTiers]), and repeating them here would warn twice about one fact.
//
// The fields are `crewlet run`'s Tier A advisories' (`config` names the tier,
// so one query finds both), plus the revision, the epoch, and the seat or unit.
func reportCompanyAdvisories(ctx context.Context, logger *slog.Logger,
	target coord.Activation, cfg *config.Company,
) {
	for _, w := range slices.Concat(cfg.AdvisoryWarnings(), BoxCredentialWarnings(cfg)) {
		attrs := []slog.Attr{
			slog.String("config", "company"),
			slog.String("revision", target.RevisionID),
			slog.Int64("epoch", target.Epoch),
			slog.String("path", w.Path),
			slog.String("detail", w.Message),
		}
		if w.Seat != "" {
			attrs = append(attrs, slog.String("seat", w.Seat))
		}
		if w.Unit != "" {
			attrs = append(attrs, slog.String("unit", w.Unit))
		}
		logger.LogAttrs(ctx, slog.LevelWarn, "config_advisory", attrs...)
	}
}

// bridgeGap is a cell this company's executors launch in whose boxes cannot
// reach this node's tool bridge, and what to set so they can.
type bridgeGap struct {
	cell config.Placement
	// seat is the first seat whose executor launches in the cell, and
	// seats how many do.
	seat  string
	seats int
	fix   string
}

// bridgeGaps is every cell the company's executors launch in whose boxes this
// node's tool bridge does not reach. Per [mcpbridge.Bridge.Join] a box dials a
// node serving the bridge's route, which relays to the seat's holder, so:
// offline (an ingress node on api.port 0) serves no route and nothing reaches
// it in any cell; with no bridge nothing gives a box an address; otherwise
// [mcpbridge.Bridge.Reaches] decides. A NON-ingress node on api.port 0 is not
// offline: its boxes reach it through an ingress node's relay, so
// [mcpbridge.BaseURLVar] is its fix.
//
// The cells are [config.Company.SeatBoxes]', not the catalogue's default
// alone: a fallback member's `run_in` is launched in too, and is the one an
// operator forgets.
func bridgeGaps(cfg *config.Company, offline bool, bridge *mcpbridge.Bridge) []bridgeGap {
	var gaps []bridgeGap
	at := map[config.Placement]int{}
	for box := range cfg.SeatBoxes() {
		if box.Cell == "" {
			continue
		}
		// One box per seat per cell, so a cell seen again is another seat.
		if i, seen := at[box.Cell]; seen {
			gaps[i].seats++
			continue
		}
		var fix string
		switch {
		case offline:
			fix = "this node serves the API with api.port 0, so it binds no listener for " +
				"the bridge's route and nothing relays a box's call to it: set api.port on " +
				"this node"
		case bridge == nil:
			fix = "this node has no tool bridge a box can dial: set " + mcpbridge.BaseURLVar +
				" to the fleet's address a box reaches (any node that serves the API, or a " +
				"load balancer in front of them)"
		case bridge.Reaches(box.Cell == config.PlacementDirect) != "":
			continue
		default:
			// NOT "the box is not on this host": a `container` box is, in its own
			// network, and a `direct` box on a node binding no loopback listener has
			// nothing there to dial; either way the fleet's address is its only one.
			// The Linux bind is part of the fix: Docker Engine and Podman on Linux
			// deliver a box's calls to the runtime's name for the host from the bridge
			// network, which a loopback bind never hears; Docker Desktop and Podman's
			// machine deliver them on the host's loopback, which the bridge admits
			// under that name (ContainerReachWarnings, internal/api/mcpbridge).
			fix = "this node hands a box in this cell no listener of its own (the box is " +
				"not on this host's loopback, or this node binds no listener there: " +
				"api.port 0, or api.host one address that is not loopback), so it " +
				"dials the fleet's address alone: set " + mcpbridge.BaseURLVar + " to the " +
				"fleet's address that box reaches (a loopback one reaches a container box " +
				"on this host under its runtime's name for the host, which on Linux needs " +
				"the listener it names to bind beyond loopback: api.host, or " +
				"api.public.host where api.public.port is set)"
		}
		at[box.Cell] = len(gaps)
		gaps = append(gaps, bridgeGap{cell: box.Cell, seat: box.Seat.Handle, seats: 1, fix: fix})
	}
	return gaps
}

// reportBridgeGaps logs, at WARN, one executor_bridge_unreachable line per
// cell [bridgeGaps] finds for the applied revision.
//
// ONCE PER APPLIED EPOCH, from the apply's success branch, not per turn: every
// launch in such a cell is refused with the same fix (the refusal stays — a
// run that cannot call a tool must not start), and a line per turn buries it.
// ONLY ON A NODE THAT RUNS SEATS: no other node launches a run, so the line
// there would be about its peers' runs, read on the wrong node.
func (e *Engine) reportBridgeGaps(ctx context.Context, logger *slog.Logger,
	target coord.Activation, cfg *config.Company,
) {
	if !e.profile.RunsSeats() {
		return
	}
	offline := e.boot != nil && e.boot.API.Port == 0 && e.profile.RunsIngress()
	for _, gap := range bridgeGaps(cfg, offline, e.bridge) {
		logger.LogAttrs(ctx, slog.LevelWarn, "executor_bridge_unreachable",
			slog.String("revision", target.RevisionID),
			slog.Int64("epoch", target.Epoch),
			slog.String("cell", string(gap.cell)),
			slog.String("seat", gap.seat),
			slog.Int("seats", gap.seats),
			slog.String("detail", "every executor run this node launches in the "+
				string(gap.cell)+" cell is refused: "+gap.fix))
	}
}

// reportContainerReach logs [ContainerReachWarnings] for the applied revision
// against this node's Tier A and host, as config_advisory lines at WARN: at
// apply, because only this node knows both halves (its bind and the company's
// cells). `config` names the tier the remedy is written in.
func reportContainerReach(ctx context.Context, logger *slog.Logger,
	target coord.Activation, boot *config.Bootstrap, cfg *config.Company,
) {
	for _, w := range ContainerReachWarnings(boot, cfg, runtime.GOOS) {
		logger.LogAttrs(ctx, slog.LevelWarn, "config_advisory",
			slog.String("config", "bootstrap"),
			slog.String("revision", target.RevisionID),
			slog.Int64("epoch", target.Epoch),
			slog.String("path", w.Path),
			slog.String("detail", w.Message))
	}
}
