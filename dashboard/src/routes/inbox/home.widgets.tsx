/**
 * Inbox's widget: the updates waiting for the reader, as the Inbox lists them.
 *
 * THE SIDEBAR'S READING. The unread primary notices are what the Inbox row's
 * badge counts, and the count on this card IS that count: `useInboxCounts`,
 * the one reading the frame asks for every surface that says how much waits
 * (`lib/useInboxCounts.ts`), floor and all ("50+"). The card's own question
 * asks only for the few rows it draws, so its page is no count: counted off
 * that page, the card said "6 unread" beside a sidebar saying "12", and never
 * marked its figure as the floor the page's cursor said it was. A row opens
 * that notice in the Inbox, where reading it marks it read; nothing here
 * marks anything.
 */

import { useMemo } from "react";
import { List, ListItem } from "@crewlethq/ui";
import { InboxGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useInboxCounts } from "~/lib/useInboxCounts.ts";
import { useViewer } from "~/lib/viewer.ts";
import { Badge } from "~/routes/inbox/InboxColumn.tsx";
import {
  noticeRows,
  noticeText,
  noticeTitle,
  rowName,
  rowWho,
  shortWhen,
} from "~/routes/inbox/model.ts";
import {
  WidgetCard,
  WidgetNote,
  WidgetQuery,
  defineWidgets,
  gapsOf,
} from "~/routes/home/board/kit.tsx";
import { ROWS, countLabel } from "~/routes/worklet/taskRows.tsx";

function Updates() {
  const viewer = useViewer();
  const owner = viewer.owner;
  const read = useQuery(
    "work_inbox",
    owner ? { handle: owner, limit: ROWS, unread: true, primary_only: true } : undefined,
    { enabled: owner !== "" && viewer.worklet, pollMs: 60_000, refetchOnInboxOf: owner },
  );
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const rows = noticeRows(read.data?.notices ?? []);
  const { waiting, capped } = useInboxCounts();
  const unread = waiting === null ? null : countLabel(waiting, capped);
  return (
    <WidgetCard
      title={waiting !== null && waiting > 0 ? `Inbox · ${unread} unread` : "Inbox"}
      from="inbox"
    >
      {!viewer.loading && owner === "" ? (
        <WidgetNote>This sign-in has no inbox.</WidgetNote>
      ) : (
        <WidgetQuery
          error={read.error}
          refusal={read.refusal}
          loading={read.loading && !read.data}
          gaps={gapsOf([read.data])}
          empty={
            read.data && rows.length === 0
              ? { title: "You are caught up", hint: "New updates that need you will land here." }
              : undefined
          }
        >
          <List className="home-tasks">
            {rows.map((row) => {
              if (row.kind !== "notice") return null;
              const who = rowWho(row, index);
              const said = noticeText(row.notice, index);
              return (
                <ListItem
                  key={row.key}
                  href={href(["inbox"], { row: row.key })}
                  leading={<Badge row={row} who={who} size={20} />}
                  trailing={<span className="home-tasks-when">{shortWhen(row.at, now)}</span>}
                >
                  <span className="home-tasks-stack">
                    <span className="home-tasks-title">
                      {`${rowName(row, who)} · ${noticeTitle(row.notice)}`}
                    </span>
                    <span className="home-tasks-meta">
                      {row.notice.subject_key ? (
                        <span className="mono">{row.notice.subject_key}</span>
                      ) : null}
                      {said ? `${row.notice.subject_key ? " · " : ""}${said}` : null}
                    </span>
                  </span>
                </ListItem>
              );
            })}
          </List>
          {waiting !== null && (capped || waiting > rows.length) ? (
            <a className="home-card-more t-link" href={href(["inbox"])}>
              {`All ${unread} unread`}
            </a>
          ) : null}
        </WidgetQuery>
      )}
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "inbox.updates",
    from: "inbox",
    label: "Inbox",
    description: "Unread updates that need you",
    icon: InboxGlyph,
    hue: "rose",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    // THE UPDATES ARE WORKLET'S NOTICES, while the Inbox itself stays
    // for the conditions that arrive without it.
    service: () => "tracker",
    Component: Updates,
  },
]);
