/**
 * Who runs on which model, under the list of models: a titled section, one
 * card, one row anatomy for everything that runs on a model ([WhoRuns]). Two
 * quiet groups, Helpers (Ask Crewlet and the Advisor) and Seats, their rows
 * divided by hairlines; each row its identity on the left (a helper's product
 * mark and name, a seat's avatar and name linking to the seat) and its model's
 * quiet picker on the right, in one column of one width. No header row: the
 * rows say what they are.
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

import { useId, useMemo, useState, type ReactNode } from "react";
import {
  Card,
  Menu,
  Section,
  Select,
  TableEntityCell,
  Text,
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
 * One row: who on the left, the model on the right in the card's one column,
 * and the row's menu at its end (an empty place where it has none, so every
 * picker stands at one width). `lines` are a split seat's categories, each
 * its short word under the name and its picker in the same column.
 */
function Row({
  who,
  model,
  menu,
  lines,
}: {
  who: ReactNode;
  model?: ReactNode;
  menu?: ReactNode;
  lines?: readonly { key: string; label: string; model: ReactNode }[];
}) {
  return (
    <li className="models-run">
      <span className="models-run__who">{who}</span>
      <span className="models-run__model">{model}</span>
      <span className="models-run__menu">{menu}</span>
      {lines?.map((line) => (
        <span key={line.key} className="models-run__line">
          <span className="models-run__label" aria-hidden="true">
            {line.label}
          </span>
          <span className="models-run__model">{line.model}</span>
        </span>
      ))}
    </li>
  );
}

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
  // WITH NO MODEL THERE IS NOTHING TO PICK (the page then draws its first-run card instead).
  const picks = data.admin && data.models.length > 0;
  const helpersId = useId();
  const seatsId = useId();
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
      <Card padding="none" className="models-runs">
        <section className="models-runs__group" aria-labelledby={helpersId}>
          <Text as="h3" variant="label" tone="secondary" id={helpersId}>
            Helpers
          </Text>
          <ul className="models-runs__list" aria-labelledby={helpersId}>
            {(["assistant", "advisor"] as const).map((helper) => {
              const key = data[helper].key ?? "";
              return (
                <Row
                  key={helper}
                  who={
                    <span className="models-run__name">
                      <JobGlyph job={helper} size="sm" />
                      <span>
                        {helper === "assistant" ? ASSISTANT_USE.label : ADVISOR_USE.label}
                      </span>
                    </span>
                  }
                  model={
                    picks ? (
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
                    )
                  }
                />
              );
            })}
          </ul>
        </section>
        <section className="models-runs__group" aria-labelledby={seatsId}>
          <Text as="h3" variant="label" tone="secondary" id={seatsId}>
            Seats
          </Text>
          {data.seats.length === 0 ? (
            <Text as="p" variant="caption" tone="tertiary">
              No agent seat yet.
            </Text>
          ) : (
            <ul className="models-runs__list" aria-labelledby={seatsId}>
              {data.seats.map((s) => (
                <SeatRow
                  key={s.handle}
                  seat={s}
                  one={oneOf(s.handle)}
                  cells={
                    Object.fromEntries(
                      CATEGORY_ORDER.map((c) => [c, cellOf(s.handle, c)]),
                    ) as Record<JobCategory, CellState>
                  }
                  admin={picks}
                  options={options}
                  byKey={byKey}
                  onPick={(key, categories) => setMoving({ key, seat: s.handle, categories })}
                />
              ))}
            </ul>
          )}
        </section>
        <SeatMoveDialog ask={moving} data={data} onClose={() => setMoving(null)} />
      </Card>
    </Section>
  );
}

/**
 * One seat's row: one picker where every job runs on one model (or none
 * does), a picker per category where they differ or once the reader splits it
 * from the row's menu. A member reads the values.
 */
function SeatRow({
  seat,
  one,
  cells,
  admin,
  options,
  byKey,
  onPick,
}: {
  seat: Seat;
  /** The model every job runs on, "" for none, or null where the jobs differ. */
  one: string | null;
  cells: Record<JobCategory, CellState>;
  admin: boolean;
  options: Record<JobCategory | "one", SelectOption[]>;
  byKey: ReadonlyMap<string, ModelView>;
  onPick: (key: string, categories: readonly JobCategory[]) => void;
}) {
  // SPLIT where its jobs differ; the reader's menu splits or folds it.
  const [split, setSplit] = useState(one === null);
  const name = seat.name || seat.handle;
  const who = (
    <TableEntityCell
      {...linkedSeat({
        handle: seat.handle,
        name: seat.name,
        kind: seat.kind,
        avatar: seat.avatar,
      })}
    />
  );
  const lineOf = (c: JobCategory) => {
    const cell = cells[c];
    const key = cell.kind === "one" ? cell.key : "";
    // A HAND SPLIT NAMES ITS MODELS, never a bare "Split" that says nothing.
    const placeholder = cell.kind === "split" ? cell.keys.join(" + ") : "Not set";
    return {
      key: c,
      label: CATEGORY_WORDS[c].short,
      model: admin ? (
        <Select
          width="auto"
          size="sm"
          appearance="quiet"
          ariaLabel={`${CATEGORY_WORDS[c].short} for ${name}`}
          value={key || undefined}
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
    return one !== null ? (
      <Row who={who} model={<Value model={byKey.get(one)} />} />
    ) : (
      <Row who={who} lines={CATEGORY_ORDER.map(lineOf)} />
    );
  }
  const menu = (
    <Menu
      label={`More for ${name}`}
      icon={<EllipsisGlyph size="sm" />}
      triggerVariant="ghost"
      align="end"
      items={[
        split
          ? { key: "one", label: "Use one model", onSelect: () => setSplit(false) }
          : { key: "split", label: "Split by job", onSelect: () => setSplit(true) },
      ]}
    />
  );
  return split ? (
    <Row who={who} menu={menu} lines={CATEGORY_ORDER.map(lineOf)} />
  ) : (
    <Row
      who={who}
      menu={menu}
      model={
        <Select
          width="auto"
          size="sm"
          appearance="quiet"
          ariaLabel={`${name}'s model`}
          value={one || undefined}
          // A SEAT WHOSE JOBS DIFFER, folded to one picker: the next pick sets all three.
          placeholder={one === null ? "Different per job" : "Not set"}
          options={options.one}
          onChange={(v) => String(v) !== one && onPick(String(v), CATEGORY_ORDER)}
        />
      }
    />
  );
}
