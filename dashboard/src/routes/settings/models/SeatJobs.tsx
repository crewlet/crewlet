/**
 * Who runs on which model, under the list of models: a titled section, one
 * card, the kit's assignment list ([WhoRuns]). Two groups, Helpers (Ask
 * Crewlet and the Advisor) and Seats; each row its identity (a helper's mark
 * and name, a seat's avatar and name linking to the seat) and, right after
 * it, its model's picker. A seat whose pick waits for its confirm says what it
 * ran on before. No header row: the rows say what they are.
 *
 * WHAT A SEAT RUNS IS THE ENGINE'S ANSWER: each model's `uses` (the engine's
 * `used_by`), never resolved here.
 *
 * ONE PICKER FOR A SEAT ON ONE MODEL, which sets all three categories. "Split
 * by job" in the row's menu shows a picker per category under the seat's name
 * (Coding, Reviews, Routines), and "Use one model" folds them back; a seat
 * whose jobs already differ shows its three. A category whose two jobs the
 * document split by hand names its models on its picker ("sonnet + haiku").
 *
 * ONE INTERACTION FOR EVERY PICKER: a pick asks once, then saves in place, as
 * the helpers' do (`flows/SeatMove.tsx`): the model's uses with this seat's
 * categories added (all three from the one picker), written job by job, as
 * Finish and save writes them.
 *
 * EVERY MODEL IS NAMED IN FULL: its harness's mark, its key and the model it
 * runs by name (`modelPicker.tsx`). A model's condition is said once, by the
 * alarms and the list above, and in a word on each option; a fast model is
 * marked inside the pickers of a category with a deadline (`FAST_NOTE`). A
 * member reads the same rows with no picker and no menu.
 */

import { useMemo, useState } from "react";
import {
  AssignmentList,
  Card,
  Menu,
  Section,
  Select,
  Text,
  type AssignmentRow,
  type SelectOption,
} from "@crewlethq/ui";
import { EllipsisGlyph, GaugeGlyph } from "@crewlethq/icons/glyphs";
import { linkedSeat } from "~/app/frame/cells.tsx";
import {
  CATEGORY_JOBS,
  CATEGORY_ORDER,
  SEAT_JOBS,
  type JobCategory,
  type SeatJob,
} from "~/lib/modelUses.ts";
import type { Seat } from "~/lib/seats.ts";
import { HelperModel, type HelperChoice } from "~/components/HelperModel.tsx";
import type { ModelsData, ModelView } from "./data.ts";
import { JobGlyph } from "./JobGlyph.tsx";
import { modelLabel, modelMark, modelOption } from "./modelPicker.tsx";
import { SeatMoveDialog, type SeatMoveAsk } from "./flows/SeatMove.tsx";
import { ADVISOR_USE, ASSISTANT_USE, CATEGORY_WORDS, FAST_NOTE, isFastModel } from "./words.ts";

/** What runs a seat's category: one model, a split across its jobs, or nothing reported. */
type CellState =
  | { kind: "one"; key: string }
  /** A category whose jobs the document split by hand, with the models it runs on. */
  | { kind: "split"; keys: string[] }
  | { kind: "none" };

/** A fast model's mark inside a picker whose category has a deadline. */
const FAST = (
  <span className="models-option-fast" title="Fast model">
    <GaugeGlyph size="xs" />
    <span className="sr-only">Fast model</span>
  </span>
);

/**
 * A model as a member reads it: its mark, its key and its name, or the fallback,
 * in the quiet ink where it says nothing runs ("Not set") and not for a hand split's models.
 */
function Value({
  model,
  fallback = "Not set",
  absent = true,
}: {
  model: ModelView | undefined;
  fallback?: string;
  absent?: boolean;
}) {
  return (
    <Text
      as="span"
      variant="body"
      {...(!model && absent ? { tone: "tertiary" as const } : {})}
      className="models-run__value"
    >
      {model ? (
        <>
          {modelMark(model)}
          {modelLabel(model)}
        </>
      ) : (
        fallback
      )}
    </Text>
  );
}

