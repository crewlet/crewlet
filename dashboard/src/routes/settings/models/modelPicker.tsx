/**
 * A model as every model picker draws it: its harness's mark, its key and,
 * quieter on the same line, the model it runs by name ("sub-4 · Claude Opus
 * 5.5", `modelNames.ts`), with its condition in a word on an option. Every
 * picker lists the Fleets first, then the Models.
 */

import type { ReactNode } from "react";
import { StatusDot, type SelectOption } from "@crewlethq/ui";
import { GaugeGlyph, LayersGlyph } from "@crewlethq/icons/glyphs";
import type { Fleet } from "~/protocol/fleets.ts";
import type { ModelView } from "./data.ts";
import { fleetLabel, fleetRef, fleetRoom } from "~/lib/fleets.ts";
import { HarnessMark } from "./HarnessMark.tsx";
import { HEALTH, isFastModel } from "./words.ts";

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

/** A fleet as a picker's option: its name, and how many of its members have room. */
export function fleetOption(f: Fleet): SelectOption {
  return {
    value: fleetRef(f.key),
    label: fleetLabel(f),
    text: fleetLabel(f),
    group: "Fleets",
    leading: (
      <span className="models-option-mark">
        <LayersGlyph size="sm" />
      </span>
    ),
    description: fleetRoom(f),
  };
}

/** A fast model's mark inside a picker whose job runs on a deadline. */
const FAST = (
  <span className="models-option-fast" title="Fast model">
    <GaugeGlyph size="xs" />
    <span className="sr-only">Fast model</span>
  </span>
);

/**
 * A picker's options: the Fleets, then the Models. `coding` keeps only what
 * runs a seat's executor; `fast` marks a fast model; `helper` holds a model the
 * engine says cannot run a helper, with its reason.
 */
export function pickerOptions(
  models: readonly ModelView[],
  fleets: readonly Fleet[],
  use: { coding?: boolean; fast?: boolean; helper?: boolean; executors?: readonly string[] },
): SelectOption[] {
  // A HELPER RUNS ON A FLEET ONLY WHERE EVERY MEMBER CAN RUN ONE: the engine's reason, by member.
  const helperProblem = (f: Fleet) => {
    for (const m of f.members) {
      const problem = models.find((x) => x.key === m.entry)?.admin?.helper_problem;
      if (problem) return `${m.entry}: ${problem}`;
    }
    return null;
  };
  const fleetOptions = fleets
    .filter((f) => !use.coding || !f.harness || (use.executors ?? []).includes(f.harness))
    .map((f) => {
      const problem = use.helper ? helperProblem(f) : null;
      return problem ? { ...fleetOption(f), description: problem, disabled: true } : fleetOption(f);
    });
  const modelOptions = models
    .filter((m) => !use.coding || m.executor)
    .map((m) => {
      const option = modelOption(m, use.fast && isFastModel(m.model) ? FAST : undefined);
      const problem = use.helper ? (m.admin?.helper_problem ?? null) : null;
      return {
        ...option,
        group: "Models",
        ...(problem !== null ? { description: problem, disabled: true } : {}),
      };
    });
  return [...fleetOptions, ...modelOptions];
}
