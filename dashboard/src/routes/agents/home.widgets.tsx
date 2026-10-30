/**
 * What the Agents workspace offers for Home: one seat's card, the roster by
 * state, and the schedules that fire next.
 *
 * THE ROSTER'S OWN READING. A seat's state is `activityOf` over the agents
 * push, bucketed as the roster buckets it, and a schedule is the row the
 * Schedules screen asks for, so Home and Agents can never disagree about who
 * is working or what runs next.
 */

import { useMemo } from "react";
import { List, ListItem, StatCard, StatGroup, Tag, type StatCardTone } from "@crewlethq/ui";
import { CalendarClockGlyph, UserGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { SeatCard } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, inTime, plural, tsKey } from "~/lib/format.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { activityOf, indexOrg, nameOfIn, seatLookup, stateGroupWord, toneOf } from "~/lib/seats.ts";
import type { ScheduleRow } from "~/protocol/index.ts";
import { scopeLabel } from "~/routes/agents/Schedules.tsx";
import {
  ChoiceNote,
  WidgetCard,
  WidgetQuery,
  defineWidgets,
  type WidgetChoices,
} from "~/routes/home/board/kit.tsx";

/** Every seat the chart holds, by name: what an Agent card can follow. */
function useSeatChoices(): WidgetChoices {
  const org = useOrg();
  return useMemo(() => {
    const seats = indexOrg(org).seats.filter((s) => s.handle !== "");
    return {
      loading: org === null,
      options: seats
        .map((s) => ({ value: s.handle, label: s.name }))
        .sort((a, b) => a.label.localeCompare(b.label)),
      empty: "No seats on the org chart yet.",
    };
  }, [org]);
}

function AgentCard({ params }: { params: Readonly<Record<string, string>> }) {
  const org = useOrg();
  const agents = useAgents();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = params.seat ? index.byHandle.get(params.seat) : undefined;
  if (!seat) {
    return (
      <WidgetCard title="Agent" from="org-roster">
        <ChoiceNote thing="seat" />
      </WidgetCard>
    );
  }
  return (
    <div className="home-agents-seat">
      <SeatCard
        seat={seat}
        agent={agents.find((a) => a.id === seat.handle)}
        nameOf={nameOfIn(index)}
      />
    </div>
  );
}

/**
 * The roster's state buckets an operator acts on, each headed by the roster's
 * own group word (`stateGroupWord`) and in the tone its pill takes (`toneOf`),
 * so a tile here and a group on the roster can never name one state twice.
 */
const STATES = (["working", "needs", "stopped", "idle"] as const).map((key) => ({
  key,
  label: stateGroupWord(key),
  tone: toneOf(key) satisfies StatCardTone,
}));

/** Two names and how many more: "Crewlet-PM, Crewlet-SWE and 3 more". */
function namesLine(names: string[]): string {
  if (names.length === 0) return "None";
  const shown = names.slice(0, 2).join(", ");
  return names.length > 2 ? `${shown} and ${names.length - 2} more` : shown;
}

function RosterStatus() {
  const org = useOrg();
  const agents = useAgents();
  const buckets = useMemo(() => {
    const byHandle = new Map(agents.map((a) => [a.id, a]));
    const out = new Map<string, string[]>();
    for (const seat of indexOrg(org).seats) {
      if (seat.kind !== "agent") continue;
      const state = activityOf(byHandle.get(seat.handle));
      out.set(state, [...(out.get(state) ?? []), seat.name]);
    }
    return out;
  }, [org, agents]);
  return (
    <StatGroup columns={4}>
      {STATES.map((s) => {
        const names = buckets.get(s.key) ?? [];
        return (
          <StatCard
            key={s.key}
            label={s.label}
            value={names.length}
            sub={namesLine(names)}
            tone={names.length > 0 ? s.tone : "neutral"}
          />
        );
      })}
    </StatGroup>
  );
}

const UPCOMING = 5;

/** Schedules that cannot fire first, since each is a defect; then by when they fire next. */
function upcoming(rows: readonly ScheduleRow[]): ScheduleRow[] {
  const rank = (s: ScheduleRow) => (s.problem ? 0 : s.next_run ? 1 : 2);
  return [...rows]
    .filter((s) => s.enabled || s.problem)
    .sort(
      (a, b) =>
        rank(a) - rank(b) ||
        (tsKey(a.next_run) || Number.MAX_SAFE_INTEGER) -
          (tsKey(b.next_run) || Number.MAX_SAFE_INTEGER),
    )
    .slice(0, UPCOMING);
}

function NextSchedules() {
  const now = useNow();
  const org = useOrg();
  const who = useMemo(() => seatLookup(indexOrg(org)), [org]);
  // The Schedules screen's own read and poll: a cron's next fire moves in minutes.
  const { data, loading, error, refusal } = useQuery("schedules", undefined, { pollMs: 30_000 });
  const rows = upcoming(data?.schedules ?? []);
  return (
    <WidgetCard title="Upcoming schedules" from="schedules">
      <WidgetQuery
        error={error}
        refusal={refusal}
        loading={loading && !data}
        // NOTHING TO READ A GAP FROM: the schedules' answer carries no flag
        // saying what it could not account for.
        gaps={[]}
        empty={
          data && rows.length === 0
            ? {
                title: "Nothing is scheduled",
                hint: "Recurring work is declared on a seat or a team.",
              }
            : undefined
        }
      >
        <List>
          {rows.map((s) => (
            <ListItem
              key={`${s.scope_type}:${s.scope_id}:${s.name}`}
              href={href(["company", "schedules", s.scope_type, s.scope_id, s.name])}
              tone={s.problem ? "danger" : "default"}
              dangerLabel="Cannot fire"
              trailing={
                s.problem ? (
                  <Tag variant="danger" title={s.problem}>
                    cannot fire
                  </Tag>
                ) : s.next_run ? (
                  <span className="t-caption" title={fmtDateTime(s.next_run)}>
                    {inTime(s.next_run, now)}
                  </span>
                ) : undefined
              }
            >
              <span className="home-widget-line">
                <span className="truncate">{s.name}</span>
                <span className="t-caption truncate">
                  {scopeLabel(s.scope_type, s.scope_name, who)}
                </span>
              </span>
            </ListItem>
          ))}
        </List>
        {(data?.schedules.length ?? 0) > rows.length ? (
          <a className="home-card-more t-link" href={href(["company", "schedules"])}>
            {`All ${plural(data?.schedules.length ?? 0, "schedule")}`}
          </a>
        ) : null}
      </WidgetQuery>
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "org.seat",
    from: "org-roster",
    label: "Agent",
    description: "One seat's state and current task",
    icon: UserGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 6, vertical: false },
    params: [
      {
        key: "seat",
        label: "Seat",
        helper: "Any seat on the org chart, agent or person.",
        useChoices: useSeatChoices,
      },
    ],
    Component: AgentCard,
  },
  {
    key: "org.roster",
    from: "org-roster",
    label: "Roster by state",
    description: "Agents working, waiting or stopped",
    icon: UsersGlyph,
    hue: "green",
    size: { w: 12, minW: 6, minH: 6, vertical: false },
    once: true,
    Component: RosterStatus,
  },
  {
    key: "schedules.upcoming",
    from: "schedules",
    label: "Upcoming schedules",
    description: "Recurring work and when it runs next",
    icon: CalendarClockGlyph,
    hue: "amber",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: NextSchedules,
  },
]);