/** Who runs on which model: Ask Crewlet and the Advisor, then every seat, on one card. */
export function WhoRuns({ data }: { data: ModelsData }) {
  /** A seat's pick, waiting for its one confirm. */
  const [moving, setMoving] = useState<SeatMoveAsk | null>(null);
  /** Seats the reader split by job, or folded back to one picker, by handle. */
  const [splits, setSplits] = useState<Record<string, boolean>>({});
  // WITH NO MODEL THERE IS NOTHING TO PICK (the page then draws its first-run card instead).
  const picks = data.admin && data.models.length > 0;
  const byKey = useMemo(() => new Map(data.models.map((m) => [m.key, m])), [data.models]);
  const runs = useMemo(() => {
    const out = new Map<string, string>();
    for (const m of data.models)
      for (const job of SEAT_JOBS)
        for (const seat of m.uses.jobs[job]) out.set(`${seat}/${job}`, m.key);
    return out;
  }, [data.models]);
  const keyFor = (seat: string, job: SeatJob) => runs.get(`${seat}/${job}`) ?? "";
  const cellOf = (seat: string, category: JobCategory): CellState => {
    const keys = new Set(CATEGORY_JOBS[category].map((job) => keyFor(seat, job)));
    if (keys.size > 1) return { kind: "split", keys: [...keys].map((k) => k || "no model") };
    const [only] = keys;
    return only ? { kind: "one", key: only } : { kind: "none" };
  };
  /** The model a seat runs every job on: its key, "" where it runs none, or null where its jobs differ. */
  const oneOf = (seat: string): string | null => {
    const key = keyFor(seat, SEAT_JOBS[0]!);
    return SEAT_JOBS.every((j) => keyFor(seat, j) === key) ? key : null;
  };

  // CODING IS OFFERED ONLY ON A HARNESS THAT CAN RUN A SEAT'S EXECUTOR, and so
  // is the one picker, which sets coding with the rest.
  const options = useMemo(() => {
    const coders = data.models.filter((m) => m.executor);
    return {
      one: coders.map((m) => modelOption(m)),
      ...Object.fromEntries(
        CATEGORY_ORDER.map((c) => [
          c,
          (c === "coding" ? coders : data.models).map((m) =>
            modelOption(m, FAST_NOTE[c] && isFastModel(m.model) ? FAST : undefined),
          ),
        ]),
      ),
    } as Record<JobCategory | "one", SelectOption[]>;
  }, [data.models]);
  const choices: HelperChoice[] = useMemo(
    () =>
      data.models.flatMap((m) =>
        m.admin
          ? [
              {
                key: m.key,
                model: m.model,
                problem: m.admin.helper_problem,
                leading: modelMark(m),
                label: modelLabel(m),
              },
            ]
          : [],
      ),
    [data.models],
  );

  return (
    <Section
      spacing="tight"
      title="Who runs on which model"
      description={
        picks
          ? "Choose the model each helper and seat runs on."
          : "The model each helper and seat runs on."
      }
    >
      <Card padding="none">
        <AssignmentList
          groups={[
            {
              key: "helpers",
              label: "Helpers",
              rows: (["assistant", "advisor"] as const).map((helper): AssignmentRow => {
                const key = data[helper].key ?? "";
                return {
                  key: helper,
                  name: helper === "assistant" ? ASSISTANT_USE.label : ADVISOR_USE.label,
                  icon: <JobGlyph job={helper} size="sm" />,
                  value: picks ? (
                    <HelperModel
                      helper={helper}
                      current={key || null}
                      choices={choices}
                      canAssign
                      appearance="quiet"
                      onApplied={data.reload}
                    />
                  ) : (
                    <Value model={byKey.get(key)} />
                  ),
                };
              }),
            },
            ...(data.seats.length > 0
              ? [
                  {
                    key: "seats",
                    label: "Seats",
                    rows: data.seats.map((s) =>
                      seatRow({
                        seat: s,
                        one: oneOf(s.handle),
                        cells: Object.fromEntries(
                          CATEGORY_ORDER.map((c) => [c, cellOf(s.handle, c)]),
                        ) as Record<JobCategory, CellState>,
                        admin: picks,
                        options,
                        byKey,
                        split: splits[s.handle] ?? oneOf(s.handle) === null,
                        onSplit: (on) => setSplits((now) => ({ ...now, [s.handle]: on })),
                        moving: moving?.seat === s.handle ? moving : null,
                        onPick: (key, categories) => setMoving({ key, seat: s.handle, categories }),
                      }),
                    ),
                  },
                ]
              : []),
          ]}
        />
        {data.seats.length === 0 ? (
          <Text as="p" variant="caption" tone="tertiary" className="models-runs__empty">
            No agent seat yet.
          </Text>
        ) : null}
        <SeatMoveDialog ask={moving} data={data} onClose={() => setMoving(null)} />
      </Card>
    </Section>
  );
}

