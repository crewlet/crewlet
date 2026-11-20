/**
 * The two sections under the list of models, in this order: Ask Crewlet and
 * the Advisor on one line ([Helpers]), then the seats ([SeatJobs]).
 *
 * WHAT A SEAT RUNS IS THE ENGINE'S ANSWER: each model's `uses` (the engine's
 * `used_by`), never resolved here.
 *
 * ONE PICKER FOR A SEAT ON ONE MODEL. A seat whose every job runs on one model
 * shows one picker, which sets all three categories; "Split by job" shows a
 * picker per category, and a seat whose jobs already differ shows its three.
 * A category whose two jobs the document split by hand reads "Split" on its
 * picker.
 *
 * A PICK IS NOT A WRITE. Choosing a model opens THAT model's wizard on Use it
 * for with this seat's categories added (all three from the one picker), so the
 * change is reviewed and saved as any change to a model is, job by job.
 *
 * NOTHING SAID TWICE. Every model named in a picker carries its harness's
 * mark; a model's condition is said once, by the alarms and the list above,
 * and in a word on each option. A fast model is marked inside the pickers of
 * the categories with a deadline (`FAST_NOTE`), never in a note per row. A
 * member reads the same values with no picker.
 */

import { useMemo, useState } from "react";
import { Button, Card, Select, TableEntityCell, Text, type SelectOption } from "@crewlethq/ui";
import { GaugeGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
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
import { modelMark, modelOption } from "./modelPicker.tsx";
import type { OnFlow } from "./parts.tsx";
import { ADVISOR_USE, ASSISTANT_USE, CATEGORY_WORDS, FAST_NOTE, isFastModel } from "./words.ts";

/** What runs a seat's category: one model, a split across its jobs, or nothing reported. */
type CellState = { kind: "one"; key: string } | { kind: "split" } | { kind: "none" };

/** A fast model's mark inside a picker whose category has a deadline. */
const FAST = (
  <span className="models-option-fast" title="Fast model">
    <GaugeGlyph size="xs" />
    <span className="sr-only">Fast model</span>
  </span>
);

/** The model each job of each seat runs on, from the models' own uses. */
function useRuns(models: readonly ModelView[]) {
  return useMemo(() => {
    const out = new Map<string, string>();
    for (const m of models)
      for (const job of SEAT_JOBS)
        for (const seat of m.uses.jobs[job]) out.set(`${seat}/${job}`, m.key);
    return out;
  }, [models]);
}

/** The seats: one picker a seat on one model, a picker per category a split one. */
export function SeatJobs({ data, onFlow }: { data: ModelsData; onFlow: OnFlow }) {
  const runs = useRuns(data.models);
  const keyFor = (seat: string, job: SeatJob) => runs.get(`${seat}/${job}`) ?? "";
  const cellOf = (seat: string, category: JobCategory): CellState => {
    const keys = new Set(CATEGORY_JOBS[category].map((job) => keyFor(seat, job)));
    if (keys.size > 1) return { kind: "split" };
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
  const byKey = useMemo(() => new Map(data.models.map((m) => [m.key, m])), [data.models]);

  return (
    <Card padding="none">
      <Card.Header>
        <Card.Title>Seats</Card.Title>
      </Card.Header>
      <DataGrid<Seat>
        name="seats"
        rows={data.seats}
        rowKey={(s) => s.handle}
        empty={{ title: "No agent seat yet", hint: "Seats added to the company appear here." }}
        columns={[
          {
            key: "seat",
            header: "Seat",
            // A SEAT'S WHOLE NAME at a desktop width (QA M16), and no wider: the
            // room is the pickers', so a split seat's three stay on one line.
            floor: "12rem",
            width: "16rem",
            cell: (s) => (
              <TableEntityCell
                {...linkedSeat({ handle: s.handle, name: s.name, kind: s.kind, avatar: s.avatar })}
              />
            ),
          },
          {
            key: "model",
            header: "Model",
            floor: "16rem",
            cell: (s) => (
              <SeatModels
                seat={s}
                one={oneOf(s.handle)}
                cells={
                  Object.fromEntries(CATEGORY_ORDER.map((c) => [c, cellOf(s.handle, c)])) as Record<
                    JobCategory,
                    CellState
                  >
                }
                admin={data.admin}
                options={options}
                byKey={byKey}
                onPick={(key, categories) =>
                  onFlow("edit", key, {
                    step: "assign",
                    add: categories.map((job) => ({ job, seat: s.handle })),
                  })
                }
              />
            ),
          },
        ]}
      />
    </Card>
  );
}

/**
 * One seat's model: one picker where every job runs on one model (or none
 * does), with "Split by job" beside it; a picker per category where they
 * differ, or once the reader splits it. A member reads the values.
 */
function SeatModels({
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
  const [opened, setOpened] = useState(false);
  const name = seat.name || seat.handle;
  const split = one === null || opened;
  if (!admin) {
    const named = (key: string) => {
      const m = byKey.get(key);
      return (
        <span className="models-seat__value">
          {m ? modelMark(m) : null}
          {key || "Not set"}
        </span>
      );
    };
    return (
      <Text as="span" variant="body" className="models-seat">
        {one !== null
          ? named(one)
          : CATEGORY_ORDER.map((c) => {
              const cell = cells[c];
              return (
                <span key={c} className="models-seat__job">
                  <span className="models-seat__label">{CATEGORY_WORDS[c].short}</span>
                  {cell.kind === "one"
                    ? named(cell.key)
                    : cell.kind === "split"
                      ? "Split"
                      : "Not set"}
                </span>
              );
            })}
      </Text>
    );
  }
  return (
    <span className="models-seat">
      {split ? (
        CATEGORY_ORDER.map((c) => {
          const cell = cells[c];
          const key = cell.kind === "one" ? cell.key : "";
          return (
            <span key={c} className="models-seat__job">
              <span className="models-seat__label" aria-hidden="true">
                {CATEGORY_WORDS[c].short}
              </span>
              <Select
                className="models-seat__select"
                width="auto"
                size="sm"
                ariaLabel={`${CATEGORY_WORDS[c].short} for ${name}`}
                value={key || undefined}
                placeholder={cell.kind === "split" ? "Split" : "Not set"}
                options={options[c]}
                onChange={(v) => String(v) !== key && onPick(String(v), [c])}
              />
            </span>
          );
        })
      ) : (
        <Select
          className="models-seat__select"
          width="auto"
          size="sm"
          ariaLabel={`${name}'s model`}
          value={one || undefined}
          placeholder="Not set"
          options={options.one}
          onChange={(v) => String(v) !== one && onPick(String(v), CATEGORY_ORDER)}
        />
      )}
      {/* ONLY A SEAT ON ONE MODEL GOES BACK TO ONE PICKER; one already split shows its three. */}
      {one !== null ? (
        <Button
          variant="ghost"
          size="small"
          aria-expanded={opened}
          onClick={() => setOpened((o) => !o)}
        >
          {opened ? "One model" : "Split by job"}
        </Button>
      ) : null}
    </span>
  );
}

/** Ask Crewlet and the Advisor on one line: each its mark, its name and its picker. */
export function Helpers({ data }: { data: ModelsData }) {
  const choices: HelperChoice[] = data.models.flatMap((m) =>
    m.admin
      ? [{ key: m.key, model: m.model, problem: m.admin.helper_problem, leading: modelMark(m) }]
      : [],
  );
  return (
    <Card>
      <Card.Body>
        <ul className="models-helpers" aria-label="Ask Crewlet and the Advisor">
          {(["assistant", "advisor"] as const).map((helper) => {
            const key = data[helper].key ?? "";
            const model = data.models.find((m) => m.key === key);
            return (
              <li key={helper} className="models-helpers__row">
                <span className="models-job">
                  <JobGlyph job={helper} size="sm" />
                  <span>{helper === "assistant" ? ASSISTANT_USE.label : ADVISOR_USE.label}</span>
                </span>
                {data.admin ? (
                  <HelperModel
                    helper={helper}
                    current={key || null}
                    choices={choices}
                    canAssign
                    onApplied={data.reload}
                  />
                ) : (
                  <Text as="span" variant="body" className="models-seat__value">
                    {model ? modelMark(model) : null}
                    {key || "Not set"}
                  </Text>
                )}
              </li>
            );
          })}
        </ul>
      </Card.Body>
    </Card>
  );
}
