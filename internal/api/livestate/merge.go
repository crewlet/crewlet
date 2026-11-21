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
// in flight: every other [Overlay] field (activity, stop reason, phase, turn,
// budget, pause, usage limit, inbox, last error). A test reads the struct's
// tags to hold that, because a field the projection keeps and this does not
// write reaches no client at all.
//
// THE SIZE OF WHAT IT WRITES IS BOUNDED BY THE SEAT, NOT BY THE CALL, which
// grows with the turn: a roster read carrying every call would exceed a fixed
// budget like the assistant's 64 KiB step exactly while the company works. The
// roster ([LiveState.MergeRoster]) writes this half alone; the call has
// `live_call`. THE STATE IS ALWAYS WRITTEN, like every key below: a client reads
// a missing key as "unchanged", so a restarted seat would keep its stop reason.
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
	row["provider_limit"] = o.ProviderLimit
	row["waiting"] = o.Waiting
}
