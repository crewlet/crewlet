/**
 * The knowledge base — the company's own pages.
 *
 * # Browsing and searching are different questions
 *
 * This screen BROWSES: a container, a tree, a page and its history. The
 * Knowledge screen SEARCHES, and ranks. Folding them together would make the
 * common case — "show me what the platform team has written down" — a search
 * for a word somebody has to guess.
 *
 * # A company on Confluence has none of this
 *
 * The `pages` question is registered only where this node runs the native
 * knowledge base. On Confluence there is no local copy to browse, by design:
 * search there is live at query time and there is no index to walk.
 *
 * # Read-only, for the reason the tracker is
 *
 * A page is written by a seat's own tools or by an operator through MCP, both
 * attributed to somebody. A dashboard button would write as "the dashboard",
 * which is nobody and cannot be asked why.
 */

import { useMemo } from "react";
import { renderMarkdown } from "~/lib/markdown.ts";
import { collapse, diffLines, diffStat, type DiffSection } from "~/lib/diff.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  Badge,
  Empty,
  Panel,
  SearchInput,
  Segmented,
  Select,
  Skeleton,
  cx,
} from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import type { PageRevision } from "~/protocol/index.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

const STATUS_TONE: Record<string, "positive" | "caution" | "critical" | "info" | "neutral"> = {
  published: "positive",
  draft: "caution",
  trashed: "neutral",
};

