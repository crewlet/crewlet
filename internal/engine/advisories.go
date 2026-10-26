package engine

import (
	"context"
	"log/slog"

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
// THE ADVISORIES ONLY. The dangling references and the admission rules a
// revision breaks are the two other kinds of [config.Warning], and the apply
// logs each already under its own name (org_dangling_reference,
// org_admission_warning); repeating them here would warn twice about one
// fact.
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
// node's tool bridge does not reach: all of them where the node binds no
// listener, and otherwise each cell [mcpbridge.Bridge.Reaches] has no address
// for — a `direct` box dials the node's own listener on loopback, and a box in
// any other cell needs [mcpbridge.BaseURLVar].
//
// THE CELLS THE SEATS' BOXES ARE PLACED IN ([config.Company.SeatBoxes]), and
// not the catalogue's default alone: a fallback member's `run_in` is a cell a
// launch reaches too, and the one an operator forgets.
func bridgeGaps(cfg *config.Company, listens bool, bridge *mcpbridge.Bridge) []bridgeGap {
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
		case !listens:
			fix = "this node binds no HTTP listener (api.port is 0), so it serves no " +
				"tool bridge at all: set api.port on this node"
		case bridge.Reaches(box.Cell == config.PlacementDirect) != "":
			continue
		case box.Cell == config.PlacementDirect:
			fix = "a direct box dials this node's own listener on loopback, and this " +
				"engine was given none to dial: set " + mcpbridge.BaseURLVar +
				" to a URL this host reaches the node at"
		default:
			fix = "a box in this cell is not on this node's host, so it cannot dial the " +
				"node's loopback: set " + mcpbridge.BaseURLVar + " to a URL that box " +
				"reaches this node at"
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
// without the role opens no session and serves no bridge, and a line telling
// it to would be about its peers' setting, read on the wrong node.
func (e *Engine) reportBridgeGaps(ctx context.Context, logger *slog.Logger,
	target coord.Activation, cfg *config.Company,
) {
	if !e.profile.RunsSeats() {
		return
	}
	listens := e.boot == nil || e.boot.API.Port != 0
	for _, gap := range bridgeGaps(cfg, listens, e.bridge) {
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
