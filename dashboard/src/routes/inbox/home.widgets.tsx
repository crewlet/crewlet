/**
 * Inbox's widget: the updates waiting for the reader, as the Inbox lists them.
 *
 * THE SIDEBAR'S QUESTION. The unread primary notices are what the Inbox row's
 * badge counts (`lib/useInboxCounts.ts`), asked of the same record with the
 * same push, so the number on this card and the number in the sidebar are one
 * reading. A row opens that notice in the Inbox, where reading it marks it
 * read; nothing here marks anything.
 */

import { useMemo } from "react";
import { List, ListItem } from "@crewlethq/ui";
import { InboxGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { Badge } from "~/routes/inbox/InboxColumn.tsx";
import {
  changeWords,
  noticeRows,
  noticeText,
  noticeTitle,
  rowName,
  rowWho,
  shortWhen,
} from "~/routes/inbox/model.ts";
import { WidgetCard, WidgetNote, defineWidgets } from "~/routes/home/board/kit.tsx";
import { ROWS, countLabel } from "~/routes/work/taskRows.tsx";

function Updates() {
  const viewer = useViewer();
  const owner = viewer.owner;
  // THE COMPOSED INBOX, every native domain's notices, as the sidebar counts
  // them: asked for every reader with a record, each source under its own
  // permission on the engine's side.
  const read = useQuery(
    "inbox",
    owner ? { handle: owner, limit: ROWS, unread: true, primary_only: true } : undefined,
    { enabled: owner !== "", pollMs: 60_000, refetchOnInboxOf: owner },
  );
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const rows = noticeRows(read.data?.notices ?? []);
  const unread = read.data?.primary;
  return (
    <WidgetCard
      title={unread !== undefined && unread > 0 ? `Inbox · ${countLabel(unread)} unread` : "Inbox"}
      from="inbox"
    >
      {!viewer.loading && owner === "" ? (
        <WidgetNote>This sign-in has no inbox.</WidgetNote>
      ) : (
        <QueryState
          error={read.error}
          refusal={read.refusal}
          loading={read.loading && !read.data}
          empty={
            read.data && rows.length === 0
              ? { title: "You are caught up", hint: "New updates that need you will land here." }
              : undefined
          }
        >
          <List className="home-tasks">
            {rows.map((row) => {
              if (row.kind === "change") {
                const words = changeWords(row.notice);
                return (
                  <ListItem
                    key={row.key}
                    href={href(["inbox"], { row: row.key })}
                    leading={<Badge row={row} who={null} size={20} />}
                    trailing={<span className="home-tasks-when">{shortWhen(row.at, now)}</span>}
                  >
                    <span className="home-tasks-stack">
                      <span className="home-tasks-title">
                        {`${rowName(row, null)} · ${words.text}`}
                      </span>
                      {words.excerpt ? (
                        <span className="home-tasks-meta">{words.excerpt}</span>
                      ) : null}
                    </span>
                  </ListItem>
                );
              }
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
          {unread !== undefined && unread > rows.length ? (
            <a className="home-card-more t-link" href={href(["inbox"])}>
              {`All ${countLabel(unread)} unread`}
            </a>
          ) : null}
        </QueryState>
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
    // EVERY NATIVE DOMAIN'S NOTICES, so no one service: a reader on a company
    // that runs no Crewlet Works still hears about the changes they are asked
    // to review.
    Component: Updates,
  },
]);
