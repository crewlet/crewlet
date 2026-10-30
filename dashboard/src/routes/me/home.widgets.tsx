/**
 * My work's widgets: the reader's own day, read by the record My work reads
 * it by (`ViewerState.owner`), never anybody else's.
 *
 * ONE QUESTION FOR ALL OF THEM. My work asks `work_my_work` once and draws
 * every section from the answer; each widget here asks the same question with
 * the same poll and the same push (`refetchOnInboxOf`), so a widget is never a
 * different reading of the day than the page it opens.
 */

import { useMemo } from "react";
import { List, ListItem, StatCard, StatGroup } from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  CircleCheckGlyph,
  ListOrderedGlyph,
  MessageSquareGlyph,
  ZapGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { itemPath } from "~/lib/work.ts";
import { askQuestion } from "~/routes/inbox/model.ts";
import type { WorkClaimTotal, WorkMyWork, WorkSummary } from "~/protocol/index.ts";
import {
  PARTIAL_FIGURE,
  WidgetCard,
  WidgetNote,
  WidgetQuery,
  defineWidgets,
  gapsOf,
} from "~/routes/home/board/kit.tsx";
import { MoreLink, ROWS, TaskList, countLabel } from "~/routes/worklet/taskRows.tsx";

/** The reader's own day, as My work asks it. */
function useMyWork() {
  const viewer = useViewer();
  const whose = viewer.owner;
  const read = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "" && viewer.worklet,
    pollMs: 30_000,
    refetchOnInboxOf: whose,
  });
  return { viewer, read };
}

/** Said in place of a day for a reader with no record of their own (a machine token). */
const NO_RECORD = "This sign-in has no work record.";

/**
 * One block of the day as a card: its rows, its whole count beside its name,
 * and a link to the section that lists all of it.
 */
function Block({
  title,
  from,
  to,
  pick,
  empty,
  ordinals = false,
}: {
  title: string;
  from: string;
  to: string[];
  pick: (day: WorkMyWork) => { rows: WorkSummary[]; total: WorkClaimTotal };
  empty: { title: string; hint: string };
  ordinals?: boolean;
}) {
  const { viewer, read } = useMyWork();
  const block = read.data ? pick(read.data) : undefined;
  const rows = block?.rows.slice(0, ROWS) ?? [];
  return (
    <WidgetCard
      title={block ? `${title} · ${countLabel(block.total.total, block.total.capped)}` : title}
      from={from}
    >
      {!viewer.loading && viewer.owner === "" ? (
        <WidgetNote>{NO_RECORD}</WidgetNote>
      ) : (
        <WidgetQuery
          error={read.error}
          refusal={read.refusal}
          loading={read.loading && !read.data}
          gaps={gapsOf([read.data])}
          empty={read.data && rows.length === 0 ? empty : undefined}
        >
          {ordinals ? (
            <List variant="numbered" className="home-tasks">
              {rows.map((row) => (
                <ListItem key={row.id} href={href(itemPath(row))}>
                  <span className="home-tasks-line">
                    <span className="home-tasks-key mono">{row.key}</span>
                    <span className="home-tasks-title">{row.title}</span>
                  </span>
                </ListItem>
              ))}
            </List>
          ) : (
            <TaskList rows={rows} />
          )}
          <MoreLink shown={rows.length} total={block?.total} to={to} noun="task" />
        </WidgetQuery>
      )}
    </WidgetCard>
  );
}

/** The asks waiting on the reader: who asked, what, and how long ago. */
function AskedOfMe() {
  const { viewer, read } = useMyWork();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const asks = (read.data?.asked_of_me ?? []).filter((a) => a.open !== false);
  const total = read.data?.totals.asked_of_me;
  const rows = asks.slice(0, ROWS);
  return (
    <WidgetCard
      title={total ? `Asked of me · ${countLabel(total.total, total.capped)}` : "Asked of me"}
      from="me-asked-of-me"
    >
      {!viewer.loading && viewer.owner === "" ? (
        <WidgetNote>{NO_RECORD}</WidgetNote>
      ) : (
        <WidgetQuery
          error={read.error}
          refusal={read.refusal}
          loading={read.loading && !read.data}
          gaps={gapsOf([read.data])}
          empty={
            read.data && rows.length === 0
              ? {
                  title: "Nothing is asked of you",
                  hint: "Questions put to you on a task land here.",
                }
              : undefined
          }
        >
          <List className="home-tasks">
            {rows.map((ask) => (
              <ListItem
                key={ask.comment}
                href={href(itemPath(ask))}
                leading={<MessageSquareGlyph size="sm" />}
                trailing={<span className="home-tasks-when">{relTime(ask.asked_at, now)}</span>}
              >
                <span className="home-tasks-stack">
                  <span className="home-tasks-title">{askQuestion(ask)}</span>
                  <span className="home-tasks-meta">
                    <span className="mono">{ask.key}</span>
                    {` · ${index.byHandle.get(ask.asked_by)?.name ?? ask.asked_by}`}
                  </span>
                </span>
              </ListItem>
            ))}
          </List>
          <MoreLink shown={rows.length} total={total} to={["me", "asked-of-me"]} noun="question" />
        </WidgetQuery>
      )}
    </WidgetCard>
  );
}

