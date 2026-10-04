/**
 * Recurring work: what fires, when it next fires, and how it last went.
 *
 * THE NAMES HERE ARE THE SERVER'S. This screen used to read `scope`,
 * `scope_name`, `last_run` and `last_outcome` off a schedule and `name`,
 * `scope_name` and `detail` off a run; `schedule.Row` serialises `scope_type`
 * and `scope_id` and has no last-run field at all, and a ledger row carries
 * `schedule_name`, `fire_label`, `target_handle`, `scheduled_at` and
 * `trace_id`. Seven cells rendered `undefined` on every row, and the sort and
 * row keys they were built from collapsed every row onto one key.
 *
 * The last fire is JOINED from the ledger rather than read off the row, on the
 * identity the ledger keys on — (scope_type, scope_id, name) — because that is
 * where the fact actually lives.
 *
 * THE ID IS NOT THE NAME. `scope_id` is the scope's IDENTITY — a seat's agent
 * id, a unit's key — which a rename does not move, so every key, join and
 * address here is built from it. `scope_name` is what a person reads (the
 * seat's handle, the unit's key) and draws and links the scope and nothing
 * else ([scopeName]): drawn from the id, a role schedule's scope read as a
 * uuid and linked to a page nobody has.
 *
 * AND THE LEDGER HAS THREE OUTCOMES. `schedule.Outcome` is `fired`,
 * `skipped_catchup` or `skipped_paused`: it is a DISPATCH ledger, not a
 * turn-outcome one, so "recent failures" counted a value the engine cannot
 * emit and read 0 for ever on every company. What it can honestly say is how
 * many ticks were skipped, which is the catchup cap doing its job, a node that
 * was down too long, or a seat a person had paused.
 */

