/**
 * The work board — the company's own tracker.
 *
 * # Why this screen exists at all, and what it is not
 *
 * It is NOT a second tracker. A company running Jira has none of these
 * questions registered, and this screen says so rather than drawing an empty
 * board: an operator who wired Jira and then found a blank Crewlet board
 * would reasonably conclude their integration was broken.
 *
 * # An unhydrated projection is not an empty company
 *
 * Every row here comes from this node's own projection of the fleet's record,
 * which is the same copy a seat's tools read — so an operator and an agent
 * looking at one item see one item. A node that has not finished its boot
 * reconcile REFUSES rather than answering empty, and `QueryState` renders
 * that refusal as "still catching up". Drawing it as "no work" would be an
 * answer somebody acts on, by filing the duplicate.
 *
 * # Read-only, deliberately
 *
 * Nothing here writes. An item is filed and moved by a seat's own tools, or
 * by an operator through the MCP surface, and both are attributed to
 * somebody — where a board button would write as "the dashboard", which is
 * not a person and not a seat and cannot be asked why.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  Badge,
  Chip,
  Empty,
  Panel,
  SearchInput,
  Segmented,
  Skeleton,
  Stat,
  StatRow,
} from "~/ui/primitives.tsx";
import { Select } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { WorkStatus, WorkSummary } from "~/protocol/index.ts";

/** The board's own vocabulary, rendered. A closed set, so a status the engine
 *  adds later shows as itself rather than vanishing from the filter. */
const STATUSES: { value: WorkStatus; label: string }[] = [
  { value: "todo", label: "To do" },
  { value: "in_progress", label: "In progress" },
  { value: "blocked", label: "Blocked" },
  { value: "in_review", label: "In review" },
  { value: "done", label: "Done" },
];

const STATUS_TONE: Record<string, "positive" | "caution" | "critical" | "info" | "neutral"> = {
  todo: "neutral",
  in_progress: "info",
  blocked: "critical",
  in_review: "caution",
  done: "positive",
};

const PRIORITY_TONE: Record<string, "positive" | "caution" | "critical" | "info" | "neutral"> = {
  low: "neutral",
  normal: "neutral",
  high: "caution",
  urgent: "critical",
};

