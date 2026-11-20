/**
 * A model as every model picker on Settings › Models draws it: its harness's
 * mark, its key and, quieter on the same line, the model it runs by name
 * ("sub-4 · Claude Opus 5.5", `modelNames.ts`), on the trigger while it is the
 * one chosen and on each option, with its condition in a word on an option.
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

/** A model's key and, quieter, the model it runs by name: one line, cut at its end where it is long. */
export function modelLabel(m: Pick<ModelView, "key" | "modelName">): ReactNode {
  return (
    <span className="models-option-label" title={`${m.key} · ${m.modelName}`}>
      <span className="models-option-key">{m.key}</span>
      {/* SPACES KEPT IN THE TEXT, so the name reads "sub-4 · Claude Opus 5.5" to every reader. */}
      <span className="models-option-sep">{" · "}</span>
      <span className="models-option-name">{m.modelName}</span>
    </span>
  );
}

/** A model as a picker's option, with `extra` beside its condition (a fast model's mark). */
export function modelOption(m: ModelView, extra?: ReactNode): SelectOption {
  const health = HEALTH[m.entry.health];
  return {
    value: m.key,
    label: modelLabel(m),
    text: m.key,
    leading: modelMark(m),
    hint: (
      <span className="models-option-hint">
        <StatusDot tone={health.tone}>{health.label}</StatusDot>
        {extra}
      </span>
    ),
  };
}
