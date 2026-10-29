/**
 * What a model is for, in two questions: what it should do (every seat job,
 * one category of work, the Assistant, or none yet) and for which seats
 * (chips, with all of them a switch away). Each category is named for the
 * work it covers, with its glyph; the engine fields it sets are in its
 * tooltip, and choosing it writes every one of them in the same revision.
 * Setting categories seat by seat is there, folded, for a model one choice
 * does not describe.
 *
 * THE CATEGORIES ARE `lib/modelUses.ts`'s (`CATEGORY_ORDER`, `CATEGORY_JOBS`)
 * and their words `words.ts`'s: nothing here names a pairing of jobs, so a
 * change of pairing is made in one place.
 *
 * EVERYTHING COMES FIRST, for a company that runs one model for all of it:
 * the seat's `llm` alone, every seat job resolving to it. The Assistant is not
 * a seat job, so Everything leaves it alone and it is a choice of its own,
 * company-wide, with no seats to pick.
 *
 * AN EDIT KEEPS ITS SEATS. Choosing what a model does, in an edit, starts from
 * the seats it serves now, never every seat: an edit made for its sign-in
 * moved two seats off the model they ran on when a choice reset its seats to
 * all of them. Adding a seat takes the reader's own chip or switch, and
 * Review lists every seat that changes model before anything is written
 * ([seatChangesOf]). A category with a deadline says quietly when the model is
 * not a fast one.
 */

import { useMemo } from "react";
import {
  Button,
  Callout,
  Checkbox,
  ChoiceList,
  Disclosure,
  FilterChip,
  FilterChipGroup,
  Switch,
  Text,
  Tooltip,
  type ChoiceListOption,
} from "@crewlethq/ui";
import { listed } from "~/lib/format.ts";
import {
  CATEGORY_JOBS,
  CATEGORY_ORDER,
  SEAT_JOBS,
  categoryState,
  intentOf,
  usesForIntent,
  withCategory,
  type Intent,
  type JobCategory,
  type SeatJob,
  type Uses,
} from "~/lib/modelUses.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { ModelsData } from "../../data.ts";
import { JobGlyph, JobName } from "../../JobGlyph.tsx";
import { seatName } from "../../places.ts";
import {
  ASSISTANT_USE,
  CATEGORY_WORDS,
  EVERYTHING,
  FAST_NOTE,
  categoryTip,
  isFastModel,
  product,
} from "../../words.ts";
import { canExecute, type Draft } from "../draft.ts";

/** The choices, in the order offered: every seat job, each category, the Assistant, none yet. */
const CHOICES: readonly Exclude<Intent, "custom">[] = [
  "all",
  ...CATEGORY_ORDER,
  "assistant",
  "none",
];

type Patch = Partial<Pick<Draft, "uses" | "intent" | "webDenied">>;

/** The model each seat runs a job on now, by handle, as the engine reports it. */
function currentFor(data: Pick<ModelsData, "models">, job: SeatJob): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of data.models) for (const seat of m.uses.jobs[job]) out.set(seat, m.key);
  return out;
}

/**
 * Every seat that changes model when `key`'s uses go from `was` to `next`, by
 * its name: a seat's category added to `key` says the model it runs on today
 * and that it moves to `key`; one taken off says it no longer runs on `key`.
 * The Assistant too. What Review lists before a write, so no move is a
 * surprise. Each line is a change from today; an edit that moves nothing has
 * none.
 */
export function seatChangesOf(
  data: Pick<ModelsData, "models" | "seats" | "assistant">,
  key: string,
  was: Uses,
  next: Uses,
): string[] {
  const name = key || "this model";
  const lines: string[] = [];
  for (const s of data.seats) {
    for (const category of CATEGORY_ORDER) {
      const jobs = CATEGORY_JOBS[category];
      const added = jobs.filter(
        (j) => next.jobs[j].includes(s.handle) && !was.jobs[j].includes(s.handle),
      );
      const removed = jobs.filter(
        (j) => was.jobs[j].includes(s.handle) && !next.jobs[j].includes(s.handle),
      );
      const what = `${s.name || s.handle}, ${CATEGORY_WORDS[category].label.toLowerCase()}`;
      if (added.length > 0) {
        const today = [
          ...new Set(
            added.flatMap((j) => {
              const now = currentFor(data, j).get(s.handle);
              return now && now !== key ? [now] : [];
            }),
          ),
        ];
        lines.push(`${what}: ${today.length > 0 ? listed(today) : "no model"} → ${name}`);
      }
      if (removed.length > 0) lines.push(`${what}: no longer on ${name}`);
    }
  }
  if (next.assistant && !was.assistant) {
    const today = data.assistant.enabled ? data.assistant.key : null;
    lines.push(`The Dashboard Assistant: ${today && today !== key ? today : "no model"} → ${name}`);
  }
  return lines;
}