import { useMemo, type ReactNode } from "react";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  AvatarStack,
  Card,
  EmptyState,
  EmptyValue,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import {
  CalendarGlyph,
  FileTextGlyph,
  CircleAlertGlyph,
  ClockGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { seatBadge } from "~/ui/SeatAvatar.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup, seatPath, unitRoute, type SeatKind } from "~/lib/seats.ts";
import { describe as describeCron, nextFires } from "~/lib/cron.ts";
import {
  fmtDateTime,
  fmtDuration,
  inTime,
  inTimeExact,
  relTime,
  tsKey,
  plural,
} from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { LogRefusal, QueryRefusal, ScheduleRow, ScheduleRunRow } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { href } from "~/app/router.tsx";
import { AgentsHeader, useAgentsCounts } from "./header.tsx";

/** The ledger's three outcomes, and nothing else — see the note above.
 *
 *  NEITHER SKIP IS AMBER. Amber is the one state that asks for a person, and
 *  neither skip does: a paused seat's fire follows the decision somebody
 *  already made, and a missed tick outside the catchup window is the cap doing
 *  its job. A catchup skip was drawn amber, beside the raw enum, on every row
 *  of a company whose node had been down overnight — a page of warnings about
 *  nothing anybody could act on. */
const OUTCOME_TONE: Record<string, "success" | "neutral"> = {
  fired: "success",
  skipped_catchup: "neutral",
  skipped_paused: "neutral",
} satisfies Record<LedgerOutcome, "success" | "neutral">;

/** What each outcome is called on screen — the ledger's enum is a wire value,
 *  and `skipped_catchup` in a pill is a column name, not a word. */
const OUTCOME_LABEL: Record<string, string> = {
  fired: "fired",
  skipped_catchup: "skipped · missed",
  skipped_paused: "skipped · paused",
} satisfies Record<LedgerOutcome, string>;

/** An outcome as a pill: the word, the tone, and why a skip was one. */
export function OutcomeTag({ outcome }: { outcome: string }) {
  return (
    <Tag variant={OUTCOME_TONE[outcome] ?? "neutral"} title={OUTCOME_WHY[outcome]}>
      {OUTCOME_LABEL[outcome] ?? outcome}
    </Tag>
  );
}

/** Why a fire was skipped, for the badge's title — one sentence per skip, held
 *  to the ledger's own vocabulary by the type so a new skip cannot arrive
 *  unexplained. */
const OUTCOME_WHY: Record<string, string> = {
  skipped_catchup: "the tick was missed and fell outside the catchup window",
  skipped_paused: "the seat was paused when the fire came due, so it was recorded and not sent",
} satisfies Record<Exclude<LedgerOutcome, "fired">, string>;

/** Every outcome the ledger can record. */
type LedgerOutcome = Exclude<ScheduleRunRow["outcome"], "">;

/**
 * Who a schedule wakes, in a cell: one seat by name, several by face and count.
 *
 * THE FACES ARE THE CHART'S. Each badge is drawn from the seat's badge name
 * (`seatBadge`), as every other badge of a seat is, so "Agent SWE", "Agent
 * Frontend SWE" and "Agent AI Systems Engineer" are SW, FS and AS here as they
 * are on the chart and the roster — handed the whole name, the kit made AS, AI
 * and AA of them, three badges nobody could match to a card. And at the kit's
 * default step, the design's stack: at the extra-small one the 8px each badge
 * is overlapped by cut every monogram's second letter away.
 */
export function Wakes({ runners, who }: { runners: string[]; who: Who }) {
  if (runners.length === 0) return <EmptyValue label="Nobody — this schedule wakes no seat" />;
  if (runners.length === 1) return <SeatChip handle={runners[0]!} {...who(runners[0]!)} />;
  const names = runners.map((h) => who(h).name);
  return (
    <span className="row gap-1" title={names.join(", ")}>
      <AvatarStack
        max={3}
        decorative
        members={runners.map((h) => {
          const seat = who(h);
          return { id: h, ...seatBadge(seat.name, seat.kind) };
        })}
      />
      {/* ELLIPSISED, never clipped: in a column held at its cap beside a
          peek the count was cut to "3 sea" at the cell's edge. The whole
          answer is the title's and the screen reader's. */}
      <span className="t-caption truncate">{plural(runners.length, "seat")}</span>
      <span className="sr-only">: {names.join(", ")}</span>
    </span>
  );
}

/**
 * The narrowest a schedule's name is drawn in a grid: twelve ems, which holds
 * a name like `morning-tracker-review` whole. A schedule's name is a slug, one
 * word the engine made of the author's, so a column narrower than the word is
 * a column that says nothing — beside a peek at 1280 the runs grid drew every
 * one as "b.". ONE FLOOR FOR BOTH GRIDS, because the two name one thing.
 */
const NAME_FLOOR = "12rem";

/**
 * The narrowest the one schedule's Woke column is drawn: a seat's badge and a
 * name like "Agent Frontend SWE" whole. It is the column that FILLS that grid,
 * so this is only the width below which it would start cutting names.
 */
const WOKE_FLOOR = "12rem";

/**
 * The track an outcome column takes: its own content, whole. A grid caps a
 * content-sized column at a fifth of itself because a value like a seat's name
 * has no ceiling; an outcome does — it is one of [OUTCOME_LABEL]'s three
 * words — so the cap only ever cut the skip pills.
 */
const OUTCOME_WIDTH = "max-content";

/** The identity a ledger row and a schedule row share. */
function identity(scopeType: string, scopeID: string, name: string): string {
  return `${scopeType}:${scopeID}:${name}`;
}

const rowID = (s: ScheduleRow) => identity(s.scope_type, s.scope_id, s.name);
const runID = (r: ScheduleRunRow) => identity(r.scope_type, r.scope_id, r.schedule_name);

/**
 * A schedule's address, as the FRAME spells it.
 *
 * THREE SEGMENTS JOINED BY A SLASH, which is `objects.ts`'s own grammar for
 * this kind and not a second spelling of [identity]: that one is a map key
 * inside this screen and may use whatever separator it likes, this one is
 * carried in a URL and read back by `KINDS.schedule.pathOf`. Both are all
 * three segments for the same reason — two units may each declare a
 * "standup", and a role and a unit may both — so an address that carried the
 * name alone would open whichever one the rail happened to find first.
 */
function scheduleRef(scopeType: string, scopeId: string, name: string): ObjectRef {
  return { kind: "schedule", id: [scopeType, scopeId, name].join("/") };
}

/**
 * Where a scope's own page lives: a seat for a role schedule, a unit for a
 * unit one — FROM THE NAME AND NOT THE ID ([scopeName]): a link built from the
 * id would reach a uuid nobody has a page for.
 */
function scopePath(scopeType: string, name: string): string[] {
  return scopeType === "role" ? seatPath({ handle: name, name }) : unitRoute({ id: name });
}

/** What a person calls a scope: the seat's handle, the unit's key. */
function scopeName(row: { scope_name: string }): string {
  return row.scope_name;
}

/**
 * What a schedule IS — the four facts, in one order, for the page and the rail.
 *
 * ONE BUILDER, for the reason `ObjectHeader` exists: a schedule is recognised
 * by WHEN it fires, and "when" is two facts that are easy to mistake for each
 * other — the EXPRESSION, which is what its author wrote, and NEXT, which is
 * what the engine worked out from it in the row's own timezone. A page that
 * led with one and a rail that led with the other would make a reader
 * re-derive the schedule every time it changed frame.
 */
export function scheduleFacts(row: ScheduleRow, now: number, who: Who): Fact[] {
  return [
    {
      label: "Cron",
      // THE EXPRESSION OVER WHAT IT MEANS, as the grid the schedule was
      // opened from draws it — see [CronReading]. WHOLE, because the stack is
      // two lines by construction: clamped, the expression took the first and
      // the sentence under it was cut to "every 20 minutes…".
      value: <CronReading cron={row.cron} />,
      whole: true,
    },
    { label: "Timezone", value: row.timezone },
    {
      label: "Next",
      // THE ENGINE'S ANSWER, and the REASON where it has none. A schedule
      // whose timezone was renamed has an empty `next_run` and would read as
      // merely idle — the row says so in its own `problem` field and this is
      // the only place a reader sees it.
      value: row.next_run ? (
        <span title={fmtDateTime(row.next_run)}>{inTime(row.next_run, now)}</span>
      ) : row.problem ? (
        <Tag variant="danger" title={row.problem}>
          {row.problem}
        </Tag>
      ) : (
        <span className="muted">
          {row.enabled ? "never — the calendar does not reach it" : "disabled"}
        </span>
      ),
    },
    {
      // WHO A FIRE REACHES, resolved by the engine from the scope and the
      // target rather than worked out here: a unit schedule targeting `each`
      // wakes every member, one targeting `lead` wakes one seat, and a role
      // schedule's target is meaningless rather than defaulted.
      //
      // BY NAME, AS THE GRID SAYS IT. This fact joined the raw handles —
      // "agent-swe, agent-frontend-swe, agent…" — in the header of the one
      // page about the schedule, while the row it was opened from drew the
      // same seats by face and name: a handle is an address, not a label.
      label: "Wakes",
      value: <Wakes runners={row.runners ?? []} who={who} />,
    },
  ];
}

/** Who a handle is: its seat's name and kind, off the chart the screen holds. */
type Who = (handle: string) => { name: string; kind?: SeatKind };

/**
 * A cron expression and what it means: the expression's chip, and the
 * sentence on the line under it.
 *
 * ONE DRAWING FOR THE GRID AND THE HEADER. A reader who knows cron reads
 * `0 9 * * 1-5`; a founder reads five numbers and a dash on the one screen
 * that says when the company wakes itself up, so both are said. The grid
 * stacked them while the schedule's own header ran the sentence on after the
 * chip, so one value was drawn two ways on one page. An expression this build
 * cannot read shows as itself with nothing claimed about it.
 *
 * THE CHIP HUGS ITS TEXT (`.cron-cell` in screens.css): in a column flex box
 * it was stretched to the width of the sentence under it, a box round blank
 * space.
 */
export function CronReading({ cron, timezone }: { cron: string; timezone?: string }) {
  const said = describeCron(cron);
  return (
    <span className="col cron-cell">
      <code className="inline nowrap" title={timezone ? `timezone: ${timezone}` : undefined}>
        {cron}
      </code>
      {said && <span className="t-caption">{said}</span>}
    </span>
  );
}

/**
 * Where a schedule is declared, in the reader's words: "Seat · Agent PM" for a
 * role's schedule, "Unit · Core" for a unit's.
 *
 * ONE PHRASE FOR THE EYEBROW AND THE DEFINITION. The eyebrow drew the address
 * (`role:agent-pm`) in the mono face, a line above a definition that said the
 * same scope by name — a seat is named, never shown by its handle.
 */
export function scopeLabel(scopeType: string, name: string, who: Who): string {
  return scopeType === "role" ? `Seat · ${who(name).name}` : `Unit · ${name}`;
}

/**
 * Where a schedule is declared, in a cell: a role's SEAT, or a unit.
 *
 * A ROLE SCOPE IS A SEAT and a unit scope is not, which is why only half of
 * this is a person: `SeatCell` is the one rendering of a seat in a grid, and a
 * unit is a name with no avatar, no state and no page of people behind it.
 * ONE CELL FOR BOTH GRIDS: the company's fires drew a role's scope as its raw
 * handle in an outline pill (`agent-pm`) one card below the grid that drew the
 * same scope as "Agent PM" with its badge.
 */
export function ScopeCell({
  scopeType,
  name,
  who,
}: {
  scopeType: string;
  /** The scope as a person reads it ([scopeName]), never the ledger's id. */
  name: string;
  who: Who;
}) {
  return scopeType === "role" ? (
    <SeatCell handle={name} {...who(name)} />
  ) : (
    <Tag appearance="outline">{name}</Tag>
  );
}

/**
 * The one thing about a schedule that is a STATE rather than identity.
 *
 * Three badges in the page bar once — which is where a screen's CONTROLS
 * live, so a schedule's own condition was drawn in the one strip that is not
 * about the object. Only one of them can be true at a time and they are not
 * equally bad: a DISABLED schedule is somebody's decision and a schedule that
 * CANNOT FIRE is a defect, so only the second carries a tone.
 */
function scheduleStatus(row: ScheduleRow | undefined): ReactNode {
  if (!row) {
    return (
      <Tag appearance="outline" title="the ledger outlives the configuration">
        no longer declared
      </Tag>
    );
  }
  if (row.problem) {
    return (
      <Tag variant="danger" title={row.problem}>
        cannot fire
      </Tag>
    );
  }
  if (!row.enabled) return <Tag appearance="outline">disabled</Tag>;
  return undefined;
}

/**
 * The definition itself, on the page and in the rail alike.
 *
 * THREE OF THESE ROWS HAD NO SURFACE AT ALL. `target`, `catchup` and
 * `timeout_seconds` are on every schedule the engine serialises and were
 * rendered nowhere: whether a missed tick fires late and how long a fire may
 * run before the engine stops it are the two questions asked of a schedule
 * that misbehaved, and the only way to answer either was to read the company
 * configuration.
 */
export function ScheduleDefinition({ row, who }: { row: ScheduleRow; who: Who }) {
  return (
    <Card>
      <Card.Header icon={<FileTextGlyph size="sm" />}>
        <Card.Title>Definition</Card.Title>
      </Card.Header>
      <PropertiesRail
        groups={[
          {
            properties: [
              { label: "Task", value: row.task },
              // NO "MEANS" ROW. What the expression means is said whole in the
              // header's Cron fact now, a hundred pixels above this card in
              // the same column, and a header and a rail stacked in one
              // column are one reading (`ObjectHeader`): the sentence twice
              // was the same answer given twice.
              {
                label: "Scope",
                // A ROLE'S SEAT BY ITS NAME, a unit by its own: `role ·
                // agent-pm` was the one place on the page a seat was still
                // its handle. Linked, which the eyebrow's phrase is not.
                value: scopeLabel(row.scope_type, scopeName(row), who),
                path: scopePath(row.scope_type, scopeName(row)),
              },
              {
                label: "Target",
                // A ROLE SCHEDULE'S TARGET IS MEANINGLESS rather than
                // defaulted, which is `schedule.Row`'s own rule — so the row
                // says which of the two an empty value is instead of drawing
                // the dash that means "set to nothing".
                value:
                  row.target ||
                  (row.scope_type === "role" ? (
                    <span className="muted">not used by a role schedule</span>
                  ) : (
                    ""
                  )),
                title: "which members of the unit a fire reaches",
              },
              {
                label: "Catchup",
                value: row.catchup
                  ? "a tick missed while the node was down fires late"
                  : "a missed tick is dropped",
              },
              {
                label: "Cap",
                // ABSENT IS NOT ZERO here either: a schedule with no cap
                // declared runs to the engine's own default rather than for
                // no time at all.
                value: row.timeout_seconds > 0 ? fmtDuration(row.timeout_seconds * 1000) : "",
                title: "wall clock one fire may take before the engine stops it",
              },
            ],
          },
        ]}
      />
    </Card>
  );
}

/**
 * How many fires ahead the page works out.
 *
 * Five: enough that a weekly schedule shows a month and a daily one shows a
 * working week, and few enough that the panel is read rather than scrolled.
 */
const UPCOMING_FIRES = 5;

/**
 * And how many the rail does.
 *
 * Three, for the question the rail asks rather than the one the page does:
 * "every 4 hours" and "at 4am" have the same NEXT fire for most of the day
 * and differ on the one after, so two is the smallest number that can answer
 * "is this the schedule I meant" and the third is what tells a weekly from a
 * fortnightly. More than that is a list scrolled inside a 420 px column, and
 * `Open ↗` is one click from the page that shows five.
 */
const PEEK_FIRES = 3;

/** And how many of its last fires — see [PEEK_FIRES]; the page shows the page. */
const PEEK_RUNS = 3;

/**
 * The fires after the next one.
 *
 * THE ENGINE IS THE AUTHORITY on the next fire and the facts above say so.
 * What it does NOT say is the one after — and the fires after the next are
 * what tell a reader whether an expression means what its author thought.
 *
 * Exported for its own suite, in this tree's idiom (`Runs.BridgeLog`,
 * `Seat.CounterpartyRow`): the claim worth pinning is which ZONE the
 * expression is worked out in, and the screen around it needs an org, a
 * roster and three queries before this panel draws at all.
 */
export function NextFires({ row, now, count }: { row: ScheduleRow; now: number; count: number }) {
  // ANCHORED TO THE MINUTE, not to the ticking clock: the list changes when a
  // fire passes, and recomputing it every second would rebuild the panel
  // sixty times for an answer that moves once.
  const minute = Math.floor(now / 60_000);
  // IN THE ROW'S OWN ZONE, which `nextFires` requires and refuses to default —
  // the engine resolves the row's timezone and evaluates the expression there,
  // so a list worked out in UTC was wrong by the zone's STANDING offset on
  // every row of every company that does not run in UTC, not merely across a
  // daylight-saving change. The engine names the zone on EVERY row: a schedule
  // that names none of its own fires on the company's clock (ADR-0018) and
  // arrives carrying it, so there is nothing here to default — and a default
  // of UTC was exactly the wrong clock for such a row. A zone this runtime
  // cannot read yields no fires at all, which is the same refusal an
  // unparseable expression gets and is what `problem` on the row names.
  const upcoming = useMemo(
    () => (row.cron ? nextFires(row.cron, new Date(minute * 60_000), row.timezone, count) : []),
    [row.cron, row.timezone, minute, count],
  );
  // ONE FIRE IS NOT A SERIES. The next fire is already a fact in the header,
  // so a panel repeating it alone would be the same value twice.
  if (upcoming.length <= 1) return null;
  return (
    <Card>
      <Card.Header
        icon={<CalendarGlyph size="sm" />}
        subtitle={`the next ${upcoming.length} fires this expression works out to, evaluated in ${row.timezone}`}
      >
        <Card.Title>And after that</Card.Title>
      </Card.Header>
      <ol className="fires">
        {upcoming.map((at) => (
          <li key={at.toISOString()} className="row gap-2">
            <span className="mono t-caption">{fmtDateTime(at.toISOString())}</span>
            <span className="spacer" />
            {/* TO THE MINUTE, because the rows are read against each other:
                one unit read "in 1h" beside "in 1h" for two fires twenty
                minutes apart. */}
            <span className="t-caption">{inTimeExact(at.toISOString(), now)}</span>
          </li>
        ))}
      </ol>
      {row.timezone && row.timezone.toUpperCase() !== "UTC" && (
        // TWO ZONES ARE IN PLAY AND THEY ARE NOT THE SAME ONE. The expression
        // is worked out in the SCHEDULE's zone — the engine's own, so the list
        // no longer drifts from it — and each instant is then rendered in the
        // READER's, like every other timestamp on this screen. Saying which is
        // which beats a reader working out why `0 9 * * *` in Asia/Tokyo is
        // listed at midnight.
        <p className="t-caption" style={{ marginTop: "var(--spacing-2)" }}>
          This schedule is evaluated in <code className="inline">{row.timezone}</code>; the instants
          above are the same fires shown in your own timezone.
        </p>
      )}
    </Card>
  );
}

/** What an address that names no schedule and no fire means. */
const NO_SCHEDULE_HINT =
  "A schedule is addressed by all three of its segments — scope type, scope, name. A company revision may have removed this one, and the retention sweep may have taken its fires.";

/** And what the ledger outliving the configuration means, wherever it shows. */
const UNDECLARED_HINT =
  "The company configuration no longer declares this schedule. Its history is kept until the retention sweep takes it.";

export function Schedules({ scope = [] }: { scope?: string[] }) {
  const now = useNow();
  const org = useOrg();
  // WHO A HANDLE IS. A schedule's scope and every fire's target are handles,
  // and a handle is an address rather than a label: drawn bare, this grid put
  // `agent-ai-systems-engineer` where the seat's name belongs, built the
  // badge's monogram out of it, and drew a person's seat as an agent's.
  const index = useMemo(() => indexOrg(org), [org]);
  const who = useMemo(() => seatLookup(index), [index]);
  // THE SAME FIGURES ON EVERY AGENTS TAB, this one's included: a strip whose
  // numbers came and went with the tab would read as counts that changed.
  useAgentsCounts(index);
  const { open: openPeek } = usePeekControls();
  // `#/agents/schedules/{scope_type}/{scope_id}/{name}` names ONE schedule.
  // Three segments because a schedule's identity is all three: two units may
  // each declare a "standup", and a role and a unit may both.
  const [scopeType, scopeId, scheduleName] = scope;
  const detail = Boolean(scopeType && scopeId && scheduleName);
  // ONE SCHEDULE'S OWN HISTORY, which the company-wide ledger below cannot
  // be: `schedules.recent_runs` is fifty rows across every schedule, so
  // twenty hourly ones fill it in two and a half hours and "did the standup
  // fire this week" is unanswerable. The three segments were destructured
  // here and never read, so this address rendered the whole list.
  const one = useQuery(
    "schedule_runs",
    { scope_type: scopeType, scope_id: scopeId, name: scheduleName },
    { enabled: detail, pollMs: 30_000 },
  );
  // Schedules are pushed on a config apply, and the RUNS are not pushed at
  // all — so this polls, slowly, because a cron's next fire moves in minutes.
  const { data, loading, error, refusal } = useQuery("schedules", undefined, { pollMs: 30_000 });

  const schedules = data?.schedules ?? [];
  const runs = data?.recent_runs ?? [];
  const due = schedules.filter((s) => tsKey(s.next_run) > 0 && tsKey(s.next_run) - now < 3_600_000);
  // A schedule that cannot fire at all, which is a defect rather than a
  // choice: a cron nobody can parse, a timezone that no longer exists. The
  // row says so in its own `problem` field and a blank Next cell was the
  // only symptom.
  const broken = schedules.filter((s) => s.problem);
  // THE SOONEST OF THEM, which is what a one-line caption has room for: a
  // schedule's name is founder prose, so joining them was cut mid-word and named
  // a schedule nobody declared. WHICH schedules is the table below's question —
  // every row carries its own Next cell.
  const soonest = due.length
    ? due.reduce((best, s) => (tsKey(s.next_run) < tsKey(best.next_run) ? s : best))
    : undefined;

  // THE ORDER `[` AND `]` WALK, published only from the LIST. On the detail
  // route this same component renders one schedule, and a stepper walking
  // every schedule in the company would step a rail opened from a screen its
  // rows are not on.
  usePeekNeighbours(
    useMemo(
      () => (detail ? [] : schedules.map((s) => scheduleRef(s.scope_type, s.scope_id, s.name))),
      [detail, schedules],
    ),
  );

  // The last fire per schedule, from the ledger. `Recent` returns newest
  // first, so the first row per identity is the latest one.
  const lastFire = new Map<string, ScheduleRunRow>();
  for (const run of runs) if (!lastFire.has(runID(run))) lastFire.set(runID(run), run);

  const chosen = detail
    ? schedules.find(
        (s) => s.scope_type === scopeType && s.scope_id === scopeId && s.name === scheduleName,
      )
    : undefined;

  /**
   * A row's click, for the two grids whose rows point at a schedule.
   *
   * THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's one copy
   * of "which clicks mean elsewhere" and reads a mouse event — ⌘, ctrl,
   * shift, alt and the middle button belong to the browser, which is what
   * keeps the row a real link to the schedule's page. The `enter` chord
   * carries no button at all and is never "open elsewhere".
   */
  function openSchedule(ref: ObjectRef, e: React.MouseEvent | React.KeyboardEvent): void {
    const go = () => openPeek(ref);
    if (!("button" in e)) {
      go();
      return;
    }
    rowPeekHandler(go)?.(e);
  }

  // THE THREE SEGMENTS SPELLED OUT rather than `detail` reused, because this
  // is the one place their being present has to NARROW them: a boolean says
  // the same thing to a reader and nothing at all to the compiler.
  if (scopeType && scopeId && scheduleName) {
    return (
      <OneSchedule
        scopeType={scopeType}
        scopeId={scopeId}
        name={scheduleName}
        row={chosen}
        // WHETHER AN ABSENT ROW MEANS ANYTHING YET. `chosen` is undefined both
        // while the company's schedules are in flight and when none of them
        // is this one, and those say opposite things about the fires below.
        known={Boolean(data)}
        runs={one.data?.runs ?? []}
        truncated={Boolean(one.data?.truncated)}
        loading={one.loading}
        error={one.error}
        refusal={one.refusal}
        now={now}
      />
    );
  }

  return (
    <>
      {/* THE AGENTS PAGE BAR, as every Agents section carries it — Find a
          seat (a found seat opens beside the list, as on Teams), Edit org and
          Add seat. Without it the controls vanished from the bar the moment a
          reader moved from the roster to this tab, and came back when they
          moved away. */}
      <AgentsHeader index={index} />
      {/* NO COUNT IN THE BAR AND NO INTRODUCTION: the section's tab carries
          the figure ("Schedules 6"), which the bar's "6 schedules defined"
          restated a few pixels above it, and a working screen opens on its
          work, as the others do. What a schedule guarantees is in the docs. */}
      {/* The flush Panel is gone: StatGroup draws that surface itself. */}
      <StatGroup columns={3}>
        <StatCard
          icon={<CalendarGlyph size="xs" />}
          label="Schedules"
          value={schedules.length}
          sub="across every seat and unit"
        />
        <StatCard
          icon={<ClockGlyph size="xs" />}
          label="Firing within the hour"
          value={due.length}
          sub={soonest ? `the soonest ${inTime(soonest.next_run, now)}` : "nothing due soon"}
        />
        <StatCard
          icon={<CircleAlertGlyph size="xs" />}
          label="Cannot fire"
          tone={broken.length ? "danger" : undefined}
          value={broken.length}
          // A `problem` IS FREE TEXT the engine writes — no closed set to split
          // on — so the only bounded thing to say is where the reasons are. Each
          // broken row prints its own `problem` in its Next cell, which is the
          // one place it is legible in full.
          sub={
            broken.length
              ? "each one says why in its Next cell"
              : "every expression parses and every timezone resolves"
          }
        />
      </StatGroup>

      {loading && !schedules.length && (
        <Skeleton variant="text" rows={4} label="Loading schedules" />
      )}
      <QueryState
        error={error}
        refusal={refusal}
        loading={loading}
        empty={
          schedules.length
            ? undefined
            : {
                title: "No recurring work is defined",
                hint: "Add a schedules block to a role or a unit in the company configuration — a standup, a nightly audit, a weekly report.",
              }
        }
      >
        <Card padding="none">
          <Card.Header icon={<CalendarGlyph size="sm" />} count={schedules.length}>
            <Card.Title>Defined</Card.Title>
          </Card.Header>
          <DataGrid
            rows={schedules}
            rowKey={rowID}
            // A SCHEDULE HAS AN ADDRESS, and it is its whole identity: two
            // units may each declare a "standup", and a role and a unit may
            // both, so the trail is all three segments. THE FRAME'S OWN
            // ANSWER to where that address lives, rather than a second copy
            // of the route — the rail's `Open ↗` is built from the same
            // reference, so the link a row carries and the way out of the
            // panel it opens can never name different pages.
            rowHref={(s) => peekHref(scheduleRef(s.scope_type, s.scope_id, s.name))}
            onRowActivate={(s, e) => openSchedule(scheduleRef(s.scope_type, s.scope_id, s.name), e)}
            defaultSort="next"
            columns={[
              {
                key: "name",
                header: "Name",
                // THE SCHEDULE'S IDENTIFIER KEEPS TWELVE EMS — a name like
                // `morning-tracker-review` whole — and the columns below that
                // a schedule's own page answers give way first, in their
                // `drop` order: how it last went (the runs below say it too),
                // the raw expression (Next says when), then whose it is (Wakes
                // says who). At 1280 every name read "backlog…".
                floor: NAME_FLOOR,
                sortValue: (s) => s.name,
                cell: (s) => (
                  <span className="row gap-1">
                    <span className="truncate">{s.name}</span>
                    {!s.enabled && <Tag appearance="outline">disabled</Tag>}
                  </span>
                ),
              },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                drop: 3,
                sortValue: (s) => `${s.scope_type}:${scopeName(s)}`,
                cell: (s) => <ScopeCell scopeType={s.scope_type} name={scopeName(s)} who={who} />,
              },
              {
                key: "cron",
                header: "Cron",
                shrink: true,
                drop: 2,
                sortValue: (s) => s.cron,
                cell: (s) => <CronReading cron={s.cron} timezone={s.timezone} />,
              },
              {
                key: "task",
                header: "Task",
                floor: "8rem",
                cell: (s) => <TextCell>{s.task}</TextCell>,
              },
              {
                // WHO A FIRE REACHES, resolved by the engine from the scope
                // and the target — a unit schedule targeting `each` wakes
                // every member, one targeting `lead` wakes one seat, and a
                // role schedule's target is meaningless rather than defaulted.
                // ONE SEAT BY NAME, SEVERAL BY FACE AND COUNT. This was a
                // chip per seat in a column that never wraps, so a unit
                // schedule waking three drew three chips whose names were
                // cut to "A" and "Age…" — three badges saying nothing. A
                // schedule waking several is read as "who, roughly, and how
                // many"; the names are in the title and said to a screen
                // reader, and the schedule's own page lists them.
                key: "runners",
                header: "Wakes",
                shrink: true,
                cell: (s) => <Wakes runners={s.runners ?? []} who={who} />,
              },
              {
                key: "next",
                header: "Next",
                shrink: true,
                sortValue: (s) => tsKey(s.next_run) || Number.MAX_SAFE_INTEGER,
                // NOT `DateCell`: this is the only column in the product that
                // reads FORWARD, and `relTime` spells a future instant as an
                // elapsed one.
                cell: (s) =>
                  s.next_run ? (
                    <span className="t-caption" title={fmtDateTime(s.next_run)}>
                      {inTime(s.next_run, now)}
                    </span>
                  ) : s.problem ? (
                    // THE REASON, not a blank. A schedule whose timezone was
                    // renamed shows nothing under Next and looks merely idle.
                    <Tag variant="danger" title={s.problem}>
                      {s.problem}
                    </Tag>
                  ) : (
                    <EmptyValue label="Disabled, or the calendar never reaches it" />
                  ),
              },
              {
                key: "last",
                header: "Last",
                shrink: true,
                drop: 1,
                sortValue: (s) => tsKey(lastFire.get(rowID(s))?.fired_at ?? ""),
                cell: (s) => {
                  const run = lastFire.get(rowID(s));
                  if (!run) return <EmptyValue label="This schedule has never fired" />;
                  return (
                    <span className="row gap-1">
                      <DateCell at={run.fired_at} now={now} />
                      {run.outcome && <OutcomeTag outcome={run.outcome} />}
                    </span>
                  );
                },
              },
            ]}
          />
        </Card>

        <Card padding="none">
          <Card.Header icon={<ClockGlyph size="sm" />} count={runs.length}>
            <Card.Title>Recent runs</Card.Title>
          </Card.Header>
          <DataGrid
            name="runs"
            rows={runs}
            rowKey={(r) => `${r.fired_at}:${runID(r)}:${r.fire_label}`}
            // A FIRE IS NOT AN OBJECT — the ledger row has no page of its own
            // — so a row here opens the SCHEDULE it is a fire of. That is the
            // question this table raises: a reader scanning the company's
            // fires stops at one and asks what it was.
            rowHref={(r) => peekHref(scheduleRef(r.scope_type, r.scope_id, r.schedule_name))}
            onRowActivate={(r, e) =>
              openSchedule(scheduleRef(r.scope_type, r.scope_id, r.schedule_name), e)
            }
            defaultSort="-fired"
            empty={{
              title: "No runs recorded",
              hint: "A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record.",
            }}
            columns={[
              {
                key: "fired",
                header: "Fired",
                shrink: true,
                sortValue: (r) => tsKey(r.fired_at),
                cell: (r) => <DateCell at={r.fired_at} now={now} />,
              },
              {
                // THE ROW'S IDENTITY KEEPS ITS NAME WHOLE, as the grid above
                // does, and the facts give way around it in their `drop`
                // order. It was the one flexible column among five sized to
                // their content, so beside a peek at 1280 it was drawn 35px
                // wide — every row read "b." — while the scope repeated the
                // seat the fire woke one column over.
                key: "name",
                header: "Schedule",
                floor: NAME_FLOOR,
                sortValue: (r) => r.schedule_name,
                cell: (r) => <TextCell>{r.schedule_name}</TextCell>,
              },
              {
                // GOES FIRST: a role schedule's scope IS the seat it wakes, so
                // this column says what Woke says on every role row, and a
                // unit's scope is one press away in the schedule it opens.
                key: "scope",
                header: "Scope",
                shrink: true,
                drop: 1,
                sortValue: (r) => `${r.scope_type}:${scopeName(r)}`,
                cell: (r) => <ScopeCell scopeType={r.scope_type} name={scopeName(r)} who={who} />,
              },
              {
                // WHICH SEAT this fire woke. A unit schedule targeting `each`
                // writes one ledger row per member, so without this column
                // three rows read as one fire repeated. It goes LAST of the
                // three, after the tick, because it is the one fact about a
                // fire the schedule it opens cannot say.
                key: "target",
                header: "Woke",
                shrink: true,
                drop: 3,
                cell: (r) => <SeatCell handle={r.target_handle} {...who(r.target_handle)} />,
              },
              {
                key: "outcome",
                header: "Outcome",
                // DRAWN WHOLE: the ledger's three words are a closed set whose
                // longest is "skipped · missed", so the track is its content
                // (`OUTCOME_WIDTH`) rather than a fifth of the grid, which cut
                // both skips to "skipped · …" beside a peek at 1280 — the one
                // word a skip row says.
                width: OUTCOME_WIDTH,
                sortValue: (r) => r.outcome,
                // A BADGE, NOT `StatusCell`: the ledger's two words are a
                // dispatch vocabulary rather than a lifecycle, and this is
                // the pill the Last column above draws for the same value.
                // AN ABSENT OUTCOME IS NOT A WORD, though — a badge reading
                // "—" claims the ledger recorded something it did not.
                cell: (r) =>
                  r.outcome ? (
                    <OutcomeTag outcome={r.outcome} />
                  ) : (
                    <EmptyValue label="The ledger recorded no outcome for this fire" />
                  ),
              },
              {
                // THE TICK, which is not the instant it ran: a catchup run
                // fires now for a tick that was due earlier, and the pair is
                // the only way to see that. ABSOLUTE rather than `DateCell`
                // for exactly that reason — two relative times an hour apart
                // read as one fire, and the tick's own label is the ledger's
                // at-most-once key. It gives way second: beside Fired it
                // differs only on a catchup, and the peek says it too.
                // TRUNCATED rather than clipped, so a column held at its cap
                // ends in an ellipsis, over the tick's own label in the title.
                key: "due",
                header: "For the tick",
                shrink: true,
                drop: 2,
                sortValue: (r) => tsKey(r.scheduled_at),
                cell: (r) =>
                  r.scheduled_at ? (
                    <span className="t-caption truncate" title={r.fire_label}>
                      {fmtDateTime(r.scheduled_at)}
                    </span>
                  ) : (
                    <EmptyValue label="The ledger recorded no tick for this fire" />
                  ),
              },
            ]}
          />
        </Card>
      </QueryState>
    </>
  );
}

