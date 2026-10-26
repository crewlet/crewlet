/**
 * Settings › Limits: the caps the engine holds the company's work to, as it
 * ENFORCES them, and the one place they are drawn.
 *
 * # What the engine enforces, never a copy of it
 *
 * Every number is the engine's own answer (`limits`, an administrator's):
 * the value it holds a turn, a delegation call or a wake-up to, with its
 * default applied where the company document writes none. The page keeps no
 * default of its own, so it can never say a number the engine does not hold.
 * The applied company does not remember whether a value was written, so the
 * page says only what is true: whether the value in force is the engine's
 * default. A default says so as a quiet word; a value that differs carries a
 * mark and the default it replaced, which is the one row a reader scanning
 * the page came to find. A value that switches its limit off says Off rather
 * than 0.
 *
 * # One table, drawn as one
 *
 * Each group is a card holding a [SettingsTable]: the cap's name with its key
 * in the company document under it, the value in tabular figures, and its
 * source. The table's tracks are fractions of the card, so every card on the
 * page puts its values and sources on the same two lines, and each group
 * takes a glyph of its own so the cards are told apart at a glance.
 *
 * # Engine settings, not the company's policies
 *
 * These caps were drawn beside the standing policies once. They are not
 * policies the company writes for its agents to read; they are what the engine
 * stops a turn at, so they live here, in Settings' Runtime group, and Company
 * › Policies says where they are. Read only: each is a key of the company
 * document, changed there, and the key is drawn under each cap's name.
 */

import { Card, EmptyState, SettingsTable, PageNote } from "@crewlethq/ui";
import { BellGlyph, CalendarClockGlyph, GaugeGlyph, NetworkGlyph } from "@crewlethq/icons/glyphs";
import type { ReactNode } from "react";
import { QueryState } from "~/components/common.tsx";
import { fmtSetting } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { LimitsAnswer } from "~/contract/limits.ts";

type LimitRow = LimitsAnswer["limits"][number];

/** Each group's heading and glyph, in the engine's drawing order. */
const LIMIT_GROUPS: Readonly<
  Record<LimitRow["group"], { title: string; icon: ReactNode; note?: string }>
> = {
  turn: { title: "Guardrails on every turn", icon: <GaugeGlyph size="sm" /> },
  delegation: { title: "Delegation", icon: <NetworkGlyph size="sm" /> },
  notifications: { title: "Notifications", icon: <BellGlyph size="sm" /> },
  schedules: {
    title: "Schedules",
    icon: <CalendarClockGlyph size="sm" />,
    // THE ONE CAP A SCHEDULE OVERRIDES, said once over its row rather than
    // as a clause that wrapped the row's name onto two lines.
    note: "A schedule may set its own time limit.",
  },
};

/**
 * What each cap is called, by its key in the company document. Words only: a
 * key this build has no words for is drawn by the key itself, so a cap a
 * newer engine adds is still shown.
 */
const LABELS: Readonly<Record<string, string>> = {
  "turn_engine.max_iterations": "Review rounds per turn",
  "turn_engine.onboarding_max_tool_rounds": "Tool rounds on a first turn",
  "turn_engine.delegation_depth_limit": "Delegation depth",
  "turn_engine.delegation.max_parallel": "Workers at once",
  "turn_engine.delegation.max_tasks_per_call": "Tasks per call",
  "turn_engine.delegation.max_turns": "Turns per worker",
  "turn_engine.delegation.budget_fraction": "Share of the budget a call may spend",
  "turn_engine.delegation.task_timeout_seconds": "One task's time limit",
  "turn_engine.delegation.call_timeout_seconds": "One call's time limit",
  notification_rate_limit: "Wake-ups per seat",
  notification_coalesce_max_batch: "Notices gathered into one wake-up",
  "schedules.timeout_seconds": "A scheduled turn's time limit",
};

/**
 * A number of a cap's unit, in words: the value in force, or the default it
 * replaced. A value that switches the limit off says Off, and so does a
 * default of zero for a cap whose zero is off.
 */
function amountWords(row: LimitRow, amount: number, off: boolean): string {
  if (off) return "Off";
  switch (row.unit) {
    case "seconds":
      return fmtSetting(amount);
    case "fraction":
      return `${Math.round(amount * 100)}%`;
    case "per_second":
      return `${amount.toLocaleString()} per second`;
    case "count":
      return amount.toLocaleString();
  }
}

/**
 * Where a cap's value comes from: the engine's default, or a value the
 * company document sets, with the default it replaced.
 */
export function limitSource(row: LimitRow): { changed: boolean; words: string } {
  if (row.value === row.default) return { changed: false, words: "Default" };
  return { changed: true, words: `Changed from ${amountWords(row, row.default, row.default_off)}` };
}

export function Limits() {
  const { data, loading, error, refusal, detail } = useQuery("limits", undefined, {
    pollMs: 300_000,
  });
  const rows = data?.limits ?? [];
  const groups = (Object.keys(LIMIT_GROUPS) as LimitRow["group"][])
    .map((group) => ({ group, rows: rows.filter((r) => r.group === group) }))
    .filter((g) => g.rows.length > 0);
  return (
    <>
      <PageNote>
        The caps the engine holds the company&rsquo;s work to, read from the company document.
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && rows.length === 0 && (
          <EmptyState
            icon={<GaugeGlyph size="xl" />}
            title="No limits to show"
            description="This node runs no company, so no turn runs to be limited."
          />
        )}
        {/* THE SECTION'S OWN RHYTHM: the cards stand in the section body,
            whose gap every Settings page shares, rather than in a stack of
            their own a step tighter. */}
        {groups.map(({ group, rows: caps }) => (
          <LimitGroupCard key={group} group={group} caps={caps} />
        ))}
      </QueryState>
    </>
  );
}

/** One group of caps: a card whose table is named by the card's title. */
function LimitGroupCard({ group, caps }: { group: LimitRow["group"]; caps: LimitRow[] }) {
  const { title, icon, note } = LIMIT_GROUPS[group];
  const titleID = `limits-${group}`;
  return (
    // FLUSH, so the table's row rules reach the card's edges under the
    // head's own rule rather than stopping at a body's inset.
    <Card padding="none">
      <Card.Header icon={icon} subtitle={note}>
        <Card.Title id={titleID}>{title}</Card.Title>
      </Card.Header>
      <SettingsTable aria-labelledby={titleID}>
        {caps.map((cap) => {
          const source = limitSource(cap);
          return (
            <SettingsTable.Row
              key={cap.key}
              label={LABELS[cap.key] ?? cap.key}
              settingKey={cap.key}
              source={source.words}
              changed={source.changed}
            >
              {amountWords(cap, cap.value, cap.off)}
            </SettingsTable.Row>
          );
        })}
      </SettingsTable>
    </Card>
  );
}
