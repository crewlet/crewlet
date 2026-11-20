/**
 * A model as every model picker on Settings › Models draws it: its harness's
 * mark before its key, on the trigger while it is the one chosen and on each
 * option, its model id quiet under it, and its condition in a word.
 */

import type { ReactNode } from "react";
import { StatusDot, type SelectOption } from "@crewlethq/ui";
import type { ModelView } from "./data.ts";
import { HarnessMark } from "./HarnessMark.tsx";
import { HEALTH } from "./words.ts";

/** The kit's small icon step: a mark beside a picker's words, at their height. */
const PICKER_MARK = 16;

/** A model's harness mark, sized for a picker's trigger, an option or a menu item. */
export function modelMark(m: Pick<ModelView, "harness">): ReactNode {
  return (
    <span className="models-option-mark" data-harness={m.harness}>
      <HarnessMark harness={m.harness} size={PICKER_MARK} />
    </span>
  );
}

/** A model as a picker's option, with `extra` beside its condition (a fast model's mark). */
export function modelOption(m: ModelView, extra?: ReactNode): SelectOption {
  const health = HEALTH[m.entry.health];
  return {
    value: m.key,
    label: m.key,
    text: m.key,
    leading: modelMark(m),
    description: <span className="mono">{m.model}</span>,
    hint: (
      <span className="models-option-hint">
        <StatusDot tone={health.tone}>{health.label}</StatusDot>
        {extra}
      </span>
    ),
  };
}