/** Every seat a model's uses name, for any job. */
function servedBy(uses: Uses): string[] {
  return [...new Set(SEAT_JOBS.flatMap((j) => uses.jobs[j]))].sort();
}

const jobsOf = (intent: Exclude<Intent, "custom">): readonly SeatJob[] =>
  intent === "all"
    ? SEAT_JOBS
    : intent === "none" || intent === "assistant"
      ? []
      : CATEGORY_JOBS[intent];

/** The seats a choice starts with: those whose model for it cannot answer now, or else every seat. */
export function sensibleSeats(
  data: Pick<ModelsData, "models" | "seats">,
  intent: Exclude<Intent, "custom">,
): string[] {
  const all = data.seats.map((s) => s.handle);
  const jobs = jobsOf(intent);
  if (jobs.length === 0) return [];
  if (intent === "all") return all;
  const stuck = new Set<string>();
  for (const m of data.models) {
    const health = m.entry.health;
    if (health !== "sitting_out" && health !== "rejected" && health !== "no_sign_in") continue;
    for (const job of jobs) for (const seat of m.uses.jobs[job]) stuck.add(seat);
  }
  return stuck.size > 0 ? all.filter((h) => stuck.has(h)) : all;
}

/**
 * The plain-language account of a draft's uses, for the review: one sentence
 * per group of categories with the same seats, what each replaces, and the
 * Assistant on a line of its own. Seats by their names, never their handles
 * (design QA M8).
 */
export function summaryOf(data: Pick<ModelsData, "models" | "seats" | "assistant">, draft: Draft) {
  const key = draft.key || "this model";
  const all = data.seats.map((s) => s.handle);
  const name = (h: string) => seatName(data.seats, h);
  const who = (seats: readonly string[]) =>
    seats.length === all.length && all.length > 1 ? "Every seat" : listed(seats.map(name));
  const lines: string[] = [];
  if (draft.intent === "all") {
    const seats = draft.uses.jobs.executor;
    lines.push(`${who(seats)} will use ${key} for everything.`);
  } else {
    const groups = new Map<string, JobCategory[]>();
    for (const c of CATEGORY_ORDER) {
      const seats = all.filter((h) => categoryState(draft.uses, h, c) !== "none");
      if (seats.length === 0) continue;
      const id = seats.join(",");
      groups.set(id, [...(groups.get(id) ?? []), c]);
    }
    for (const [id, categories] of groups) {
      const seats = id.split(",");
      lines.push(
        `${who(seats)} will ${listed(categories.map((c) => CATEGORY_WORDS[c].verb))} with ${key}.`,
      );
    }
  }
  if (draft.uses.assistant) lines.push(`The Dashboard Assistant will answer with ${key}.`);
  if (lines.length === 0)
    return [`No seat uses ${key} yet. Every seat stays on the model it has now.`];
  return lines;
}

/** A choice's detail: what it does, and its note on a line of its own. */
function Detail({ hint, note }: { hint: string; note?: string }) {
  return (
    <span className="models-choice-detail">
      <span>{hint.replace(/\.$/, "")}</span>
      {note ? (
        <Text as="span" variant="caption" tone="tertiary">
          {note}
        </Text>
      ) : null}
    </span>
  );
}

