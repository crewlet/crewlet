package livestate

// mergeOverlay writes the live fields onto a static config row: the seat's
// status ([mergeStatus]) and its call in flight.
//
// Field by field rather than through a JSON round trip, because the row is a
// map the caller owns and a round trip would re-encode every static field it
// already holds, and because the KEYS are the frozen wire protocol, so they
// belong written out where a reader can see them next to the struct tags they
// must match.
func mergeOverlay(row map[string]any, o Overlay) {
	mergeStatus(row, o)
	row["live_call"] = o.LiveCall
	row["live_call_seq"] = o.LiveCallSeq
}

// mergeStatus writes everything the overlay says about a seat EXCEPT its call
// in flight: what the seat is doing, why it stopped, its phase, its turn, its
// last turn, its budget, its pause and its last error.
//
// THE SIZE OF WHAT IT WRITES IS BOUNDED BY THE SEAT, NOT BY THE CALL. A call
// in flight carries its prompt, its rounds and its tool executions, which grow
// with the turn: measured on a live company, one seat's call was 8.8 KB of a
// 9.7 KB row mid-turn. A read of every seat that carried each one's call was
// therefore as large as the company's busiest turns, and a caller reading the
// roster under a fixed budget (the assistant's 64 KiB step) was refused it
// exactly while the company was working. The roster ([LiveState.MergeRoster])
// writes this half alone; the call has its own question (`live_call`).
//
// THE STATE IS ALWAYS WRITTEN, like every key below. The row it is merged onto
// is the roster's, which carries no state of its own (the projection is the
// only place a seat's state is computed), and a client merges a pushed row over
// what it holds, so a key left out reads as "unchanged" and a seat that
// started again would keep the reason it had stopped for.
func mergeStatus(row map[string]any, o Overlay) {
	row["activity"] = o.Activity
	row["stopped_reason"] = o.StoppedReason
	row["current_phase"] = o.CurrentPhase
	row["current_iteration"] = o.CurrentIteration
	row["last_error"] = o.LastError
	row["budget"] = o.Budget
	row["turn"] = o.Turn
	row["last_turn"] = o.LastTurn
	row["paused"] = o.Paused
}
