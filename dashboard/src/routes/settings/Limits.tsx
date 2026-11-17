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
 * › Policies says where they are.
 *
 * # Changed where they are read
 *
 * Each cap the company document carries (`settable`, the engine's own word
 * for it) has a pencil beside its value, drawn for every reader and disabled
 * with the reason for one who cannot write the document. It opens a field
 * for that one cap, which says a value outside the cap's range as it is
 * typed ("At least 1."), from the range the engine's answer carries, and
 * holds Save while the value is that or is the one in force already. The
 * value is then checked against the whole company by the engine before it is
 * stored (`lib/useSettingWrite.ts`), and a refusal is said in a person's
 * words, never the document's keys (`lib/limits.ts`'s limitRefusal).
 * An empty field is the default, which removes the key (`lib/limits.ts`).
 * The value a save wrote is drawn, marked as applying, until this node has
 * applied the revision and the answer carries it. The schedule timeout is
 * the engine's constant and has no pencil: each schedule sets its own.
 */

import { Card, EmptyState, PageNote, SettingsTable } from "@crewlethq/ui";
import { BellGlyph, CalendarClockGlyph, GaugeGlyph, NetworkGlyph } from "@crewlethq/icons/glyphs";
import type { ReactNode } from "react";
import { QueryState } from "~/components/common.tsx";
import { LimitSetting } from "~/components/LimitSetting.tsx";
import type { LimitRow } from "~/lib/limits.ts";
import { useQuery } from "~/lib/useQuery.ts";

/**
 * The groups this page draws: the caps on the company's work. The engine
 * answers other settings as rows of the same shape for the pages they belong
 * to (the delegated-decision limits on Delegated decisions), never here.
 */
type PageGroup = "turn" | "delegation" | "notifications" | "schedules";

/** Each group's heading, in the engine's drawing order. */
const LIMIT_GROUPS: Readonly<Record<PageGroup, { title: string; note?: string; icon: ReactNode }>> =
  {
    turn: { title: "Guardrails on every turn", icon: <GaugeGlyph size="sm" /> },
    delegation: { title: "Delegation", icon: <NetworkGlyph size="sm" /> },
    notifications: { title: "Notifications", icon: <BellGlyph size="sm" /> },
    // The one cap a schedule overrides, said once over its row.
    schedules: {
      title: "Schedules",
      note: "A schedule can set its own limit",
      icon: <CalendarClockGlyph size="sm" />,
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
  "turn_engine.extension_round_step": "Rounds one extension may grant a first turn",
  "turn_engine.delegation_depth_limit": "Delegation depth",
  "turn_engine.executor_min_budget_tokens": "Tokens an executor launch needs",
  "turn_engine.delegation.max_parallel": "Workers at once",
  "turn_engine.delegation.max_tasks_per_call": "Tasks per call",
  "turn_engine.delegation.max_turns": "Turns per worker",
  "turn_engine.delegation.max_turns_ceiling": "Most turns a worker may ask for",
  "turn_engine.delegation.min_tokens_per_task": "Fewest tokens a worker is given",
  "turn_engine.delegation.budget_fraction": "Share of the budget a call may spend",
  "turn_engine.delegation.task_timeout_seconds": "One task's time limit",
  "turn_engine.delegation.call_timeout_seconds": "One call's time limit",
  notification_rate_limit: "Wake-ups per seat",
  notification_coalesce_max_batch: "Notices gathered into one wake-up",
  "schedules.timeout_seconds": "A scheduled turn's time limit",
};

export function Limits() {
  const { data, loading, error, refusal, detail, refetch } = useQuery("limits", undefined, {
    pollMs: 300_000,
  });
  const rows = data?.limits ?? [];
  const groups = (Object.keys(LIMIT_GROUPS) as PageGroup[])
    .map((group) => ({ group, rows: rows.filter((r) => r.group === group) }))
    .filter((g) => g.rows.length > 0);
  return (
    <>
      <PageNote>The caps every turn, delegation and wake-up is held to.</PageNote>
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
          <LimitGroupCard key={group} group={group} caps={caps} onApplied={refetch} />
        ))}
      </QueryState>
    </>
  );
}

/** One group of caps: a card whose table is named by the card's title. */
function LimitGroupCard({
  group,
  caps,
  onApplied,
}: {
  group: PageGroup;
  caps: LimitRow[];
  onApplied: () => void;
}) {
  const { title, note, icon } = LIMIT_GROUPS[group];
  const titleID = `limits-${group}`;
  return (
    // FLUSH, so the table's row rules reach the card's edges under the
    // head's own rule rather than stopping at a body's inset.
    <Card padding="none">
      <Card.Header icon={icon} count={caps.length} subtitle={note}>
        <Card.Title id={titleID}>{title}</Card.Title>
      </Card.Header>
      <SettingsTable aria-labelledby={titleID}>
        {caps.map((cap) => (
          <LimitSetting
            key={cap.key}
            cap={cap}
            label={LABELS[cap.key] ?? cap.key}
            area="Limits"
            reread="Reload to see the limits as they are now."
            onApplied={onApplied}
          />
        ))}
      </SettingsTable>
    </Card>
  );
}