/** The day in four counts, each opening its section. */
function Glance() {
  const { viewer, read } = useMyWork();
  if (!viewer.loading && viewer.owner === "") {
    return (
      <WidgetCard title="My work" from="me">
        <WidgetNote>{NO_RECORD}</WidgetNote>
      </WidgetCard>
    );
  }
  const totals = read.data?.totals;
  const loading = read.loading && !read.data;
  const tiles: { key: string; label: string; to: string[]; total?: WorkClaimTotal }[] = [
    { key: "queue", label: "Queue", to: ["me"], total: totals?.assigned },
    { key: "asked", label: "Asked of me", to: ["me", "asked-of-me"], total: totals?.asked_of_me },
    {
      key: "unblocked",
      label: "Unblocked",
      to: ["me", "unblocked"],
      total: totals?.unblocked_recent,
    },
    {
      key: "collaborating",
      label: "Collaborating",
      to: ["me", "collaborating"],
      total: totals?.collaborating,
    },
  ];
  if (read.error) {
    return (
      <WidgetCard title="My work" from="me">
        <WidgetQuery error={read.error} refusal={read.refusal} loading={false} gaps={[]} />
      </WidgetCard>
    );
  }
  // A COUNT OVER RECORDS THIS BUILD COULD NOT READ MAY BE SHORT, so each
  // tile says so on its second line rather than drawing it as the day.
  const partial = gapsOf([read.data]).length > 0;
  return (
    <StatGroup columns={4} aria-label="My work">
      {tiles.map((t) => (
        <StatCard
          key={t.key}
          flush
          loading={loading}
          label={
            <a className="t-link" href={href(t.to)}>
              {t.label}
            </a>
          }
          value={t.total ? countLabel(t.total.total, t.total.capped) : "0"}
          sub={partial ? PARTIAL_FIGURE : undefined}
          subTone={partial ? "warning" : undefined}
        />
      ))}
    </StatGroup>
  );
}

export const widgets = defineWidgets([
  {
    key: "me.glance",
    from: "me",
    label: "My work at a glance",
    description: "Counts of your queue, asks and shared work",
    icon: CircleCheckGlyph,
    hue: "blue",
    size: { w: 6, minW: 4, minH: 6, vertical: false },
    once: true,
    Component: Glance,
  },
  {
    key: "me.queue",
    from: "me",
    label: "My queue",
    description: "Open tasks assigned to you",
    icon: ArrowUpRightGlyph,
    hue: "cyan",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: function MyQueue() {
      return (
        <Block
          title="My queue"
          from="me"
          to={["me"]}
          pick={(d) => ({ rows: d.assigned, total: d.totals.assigned })}
          empty={{ title: "Your queue is clear", hint: "Tasks assigned to you will appear here." }}
        />
      );
    },
  },
  {
    key: "me.priorities",
    from: "me",
    label: "My priorities",
    description: "Your queue in priority order",
    icon: ListOrderedGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: function MyPriorities() {
      return (
        <Block
          title="My priorities"
          from="me"
          to={["me"]}
          ordinals
          pick={(d) => ({ rows: d.priorities, total: d.totals.priorities })}
          empty={{ title: "No priorities set", hint: "Order your queue in My work to set them." }}
        />
      );
    },
  },
  {
    key: "me.asked",
    from: "me-asked-of-me",
    label: "Asked of me",
    description: "Questions waiting on your answer",
    icon: MessageSquareGlyph,
    hue: "amber",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: AskedOfMe,
  },
  {
    key: "me.unblocked",
    from: "me-unblocked",
    label: "Unblocked",
    description: "Your work whose blockers finished",
    icon: ZapGlyph,
    hue: "green",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: function Unblocked() {
      return (
        <Block
          title="Unblocked"
          from="me-unblocked"
          to={["me", "unblocked"]}
          pick={(d) => ({ rows: d.unblocked_recent, total: d.totals.unblocked_recent })}
          empty={{
            title: "Nothing newly unblocked",
            hint: "A task appears here when its blocker finishes.",
          }}
        />
      );
    },
  },
]);
