import { plural } from "./format.ts";
import { andList } from "./words.ts";

/**
 * Calls a record did not keep, counted by tool — an entry of the executors
 * board's `bridge_elided_calls`, or of a phase record's `elided_calls`. `name`
 * is absent where not even the tool was kept.
 */
export interface DroppedCalls {
  name?: string;
  calls: number;
  failed?: number;
}

/**
 * What the calls a run's record dropped were, by tool, as a sentence that
 * follows the count: " By tool: read_file 120 times (3 failed) and
 * post_message once." Empty where the record kept no tool of any of them.
 * Calls past what the record keeps of them keep their count and lose their
 * tool, and are said as such.
 *
 * ONE WORDING for the board's log and the turn's execute card, so a run's
 * dropped middle reads the same wherever a reader meets it.
 */
export function droppedWords(dropped: readonly DroppedCalls[]): string {
  const byTool = new Map<string, { calls: number; failed: number }>();
  let unnamed = 0;
  for (const entry of dropped) {
    if (!entry.name) {
      unnamed += entry.calls;
      continue;
    }
    const tally = byTool.get(entry.name) ?? { calls: 0, failed: 0 };
    tally.calls += entry.calls;
    tally.failed += entry.failed ?? 0;
    byTool.set(entry.name, tally);
  }
  const parts = [...byTool.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, { calls, failed }]) => {
      const times = calls === 1 ? "once" : `${calls.toLocaleString()} times`;
      return failed > 0
        ? `${name} ${times} (${failed.toLocaleString()} failed)`
        : `${name} ${times}`;
    });
  if (unnamed > 0) parts.push(`${plural(unnamed, "call")} to tools the record didn’t keep`);
  return parts.length === 0 ? "" : ` By tool: ${andList(parts)}.`;
}
