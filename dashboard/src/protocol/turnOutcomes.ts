/**
 * `GET /tokens/turn-outcomes`: the `turn_outcomes` question, Spend › Overview.
 * The engine's own shape is `queries.TurnOutcomesAnswer`; `rows` is always a list.
 */

/** One agent's started turns under one wake reason, as the turn ledger keys it. */
export interface TurnOutcomeRow {
  seat: string;
  agent_id: string;
  /** `schedule:<name>`, `work_item:<reason>`, `chat_message:<reason>` and the like. */
  wake: string;
  turns: number;
  tokens: number;
  /** The turns that committed nothing, and their tokens. */
  nothing: number;
  nothing_tokens: number;
}

export interface TurnOutcomesAnswer {
  from: string;
  to: string;
  rows: TurnOutcomeRow[];
}
