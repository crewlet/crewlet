package engine

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// reportCompanyAdvisories logs every advisory the applied revision raises
// ([config.Company.AdvisoryWarnings]), one config_advisory line at WARN each,
// in the order the document raises them.
//
// AT APPLY, on every node, for the reason [reportDanglingRefs] gives: a
// revision reaches a node through a write another node served, a newer peer
// or a seed file, and only the apply sees the document the node is actually
// running. The seed is the case this exists for. A `/config` write and a
// dry run answer their advisories to whoever made them, but a company a node
// imports from `-company` or `-import-company` is answered to nobody, so
// without this line a company with nobody to invite, or a human seat nobody
// can message, ran silent for as long as nobody ran `crewlet validate` on the
// file.
//
// THE COMPANY'S OWN ADVISORIES ONLY ([config.Company.AdvisoryWarnings]). The
// dangling references and the admission rules a revision breaks are the two
// other kinds of [config.Warning], and what the company raises beside this
// node's Tier A ([config.NodeTierWarnings]) is a third set of warnings, read
// from both documents rather than from the company alone; the apply logs each
// already under its own name (org_dangling_reference, org_admission_warning,
// config_tier_warning — see [warnTiers]), and repeating them here would warn
// twice about one fact.
//
// The same event and the same three fields as the advisories `crewlet run`
// logs for its Tier A, with `config` naming the tier, so one query finds
// both; plus the revision and the epoch, because a company changes while the
// node runs and the line has to say which document it is about, and the
// seat or the unit when the advisory is about one, so it can be found without
// reading the path.
func reportCompanyAdvisories(ctx context.Context, logger *slog.Logger,
	target coord.Activation, cfg *config.Company,
) {
	for _, w := range cfg.AdvisoryWarnings() {
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
// node's tool bridge does not reach.
//
// THE FLEET'S REACH RULE ([mcpbridge.Bridge.Join]): a box dials a node that
// serves the bridge's route, and that node relays the call to whichever node
// holds the run's seat. So:
//
//   - offline is a node that serves the API with no listener (an ingress node
//     on api.port 0): it serves no route and joins no relay, so nothing
//     reaches a session it opens, in any cell, whatever else is set;
//   - with no bridge at all, nothing gives a box an address — not the fleet's
//     ([mcpbridge.BaseURLVar]), and not this node's own listener, which a
//     node has where it binds api.port where loopback reaches it;
//   - otherwise a cell is reached where [mcpbridge.Bridge.Reaches] has an
//     address for it: a `direct` box dials the node's own listener on
//     loopback where it has one — served to anyone on an ingress node, and
//     to this host's loopback alone on a node without the role — and every
//     cell dials the fleet's address where the variable gives one.
//
// A node WITHOUT the ingress role on api.port 0 is not offline: it binds
// nothing, and its boxes reach it through an ingress node's relay at the
// fleet's address, so the variable is its fix.
//
// THE CELLS THE SEATS' BOXES ARE PLACED IN ([config.Company.SeatBoxes]), and
// not the catalogue's default alone: a fallback member's `run_in` is a cell a
// launch reaches too, and the one an operator forgets.
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
			// NOT "the box is not on this host": a `container` box is, in a
			// network of its own, and a `direct` box on a node that binds
			// no listener loopback reaches (api.port 0, or one address that
			// is not loopback) shares the loopback but has nothing there to
			// dial. Either way the fleet's address is the box's only one.
			//
			// THE LINUX BIND IS PART OF THE FIX: a loopback address reaches a
			// container box under its runtime's name for the host, which Docker
			// Engine and Podman on Linux deliver from the bridge network, where
			// a loopback bind never hears it (ContainerReachWarnings). Docker
			// Desktop and Podman's machine deliver it on the host's loopback,
			// which the bridge admits under that name (internal/api/mcpbridge).
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

// reportBridgeGaps logs, at WARN, each cell the applied revision's executors
// launch in whose boxes this node's tool bridge does not reach
// ([bridgeGaps]): one executor_bridge_unreachable line per cell.
//
// ONCE PER APPLIED EPOCH, from the apply's success branch for the reason
// [reportCompanyAdvisories] runs there, rather than per turn: every launch in
// such a cell is refused naming the same fix, and a seat's every turn
// repeating it buries the one line an operator needs under the noise of the
// rest. The refusal stays — a run that cannot call a tool must not start —
// and this is the sentence that says, before the first turn, that it will
// come.
//
// ONLY ON A NODE THAT RUNS SEATS, because a launch happens only there: a node
// without the role launches no run and opens no session of its own — at most
// it relays a box's call to the seat's holder — and a line telling it to set
// what a launch needs would be about its peers' runs, read on the wrong node.
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
// against this node's own Tier A and host, as config_advisory lines at WARN.
//
// AT APPLY, for [reportCompanyAdvisories]' reason, and because only this
// node knows both halves: the bind is its Tier A and the cell is the company
// it now runs. `config` names the tier the remedy is written in.
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