export function Work() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  // ONE CLOCK for the screen, ticking on its own: a relative time computed
  // from Date.now() at render is frozen until something else re-renders, so
  // "2 minutes ago" stays that for an hour on a screen nobody touches.
  const now = useNow();

  const [project, setProject] = useParam("project", "");
  const [status, setStatus] = useParam("status", "");
  const [assignee, setAssignee] = useParam("assignee", "");
  const [q, setQ] = useParam("q", "");
  // `open` is THREE-STATED on the wire and here: an absent filter asks for
  // everything, and reading it as false would show only finished work.
  const [scope, setScope] = useParam("scope", "open");

  const params: Record<string, unknown> = {};
  if (project) params.project = project;
  if (status) params.status = status;
  if (assignee) params.assignee = assignee;
  if (q) params.q = q;
  if (scope === "open") params.open = true;
  if (scope === "closed") params.open = false;

  // A change to an item publishes onto the seat inbox rather than to the
  // dashboard socket, so there is no push behind this and a poll is correct.
  // Twenty seconds: a board is read, not watched, and a tracker's own pace is
  // a person typing a comment.
  const { data, loading, error } = useQuery("work_items", params, { pollMs: 20_000 });

  const rows = useMemo(
    () => [...(data?.items ?? [])].sort((a, b) => tsKey(b.updated_at) - tsKey(a.updated_at)),
    [data],
  );

  const projects = useMemo(() => {
    const keys = new Set<string>(Object.keys(data?.minted ?? {}));
    for (const item of rows) keys.add(item.project);
    return [...keys].sort();
  }, [data, rows]);

  const byStatus = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const item of rows) counts[item.status] = (counts[item.status] ?? 0) + 1;
    return counts;
  }, [rows]);

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;

  return (
    <>
      <ScreenHead
        title="Work"
        sub="The company's own tracker — every item, who owns it and what moved it. Read-only here: work is filed and moved by the seats themselves, so every change is attributed to somebody."
      />

      {/* The counts are of what is ON SCREEN, and the label says so. A header
          that reported the page's length as the project's size would say
          "50 items" for every project with more than fifty. */}
      {!loading && !error && (
        <StatRow cols={4}>
          <Stat label="Shown" value={rows.length} sub={`of at most ${data?.limit ?? 0}`} />
          <Stat label="In progress" value={byStatus.in_progress ?? 0} />
          <Stat
            label="Blocked"
            value={byStatus.blocked ?? 0}
            icon={byStatus.blocked ? "alert" : undefined}
          />
          <Stat label="In review" value={byStatus.in_review ?? 0} />
        </StatRow>
      )}

      <div className="toolbar">
        <div style={{ flex: 1, maxWidth: 360 }}>
          <SearchInput
            value={q}
            onChange={setQ}
            ariaLabel="Find an item by key or title"
            placeholder="ENG-42, or words from the title"
          />
        </div>
        <Select
          value={project}
          onChange={setProject}
          ariaLabel="Project"
          anyLabel="Every project"
          options={projects}
        />
        <Select
          value={status}
          onChange={setStatus}
          ariaLabel="Status"
          anyLabel="Any status"
          options={STATUSES.map((s) => s.value)}
        />
        {/* THREE SEGMENTS, not a checkbox: "open", "closed" and "everything"
            are three real questions, and a two-state control would make the
            third unreachable — which is how a board that can never show a
            closed item ships. */}
        <Segmented
          value={scope}
          onChange={setScope}
          ariaLabel="Open or closed"
          options={[
            { value: "open", label: "Open" },
            { value: "closed", label: "Closed" },
            { value: "", label: "All" },
          ]}
        />
        {assignee && (
          <Chip on onClick={() => setAssignee("")} title="Clear this filter">
            {seatName(assignee)}
          </Chip>
        )}
      </div>

      {loading && <Skeleton rows={6} />}

      <QueryState
        error={error}
        loading={loading}
        empty={
          rows.length
            ? undefined
            : {
                title: "Nothing matches",
                hint: "No item on this node's copy of the tracker matches these filters. Widen them, or check that work is being filed at all.",
              }
        }
      >
        <Panel>
          <DataTable
            rows={rows}
            rowKey={(r) => r.id}
            defaultSort={{ key: "updated", dir: "desc" }}
            columns={[
              {
                key: "key",
                header: "Key",
                shrink: true,
                sortValue: (r) => r.key,
                cell: (r) => (
                  <a className="mono" href={href(["work", r.key])}>
                    {r.key}
                  </a>
                ),
              },
              {
                key: "title",
                header: "Title",
                sortValue: (r) => r.title,
                cell: (r) => (
                  <a href={href(["work", r.key])} className="truncate">
                    {r.title}
                  </a>
                ),
              },
              {
                key: "type",
                header: "Type",
                shrink: true,
                sortValue: (r) => r.type,
                cell: (r) => <Badge outline>{r.type}</Badge>,
              },
              {
                key: "status",
                header: "Status",
                shrink: true,
                sortValue: (r) => r.status,
                cell: (r) => (
                  <Badge tone={STATUS_TONE[r.status] ?? "neutral"} dot>
                    {STATUSES.find((s) => s.value === r.status)?.label ?? r.status}
                  </Badge>
                ),
              },
              {
                key: "priority",
                header: "Priority",
                shrink: true,
                sortValue: (r) => r.priority,
                cell: (r) =>
                  r.priority && r.priority !== "normal" ? (
                    <Badge tone={PRIORITY_TONE[r.priority] ?? "neutral"}>{r.priority}</Badge>
                  ) : (
                    <span className="dim">—</span>
                  ),
              },
              {
                key: "assignee",
                header: "Assignee",
                shrink: true,
                sortValue: (r) => r.assignee ?? "",
                cell: (r) =>
                  r.assignee ? (
                    <SeatChip name={seatName(r.assignee)} handle={r.assignee} />
                  ) : (
                    // NOBODY IS A STATE, and the one worth seeing: an
                    // unassigned item routes to the project's lead, and a
                    // project with no lead routes to nobody at all.
                    <span className="dim">Unassigned</span>
                  ),
              },
              {
                key: "updated",
                header: "Updated",
                shrink: true,
                align: "right",
                sortValue: (r) => tsKey(r.updated_at),
                cell: (r) => (
                  <span title={fmtDateTime(r.updated_at)}>{relTime(r.updated_at, now)}</span>
                ),
              },
            ]}
          />
        </Panel>
      </QueryState>

      {!loading && !error && projects.length === 0 && rows.length === 0 && (
        <Empty
          icon="inbox"
          title="No work has been filed yet"
          hint="Seats file work with create_work_item, and an inbound webhook or a schedule is usually what starts them. A project's key comes from a unit's `project` field."
        />
      )}
    </>
  );
}

