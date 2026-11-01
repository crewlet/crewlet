/**
 * Models by job: the model each seat runs each category of work on, as one
 * grid of seats by the categories, each cell one picker; and the Dashboard
 * Assistant, the company's own, on one line above it.
 *
 * WHAT A SEAT RUNS IS THE ENGINE'S ANSWER: each model's `uses` (the engine's
 * `used_by`), never resolved here. A category whose jobs the document split
 * across two models (written by hand) says which job runs where, so a picker
 * never claims one model for work two do.
 *
 * A PICK IS NOT A WRITE. Choosing a model in a cell opens THAT model's wizard
 * on Use it for with this seat's category added, so the change is reviewed
 * and saved as any change to a model is. The address it opens carries the
 * step, the seat and the category, and nothing chosen for the reader.
 *
 * Each cell says its model's condition (design QA M19): a seat whose reviews
 * stopped because the model cannot sign in reads so where its model is named.
 * A member reads the same grid, values in the same type an admin's pickers
 * draw them in (QA Nit 7), and no picker.
 */

import { useId, useMemo } from "react";
import {
  Callout,
  Card,
  Select,
  StatusDot,
  TableEntityCell,
  Tag,
  Text,
  Tooltip,
  VisuallyHidden,
  type SelectOption,
} from "@crewlethq/ui";
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
import type { ModelHealth } from "~/contract/models.ts";
import type { ModelsData, ModelView } from "./data.ts";
import { JobName } from "./JobGlyph.tsx";
import { HealthLine, type OnFlow } from "./parts.tsx";
import { ASSISTANT_USE, CATEGORY_WORDS, FAST_NOTE, HEALTH, isFastModel } from "./words.ts";

/**
 * One engine job by name, for a category the document split: "Reviews on
 * sonnet; task delegations on haiku". Keyed by the engine's job, so it holds
 * whichever category a job is paired into.
 */
const JOB_WORDS: Record<SeatJob, string> = {
  executor: "coding",
  review: "reviews",
  subagent: "task delegations",
  auxiliary: "routines",
  judge: "onboarding evaluations",
};

/** The conditions a cell calls out: the ones that stop or will stop the work it names. */
const ALARMING: ReadonlySet<ModelHealth> = new Set([
  "rejected",
  "no_sign_in",
  "sitting_out",
  "expiring",
]);

/** Why a condition matters to the job in the cell, for its tooltip and its description. */
function whyOf(model: ModelView): string {
  switch (model.entry.health) {
    case "rejected":
      return `The model ${model.key} cannot sign in: its sign-in was refused. This job moves to a fallback model, or stops.`;
    case "no_sign_in":
      return `Nothing signs the model ${model.key} in. This job moves to a fallback model, or stops.`;
    case "sitting_out":
      return `The model ${model.key} sits out a usage limit. This job moves to a fallback model, or waits for the reset.`;
    case "expiring":
      return `The credential the model ${model.key} signs in with expires soon. Renew it from the model's page.`;
    default:
      return "";
  }
}

/** What runs a seat's category: one model, a split across several, or nothing reported. */
type CellState =
  | { kind: "one"; key: string }
  | { kind: "split"; jobs: { job: SeatJob; key: string }[] }
  | { kind: "none" };