/**
 * One schedule: what it is, and every fire this node's ledger still holds.
 *
 * # Why this is its own read
 *
 * `schedules.recent_runs` is the COMPANY's fifty most recent fires. A company
 * with twenty hourly schedules fills that page in two and a half hours, so a
 * screen wanting one schedule's history had to page the company's and filter —
 * which is exactly the shape that makes a reader conclude a schedule stopped
 * running when it simply fell off the end.
 *
 * # A schedule the config no longer declares still has a history
 *
 * The ledger outlives the configuration: a schedule somebody removed has rows
 * until the retention sweep takes them, and this page shows them. So the
 * header degrades to the identity rather than refusing — "there is no such
 * schedule" would be wrong about the rows underneath it.
 */
function OneSchedule({
  scopeType,
  scopeId,
  name,
  row,
  known,
  runs,
  truncated,
  loading,
  error,
  refusal,
  now,
}: {
  scopeType: string;
  scopeId: string;
  name: string;
  row?: ScheduleRow;
  /** Whether the company's schedules have answered — see the caller. */
  known: boolean;
  runs: ScheduleRunRow[];
  truncated: boolean;
  loading: boolean;
  error: string | null;
  refusal: QueryRefusal | LogRefusal | null;
  now: number;
}) {
  usePageLabels({ [[scopeType, scopeId, name].join("/")]: name });
  const org = useOrg();
  // WHO A HANDLE IS — see the same lookup on the screen this page came from.
  const who = useMemo(() => seatLookup(indexOrg(org)), [org]);
  return (
    <>
      <PageActions>
        {
          <a className="t-link" href={href(["agents", "schedules"])}>
            All schedules →
          </a>
        }
      </PageActions>

      {/* THE SCOPE, THE CONDITION AND THE THREE TILES ARE THE HEADER'S NOW.
          The scope was a badge in the page bar and the condition was two more
          beside it — a schedule's identity drawn in the strip that holds a
          screen's controls — and the tiles under them repeated the three
          facts the header already carries. Nothing is lost: the expression's
          description rides beside the expression, the reason a schedule
          cannot fire is still the Next fact, and the runners are still named
          in full. */}
      <ObjectHeader
        kind="Schedule"
        icon="calendar"
        within={scopeLabel(scopeType, row?.scope_name || scopeId, who)}
        title={name}
        // NEITHER THE BADGE NOR THE NOTE MAKES THE CLAIM BEFORE THE ANSWER
        // DOES. "No longer declared" is a statement about the company
        // configuration, and a read still in flight has not seen one.
        status={known ? scheduleStatus(row) : undefined}
        facts={row ? scheduleFacts(row, now, who) : undefined}
      />
      <PageNote>
        {row ? row.task : known ? UNDECLARED_HINT : "One schedule, and every fire it has left."}
      </PageNote>

      {row && <ScheduleDefinition row={row} who={who} />}
      {row && <NextFires row={row} now={now} count={UPCOMING_FIRES} />}

      <QueryState
        error={error}
        refusal={refusal}
        loading={loading}
        empty={
          runs.length
            ? undefined
            : {
                title: "This schedule has not fired",
                hint: "A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record.",
              }
        }
      >
        <Card padding="none">
          <Card.Header
            icon={<ClockGlyph size="sm" />}
            count={runs.length}
            // SAYS WHEN IT CUT: a page that filled is indistinguishable from a
            // schedule that has fired exactly that many times.
            subtitle={
              truncated ? `the newest ${runs.length} — older fires are past this page` : undefined
            }
          >
            <Card.Title>Fires</Card.Title>
          </Card.Header>
          <DataGrid
            name="fires"
            rows={runs}
            rowKey={(r) => `${r.fired_at}:${r.fire_label}:${r.target_handle}`}
            defaultSort="-fired"
            columns={[
              {
                key: "fired",
                header: "Fired",
                shrink: true,
                sortValue: (r) => tsKey(r.fired_at),
                cell: (r) => <DateCell at={r.fired_at} now={now} />,
              },
              {
                // THE ONE COLUMN THAT FILLS. Every column here was sized to
                // its content, so the grid ended 580px into a 1142px card and
                // its head band stopped mid-card over nothing. Who a fire woke
                // is the fact a reader scans this list for, and a name is the
                // value that has a use for the room.
                key: "target",
                header: "Woke",
                floor: WOKE_FLOOR,
                cell: (r) => <SeatCell handle={r.target_handle} {...who(r.target_handle)} />,
              },
              {
                key: "outcome",
                header: "Outcome",
                width: OUTCOME_WIDTH,
                sortValue: (r) => r.outcome,
                cell: (r) =>
                  r.outcome ? (
                    <OutcomeTag outcome={r.outcome} />
                  ) : (
                    <EmptyValue label="The ledger recorded no outcome for this fire" />
                  ),
              },
              {
                // THE TICK, which is not the instant it ran: a catchup fire
                // runs now for a tick that was due earlier, and the pair is
                // the only way to see that — so it stays absolute where the
                // column beside it is relative.
                key: "due",
                header: "For the tick",
                shrink: true,
                sortValue: (r) => tsKey(r.scheduled_at),
                cell: (r) =>
                  r.scheduled_at ? (
                    <span className="t-caption truncate" title={r.fire_label}>
                      {fmtDateTime(r.scheduled_at)}
                    </span>
                  ) : (
                    <EmptyValue label="The ledger recorded no tick for this fire" />
                  ),
              },
            ]}
          />
        </Card>
      </QueryState>
    </>
  );
}