/** One item: its description, its thread and everything that moved it. */
export function WorkItem({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const { data, loading, error } = useQuery(
    "work_item",
    { id },
    { enabled: id !== "", pollMs: 15_000 },
  );

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const item = data?.item;

  return (
    <>
      <ScreenHead
        title={item?.key || id || "Item"}
        sub={item?.title}
        badges={
          item ? (
            <>
              <Badge tone={STATUS_TONE[item.status] ?? "neutral"} dot>
                {STATUSES.find((s) => s.value === item.status)?.label ?? item.status}
              </Badge>
              <Badge outline>{item.type}</Badge>
              {item.project && <Badge outline>{item.project}</Badge>}
            </>
          ) : undefined
        }
      />

      {loading && <Skeleton rows={8} />}

      <QueryState error={error} loading={loading}>
        {item && (
          <div className="stack">
            <Panel title="Description">
              {item.body ? (
                <div className="prose">{item.body}</div>
              ) : (
                <span className="dim">No description was written.</span>
              )}
            </Panel>

            <Panel title="Ownership">
              <StatRow cols={3}>
                <Stat
                  label="Assignee"
                  value={item.assignee ? seatName(item.assignee) : "Unassigned"}
                />
                <Stat label="Reporter" value={item.reporter ? seatName(item.reporter) : "—"} />
                {/* THE REASSIGNMENT COUNT IS A BUDGET, not trivia: an item
                    handed on too many times has stopped being work and
                    started being a hot potato, and the engine refuses the
                    next hand-off rather than letting it circle. */}
                <Stat
                  label="Hand-offs"
                  value={item.reassignments ?? 0}
                  sub="each reassignment spends the item's own budget"
                  icon={(item.reassignments ?? 0) >= 6 ? "alert" : undefined}
                />
              </StatRow>
              {item.watchers?.length ? (
                <div
                  className="row wrap"
                  style={{ gap: "var(--space-2)", marginTop: "var(--space-3)" }}
                >
                  <span className="dim">Watching:</span>
                  {item.watchers.map((w) => (
                    <SeatChip key={w} name={seatName(w)} handle={w} />
                  ))}
                </div>
              ) : null}
            </Panel>

            {data.links?.length ? (
              <Panel title="Links">
                <ul className="list">
                  {data.links.map((link) => (
                    <li
                      key={`${link.kind}:${link.other_id}`}
                      className="row"
                      style={{ gap: "var(--space-2)" }}
                    >
                      <Badge outline>{link.kind.replace(/_/g, " ")}</Badge>
                      <a href={href(["work", link.key || link.other_id])} className="mono">
                        {link.key || link.other_id}
                      </a>
                      <span className="truncate">{link.title}</span>
                      {/* The DERIVED half is the one nobody authored — an
                          editor has to change the other end. */}
                      {link.derived && <span className="dim">(the other end authored this)</span>}
                    </li>
                  ))}
                </ul>
              </Panel>
            ) : null}

            <Panel title={`Thread (${data.comments?.length ?? 0})`}>
              {data.comments?.length ? (
                <div className="stack">
                  {data.comments.map((c) => (
                    <div key={c.id} className="comment">
                      <div className="row" style={{ gap: "var(--space-2)" }}>
                        <SeatChip name={seatName(c.author)} handle={c.author} />
                        <span className="dim" title={fmtDateTime(c.created_at)}>
                          {relTime(c.created_at, now)}
                        </span>
                        {c.edited_at && <span className="dim">(edited)</span>}
                      </div>
                      <div className="prose">{c.body}</div>
                    </div>
                  ))}
                </div>
              ) : (
                <span className="dim">Nobody has commented.</span>
              )}
            </Panel>

            <Panel title="History">
              {data.history?.length ? (
                <ul className="list">
                  {data.history.map((change) => (
                    <li key={change.id} className="row" style={{ gap: "var(--space-2)" }}>
                      <Icon name="activity" size="sm" />
                      <span>{change.actor ? seatName(change.actor) : "the engine"}</span>
                      <span className="dim">{change.kind.replace(/_/g, " ")}</span>
                      {change.fields &&
                        Object.entries(change.fields).map(([field, delta]) => (
                          <span key={field} className="dim">
                            {field}: {delta.from || "—"} → {delta.to || "—"}
                          </span>
                        ))}
                      <span className="spacer" />
                      <span className="dim" title={fmtDateTime(change.created_at)}>
                        {relTime(change.created_at, now)}
                      </span>
                    </li>
                  ))}
                </ul>
              ) : (
                <span className="dim">Nothing has moved this item yet.</span>
              )}
            </Panel>
          </div>
        )}
      </QueryState>
    </>
  );
}