export function UseItFor({
  draft,
  data,
  served,
  error,
  onChange,
}: {
  draft: Draft;
  data: Pick<ModelsData, "models" | "seats" | "assistant">;
  /** In an edit, what the model is used for today: a choice starts from its seats. Null when adding. */
  served: Uses | null;
  /** The engine's refusal of a seat or the assistant this write names, in words. */
  error?: string | undefined;
  onChange: (patch: Patch) => void;
}) {
  const all = data.seats.map((s) => s.handle);
  const executor = canExecute(draft);
  const assistantOn = data.assistant.enabled;
  const was = assistantOn ? data.assistant.key : null;
  // A MODEL THE ASSISTANT RUNS ON keeps it here: the assistant must run on
  // something, so it moves when another model is chosen for it.
  const holdsAssistant = !!draft.key && was === draft.key;
  const intentSeats = useMemo(() => {
    if (draft.intent === "custom") return [];
    const job = jobsOf(draft.intent)[0];
    return job ? draft.uses.jobs[job] : [];
  }, [draft.uses, draft.intent]);
  const noCoding = `${product(draft.agent)} cannot run coding runs. Choose Claude Code or OpenCode to code.`;
  const slow = draft.model.trim() !== "" && !isFastModel(draft.model);
  const denyWeb = (
    <Button size="small" variant="secondary" onClick={() => onChange({ webDenied: true })}>
      Deny web tools for this model
    </Button>
  );
  const webRule = (
    <Callout variant="warning" title="The assistant needs web tools denied">
      <span className="models-stack">
        <span>{ASSISTANT_USE.rule}</span>
        <span>{denyWeb}</span>
      </span>
    </Callout>
  );

  const options: ChoiceListOption[] = CHOICES.map((intent) => {
    if (intent === "none") {
      return {
        value: intent,
        label: "Not yet",
        media: <JobGlyph job="none" />,
        detail: "Set it up now, assign it later.",
        separated: true,
      };
    }
    if (intent === "assistant") {
      return {
        value: intent,
        label: (
          <Tooltip content={ASSISTANT_USE.tip} placement="top">
            <span>{ASSISTANT_USE.label}</span>
          </Tooltip>
        ),
        media: <JobGlyph job="assistant" />,
        ...(holdsAssistant ? { tag: "In use" } : {}),
        detail: <Detail hint={`${ASSISTANT_USE.hint} Company-wide, with no seats.`} />,
        ...(draft.intent === "assistant" && !draft.webDenied ? { panel: webRule } : {}),
        ...(!assistantOn
          ? {
              disabled: true,
              disabledReason: "The assistant is off. Turn it on in Settings, Assistant first.",
            }
          : {}),
      };
    }
    if (intent === "all") {
      return {
        value: intent,
        label: (
          <Tooltip content={EVERYTHING.tip} placement="top">
            <span>{EVERYTHING.label}</span>
          </Tooltip>
        ),
        media: <JobGlyph job="everything" />,
        detail: <Detail hint={EVERYTHING.hint} />,
        // EVERYTHING IS NOT THE ASSISTANT (design QA Minor 28): a model that
        // allows web tools is said, so the reader does not expect it there.
        ...(draft.intent === "all" && !draft.webDenied && assistantOn
          ? {
              panel: (
                <Text as="p" variant="caption" tone="secondary">
                  The Dashboard Assistant can't use this model while web tools are allowed.
                </Text>
              ),
            }
          : {}),
        ...(!executor ? { disabled: true, disabledReason: noCoding } : {}),
      };
    }
    const category: JobCategory = intent;
    const note = FAST_NOTE[category];
    return {
      value: intent,
      label: (
        <Tooltip content={categoryTip(category)} placement="top">
          <span>{CATEGORY_WORDS[category].label}</span>
        </Tooltip>
      ),
      media: <JobGlyph job={category} />,
      // UNDER EVERYTHING each category says it is included, so the one choice reads as all three.
      ...(draft.intent === "all" ? { tag: "Included" } : {}),
      // A LINE BELOW EVERYTHING, as above Not yet: the categories are its parts.
      ...(category === CATEGORY_ORDER[0] ? { separated: true } : {}),
      detail: <Detail hint={CATEGORY_WORDS[category].hint} {...(note && slow ? { note } : {})} />,
      ...(category === "coding" && !executor ? { disabled: true, disabledReason: noCoding } : {}),
    };
  });

  const choose = (intent: Intent) => {
    if (intent === "custom") return;
    onChange({
      intent,
      uses: usesForIntent(
        intent,
        // AN EDIT KEEPS THE SEATS IT SERVES; only an add starts from a sensible set.
        served ? servedBy(served) : sensibleSeats(data, intent),
        intent === "assistant" || holdsAssistant,
      ),
    });
  };
  const pickSeats = (seats: string[]) => {
    if (draft.intent === "none" || draft.intent === "custom" || draft.intent === "assistant")
      return;
    onChange({ uses: usesForIntent(draft.intent, seats, draft.uses.assistant) });
  };
  /** One seat's category switched on or off: every job in it at once. */
  const setSeatCategory = (seat: string, category: JobCategory, on: boolean) => {
    const uses = withCategory(draft.uses, seat, category, on);
    onChange({ uses, intent: intentOf(uses) });
  };
  const seatPicker =
    draft.intent !== "none" && draft.intent !== "custom" && draft.intent !== "assistant";
  // THE ASSISTANT BESIDE SEAT WORK is said under the choice, since the choice names one use.
  const assistantBeside = draft.uses.assistant && draft.intent !== "assistant";

  return (
    <div className="models-stack models-who">
      {/* THE STEP'S LEAD ASKS THE QUESTION (design QA Nit 6): the group is named, not headed twice. */}
      <ChoiceList
        label="What this model does"
        options={options}
        value={draft.intent === "custom" ? null : draft.intent}
        onChange={(v) => choose(v as Intent)}
      />

      {assistantBeside ? (
        <Text as="p" variant="caption" tone="secondary">
          It also answers in the Dashboard Assistant
          {holdsAssistant ? ", until another model is chosen for it" : ""}.
        </Text>
      ) : null}
      {assistantBeside && !draft.webDenied ? webRule : null}
      {error ? (
        <Callout variant="danger" title="The engine refuses this">
          {error}
        </Callout>
      ) : null}

      {seatPicker ? (
        <div className="models-who__seats">
          <div className="models-who__seats-head">
            <Text as="h3" variant="label">
              For which seats?
            </Text>
            <Switch
              label="All seats"
              checked={all.length > 0 && intentSeats.length === all.length}
              onCheckedChange={(on) => pickSeats(on ? all : [])}
            />
          </div>
          <FilterChipGroup label="Seats that use this model" hideLabel>
            {data.seats.map((s) => (
              <FilterChip
                key={s.handle}
                pressed={intentSeats.includes(s.handle)}
                onPressedChange={(on) =>
                  pickSeats(
                    on ? [...intentSeats, s.handle] : intentSeats.filter((h) => h !== s.handle),
                  )
                }
              >
                <span className="models-who__chip">
                  <SeatAvatar name={s.name} kind={s.kind} avatar={s.avatar} size="xs" decorative />
                  {s.name}
                </span>
              </FilterChip>
            ))}
          </FilterChipGroup>
        </div>
      ) : null}

      <Disclosure title="Customise per seat" defaultOpen={draft.intent === "custom"}>
        <div
          className="models-who__grid"
          style={{ ["--models-who-columns" as string]: CATEGORY_ORDER.length }}
        >
          <span />
          {CATEGORY_ORDER.map((c) => (
            <Text
              key={c}
              as="span"
              variant="caption"
              tone="secondary"
              className="models-who__phase"
            >
              <JobName job={c} />
            </Text>
          ))}
          {data.seats.map((s) => (
            <div key={s.handle} className="models-who__row">
              <span className="models-who__chip">
                <SeatAvatar name={s.name} kind={s.kind} avatar={s.avatar} size="xs" decorative />
                {s.name}
              </span>
              {CATEGORY_ORDER.map((c) => {
                const state = categoryState(draft.uses, s.handle, c);
                const blocked = c === "coding" && !executor;
                return (
                  <Checkbox
                    key={c}
                    size="sm"
                    hideLabel
                    label={`${s.name}: ${CATEGORY_WORDS[c].label} on ${draft.key || "this model"}`}
                    checked={state === "all"}
                    // ONE JOB OF A CATEGORY, written by hand, reads as part of it; a press sets both.
                    indeterminate={state === "some"}
                    disabled={blocked && state === "none"}
                    title={blocked ? noCoding : undefined}
                    onCheckedChange={(next) => setSeatCategory(s.handle, c, next)}
                  />
                );
              })}
            </div>
          ))}
        </div>
        {assistantOn ? (
          <Switch
            label="Also answer in the Dashboard Assistant"
            description={
              holdsAssistant
                ? "The assistant runs on this model. Choose another model for it to move it."
                : ASSISTANT_USE.tip
            }
            checked={draft.uses.assistant}
            disabled={holdsAssistant}
            onCheckedChange={(on) => {
              const uses = { ...draft.uses, assistant: on };
              // THE ASSISTANT ALONE IS A CHOICE OF ITS OWN; beside seat work it changes no choice.
              const intent =
                draft.intent === "none" && on
                  ? "assistant"
                  : draft.intent === "assistant" && !on
                    ? "none"
                    : draft.intent;
              onChange({ uses, intent });
            }}
          />
        ) : null}
        {!executor ? (
          <Text as="p" variant="caption" tone="secondary">
            {product(draft.agent)} serves every job but coding, so the Coding column is off.
          </Text>
        ) : null}
      </Disclosure>
    </div>
  );
}
