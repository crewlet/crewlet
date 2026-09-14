/**
 * One task, whole — as a page, and as the panel the board opens beside itself.
 *
 * # The page and the peek are one definition of what a task shows
 *
 * They differ in chrome and in nothing else. Written twice they would drift,
 * and the drift has a direction: the peek is the one somebody reads forty
 * times a day, so it would be the one that kept its fields while the page
 * quietly fell behind — and a reader who clicked through from the panel to the
 * page to see "the whole thing" would find less than they started with.
 *
 * # Read-only, like the board it opens from
 *
 * A task is moved by a seat's own tools or by an operator through MCP, both
 * attributed to somebody. A button here would write as "the dashboard", which
 * is not a person and cannot be asked why.
 */

import { useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  Assignee,
  Coverage,
  DueMark,
  PriorityMark,
  RowList,
  StatusBadge,
  TypeIcon,
  type RowChrome,
} from "~/components/work.tsx";
import { Badge, Button, Panel, Skeleton, Tabs } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDate, fmtDateTime, fmtCount, fmtDuration, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import {
  describeHistory,
  fieldValueState,
  fieldValueText,
  fmtMinutes,
  typeName,
} from "~/lib/work.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import type {
  WorkComment,
  WorkFieldDef,
  WorkItemDetail,
  WorkLink,
  WorkProjectDetail,
  WorkSummary,
} from "~/protocol/index.ts";

/**
 * What a relation is CALLED from this end.
 *
 * The derived half of a dependency is the one nobody authored — `waiting_on`
 * mirrored onto the blocker — and calling both ends "waiting on" would tell a
 * reader their task is blocked by the very task it is blocking. So the word
 * comes from the pair (kind, derived) rather than from the kind alone.
 */
