// One LLM record shape, whatever it arrived as.
//
// A record reaches the dashboard two ways: as history, projected by the
// event store (`timescaledb/projections.py`), and as a live event
// envelope carrying the raw phase payload. Those two used to be
// normalized by separate hand-written field lists — agent.js had one,
// eventDetail.js had another, and the store had two more. When the phase
// event gained its failure fields, the lists that did not know about
// them silently deleted the failure: the dashboard rendered "No response
// text yet" for a call that had, in fact, failed with a reason.
//
// These normalizers pass the payload through and add only what the shape
// needs. Passing through is the point — a field added to the event
// upstream arrives here automatically instead of waiting for four
// whitelists to be updated in lockstep.

/** A record from an `agent_phase_completed` payload. */
export function recordFromPhase(payload, timestamp) {
  const p = payload || {};
  const messages = [];
  if (p.system_prompt) messages.push({ role: "system", content: p.system_prompt });
  if (p.user_prompt) messages.push({ role: "user", content: p.user_prompt });
  return {
    ...p,
    timestamp: timestamp || p.timestamp || "",
    model: p.model || p.provider_key || "",
    prompt: p.user_prompt || "",
    prompt_messages: messages,
  };
}

/** A record from an `agent_turn_completed` payload. */
export function recordFromTurn(payload, timestamp) {
  const p = payload || {};
  return {
    ...p,
    timestamp: timestamp || p.timestamp || "",
    phase: "turn",
    iteration: 0,
    prompt_messages: p.prompt_messages || [],
  };
}

/** A record from either event type, or `null` for anything else. */
export function recordFromEvent(ev) {
  if (!ev) return null;
  if (ev.type === "agent_phase_completed") {
    return recordFromPhase(ev.payload, ev.timestamp);
  }
  if (ev.type === "agent_turn_completed") {
    return recordFromTurn(ev.payload, ev.timestamp);
  }
  return null;
}

/**
 * The record for an agent's in-flight call, or `null`.
 *
 * The live call comes from the server's projection — it is the same
 * object a reconnecting tab gets in its snapshot, which is why an
 * in-flight call survives a refresh. A call the server marked failed
 * keeps its `error` and is no longer `in_progress`.
 */
export function recordFromLiveCall(call) {
  if (!call || call.turn_id === undefined) return null;
  return {
    ...call,
    // Stable identity for this call's expand/collapse state. A finished
    // record is pinned by its timestamp — a resumed Execute phase
    // publishes two records under the same (turn, phase, iteration) and
    // only the timestamp separates them — but a streaming call must not
    // be: `updated_at` moves on every round, so a timestamped key hands
    // each round a fresh identity and silently re-opens whatever the
    // reader had collapsed mid-stream. Now that reasoning streams too,
    // that is a block someone is actually reading.
    _key: `live|${call.turn_id}|${call.phase || ""}|${call.iteration || 0}`,
    _live: call.in_progress !== false,
    _failed: !!call.failed,
    timestamp: call.updated_at || "",
    iteration: call.iteration || 0,
    rounds: call.rounds || 0,
    tool_executions: call.tool_executions || [],
    prompt_messages: call.prompt_messages || [],
  };
}