/**
 * One seat's row: one picker where every job runs on one model (or none
 * does), a picker per category where they differ or once the reader splits it
 * from the row's menu. A pick waiting for its confirm shows the model picked
 * and says what the seat ran on. A member reads the values.
 */
function seatRow({
  seat,
  one,
  cells,
  admin,
  options,
  byKey,
  split,
  onSplit,
  moving,
  onPick,
}: {
  seat: Seat;
  /** The model every job runs on, "" for none, or null where the jobs differ. */
  one: string | null;
  cells: Record<JobCategory, CellState>;
  admin: boolean;
  options: Record<JobCategory | "one", SelectOption[]>;
  byKey: ReadonlyMap<string, ModelView>;
  split: boolean;
  onSplit: (split: boolean) => void;
  /** This seat's pick, while its confirm is open. */
  moving: SeatMoveAsk | null;
  onPick: (key: string, categories: readonly JobCategory[]) => void;
}): AssignmentRow {
  const name = seat.name || seat.handle;
  const link = linkedSeat({
    handle: seat.handle,
    name: seat.name,
    kind: seat.kind,
    avatar: seat.avatar,
  });
  const who = {
    key: seat.handle,
    name,
    icon: link.badge,
    href: link.href ?? undefined,
    nameTitle: link.title ?? undefined,
  };
  const picked = (categories: readonly JobCategory[]) =>
    moving !== null &&
    moving.categories.length === categories.length &&
    categories.every((c) => moving.categories.includes(c));
  // WHAT IT RAN ON, said while its pick waits: one model, or each category's.
  const was = (): string | undefined => {
    if (!moving) return undefined;
    const keys = [
      ...new Set(moving.categories.map((c) => (cells[c].kind === "one" ? cells[c].key : ""))),
    ];
    return keys.length === 1 ? keys[0] || "no model" : keys.map((k) => k || "no model").join(" + ");
  };
  const lineOf = (c: JobCategory) => {
    const cell = cells[c];
    const key = cell.kind === "one" ? cell.key : "";
    // A HAND SPLIT NAMES ITS MODELS, never a bare "Split" that says nothing.
    const placeholder = cell.kind === "split" ? cell.keys.join(" + ") : "Not set";
    return {
      key: c,
      label: CATEGORY_WORDS[c].short,
      value: admin ? (
        <Select
          width="full"
          size="sm"
          appearance="quiet"
          ariaLabel={`${CATEGORY_WORDS[c].short} for ${name}`}
          value={(picked([c]) ? moving!.key : key) || undefined}
          placeholder={placeholder}
          options={options[c]}
          onChange={(v) => String(v) !== key && onPick(String(v), [c])}
        />
      ) : (
        <Value model={byKey.get(key)} fallback={placeholder} absent={cell.kind === "none"} />
      ),
    };
  };
  if (!admin) {
    return one !== null
      ? { ...who, value: <Value model={byKey.get(one)} /> }
      : { ...who, lines: CATEGORY_ORDER.map(lineOf) };
  }
  const actions = (
    <Menu
      label={`More for ${name}`}
      icon={<EllipsisGlyph size="sm" />}
      triggerVariant="ghost"
      align="end"
      items={[
        split
          ? { key: "one", label: "Use one model", onSelect: () => onSplit(false) }
          : { key: "split", label: "Split by job", onSelect: () => onSplit(true) },
      ]}
    />
  );
  return split
    ? { ...who, was: was(), actions, lines: CATEGORY_ORDER.map(lineOf) }
    : {
        ...who,
        was: was(),
        actions,
        value: (
          <Select
            width="full"
            size="sm"
            appearance="quiet"
            ariaLabel={`${name}'s model`}
            value={(picked(CATEGORY_ORDER) ? moving!.key : one) || undefined}
            // A SEAT WHOSE JOBS DIFFER, folded to one picker: the next pick sets all three.
            placeholder={one === null ? "Different per job" : "Not set"}
            options={options.one}
            onChange={(v) => String(v) !== one && onPick(String(v), CATEGORY_ORDER)}
          />
        ),
      };
}
