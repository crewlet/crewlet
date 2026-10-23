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
 * page says only what is true: a value that is the engine's default is tagged
 * Default, and one that switches its limit off says Off rather than 0.
 *
 * # Engine settings, not the company's policies
 *
 * These caps were drawn beside the standing policies once. They are not
 * policies the company writes for its agents to read; they are what the engine
 * stops a turn at, so they live here, in Settings' Runtime group, and Company
 * › Policies says where they are. Read only: each is a key of the company
 * document, changed there.
 */

import { Card, DescriptionList, EmptyState, Tag } from "@crewlethq/ui";
import { GaugeGlyph } from "@crewlethq/icons/glyphs";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtSetting } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { LimitsAnswer } from "~/contract/limits.ts";

type LimitRow = LimitsAnswer["limits"][number];

/** Each group's heading, in the engine's drawing order. */
const GROUP_TITLES: Readonly<Record<LimitRow["group"], string>> = {
  turn: "Guardrails on every turn",
  delegation: "Delegation",
  notifications: "Notifications",
  schedules: "Schedules",
};

/**
 * What each cap is called, by its key in the company document. Words only: a
 * key this build has no words for is drawn by the key itself, so a cap a
 * newer engine adds is still shown.
 */
const LABELS: Readonly<Record<string, string>> = {
  "turn_engine.max_iterations": "Review rounds per turn",
  "turn_engine.max_tool_rounds": "Tool rounds per run",
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
  "schedules.timeout_seconds": "A scheduled turn, unless its schedule says otherwise",
};

/** A cap's value, in its own unit. */
function valueWords(row: LimitRow): string {
  switch (row.unit) {
    case "seconds":
      return fmtSetting(row.value);
    case "fraction":
      return `${Math.round(row.value * 100)}%`;
    case "per_second":
      return `${row.value.toLocaleString()} per second`;
    case "count":
      return row.value.toLocaleString();
  }
}

export function Limits() {
  const { data, loading, error, refusal, detail } = useQuery("limits", undefined, {
    pollMs: 300_000,
  });
  const rows = data?.limits ?? [];
  const groups = (Object.keys(GROUP_TITLES) as LimitRow["group"][])
    .map((group) => ({ group, rows: rows.filter((r) => r.group === group) }))
    .filter((g) => g.rows.length > 0);
  return (
    <>
      <PageNote>
        What the engine stops work at. Read only: each is a key of the company document.
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && rows.length === 0 && (
          <EmptyState
            icon={<GaugeGlyph size="xl" />}
            title="No limits to show"
            description="This node runs no company, so no turn runs to be limited."
          />
        )}
        {/* ONE TERM COLUMN FOR EVERY CARD: each sized its own to its longest
            term, so the values started at a different x in every card and
            the stack did not read as one table. */}
        <div className="col gap-4 settings-terms">
          {groups.map(({ group, rows: caps }) => (
            <Card key={group}>
              <Card.Header icon={<GaugeGlyph size="sm" />}>
                <Card.Title>{GROUP_TITLES[group]}</Card.Title>
              </Card.Header>
              <DescriptionList>
                {caps.map((cap) => (
                  <DescriptionList.Item key={cap.key} term={LABELS[cap.key] ?? cap.key}>
                    {cap.off ? (
                      <Tag size="sm" variant="neutral" appearance="outline">
                        Off
                      </Tag>
                    ) : (
                      valueWords(cap)
                    )}{" "}
                    {cap.value === cap.default && (
                      <Tag size="sm" variant="neutral" appearance="soft">
                        Default
                      </Tag>
                    )}
                  </DescriptionList.Item>
                ))}
              </DescriptionList>
            </Card>
          ))}
        </div>
      </QueryState>
    </>
  );
}