export function ByJob({ data, onFlow }: { data: ModelsData; onFlow: OnFlow }) {
  const byKey = useMemo(() => new Map(data.models.map((m) => [m.key, m])), [data.models]);

  /** The model each job of each seat runs on, from the models' own uses. */
  const runs = useMemo(() => {
    const out = new Map<string, string>();
    for (const m of data.models)
      for (const job of SEAT_JOBS)
        for (const seat of m.uses.jobs[job]) out.set(`${seat}/${job}`, m.key);
    return out;
  }, [data.models]);
  const keyFor = (seat: string, job: SeatJob) => runs.get(`${seat}/${job}`) ?? "";

  const cellOf = (seat: string, category: JobCategory): CellState => {
    const jobs = CATEGORY_JOBS[category].map((job) => ({ job, key: keyFor(seat, job) }));
    const keys = new Set(jobs.map((j) => j.key));
    if (keys.size === 1) {
      const [only] = keys;
      return only ? { kind: "one", key: only } : { kind: "none" };
    }
    return { kind: "split", jobs };
  };
  /** The model a seat runs every job on, or null where its jobs differ. */
  const everythingOf = (seat: string) => {
    const key = keyFor(seat, SEAT_JOBS[0]!);
    return key && SEAT_JOBS.every((j) => keyFor(seat, j) === key) ? key : null;
  };

  /** The options a picker offers: each model by name, its condition in a word, its model id quiet (QA M1, M19). */
  const optionsFor = (models: readonly ModelView[]): SelectOption[] =>
    models.map((m) => {
      const health = HEALTH[m.entry.health];
      return {
        value: m.key,
        label: m.key,
        text: m.key,
        description: <span className="mono">{m.model}</span>,
        hint: <StatusDot tone={health.tone}>{health.label}</StatusDot>,
        // THE TRIGGER CARRIES THE DOT where the model will not answer, so a
        // closed picker already says so.
        ...(ALARMING.has(m.entry.health) ? { leading: <StatusDot tone={health.tone} /> } : {}),
      };
    });
  const options = Object.fromEntries(
    CATEGORY_ORDER.map((c) => [
      c,
      // CODING IS OFFERED ONLY ON A CLI THAT CAN RUN A SEAT'S EXECUTOR.
      optionsFor(c === "coding" ? data.models.filter((m) => m.executor) : data.models),
    ]),
  ) as Record<JobCategory, SelectOption[]>;

  const slowFor = (key: string, category: JobCategory) => {
    const model = byKey.get(key);
    return !!FAST_NOTE[category] && !!model && !isFastModel(model.model);
  };
  const anySlow = data.seats.some((s) =>
    CATEGORY_ORDER.some((c) => {
      const cell = cellOf(s.handle, c);
      return cell.kind === "one" && slowFor(cell.key, c);
    }),
  );
  const fastNotes = CATEGORY_ORDER.flatMap((c) => (FAST_NOTE[c] ? [FAST_NOTE[c]] : []));

  return (
    <div className="models-page">
      {anySlow ? (
        <Callout variant="info" title="A fast model is recommended for some jobs">
          {fastNotes.join(" ")} Coding can take a stronger, slower model.
        </Callout>
      ) : null}

      {data.assistant.enabled ? <AssistantLine data={data} byKey={byKey} onFlow={onFlow} /> : null}

      <Card padding="none">
        <Card.Header>
          <Card.Title>Every seat, every job</Card.Title>
        </Card.Header>
        <DataGrid<Seat>
          name="jobs"
          rows={data.seats}
          rowKey={(s) => s.handle}
          empty={{ title: "No agent seat yet", hint: "Seats added to the company appear here." }}
          columns={[
            {
              key: "seat",
              header: "Seat",
              // A seat's whole name at a desktop width (QA M16).
              floor: "12rem",
              sortValue: (s) => s.name || s.handle,
              cell: (s) => {
                const every = everythingOf(s.handle);
                return (
                  <span className="models-seat-cell">
                    <TableEntityCell
                      {...linkedSeat({
                        handle: s.handle,
                        name: s.name,
                        kind: s.kind,
                        avatar: s.avatar,
                      })}
                    />
                    {every ? (
                      // ONE MODEL FOR EVERY JOB is said once, for the whole row.
                      <Tooltip
                        content={`Every job this seat does runs on ${every}.`}
                        placement="top"
                      >
                        <Tag size="xs" variant="neutral" data-above-row="">
                          Everything
                        </Tag>
                      </Tooltip>
                    ) : null}
                  </span>
                );
              },
            },
            ...CATEGORY_ORDER.map((c) => ({
              key: c,
              header: <JobName job={c} />,
              // SORTABLE, so the head is a button named once by `label` (QA
              // Minor 20: a worded head named itself twice), and the seats
              // line up by the model they run this category on.
              label: CATEGORY_WORDS[c].label,
              sortValue: (s: Seat) => {
                const cell = cellOf(s.handle, c);
                return cell.kind === "one" ? cell.key : "";
              },
              // The longest category's name stands whole in its head at a
              // desktop width (QA M16).
              floor: c === "coding" ? "9rem" : "15rem",
              cell: (s: Seat) => (
                <JobCell
                  seat={s}
                  category={c}
                  state={cellOf(s.handle, c)}
                  admin={data.admin}
                  options={options[c]}
                  byKey={byKey}
                  slow={(key) => slowFor(key, c)}
                  onPick={(key) =>
                    onFlow("edit", key, { step: "assign", add: [{ job: c, seat: s.handle }] })
                  }
                />
              ),
            })),
          ]}
        />
      </Card>
    </div>
  );
}

