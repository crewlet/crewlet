/**
 * Who runs on which model: every helper and seat, sorted by name, each with
 * what it runs on (a fleet, or a pinned model) and how that stands. A seat
 * opens to a row per job the engine resolves for it (`SEAT_JOB_FIELDS`), each
 * with its own picker. A pick asks once: a seat's writes the document
 * (`flows/SeatMove.tsx`), a helper's its setting (`HelperModel`).
 */

import { useMemo, useState, type ReactNode } from "react";
import {
  Card,
  Link,
  OrgTable,
  OrgTableName,
  Select,
  StatusDot,
  Tooltip,
  formatRelative,
  type SelectOption,
  type TreeGridColumn,
  type TreeGridContext,
  type TreeInput,
} from "@crewlethq/ui";
import { UsersGlyph } from "@crewlethq/icons/glyphs";
import { linkedSeat } from "~/app/frame/cells.tsx";
import { HelperModel, type Helper } from "~/components/HelperModel.tsx";
import { useNow } from "~/lib/clock.ts";
import { CATEGORY_OF, SEAT_JOBS, type SeatJob } from "~/lib/modelUses.ts";
import { SEAT_JOB_WORDS } from "~/lib/seatJobs.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { FleetAssignment, FleetTarget, FleetsAnswer } from "~/protocol/fleets.ts";
import type { ModelsData } from "../data.ts";
import { JobGlyph } from "../JobGlyph.tsx";
import { pickerOptions } from "../modelPicker.tsx";
import { SeatMoveDialog, type SeatMoveAsk } from "../flows/SeatMove.tsx";
import { ADVISOR_USE, ASSISTANT_USE, FAST_NOTE } from "../words.ts";
import { valueOf, wordOf as wordIn } from "./data.ts";

/** One face size for every consumer: a helper's product mark and a seat's badge. */
const FACE = 14;

const COLUMNS: TreeGridColumn[] = [
  { key: "consumer", header: "Consumers", floor: "13rem", sortable: false },
  { key: "runs", header: "Runs on", floor: "12rem", sortable: false },
  { key: "status", header: "Status", width: "10.5rem", align: "left", size: "sm", sortable: false },
];

/** What one job, or a whole consumer, runs on and who serves it now. */
interface Part {
  target: FleetTarget;
  now: string | null;
  waiting_until: string | null;
}

type Tone = "success" | "warning" | "danger" | "neutral";
interface Status {
  tone: Tone;
  short: string;
  detail: string;
}

/** A part's status: running (green), near its limit (yellow), waiting (red), or a fleet with no member yet. */
function statusOf(part: Part, fleets: FleetsAnswer, now: number): Status {
  const fleet =
    "fleet" in part.target
      ? (fleets.fleets.find((f) => "fleet" in part.target && f.key === part.target.fleet) ?? null)
      : null;
  if (!part.now && fleet && fleet.members.length === 0)
    return {
      tone: "neutral",
      short: "No members yet",
      detail: `${fleet.name} has no members yet: add one to it, or pin this to a model`,
    };
  if (!part.now) {
    const back = part.waiting_until ? formatRelative(part.waiting_until, now) : null;
    const pinned = "pinned" in part.target ? part.target.pinned : "";
    return {
      tone: "danger",
      short: back ? `Waiting, back ${back}` : "Waiting",
      detail: `Waiting: ${
        fleet ? `every member of ${fleet.name} is at its limit` : `${pinned} is at its limit`
      }${back ? `, back ${back}` : ""}`,
    };
  }
  const member = fleets.fleets.flatMap((f) => f.members).find((m) => m.entry === part.now);
  if (member?.state === "near")
    return {
      tone: "warning",
      short: "Near its limit",
      detail: `Now on ${part.now}, near its limit${fleet ? `, in ${fleet.name}` : ""}`,
    };
  return {
    tone: "success",
    short: "Running",
    detail: fleet ? `Now on ${part.now}, in ${fleet.name}` : `Runs on ${part.now}`,
  };
}

const WORST: readonly Tone[] = ["danger", "warning", "neutral", "success"];

function StatusCell({ status }: { status: Status }) {
  return (
    <Tooltip content={status.detail} placement="top">
      <span className="models-one-line" tabIndex={0} aria-label={status.detail}>
        <StatusDot tone={status.tone}>{status.short}</StatusDot>
      </span>
    </Tooltip>
  );
}