function linkHeading(link: WorkLink): string {
  switch (link.kind) {
    case "waiting_on":
      return link.derived ? "Blocks" : "Waiting on";
    case "duplicates":
      return link.derived ? "Duplicated by" : "Duplicates";
    case "page":
      return "Pages";
    default:
      return "Linked";
  }
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

export function WorkItem({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const state = useQuery("work_item", { id }, { enabled: id !== "", pollMs: 15_000 });
  const item = state.data?.task;

  // THE PROJECT'S OWN VOCABULARY, because an item carries slugs and ids: a
  // status renders in the word the team uses, a type in the name the company
  // declared, and a custom field's value as the option's NAME rather than the
  // uuid it is stored under.
  // ENABLED ON THE ITEM, not just parameterised by it. `item` is absent until
  // `work_item` answers — every mount, and every poll that finds nothing — and
  // a params object that is `undefined` is an EMPTY one on the wire rather
  // than a skipped question, so without this the engine is asked for a project
  // with no key and refuses it with `bad_params` on the way in.
  const project = useQuery("work_project", item ? { key: item.project } : undefined, {
    enabled: Boolean(item),
    pollMs: 300_000,
  });

  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: project.data?.types,
    statuses: project.data?.statuses,
  };

  return (
    <>
      <PageActions>
        {item ? (
          <>
            <StatusBadge status={item.status} defs={project.data?.statuses} />
            {state.data?.blocked && (
              <Badge tone="critical" outline>
                Blocked
              </Badge>
            )}
            {item.type && <Badge outline>{typeName(item.type, project.data?.types)}</Badge>}
            {item.archived && <Badge outline>Archived</Badge>}
          </>
        ) : undefined}
        {item ? (
          <a className="t-link" href={href(["work"], { project: item.project, item: item.key })}>
            Open on the board →
          </a>
        ) : undefined}
      </PageActions>
      {/* THE TRAIL IS THE PAGE BAR'S. This screen drew its own — workspace,
          project, key — above the title, so a reader had two breadcrumbs on
          one page, disagreeing about where they were the moment either one
          changed. `usePageLabels` below is what gives the real one this
          item's project name and title. */}

      {state.loading && !state.data && <Skeleton rows={8} />}

      <QueryState error={state.error} loading={state.loading}>
        {state.data && item && (
          <>
            <Coverage answer={state.data} />
            <div className="work-item">
              <div className="work-item-main">
                <ItemBody detail={state.data} chrome={chrome} now={now} />
              </div>
              <div className="work-item-side">
                <Panel title="Properties" icon="sliders">
                  <ItemProps detail={state.data} chrome={chrome} project={project.data} />
                </Panel>
                <ItemLinks detail={state.data} chrome={chrome} />
              </div>
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

// ---------------------------------------------------------------------------
// The peek
// ---------------------------------------------------------------------------

/**
 * The same task, beside the board it was opened from.
 *
 * IT KEEPS THE READER WHERE THEY ARE, which is the whole reason it exists: a
 * board is scanned, and sending somebody to a page and back to read one
 * description is the navigation every tracker learned not to make.
 *
 * THE CHROME IS THE FRAME'S — see `app/frame/DetailRail.tsx`. This renders the
 * item's own content and nothing else: the panel, its resize grip, the way out
 * to the page and the close control are the same on every kind of object, and
 * a second copy of them here is how the tracker's peek came to be the only
 * peek in the product.
 */
export function ItemPeek({ itemKey, chrome }: { itemKey: string; chrome: RowChrome }) {
  const now = useNow();
  const state = useQuery("work_item", { id: itemKey }, { enabled: itemKey !== "", pollMs: 15_000 });
  const item = state.data?.task;
  // GUARDED LIKE THE FULL SCREEN'S, and for the same reason: the peek mounts
  // before its item is read, so an unguarded call asks for a project with no
  // key every time somebody opens one.
  const project = useQuery("work_project", item ? { key: item.project } : undefined, {
    enabled: Boolean(item),
    pollMs: 300_000,
  });

  const inner: RowChrome = {
    ...chrome,
    types: project.data?.types ?? chrome.types,
    statuses: project.data?.statuses ?? chrome.statuses,
  };

  return (
    <>
      <div className="row gap-2">
        {item && <TypeIcon type={item.type} types={inner.types} />}
        <a className="mono t-link" href={href(["work", itemKey])}>
          {itemKey}
        </a>
      </div>
      <div className="col gap-3">
        {state.loading && !state.data && <Skeleton rows={6} />}
        <QueryState error={state.error} loading={state.loading}>
          {state.data && item && (
            <>
              <div className="peek-title">{item.title}</div>
              <div className="row wrap gap-2">
                <StatusBadge status={item.status} defs={inner.statuses} />
                {state.data.blocked && (
                  <Badge tone="critical" outline>
                    Blocked
                  </Badge>
                )}
                <PriorityMark priority={item.priority} word />
                <DueMark due={item.due_at} now={now} />
              </div>
              <Coverage answer={state.data} />
              <ItemProps detail={state.data} chrome={inner} project={project.data} compact />
              <ItemLinks detail={state.data} chrome={inner} flush />
              <ItemBody detail={state.data} chrome={inner} now={now} flush />
            </>
          )}
        </QueryState>
      </div>
    </>
  );
}

// ---------------------------------------------------------------------------
// The three shared pieces
// ---------------------------------------------------------------------------

/**
 * The children of a task, and the one rule this panel exists to keep.
 *
 * A FAILED SUBTREE READ IS NOT AN EMPTY ONE. This is the ONLY read that can
 * see a task's children — the detail answer carries links and comments but
 * never the tree below it — so nothing else on the screen contradicts it, and
 * a refusal that renders as no panel says "this task has no subtasks" about a
 * task that may have twenty. It is the same confusion the `scope` parameter
 * produced before it was corrected: the read failed, every child came back
 * absent, and the panel simply did not draw.
 *
 * So the error is drawn WHERE THE ROWS WOULD HAVE BEEN, and only a read that
 * actually answered is allowed to conclude the task is a leaf.
 */
export function Subtasks({
  rows,
  error,
  now,
  chrome,
}: {
  rows: WorkSummary[];
  error?: string | null;
  now: number;
  chrome: RowChrome;
}) {
  if (error) {
    return (
      <Panel title="Subtasks" padding="none">
        <QueryState error={error} loading={false} />
      </Panel>
    );
  }
  if (rows.length === 0) return null;
  return (
    <Panel title="Subtasks" count={rows.length} padding="none">
      <RowList rows={rows} now={now} chrome={chrome} hrefOf={(row) => href(["work", row.key])} />
    </Panel>
  );
}

/** The description, the subtasks, the checklists and the thread. */
export function ItemBody({
  detail,
  chrome,
  now,
  flush,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  now: number;
  /** Inside the peek, where the panels are the page's own chrome already. */
  flush?: boolean;
}) {
  const item = detail.task;
  const [tab, setTab] = useParam("thread", "comments");

  // A SUBTREE IS ITS OWN QUESTION. `parent=` turns the root-only subtask mode
  // off, so this is the one read that can see the children of a task — the
  // detail answer carries links and comments but never the tree below it.
  //
  // THE SERVER'S OWN PARAMETER NAMES, and only those. `scope` is this
  // screen's word for a group of statuses and the engine's grammar has no
  // such key — it REFUSES a parameter it does not read rather than ignoring
  // one, which is right, and the whole read failed. Every child was then an
  // empty result indistinguishable from a task with no subtasks, so the
  // panel never drew at all. Every status is wanted here: a subtask that is
  // done is a subtask, and hiding it makes a finished tree look empty.
  const children = useQuery(
    "work_items",
    item ? { container: `project:${item.project}`, parent: item.id, limit: 100 } : undefined,
    { enabled: Boolean(item), pollMs: 60_000 },
  );
  const subtasks: WorkSummary[] = children.data?.items ?? [];
  const comments = detail.comments ?? [];
  const history = detail.history ?? [];

  const Wrap = flush
    ? ({ title, children: body }: { title: string; children: React.ReactNode }) => (
        <section className="col gap-2">
          <div className="t-label">{title}</div>
          {body}
        </section>
      )
    : ({ title, children: body }: { title: string; children: React.ReactNode }) => (
        <Panel title={title}>{body}</Panel>
      );

  return (
    <>
      <Wrap title="Description">
        {item.body ? (
          <div className="prose">{item.body}</div>
        ) : (
          <span className="muted">No description was written.</span>
        )}
      </Wrap>

      <Subtasks rows={subtasks} error={children.error} now={now} chrome={chrome} />

      {(item.checklists ?? []).map((list) => (
        <Panel key={list.id} title={list.name} icon="check" count={list.items?.length ?? 0}>
          {(list.items ?? []).map((entry) => (
            <div key={entry.id} className={`work-check${entry.done ? " done" : ""}`}>
              <Icon name={entry.done ? "check" : "minus"} size="sm" />
              <span className="work-check-name">{entry.name}</span>
              {entry.promoted_to && (
                <a className="t-link" href={href(["work", entry.promoted_to])}>
                  became a subtask →
                </a>
              )}
              <span className="spacer" />
              {entry.assignee && <Assignee handle={entry.assignee} seatName={chrome.seatName} />}
            </div>
          ))}
        </Panel>
      ))}

      <Panel padding="tight">
        <Tabs
          ariaLabel="Thread or history"
          value={tab}
          onChange={setTab}
          options={[
            { value: "comments", label: "Thread", icon: "message", count: comments.length },
            { value: "history", label: "History", icon: "activity", count: history.length },
          ]}
        />
        {tab === "comments" ? (
          <Thread comments={comments} chrome={chrome} now={now} more={detail.comments_cursor} />
        ) : (
          <History detail={detail} chrome={chrome} now={now} />
        )}
      </Panel>
    </>
  );
}

function Thread({
  comments,
  chrome,
  now,
  more,
}: {
  comments: WorkComment[];
  chrome: RowChrome;
  now: number;
  more?: string;
}) {
  if (comments.length === 0) {
    return <span className="muted">Nobody has commented.</span>;
  }
  return (
    <div className="col gap-3" style={{ paddingTop: "var(--space-3)" }}>
      {more && (
        <span className="t-caption">
          Older comments are on the engine's own thread — this is the newest page of it.
        </span>
      )}
      {comments.map((comment) => (
        <div
          key={comment.id}
          className={`comment${comment.ask ? " work-ask" : ""}`}
          style={comment.reply_to ? { marginLeft: "var(--space-4)" } : undefined}
        >
          <div className="row gap-2 wrap">
            <SeatChip
              name={chrome.seatName?.(comment.author) ?? comment.author}
              handle={comment.author}
            />
            <span className="muted" title={fmtDateTime(comment.created_at)}>
              {relTime(comment.created_at, now)}
            </span>
            {comment.updated_at && <span className="muted">(edited)</span>}
            {/* AN ASK IS THE ONE STATE IN A THREAD THAT IS ABOUT THE READER:
                somebody owes it an answer, and it does not block a close. */}
            {comment.ask && (
              <Badge tone="caution" icon="help">
                asked {chrome.seatName?.(comment.ask) ?? comment.ask}
              </Badge>
            )}
            {comment.answers && (
              <Badge tone="info" icon="arrowRight">
                answers a question
              </Badge>
            )}
            {comment.resolved && <Badge tone="positive">Resolved</Badge>}
          </div>
          {/* A REMOVED COMMENT KEEPS ITS ROW so replies still resolve against
              something — and says so, rather than rendering as an empty one. */}
          <div className="prose">
            {comment.removed ? (
              <span className="faint">(this comment was removed)</span>
            ) : (
              comment.body
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

function History({
  detail,
  chrome,
  now,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  now: number;
}) {
  const history = detail.history ?? [];
  if (history.length === 0) {
    return <span className="muted">Nothing has moved this item yet.</span>;
  }
  return (
    <div className="work-hist" style={{ paddingTop: "var(--space-2)" }}>
      {history.map((entry) => (
        <div key={entry.id} className="work-hist-row">
          <Icon name="activity" size="xs" />
          <span className="truncate">
            <strong>
              {entry.actor ? (chrome.seatName?.(entry.actor) ?? entry.actor) : "the engine"}
            </strong>{" "}
            {describeHistory(entry)}
            {/* A COMMIT THAT ANNOUNCED NOTHING — a fact about the change
                rather than about its importance: a bulk edit is quiet by
                construction. */}
            {entry.quiet && <span className="faint"> (quiet)</span>}
            {entry.turn_id && (
              <>
                {" "}
                <a className="t-link" href={href(["activity", "turns", entry.turn_id])}>
                  turn →
                </a>
              </>
            )}
          </span>
          <span className="work-hist-when" title={fmtDateTime(entry.at)}>
            {relTime(entry.at, now)}
          </span>
        </div>
      ))}
    </div>
  );
}

/**
 * Everything about the task that is not its prose.
 *
 * AN ABSENT VALUE IS AN EM DASH, never a zero: zero is a measurement, and a
 * panel that rendered an unestimated task as "0m" and an unheld one as a blank
 * would be making two claims nobody wrote down.
 */
export function ItemProps({
  detail,
  chrome,
  project,
  compact,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  project?: WorkProjectDetail | null;
  compact?: boolean;
}) {
  const item = detail.task;
  const defs = useMemo(() => {
    const map = new Map<string, WorkFieldDef>();
    for (const group of project?.fields ?? []) {
      for (const field of group.fields) map.set(field.id, field);
    }
    return map;
  }, [project]);
  const seatName = chrome.seatName ?? ((h: string) => h);
  const spend = item.spend;

  const row = (label: string, value: React.ReactNode) => (
    <>
      <dt>{label}</dt>
      <dd>{value}</dd>
    </>
  );
  const dash = <span className="muted">—</span>;

  return (
    <dl className="work-props">
      <div className="work-props-section">State</div>
      {row("Status", <StatusBadge status={item.status} defs={chrome.statuses} />)}
      {row(
        "Priority",
        item.priority && item.priority !== "none" ? (
          <PriorityMark priority={item.priority} word />
        ) : (
          dash
        ),
      )}
      {row(
        "Type",
        item.type ? (
          <span className="row gap-1">
            <TypeIcon type={item.type} types={chrome.types} />
            {typeName(item.type, chrome.types)}
          </span>
        ) : (
          dash
        ),
      )}

      <div className="work-props-section">People</div>
      {row(
        "Assignee",
        item.assignee ? (
          <SeatChip name={seatName(item.assignee)} handle={item.assignee} />
        ) : (
          <span className="muted">Unassigned</span>
        ),
      )}
      {row(
        "Reporter",
        item.reporter ? <SeatChip name={seatName(item.reporter)} handle={item.reporter} /> : dash,
      )}
      {(item.collaborators ?? []).length > 0 &&
        row(
          "Collaborators",
          <span className="row wrap gap-1">
            {(item.collaborators ?? []).map((handle) => (
              <SeatChip key={handle} name={seatName(handle)} handle={handle} />
            ))}
          </span>,
        )}
      {(item.watchers ?? []).length > 0 &&
        row(
          "Watching",
          <span className="row wrap gap-1">
            {(item.watchers ?? []).map((handle) => (
              <span key={handle} className="row gap-1">
                <SeatChip name={seatName(handle)} handle={handle} />
                {/* MUTED IS NOT THE SAME AS NOT WATCHING, and both travel:
                    a set that carried only the difference would silently
                    re-add everybody on the next mention. */}
                {(item.muted ?? []).includes(handle) && <span className="faint">(muted)</span>}
              </span>
            ))}
          </span>,
        )}

      <div className="work-props-section">Plan</div>
      {row(
        "Sprint",
        item.sprint !== undefined ? (
          <a className="t-link" href={href(["work", item.project, "sprints"])}>
            Sprint {item.sprint}
          </a>
        ) : (
          <span className="muted">Backlog</span>
        ),
      )}
      {row("Start", item.start_at ? fmtDate(item.start_at) : dash)}
      {row(
        "Due",
        item.due_at ? (
          <span className="row gap-1">
            {fmtDate(item.due_at)}
            {item.due_all_day && <span className="faint">all day</span>}
          </span>
        ) : (
          dash
        ),
      )}
      {row("Estimate", item.estimate_minutes ? fmtMinutes(item.estimate_minutes) : dash)}
      {row("Points", item.points ? item.points : dash)}
      {(item.tags ?? []).length > 0 &&
        row(
          "Tags",
          <span className="row wrap gap-1">
            {(item.tags ?? []).map((slug) => (
              <Badge key={slug} outline>
                {project?.tags?.find((t) => t.slug === slug)?.label ?? slug}
              </Badge>
            ))}
          </span>,
        )}

      {(detail.fields ?? []).length > 0 && (
        <>
          <div className="work-props-section">Fields</div>
          {(detail.fields ?? []).map((field) => {
            const state = fieldValueState(field);
            return (
              <div key={field.id} style={{ display: "contents" }}>
                <dt>{field.name || field.slug || field.id}</dt>
                <dd>
                  {fieldValueText(field, defs, seatName)}
                  {state && <span className="work-field-state">{state}</span>}
                </dd>
              </div>
            );
          })}
        </>
      )}

      {!compact && (
        <>
          <div className="work-props-section">Cost</div>
          {/* THE REASSIGNMENT COUNT IS A BUDGET, not trivia: an item handed on
              too many times has stopped being work and started being a hot
              potato, and the engine refuses the next hand-off rather than
              letting it circle. */}
          {row(
            "Hand-offs",
            <span className="row gap-1">
              {item.reassignments ?? 0}
              {(item.reassignments ?? 0) >= 6 && <Icon name="alert" size="xs" />}
            </span>,
          )}
          {row("Turns", spend?.turns ? spend.turns : dash)}
          {row("Tokens", spend?.tokens ? fmtCount(spend.tokens) : dash)}
          {row("Model time", spend?.wall_ms ? fmtDuration(spend.wall_ms) : dash)}

          <div className="work-props-section">Record</div>
          {row("Created", item.created_at ? fmtDateTime(item.created_at) : dash)}
          {row("Updated", item.updated_at ? fmtDateTime(item.updated_at) : dash)}
          {row(
            "In status since",
            item.status_entered_at ? fmtDateTime(item.status_entered_at) : dash,
          )}
          {item.done_at ? row("Delivered", fmtDateTime(item.done_at)) : null}
          {row(
            "Filed into",
            item.filed_unit ? (
              <span className="truncate">{item.filed_unit}</span>
            ) : (
              <span className="muted">no unit</span>
            ),
          )}
          {/* ROUTING IS THE MUTABLE HALF — whose lead hears about this now —
              and it is shown only where it has MOVED, because the pair being
              equal is the ordinary case and repeating it is noise. */}
          {item.routing_unit && item.routing_unit !== item.filed_unit
            ? row("Routes to", item.routing_unit)
            : null}
          {(item.former_keys ?? []).length > 0
            ? row(
                "Former keys",
                <span className="mono">{(item.former_keys ?? []).join(", ")}</span>,
              )
            : null}
        </>
      )}
    </dl>
  );
}

/**
 * What this task is attached to, grouped by what the relation MEANS.
 *
 * The derived half of a dependency is the one nobody authored, so it is headed
 * "Blocks" rather than "Waiting on" — an editor knows which end to change, and
 * a reader is not told their task is blocked by the task it is blocking.
 */
export function ItemLinks({
  detail,
  chrome,
  flush,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  flush?: boolean;
}) {
  const links = detail.links ?? [];
  if (links.length === 0) return null;
  const groups = new Map<string, WorkLink[]>();
  for (const link of links) {
    const heading = linkHeading(link);
    const held = groups.get(heading);
    if (held) held.push(link);
    else groups.set(heading, [link]);
  }

  const body = (
    <div className="col gap-3">
      {[...groups].map(([heading, rows]) => (
        <div key={heading} className="col gap-1">
          <div className="t-label">{heading}</div>
          {rows.map((link) => (
            <div key={`${link.kind}:${link.other}`} className="row gap-2">
              <a
                className="mono t-link"
                href={href([link.kind === "page" ? "pages" : "work", link.key || link.other])}
              >
                {link.key || link.other}
              </a>
              <span className="truncate">{link.title}</span>
              {link.status && <StatusBadge status={link.status} defs={chrome.statuses} />}
              {/* THE REPAIR AND THE FINDING ARE DIFFERENT STATES: the first is
                  a commit the tracker duty writes 30 seconds later, the second
                  is an edge whose mirror was refused for good. */}
              {link.one_sided && !link.one_sided_final && (
                <Badge tone="info" title="The duty writes the other half shortly">
                  mirror pending
                </Badge>
              )}
              {link.one_sided_final && (
                <Badge tone="caution" title="The other end is gone, removed, or full">
                  one-sided
                </Badge>
              )}
            </div>
          ))}
        </div>
      ))}
    </div>
  );

  if (flush) {
    return (
      <section className="col gap-2">
        <div className="t-label">Links</div>
        {body}
      </section>
    );
  }
  return (
    <Panel title="Links" icon="link" count={links.length}>
      {body}
    </Panel>
  );
}

export { linkHeading };
