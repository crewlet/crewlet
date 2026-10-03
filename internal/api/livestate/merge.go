package livestate

// mergeOverlay writes the live fields onto a static config row.
//
// Field by field rather than through a JSON round trip, because the row is a
// map the caller owns and a round trip would re-encode every static field it
// already holds — and because the KEYS are the frozen wire protocol, so they
// belong written out where a reader can see them next to the struct tags they
// must match.
func mergeOverlay(row map[string]any, o Overlay) {
	// THE STATE IS ALWAYS WRITTEN, like every key below. The row it is
	// merged onto is the roster's, which carries no state of its own — the
	// projection is the only place a seat's state is computed — and a
	// client merges a pushed row over what it holds, so a key left out
	// reads as "unchanged" and a seat that started again would keep the
	// reason it had stopped for.
	row["activity"] = o.Activity
	row["stopped_reason"] = o.StoppedReason
	row["current_phase"] = o.CurrentPhase
	row["current_iteration"] = o.CurrentIteration
	row["live_call"] = o.LiveCall
	row["last_error"] = o.LastError
	row["budget"] = o.Budget
	row["turn"] = o.Turn
	row["last_turn"] = o.LastTurn
	row["paused"] = o.Paused
}
