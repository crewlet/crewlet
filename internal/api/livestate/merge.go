package livestate

// mergeOverlay writes the live fields onto a static config row.
//
// Field by field rather than through a JSON round trip, because the row is a
// map the caller owns and a round trip would re-encode every static field it
// already holds — and because the KEYS are the frozen wire protocol, so they
// belong written out where a reader can see them next to the struct tags they
// must match.
func mergeOverlay(row map[string]any, o Overlay) {
	row["state"] = o.State
	row["runtime_id"] = o.RuntimeID
	row["current_task"] = o.CurrentTask
	row["current_phase"] = o.CurrentPhase
	row["current_iteration"] = o.CurrentIteration
	row["input_tokens"] = o.InputTokens
	row["output_tokens"] = o.OutputTokens
	row["total_tokens"] = o.TotalTokens
	row["live_call"] = o.LiveCall
	row["last_error"] = o.LastError
	row["budget"] = o.Budget
	row["afk_reason"] = o.AFKReason
}