const HELPER_NAMES: Record<Helper, string> = {
  assistant: ASSISTANT_USE.label,
  advisor: ADVISOR_USE.label,
};

export function WhoRunsTable({ data, fleets }: { data: ModelsData; fleets: FleetsAnswer }) {
  const now = useNow();
  const picks = data.admin && data.models.length > 0;
  const [moving, setMoving] = useState<SeatMoveAsk | null>(null);
  /** A helper's pick waiting for its confirm, by helper. */
  const [helperPick, setHelperPick] = useState<Partial<Record<Helper, string | null>>>({});
  const wordOf = (value: string) => wordIn(value, fleets.fleets);
  const options = useMemo(() => {
    const of = (use: Parameters<typeof pickerOptions>[2]) =>
      pickerOptions(data.models, fleets.fleets, { executors: data.executors, ...use });
    return {
      all: of({ coding: true }),
      helper: of({ helper: true }),
      ...Object.fromEntries(
        SEAT_JOBS.map((job) => [
          job,
          of({ coding: job === "executor", fast: !!FAST_NOTE[CATEGORY_OF[job]] }),
        ]),
      ),
    } as Record<SeatJob | "all" | "helper", SelectOption[]>;
  }, [data.models, data.executors, fleets.fleets]);

  const consumers = useMemo(() => {
    const named = fleets.assignments.map((a) => ({
      a,
      name:
        a.kind === "helper"
          ? (HELPER_NAMES[a.subject as Helper] ?? a.name)
          : data.seats.find((s) => s.handle === a.subject)?.name || a.name,
    }));
    return named.sort((x, y) => x.name.localeCompare(y.name));
  }, [fleets.assignments, data.seats]);
  const bySubject = new Map(consumers.map((c) => [c.a.subject, c]));
  const rows: TreeInput[] = consumers.map(({ a, name }) =>
    a.kind === "helper"
      ? { id: `helper|${a.subject}`, label: name }
      : {
          id: `seat|${a.subject}`,
          label: name,
          children: SEAT_JOBS.map((job) => ({
            id: `job|${a.subject}|${job}`,
            label: SEAT_JOB_WORDS[job].label,
          })),
        },
  );

  /** What a seat's job runs on: its own part where its jobs differ, else the seat's. */
  const partOf = (a: FleetAssignment, job?: SeatJob): Part =>
    (job && a.jobs?.find((j) => j.job === job)) || a;
  /** The one value every job runs on, or null where they differ. */
  const oneOf = (a: FleetAssignment): string | null => (a.jobs ? null : valueOf(a.target));
  const ask = (a: FleetAssignment, jobs: readonly SeatJob[] | "all", value: string) =>
    setMoving({
      seat: a.subject,
      jobs,
      value,
      to: wordOf(value),
      from: Object.fromEntries(
        (jobs === "all" ? (["all"] as const) : jobs).map((job) => [
          job,
          job === "all"
            ? [...new Set(SEAT_JOBS.map((j) => valueOf(partOf(a, j).target)))]
                .map(wordOf)
                .join(" + ")
            : wordOf(valueOf(partOf(a, job).target)),
        ]),
      ),
    });
  /** What a row ran on, said under its name while its pick waits. */
  const wasOf = (a: FleetAssignment): string | undefined => {
    if (a.kind === "helper") {
      const pick = helperPick[a.subject as Helper];
      return pick === undefined ? undefined : `Was ${wordOf(valueOf(a.target))}`;
    }
    if (moving?.seat !== a.subject) return undefined;
    return `Was ${Object.values(moving.from).join(" + ")}`;
  };
  const pickedFor = (a: FleetAssignment, jobs: readonly SeatJob[] | "all") =>
    moving?.seat === a.subject &&
    (jobs === "all"
      ? moving.jobs === "all"
      : moving.jobs !== "all" && moving.jobs.join() === jobs.join())
      ? moving.value
      : null;

  const cell = (id: string, column: number, grid: TreeGridContext): ReactNode => {
    const [kind, subject = "", job] = id.split("|");
    const entry = bySubject.get(subject);
    if (!entry) return null;
    const { a, name } = entry;
    const key = grid.columnKey(column);
    if (kind === "job") {
      const j = job as SeatJob;
      const part = partOf(a, j);
      const value = valueOf(part.target);
      if (key === "consumer")
        return (
          <OrgTableName
            icon={<JobGlyph job={CATEGORY_OF[j]} size="md" />}
            name={SEAT_JOB_WORDS[j].label}
          />
        );
      if (key === "runs")
        return (
          <Select
            width="full"
            size="sm"
            appearance="quiet"
            inset="flush"
            hintColumn="fit"
            ariaLabel={`${SEAT_JOB_WORDS[j].label} for ${name}`}
            value={(pickedFor(a, [j]) ?? value) || undefined}
            placeholder="Not set"
            options={options[j]}
            disabled={!picks}
            onChange={(v) => String(v) !== value && ask(a, [j], String(v))}
          />
        );
      return key === "status" ? <StatusCell status={statusOf(part, fleets, now)} /> : null;
    }
    if (kind === "helper") {
      const helper = subject as Helper;
      if (key === "consumer")
        return (
          <OrgTableName
            icon={<JobGlyph job={helper} size="sm" />}
            iconTone="own"
            name={name}
            caption={wasOf(a)}
          />
        );
      if (key === "runs")
        return (
          <HelperModel
            helper={helper}
            current={valueOf(a.target) || null}
            options={options.helper}
            wordOf={wordOf}
            canAssign={picks}
            onApplied={data.reload}
            onPending={(value) =>
              setHelperPick((was) => {
                const next = { ...was };
                if (value === undefined) delete next[helper];
                else next[helper] = value;
                return next;
              })
            }
          />
        );
      return key === "status" ? <StatusCell status={statusOf(a, fleets, now)} /> : null;
    }
    const seat = data.seats.find((s) => s.handle === subject);
    const link = linkedSeat({
      handle: subject,
      name,
      kind: seat?.kind ?? "agent",
      avatar: seat?.avatar ?? null,
    });
    const one = oneOf(a);
    if (key === "consumer")
      return (
        <OrgTableName
          icon={
            <SeatAvatar
              name={name}
              size={FACE}
              kind={seat?.kind === "human" ? "human" : "agent"}
              avatar={seat?.avatar ?? null}
              decorative
            />
          }
          iconTone="own"
          name={
            link.href ? (
              <Link variant="subtle" href={link.href}>
                {name}
              </Link>
            ) : (
              name
            )
          }
          caption={wasOf(a)}
        />
      );
    if (key === "runs")
      return (
        <Select
          width="full"
          size="sm"
          appearance="quiet"
          inset="flush"
          hintColumn="fit"
          ariaLabel={`${name}'s model`}
          value={(pickedFor(a, "all") ?? one) || undefined}
          placeholder={one === null ? "Mixed" : "Not set"}
          options={options.all}
          disabled={!picks}
          onChange={(v) => String(v) !== one && ask(a, "all", String(v))}
        />
      );
    if (key === "status") {
      const all = SEAT_JOBS.map((j) => statusOf(partOf(a, j), fleets, now));
      const worst = WORST.map((t) => all.find((s) => s.tone === t)).find(Boolean)!;
      return <StatusCell status={one !== null ? statusOf(a, fleets, now) : worst} />;
    }
    return null;
  };

  const home = fleets.fleets.find((f) => f.is_default);
  return (
    <>
      <Card as="section">
        <Card.Header
          icon={<UsersGlyph size="sm" />}
          subtitle={`${
            picks
              ? "Choose the model or fleet each helper and seat runs on."
              : "The model or fleet each helper and seat runs on."
          }${home ? ` New ones start on the default fleet, ${home.name}.` : ""}`}
          subtitlePlacement="below"
        >
          <Card.Title>Who runs on which model</Card.Title>
        </Card.Header>
        <Card.Body padding="none">
          <OrgTable
            label="Consumers and what each runs on"
            layout="container"
            flush
            controls={false}
            phoneRows="labelled"
            defaultExpanded="none"
            columns={COLUMNS}
            rows={rows}
            renderCell={cell}
            cellHasControl={(id, _column, key) =>
              key === "runs" || (key === "consumer" && id.startsWith("seat|"))
            }
          />
        </Card.Body>
      </Card>
      <SeatMoveDialog ask={moving} data={data} onClose={() => setMoving(null)} />
    </>
  );
}