/**
 * One schedule, beside the list it was found in.
 *
 * # Two reads, because a schedule's definition and its history are two facts
 *
 * The DEFINITION is pushed on a config apply and arrives in the company-wide
 * `schedules` answer; the HISTORY is the dispatch ledger and is not pushed at
 * all. The rail asks for both rather than filtering the company's fifty most
 * recent fires, for the reason the page does: twenty hourly schedules fill
 * that page in two and a half hours, and a schedule whose fires fell off the
 * end is indistinguishable from one that stopped running.
 *
 * # An address with no definition behind it is not an error
 *
 * The ledger outlives the configuration. A schedule somebody removed keeps
 * its fires until the retention sweep takes them, so the rail degrades to the
 * identity and says which of the two absences it is — and only an address
 * with neither a definition nor a fire is "no such schedule".
 */
export function SchedulePeek({ scope }: { scope: string }) {
  const now = useNow();
  const org = useOrg();
  // WHO A HANDLE IS. A schedule's scope and every fire's target are handles,
  // and a handle is an address rather than a label: drawn bare, this grid put
  // `agent-ai-systems-engineer` where the seat's name belongs, built the
  // badge's monogram out of it, and drew a person's seat as an agent's.
  const who = useMemo(() => seatLookup(indexOrg(org)), [org]);
  // THE ADDRESS IS ALL THREE SEGMENTS — see [scheduleRef]. A plain split is
  // right because the engine SLUGS a schedule's name, so the only separators
  // in the token are the two this screen put there.
  const [scopeType = "", scopeId = "", name = ""] = scope.split("/");
  const addressed = Boolean(scopeType && scopeId && name);

  const defined = useQuery("schedules", undefined, { enabled: addressed, pollMs: 30_000 });
  const history = useQuery(
    "schedule_runs",
    { scope_type: scopeType, scope_id: scopeId, name },
    { enabled: addressed, pollMs: 30_000 },
  );

  const row = defined.data?.schedules?.find(
    (s) => s.scope_type === scopeType && s.scope_id === scopeId && s.name === name,
  );
  const runs = history.data?.runs ?? [];
  const loading = defined.loading || history.loading;
  const answered = Boolean(defined.data) && Boolean(history.data);
  // The read whose failure the banner shows, its code and refusal together.
  const failed = defined.error ? defined : history;

  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Schedule"
        icon="calendar"
        within={scopeLabel(scopeType, row?.scope_name || history.data?.scope_name || scopeId, who)}
        // THE NAME OUT OF THE ADDRESS, not out of the row: a schedule the
        // configuration has dropped still has a name, and a rail that waited
        // for a row to title itself would draw an empty header over a list of
        // real fires.
        title={name || scope}
        status={answered ? scheduleStatus(row) : undefined}
        facts={row ? scheduleFacts(row, now, who) : undefined}
      />
      <div className="col gap-3">
        {loading && !answered && <Skeleton variant="text" rows={6} label="Loading the schedule" />}
        <QueryState
          error={failed.error}
          refusal={failed.refusal}
          loading={loading}
          empty={
            answered && !row && runs.length === 0
              ? { title: `No schedule at “${scope}”`, hint: NO_SCHEDULE_HINT }
              : undefined
          }
        >
          {answered && (
            <>
              {/* WHICH ABSENCE THIS IS, before anything below it. A rail
                  showing fires under no definition reads as a rail that
                  failed to load one. */}
              {!row && runs.length > 0 && <p className="t-caption">{UNDECLARED_HINT}</p>}

              {row && <ScheduleDefinition row={row} who={who} />}
              {row && <NextFires row={row} now={now} count={PEEK_FIRES} />}

              <Card padding="none">
                <Card.Header
                  icon={<ClockGlyph size="sm" />}
                  count={runs.length}
                  // WHICH OF THE COUNT IS DRAWN. The chip is the ledger's own
                  // total and the rows are the newest few of it — a panel that
                  // said 12 over three rows and nothing else would read as a
                  // list that failed to finish rendering.
                  subtitle={runs.length > PEEK_RUNS ? `the newest ${PEEK_RUNS} of them` : undefined}
                >
                  <Card.Title>Last fires</Card.Title>
                </Card.Header>
                {runs.length > 0 ? (
                  <div className="list">
                    {runs.slice(0, PEEK_RUNS).map((r) => (
                      <div
                        key={`${r.fired_at}:${r.fire_label}:${r.target_handle}`}
                        className="thread-entry"
                      >
                        <div className="row gap-1">
                          {r.outcome ? (
                            <OutcomeTag outcome={r.outcome} />
                          ) : (
                            <EmptyValue label="The ledger recorded no outcome for this fire" />
                          )}
                          <span className="spacer" />
                          <span className="t-caption" title={fmtDateTime(r.fired_at)}>
                            {relTime(r.fired_at, now)}
                          </span>
                        </div>
                        <div className="row gap-1">
                          <SeatCell handle={r.target_handle} {...who(r.target_handle)} />
                          <span className="spacer" />
                          {/* THE TICK BESIDE THE FIRE, which is the only way
                              to see a catchup: this ran now for something
                              that was due earlier. */}
                          <span className="t-caption" title={r.fire_label}>
                            {r.scheduled_at ? `for ${fmtDateTime(r.scheduled_at)}` : "no tick"}
                          </span>
                        </div>
                      </div>
                    ))}
                  </div>
                ) : (
                  <EmptyState
                    size="compact"
                    icon={<ClockGlyph size={32} />}
                    title="This schedule has not fired"
                    description="A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record."
                  />
                )}
              </Card>
            </>
          )}
        </QueryState>
      </div>
    </>
  );
}