export function Pages({ container: fromPath }: { container?: string }) {
  const org = useOrg();
  const nav = useNavigator();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();

  // THE CONTAINER IS THE PATH (`#/knowledge/ENG`), because a container is an
  // object — it has an owning unit, a page tree and a purpose — and a filter
  // key made it unlinkable. Choosing one NAVIGATES rather than filtering.
  const container = fromPath ?? "";
  const setContainer = (key: string) => nav.to(key ? ["knowledge", key] : ["knowledge"]);
  const [title, setTitle] = useParam("title", "");
  // THREE STATES on the wire and three here: only the tool-skill pages
  // (auditing the catalogue), everything but them (an ordinary browse), and
  // everything. A checkbox would make one of the three unreachable.
  const [kind, setKind] = useParam("kind", "prose");

  const containers = useQuery("containers", undefined, { pollMs: 60_000 });

  const params: Record<string, unknown> = {};
  if (container) params.container = container;
  if (title) params.title = title;
  if (kind === "skills") params.skills = true;
  if (kind === "prose") params.skills = false;

  const { data, loading, error } = useQuery("pages", params, { pollMs: 20_000 });

  const rows = useMemo(
    () => [...(data?.pages ?? [])].sort((a, b) => tsKey(b.updated_at) - tsKey(a.updated_at)),
    [data],
  );
  const containerKeys = useMemo(
    () => (containers.data?.containers ?? []).map((c) => c.key).sort(),
    [containers.data],
  );
  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;

  return (
    <>
      <PageNote>
        The company's own knowledge base, browsed. To find pages about a subject rather than in a
        place, search from the Knowledge screen — it ranks.
      </PageNote>

      <div className="toolbar">
        <div style={{ flex: 1, maxWidth: 360 }}>
          <SearchInput
            value={title}
            onChange={setTitle}
            ariaLabel="Find a page by title"
            placeholder="Words from the title"
          />
        </div>
        <Select
          value={container}
          onChange={setContainer}
          ariaLabel="Container"
          anyLabel="Every container"
          options={containerKeys}
        />
        <Segmented
          value={kind}
          onChange={setKind}
          ariaLabel="Pages or tool skills"
          options={[
            { value: "prose", label: "Pages", title: "Everything but the tool-skill pages" },
            { value: "skills", label: "Tool skills", title: "The machinery a phase is offered" },
            { value: "", label: "All" },
          ]}
        />
      </div>

      {containers.data?.containers?.length ? (
        <div className="row wrap" style={{ gap: "var(--space-2)", marginBottom: "var(--space-3)" }}>
          {containers.data.containers.map((c) => (
            <Badge
              key={c.key}
              outline={container !== c.key}
              tone={container === c.key ? "info" : "neutral"}
              title={c.purpose || c.name || c.key}
              onClick={() => setContainer(container === c.key ? "" : c.key)}
              pressed={container === c.key}
            >
              {c.key}
            </Badge>
          ))}
        </div>
      ) : null}

      {loading && <Skeleton rows={6} />}

      <QueryState
        error={error}
        loading={loading}
        // ONE EMPTY STATE. There used to be two, and on a company with no
        // pages at all they rendered TOGETHER: `QueryState` fired on
        // `rows.length === 0` under one heading and a trailing `Empty` fired
        // on no containers under another, so the screen said "No pages here"
        // and "Nothing has been written down yet" one above the other, in two
        // different chromes. They are two facts, so this is one component
        // telling them apart rather than two components each telling one.
        empty={
          rows.length
            ? undefined
            : containerKeys.length === 0
              ? {
                  title: "Nothing has been written down yet",
                  hint: "A container is created the first time somebody writes into it. Give a unit a `space` and its seats will have somewhere to file what they learn.",
                }
              : {
                  title: "No pages here",
                  hint: "Nothing in this node's copy of the knowledge base matches. Seats write pages with write_page, and a page's container comes from the unit's `space` field.",
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
                key: "title",
                header: "Title",
                sortValue: (r) => r.title,
                cell: (r) => (
                  <a href={href(["knowledge", r.container, r.title])} className="truncate">
                    {r.title}
                  </a>
                ),
              },
              {
                key: "container",
                header: "Container",
                shrink: true,
                sortValue: (r) => r.container,
                cell: (r) => (
                  <Badge outline mono>
                    {r.container}
                  </Badge>
                ),
              },
              {
                key: "kind",
                header: "Kind",
                shrink: true,
                sortValue: (r) => (r.skill ? "skill" : r.onboarding ? "onboarding" : "page"),
                cell: (r) =>
                  r.skill ? (
                    // A TOOL SKILL IS MACHINERY, marked so a reader does not
                    // take it for guidance somebody wrote to be read: it is
                    // documentation the engine injects into a phase.
                    <Badge tone="info" title="Injected into a phase by the tool-skill registry">
                      tool skill
                    </Badge>
                  ) : r.onboarding ? (
                    <Badge tone="caution" title="Where a new seat's reading starts">
                      onboarding
                    </Badge>
                  ) : (
                    <span className="muted">page</span>
                  ),
              },
              {
                key: "status",
                header: "Status",
                shrink: true,
                sortValue: (r) => r.status,
                cell: (r) => (
                  <Badge tone={STATUS_TONE[r.status] ?? "neutral"} dot>
                    {r.status}
                  </Badge>
                ),
              },
              {
                key: "version",
                header: "Version",
                shrink: true,
                align: "right",
                sortValue: (r) => r.version,
                cell: (r) => <span className="mono">v{r.version}</span>,
              },
              {
                key: "author",
                header: "Author",
                shrink: true,
                sortValue: (r) => r.author ?? "",
                cell: (r) =>
                  r.author ? (
                    <SeatChip name={seatName(r.author)} handle={r.author} />
                  ) : (
                    <span className="muted">—</span>
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
    </>
  );
}

/** One page: its body, where it sits, and everything that changed it. */
/**
 * One page.
 *
 * ADDRESSED BY CONTAINER AND TITLE, which is what a person was given: the
 * engine's own `Get` takes `CONTAINER/Title` and matches the title the way the
 * fleet CLAIMED it — case-insensitively, with runs of whitespace collapsed —
 * so `ENG/deploy runbook` reaches a page called "Deploy  Runbook". A uuid
 * still resolves, because every internal link carries one.
 */
export function PageView({ container, title }: { container: string; title: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const id = `${container}/${title}`;
  const { data, loading, error } = useQuery(
    "page",
    { id },
    { enabled: id !== "/", pollMs: 20_000 },
  );
  usePageLabels(data?.page ? { [container]: container, [title]: data.page.title } : {});

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const page = data?.page;

  return (
    <>
      <PageActions>
        {page ? (
          <>
            <Badge tone={STATUS_TONE[page.status] ?? "neutral"} dot>
              {page.status}
            </Badge>
            <Badge outline mono>
              v{page.version}
            </Badge>
            {page.skill && <Badge tone="info">tool skill</Badge>}
          </>
        ) : undefined}
      </PageActions>
      <PageNote>
        {page ? (
          <span className="row wrap" style={{ gap: "var(--space-1)" }}>
            {/* THE BREADCRUMB IS THE ANCESTOR CHAIN, outermost first — a
                  page's place is what makes it findable, and a title alone
                  says nothing about which team's tree it is in. */}
            <a href={href(["knowledge", page.container])}>{page.container}</a>
            {(data.ancestors ?? []).map((a) => (
              <span key={a.id}>
                {" / "}
                <a href={href(["knowledge", page.container, a.title])}>{a.title}</a>
              </span>
            ))}
          </span>
        ) : undefined}
      </PageNote>

      {loading && <Skeleton rows={8} />}

      <QueryState error={error} loading={loading}>
        {page && (
          <>
            <Panel>
              {page.body ? (
                <div className="prose md">{renderMarkdown(page.body)}</div>
              ) : (
                <span className="muted">This page has no body.</span>
              )}
            </Panel>

            {data.children?.length ? (
              <Panel title={`Children (${data.children.length})`}>
                <ul className="list">
                  {data.children.map((child) => (
                    <li key={child.id}>
                      <a href={href(["knowledge", page.container, child.title])}>{child.title}</a>
                    </li>
                  ))}
                </ul>
              </Panel>
            ) : null}

            {page.watchers?.length ? (
              <Panel title="Watching">
                <div className="row wrap" style={{ gap: "var(--space-2)" }}>
                  {page.watchers.map((w) => (
                    <SeatChip key={w} name={seatName(w)} handle={w} />
                  ))}
                </div>
              </Panel>
            ) : null}

            <Panel title={`Comments (${data.comments?.length ?? 0})`}>
              {data.comments?.length ? (
                <div className="col gap-3">
                  {data.comments.map((c) => (
                    <div key={c.id} className="comment">
                      <div className="row" style={{ gap: "var(--space-2)" }}>
                        <SeatChip name={seatName(c.author)} handle={c.author} />
                        <span className="muted" title={fmtDateTime(c.created_at)}>
                          {relTime(c.created_at, now)}
                        </span>
                        {c.edited_at && <span className="muted">(edited)</span>}
                      </div>
                      <div className="prose md">{renderMarkdown(c.body)}</div>
                    </div>
                  ))}
                </div>
              ) : (
                <span className="muted">Nobody has commented.</span>
              )}
            </Panel>

            <PageHistory
              pageID={page.id}
              history={data.history ?? []}
              seatName={seatName}
              now={now}
            />

            <PageChanges pageID={page.id} seatName={seatName} now={now} />
          </>
        )}
      </QueryState>
    </>
  );
}

/**
 * A page's saved versions, and the body of whichever one is open.
 *
 * # A list of version numbers is not a history
 *
 * The detail answer carries revision SUMMARIES — a version, an author, a
 * message, an instant — which says a page was edited eleven times and not what
 * any of those edits did. This panel used to render exactly that and tell the
 * reader why they could not click one: "past versions are kept as metadata
 * here; reading one back is a coordination read the engine does on demand."
 * There was no such read. The bodies sat in `pages_revisions` reachable only
 * by reading the page at its head.
 *
 * # An old version is an ordinary absence
 *
 * A page keeps a bounded number of revisions, so asking for one the node no
 * longer holds is not a failure — and the panel says which of the two happened
 * rather than rendering a blank.
 */
/**
 * A line diff, rendered as the document it is.
 *
 * MONOSPACE AND LINE-NUMBERED on both sides, because the two numbers are what
 * a reader uses to find the paragraph in the version beside it. A skipped run
 * is a row of its own saying how many lines it stands for: a gap silently
 * closed makes a document edited at both ends look like one rewritten in the
 * middle.
 */
function DiffPane({ sections }: { sections: DiffSection[] }) {
  return (
    <div className="diff">
      {sections.map((section, s) => (
        <div key={s} className="diff-section">
          {section.skipped > 0 && (
            <div className="diff-skip">{plural(section.skipped, "unchanged line")}</div>
          )}
          {section.lines.map((line, i) => (
            // THE CLASS NAMES ARE LITERALS, never assembled from the value:
            // a stylesheet gate that cannot see a class cannot tell a rule
            // this file relies on from one nothing uses.
            <div
              key={i}
              className={cx(
                "diff-line",
                line.kind === "add" && "is-add",
                line.kind === "remove" && "is-remove",
              )}
            >
              <span className="diff-no">{line.before ?? ""}</span>
              <span className="diff-no">{line.after ?? ""}</span>
              <span className="diff-mark">
                {line.kind === "add" ? "+" : line.kind === "remove" ? "−" : " "}
              </span>
              <span className="diff-text">{line.text || " "}</span>
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

function PageHistory({
  pageID,
  history,
  seatName,
  now,
}: {
  pageID: string;
  history: PageRevision[];
  seatName: (handle: string) => string;
  now: number;
}) {
  // WHICH VERSION IS OPEN, as a FILTER: stepping through a page's versions
  // must not fill the back stack with every one the reader glanced at.
  const [open, setOpen] = useParam("version", "", "filter");
  const version = Number(open) || 0;
  const body = useQuery("page_revision", { page: pageID, version }, { enabled: version > 0 });
  // WHAT THIS SAVE CHANGED, which is the question somebody opens a history
  // for and which the panel could not answer: it showed any ONE version, so
  // the answer was to open two and read both.
  //
  // The PREVIOUS version by position in the list, not `version - 1`: a page
  // keeps a bounded number of revisions, so the one below this in the history
  // is the one that was actually saved before it — and off a trimmed page
  // `version - 1` is a read that comes back not found.
  const previous = useMemo(() => {
    const i = history.findIndex((rev) => rev.version === version);
    return i >= 0 ? (history[i + 1]?.version ?? 0) : 0;
  }, [history, version]);
  const [lens, setLens] = useParam("lens", "diff", "filter");
  const prior = useQuery(
    "page_revision",
    { page: pageID, version: previous },
    { enabled: version > 0 && previous > 0 && lens === "diff" },
  );
  const diff = useMemo(
    () =>
      body.data && prior.data
        ? collapse(diffLines(prior.data.body ?? "", body.data.body ?? ""))
        : [],
    [body.data, prior.data],
  );
  const stat = useMemo(() => diffStat(diff.flatMap((section) => section.lines)), [diff]);

  return (
    <Panel title={`History (${history.length})`} icon="clock">
      {history.length ? (
        <div className="list">
          {history.map((rev) => (
            <button
              key={rev.version}
              type="button"
              className={`thread-entry as-row${rev.version === version ? " selected" : ""}`}
              onClick={() => setOpen(rev.version === version ? "" : String(rev.version))}
            >
              <span className="row gap-2">
                <Icon name="file" size="sm" />
                <span className="mono">v{rev.version}</span>
                <span>{rev.author ? seatName(rev.author) : "the engine"}</span>
                {rev.message && <span className="muted truncate">{rev.message}</span>}
                <span className="spacer" />
                <span className="muted" title={fmtDateTime(rev.created_at)}>
                  {relTime(rev.created_at, now)}
                </span>
              </span>
            </button>
          ))}
        </div>
      ) : (
        <span className="muted">Only this version exists — nobody has saved over it.</span>
      )}

      {version > 0 && (
        <div style={{ marginTop: "var(--space-3)" }}>
          <QueryState error={body.error} loading={body.loading}>
            {body.data ? (
              <>
                <div className="row wrap gap-2">
                  <span className="t-caption faint">
                    Version {body.data.version}
                    {body.data.title ? ` — “${body.data.title}”` : ""}, as it was saved.
                  </span>
                  <span className="spacer" />
                  {/* THE FIRST VERSION HAS NOTHING TO COMPARE WITH, which is
                      a fact about the page rather than a lens the reader
                      failed to pick — so the control is absent rather than
                      offering a diff that can only say "everything". */}
                  {previous > 0 && (
                    <Segmented
                      ariaLabel="What to show"
                      value={lens}
                      onChange={setLens}
                      options={[
                        { value: "diff", label: `Changes from v${previous}` },
                        { value: "full", label: "The whole version" },
                      ]}
                    />
                  )}
                </div>
                {lens === "diff" && previous > 0 ? (
                  <QueryState error={prior.error} loading={prior.loading}>
                    {prior.data &&
                      (stat.identical ? (
                        // IDENTICAL IS ITS OWN ANSWER. A save that changed
                        // only the title leaves the body untouched, and a
                        // pane of unmarked lines reads as one that failed
                        // to load.
                        <Empty
                          inline
                          icon="check"
                          title="This save did not change the body"
                          hint="A page's title, its labels and its place in the tree are saved beside its body — this version's prose is the one before it."
                        />
                      ) : (
                        <>
                          <p className="t-caption">
                            <span className="diff-add-ink">+{stat.added}</span>{" "}
                            <span className="diff-del-ink">−{stat.removed}</span> against v
                            {previous}
                          </p>
                          <DiffPane sections={diff} />
                        </>
                      ))}
                  </QueryState>
                ) : (
                  <div className="prose md">
                    {body.data.body ? (
                      renderMarkdown(body.data.body)
                    ) : (
                      <span className="muted">This version had no body.</span>
                    )}
                  </div>
                )}
              </>
            ) : (
              // NOT FOUND IS NOT A FAILURE. A page keeps a bounded number
              // of revisions, so an older one is an ordinary absence — and
              // saying which of the two happened is the whole point.
              !body.loading && (
                <Empty
                  inline
                  icon="clock"
                  title="This node no longer holds that version"
                  hint="A page keeps a bounded number of revisions. The entry above is the record that it existed."
                />
              )
            )}
          </QueryState>
        </div>
      )}
    </Panel>
  );
}

/**
 * Everything that happened to this page, which is not the same as its saves.
 *
 * `pages_history` has one row per change since the domain landed — ten change
 * kinds, who made it, whether it announced anything, and the TURN that made it
 * — and the schema ships an index literally named "one page's activity". Until
 * now nothing read a single row of it: a comment, a rename, a move, a label
 * edit and a status change all happened and left no trace any screen could
 * show. Only saves appeared, through the revision list.
 *
 * THE TURN IS WHAT A WIKI CANNOT HAVE. An edit made by a seat carries the turn
 * that made it, so "why did this page change" is one click rather than a
 * search of the event log.
 */
function PageChanges({
  pageID,
  seatName,
  now,
}: {
  pageID: string;
  seatName: (handle: string) => string;
  now: number;
}) {
  const feed = useQuery("page_activity", { page: pageID }, { pollMs: 60_000 });
  const changes = feed.data?.changes ?? [];
  return (
    <Panel title={`Activity (${changes.length})`} icon="activity">
      <QueryState
        error={feed.error}
        loading={feed.loading}
        empty={
          changes.length
            ? undefined
            : {
                title: "Nothing has happened to this page",
                hint: "Every change writes an entry — a save, a comment, a rename, a move, a label. A page with none was created and left alone.",
              }
        }
      >
        <div className="list">
          {changes.map((change) => (
            <div key={change.id} className="thread-entry">
              <span className="row gap-2">
                <Badge outline>{change.kind}</Badge>
                <span>{change.actor ? seatName(change.actor) : "the engine"}</span>
                {change.quiet && (
                  <span className="faint t-caption" title="this change announced nothing">
                    quiet
                  </span>
                )}
                <span className="spacer" />
                {change.turn_id && (
                  <a
                    className="t-link t-caption"
                    href={href(["activity", "turns", change.turn_id])}
                  >
                    turn →
                  </a>
                )}
                <span className="muted" title={fmtDateTime(change.at)}>
                  {relTime(change.at, now)}
                </span>
              </span>
              {change.excerpt && <p className="t-caption faint">{change.excerpt}</p>}
            </div>
          ))}
        </div>
      </QueryState>
    </Panel>
  );
}