/** The Dashboard Assistant's one line: the model it runs on, and, for an admin, a picker of the models that may run it. */
function AssistantLine({
  data,
  byKey,
  onFlow,
}: {
  data: ModelsData;
  byKey: ReadonlyMap<string, ModelView>;
  onFlow: OnFlow;
}) {
  const key = data.assistant.key ?? "";
  const model = byKey.get(key);
  const describe = useId();
  // ONLY A MODEL THAT DENIES WEB TOOLS can run the assistant: the engine
  // refuses any other, so the picker never offers one.
  const eligible = data.models.filter((m) => m.webDenied);
  return (
    <Card>
      <Card.Body>
        <span className="models-roles__assistant">
          <JobName job="assistant" />
          {data.admin ? (
            <span className="models-roles__picker models-byjob__picker">
              <Select
                className="models-byjob__select"
                menuClassName="models-byjob__menu"
                width="auto"
                ariaLabel="Model Ask Crewlet runs on"
                aria-describedby={describe}
                size="sm"
                value={key || undefined}
                placeholder="Not set"
                options={eligible.map((m) => ({
                  value: m.key,
                  label: m.key,
                  text: m.key,
                  description: <span className="mono">{m.model}</span>,
                  hint: (
                    <StatusDot tone={HEALTH[m.entry.health].tone}>
                      {HEALTH[m.entry.health].label}
                    </StatusDot>
                  ),
                }))}
                emptyMessage="No model denies web tools yet"
                onChange={(v) =>
                  String(v) !== key &&
                  onFlow("edit", String(v), { step: "assign", add: [{ job: "assistant" }] })
                }
              />
            </span>
          ) : (
            <Text as="span" variant="body">
              {key || "Not set"}
            </Text>
          )}
          {model && ALARMING.has(model.entry.health) ? <HealthLine model={model} compact /> : null}
          <Text as="span" variant="caption" tone="secondary" id={describe}>
            {ASSISTANT_USE.hint} Company-wide, so it has no seats.{" "}
            {data.admin ? ASSISTANT_USE.rule : null}
          </Text>
        </span>
      </Card.Body>
    </Card>
  );
}

/** One seat's category: its model (a picker for an admin), the model's condition, and its notes. */
function JobCell({
  seat,
  category,
  state,
  admin,
  options,
  byKey,
  slow,
  onPick,
}: {
  seat: Seat;
  category: JobCategory;
  state: CellState;
  admin: boolean;
  options: SelectOption[];
  byKey: ReadonlyMap<string, ModelView>;
  slow: (key: string) => boolean;
  onPick: (key: string) => void;
}) {
  const noteId = useId();
  const key = state.kind === "one" ? state.key : "";
  const model = key ? byKey.get(key) : undefined;
  const alarming = model && ALARMING.has(model.entry.health) ? model : null;
  const why = alarming ? whyOf(alarming) : "";
  // A SPLIT CATEGORY SAYS WHICH JOB RUNS WHERE, in the engine's own jobs.
  const split =
    state.kind === "split"
      ? capital(state.jobs.map((j) => `${JOB_WORDS[j.job]} on ${j.key || "no model"}`).join("; "))
      : "";
  const placeholder = state.kind === "split" ? "Split" : "Not set";
  const fast = key && slow(key) ? FAST_NOTE[category] : undefined;
  const label = `${CATEGORY_WORDS[category].label} for ${seat.name || seat.handle}`;
  const notes = alarming || split || fast;
  return (
    <span
      className={alarming ? "models-byjob__cell models-byjob__cell--alarm" : "models-byjob__cell"}
    >
      {admin ? (
        <span className="models-byjob__picker">
          <Select
            className="models-byjob__select"
            menuClassName="models-byjob__menu"
            width="auto"
            ariaLabel={label}
            {...(notes ? { "aria-describedby": noteId } : {})}
            size="sm"
            value={key || undefined}
            placeholder={placeholder}
            options={options}
            onChange={(v) => String(v) !== key && onPick(String(v))}
          />
        </span>
      ) : (
        <Text as="span" variant="body">
          {key || placeholder}
        </Text>
      )}
      {notes ? (
        <span className="models-byjob__notes" id={noteId}>
          {alarming ? (
            <Tooltip content={why} placement="top">
              <span className="models-byjob__health" data-above-row="">
                <HealthLine model={alarming} compact />
                <VisuallyHidden>{why}</VisuallyHidden>
              </span>
            </Tooltip>
          ) : null}
          {split ? (
            <Text as="span" variant="caption" tone="secondary">
              {split}
            </Text>
          ) : null}
          {fast ? (
            <Tooltip content={fast} placement="top">
              <span data-above-row="">
                <Text as="span" variant="caption" tone="tertiary">
                  Fast model recommended
                </Text>
                <VisuallyHidden>. {fast}</VisuallyHidden>
              </span>
            </Tooltip>
          ) : null}
        </span>
      ) : null}
    </span>
  );
}

const capital = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
