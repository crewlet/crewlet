/**
 * The pieces more than one screen draws.
 *
 * Each of these existed two or three times in the dashboard this replaces, and
 * the copies had drifted: two different seat-tone maps, two budget bars at
 * different heights with different colours (one of them winning globally
 * because its room stylesheet loaded last), and one activity row renderer with
 * zero callers beside another with all of them.
 */

import { useEffect, useMemo, useRef, type ReactNode } from "react";
import { href, useParam } from "~/app/router.tsx";
import { fmtDateTime, fmtTime, humanize } from "~/lib/format.ts";
import { requestToken } from "~/protocol/index.ts";
import {
  runState,
  seatPath,
  seatTone,
  stateLabel,
  statusLine,
  toneOf,
  type Seat,
} from "~/lib/seats.ts";
import type { AgentRow, FeedRow, SandboxEntry } from "~/protocol/index.ts";
import type { Attention } from "~/lib/attention.ts";
import {
  ALL_ITEMS,
  Avatar,
  Button,
  Callout,
  Card,
  cx,
  DataTable,
  EmptyState,
  EmptyValue,
  formatRelative,
  InlineCode,
  RelativeTime,
  Stack,
  TableFooter,
  tableColumns,
  Tag,
  useNow,
  VisuallyHidden,
} from "@crewlethq/ui";
import type {
  DataTableItemsPerPage,
  DataTableProps,
  DataTableSortState,
  DataViewColumn,
  TableFooterLabels,
} from "@crewlethq/ui";
import {
  DatabaseGlyph,
  ErrorGlyph,
  InboxGlyph,
  InfoGlyph,
  KeyGlyph,
  ScheduleGlyph,
} from "@crewlethq/icons/glyphs";

/**
 * How tall a record block grows before it scrolls itself, in px.
 *
 * Twenty-five lines of the mono face at the size a block is set in, which is
 * about as much of a record as a reader takes in before scrolling anyway.
 * uilet's CodeBlock is unbounded without a ceiling, and the records these hold
 * are a whole turn, a whole configuration and a whole event payload: one of
 * them at nine hundred lines pushes everything under it off the screen.
 */
export const RECORD_MAX_HEIGHT = 460;

/**
 * The page sizes every table in this dashboard offers.
 *
 * The design system's own row, said here because the two defaults below are
 * chosen against it: a size a screen opens on has to be one of these, or the
 * chip row in the settings frame opens with nothing marked. The All chip the
 * frame draws beside them is the component's, and it is a WORD rather than a
 * count, so a table that gains a row does not quietly start paging.
 */
export const TABLE_PAGE_SIZES = [5, 10, 20, 50, 100];

/**
 * How many rows a table opens with, and why the two numbers differ.
 *
 * A PANEL table sits in a card beside other cards, and ten rows is as much as
 * one can take before it pushes its neighbours off the screen. A LIST screen
 * owns the whole page, where ten rows is a pager a reader works rather than
 * reads: twenty is about one screenful at the compact density, which is what
 * somebody scanning an event log wants before they reach for the chevrons.
 */
export const PANEL_PAGE_SIZE = 10;
export const LIST_PAGE_SIZE = 20;

/**
 * What a list screen's footer says once its table pages.
 *
 * The design system's own sentence is "Showing 12 of 80", and its first number
 * is how many rows the VIEW holds. A list screen hands the view everything the
 * filters kept and the table slices that into pages, so under paging the
 * sentence would claim eighty rows were on screen when twenty are drawn. The
 * count is still worth saying, because what it actually counts is the filter,
 * so this says that instead and leaves which rows are in front of the reader
 * to the pager, which announces its own page and range.
 *
 * Nothing is overridden for the unfiltered case: the footer already says
 * "80 rows" when every row matched, which is a fact about the list rather than
 * about the page and is true whether the table pages or not.
 */
export const MATCH_FOOTER_LABELS: Partial<TableFooterLabels> = {
  showing: (visible, total) => (
    <>
      {visible} of {total} match
    </>
  ),
};

/**
 * Where a table's choices live: the URL for what a reader would send someone,
 * this browser's storage for what only this browser knows.
 *
 * THE SPLIT, AND WHY IT FALLS HERE. Five of a table's choices are facts about
 * WHAT IS ON SCREEN: which page, how many rows it holds, which columns are
 * shown, in what order, and how they are sorted. Every one of them is part of
 * the answer a reader would send a colleague, so every one is a URL parameter
 * and the link carries it. The other two are facts about the BROWSER somebody
 * is reading in: whether cells wrap at this window width, and how wide each
 * column was dragged. A link cannot carry a pixel width that means anything on
 * another screen, so those two are kept under `storageKey` and only those two.
 * The design system keeps the same rule from its side: a controlled choice is
 * never read from storage and never written to it, so the two can never
 * disagree on the way back in.
 *
 * THE PARAMETERS ARE PREFIXED BY THE TABLE where a screen draws more than one,
 * because Fleet draws three and Seat draws three: `seats.page` is the seat
 * placement table's page and `duties.page` is the duty table's. A screen with
 * one table leaves the prefix off, so the event log's is plainly `?page=4`.
 *
 * THEY REPLACE RATHER THAN PUSH (`useParam`'s filter kind). A page, a size and
 * a column list are positions within the screen a reader is already on, not
 * screens they called for; and the table itself moves the page back to one
 * whenever a sort or a filter changes, which as a pushed entry would make the
 * Back button walk a maze nobody asked for. Back still leaves the screen with
 * the reader's choices intact in the entry it returns to.
 *
 * THE STORAGE KEY NAMES THE SCREEN AND THE TABLE, never the component: two
 * tables on one route would otherwise share one set of column widths, and the
 * wider of them would keep re-teaching the narrower its own.
 */
export interface TableChoicesOptions<T> {
  /** The route this table is on. It names the storage entry, never a parameter. */
  screen: string;
  /** Which table on that route. The screen's only table leaves it out. */
  table?: string;
  /** The columns as declared, which is what a hidden or reordered set is read against. */
  columns: readonly DataViewColumn<T>[];
  /**
   * How many rows the table opens with, before a reader picks a size.
   *
   * A list screen leaves it out and takes [LIST_PAGE_SIZE]; the panel default
   * is said once, by [RecordTable], rather than at each of its call sites.
   */
  defaultItemsPerPage?: number;
  /** Which column it opens ordered by. Absent from the URL means this one. */
  defaultSort?: DataTableSortState | null;
  /**
   * A value that changes whenever the SCREEN's own filters change, which puts
   * the reader back on page one.
   *
   * The design system does this for the filters and the sort it owns, and the
   * list view does not hand a screen's own filter values down to it, so the
   * one case it cannot see is the one a screen has to say. A reader who
   * narrows a log to six rows while on page four is otherwise shown the empty
   * end of a list that no longer has four pages.
   */
  filterKey?: string;
}

/** Every choice a table holds, as the props that hold it. */
export interface TableChoices {
  page: number;
  onPageChange: (next: number) => void;
  itemsPerPage: DataTableItemsPerPage;
  onItemsPerPageChange: (next: DataTableItemsPerPage) => void;
  /**
   * How many rows the screen opens with, handed to the table as well.
   *
   * Same reason as [TableChoices.defaultSort] below: the size is controlled,
   * so this seeds nothing, and what it answers is Reset to Default. Without
   * it the frame resets to the design system's own ten, so a list screen that
   * opens at twenty came back from a reset on a size it never chose and its
   * link then carried `per=10`.
   */
  defaultItemsPerPage: number;
  itemsPerPageOptions: number[];
  visibleColumns: Record<string, boolean>;
  onVisibleColumnsChange: (next: Record<string, boolean>) => void;
  columnOrder: string[];
  onColumnOrderChange: (next: string[]) => void;
  sort: DataTableSortState | null;
  onSortChange: (next: DataTableSortState | null) => void;
  /**
   * The order the screen opens in, handed to the table as well as read here.
   *
   * The sort is controlled, so this seeds nothing: what it answers is the
   * settings frame's Reset to Default, which has no default to put back
   * without it and leaves the table ordered by nothing at all. Passing it
   * also keeps the URL honest, because the parameter's own fallback is this
   * value, so a reset lands back on a clean link rather than on `sort=none`.
   */
  defaultSort: DataTableSortState | null;
  storageKey: string;
  paginated: true;
  resizable: true;
  showSettings: true;
  settingsVariant: "rich";
}

/** A sort as a URL parameter. `none` is a reader who sorted by nothing. */
function encodeSort(sort: DataTableSortState | null | undefined): string {
  return sort?.key && sort.direction ? `${sort.key}:${sort.direction}` : "none";
}

/**
 * A sort read back out of a URL, which is the least trusted place there is.
 *
 * A key no column declares is not a sort the table could apply, so it answers
 * the same as `none` rather than leaving the table ordered by a column that is
 * not there.
 */
function decodeSort(value: string, keys: readonly string[]): DataTableSortState | null {
  const [key, direction] = value.split(":");
  if (!key || !keys.includes(key)) return null;
  return { key, direction: direction === "desc" ? "desc" : "asc" };
}

/** A comma-separated list of column keys, unknown names dropped. */
function decodeKeys(value: string, keys: readonly string[]): string[] {
  return value
    .split(",")
    .map((key) => key.trim())
    .filter((key) => keys.includes(key));
}

/** A page size read back out of a URL. Anything else is the screen's own. */
function decodeItemsPerPage(value: string, fallback: number): DataTableItemsPerPage {
  if (value === ALL_ITEMS) return ALL_ITEMS;
  const count = Number.parseInt(value, 10);
  return Number.isFinite(count) && count > 0 ? count : fallback;
}

export function useTableChoices<T>({
  screen,
  table = "",
  columns,
  defaultItemsPerPage = LIST_PAGE_SIZE,
  defaultSort = null,
  filterKey,
}: TableChoicesOptions<T>): TableChoices {
  const prefix = table ? `${table}.` : "";
  // The declared keys, in the declared order, and only the ones the table will
  // hold: a column with no key is one it drops, so a hidden or reordered set
  // naming one would be naming a column nobody can see.
  //
  // Held as ONE STRING first. A screen whose cells render an elapsed time
  // rebuilds its column array on every tick, so nothing below may be keyed on
  // WHICH array arrived; what it says is the same string a second later.
  const declaredOrder = columns
    .map((column) => column.key)
    .filter(Boolean)
    .join(",");
  const defaultHidden = columns
    .filter((column) => column.key && column.defaultVisible === false)
    .map((column) => column.key)
    .join(",");
  const declared = useMemo(
    () => (declaredOrder === "" ? [] : declaredOrder.split(",")),
    [declaredOrder],
  );
  const defaultSortParam = encodeSort(defaultSort);

  const [pageParam, setPageParam] = useParam(`${prefix}page`, "1");
  // Each parameter's fallback is the value this table opens on, so a reader
  // who picks it back is a reader with a clean URL again.
  const [perParam, setPerParam] = useParam(`${prefix}per`, String(defaultItemsPerPage));
  const [sortParam, setSortParam] = useParam(`${prefix}sort`, defaultSortParam);
  const [hiddenParam, setHiddenParam] = useParam(`${prefix}hide`, defaultHidden);
  const [orderParam, setOrderParam] = useParam(`${prefix}cols`, declaredOrder);

  const page = Math.max(1, Math.trunc(Number(pageParam)) || 1);
  const itemsPerPage = decodeItemsPerPage(perParam, defaultItemsPerPage);
  const sort = useMemo(() => decodeSort(sortParam, declared), [sortParam, declared]);
  const visibleColumns = useMemo(() => {
    const hidden = new Set(decodeKeys(hiddenParam, declared));
    return Object.fromEntries(declared.map((key) => [key, !hidden.has(key)]));
  }, [hiddenParam, declared]);
  const columnOrder = useMemo(() => decodeKeys(orderParam, declared), [orderParam, declared]);

  // The page is put back to one by a filter, and by nothing else. Not by the
  // rows changing underneath it: on a live screen that is every event the
  // reader is not looking at, and it used to throw them off page four several
  // times a minute.
  const filtered = useRef<string | null>(null);
  useEffect(() => {
    const was = filtered.current;
    filtered.current = filterKey ?? null;
    if (was === null || was === (filterKey ?? null)) return;
    setPageParam("1");
  }, [filterKey, setPageParam]);

  return {
    page,
    onPageChange: (next) => setPageParam(String(Math.max(1, Math.trunc(next)))),
    itemsPerPage,
    onItemsPerPageChange: (next) => setPerParam(next === ALL_ITEMS ? ALL_ITEMS : String(next)),
    defaultItemsPerPage,
    itemsPerPageOptions: TABLE_PAGE_SIZES,
    visibleColumns,
    onVisibleColumnsChange: (next) =>
      setHiddenParam(declared.filter((key) => next[key] === false).join(",")),
    columnOrder,
    onColumnOrderChange: (next) =>
      setOrderParam(next.filter((key) => declared.includes(key)).join(",")),
    sort,
    onSortChange: (next) => setSortParam(encodeSort(next)),
    defaultSort,
    // The SCREEN and the TABLE, and nothing narrower. Two tables on one route
    // would otherwise share one set of widths and each keep re-teaching the
    // other its own. Deliberately not the record the screen is showing: a
    // seat's turn table is the same columns on every seat, so the width a
    // reader dragged on one is the width they want on the next.
    storageKey: `crewlet_table_${screen}${table ? `_${table}` : ""}`,
    paginated: true,
    resizable: true,
    showSettings: true,
    // The rich frame, always: every table here carries its title in a card
    // header or a screen header rather than in the table, so the compact frame
    // the design system picks for a titled table would never be the right one,
    // and it holds neither the column list nor the chip row.
    settingsVariant: "rich",
  };
}

/** What a panel decides about its own table. Everything else is decided once. */
export type RecordTableProps<T> = Pick<
  DataTableProps<T>,
  | "getRowKey"
  | "rowKey"
  | "onRowClick"
  | "getRowHref"
  | "isSelected"
  | "rowTone"
  | "rowActions"
  | "emptyMessage"
  | "stableOrder"
  | "loading"
  | "error"
> & {
  screen: string;
  table: string;
  rows: readonly T[];
  columns: readonly DataViewColumn<T>[];
  defaultSort?: DataTableSortState | null;
};

/**
 * A record table: the rows a panel of this dashboard holds, and the chrome a
 * reader reshapes them with.
 *
 * A COMPONENT RATHER THAN A BUNDLE OF PROPS, which is what it was. Where a
 * table's choices live is now the URL, and a screen that draws its table
 * inside a condition (every seat tab does) cannot call a hook at the point it
 * spreads a bundle in. What it draws is still entirely the design system's;
 * this adds no element of its own bar the count underneath.
 *
 * WHAT IT DECIDES, so that thirteen call sites do not each decide it: the
 * compact variant, the rich settings frame, a page of ten and the count. What
 * a call site decides is its rows, its columns, which column names the row
 * (`hideable: false`) and what the table opens sorted by.
 *
 * THE COUNT IS THE PAGE'S. The screen holds every row and this holds the page,
 * so both numbers are known exactly: ten of sixty-three, where the card header
 * above it already says sixty-three on its own.
 *
 * DENSITY IS DELIBERATELY NOT PASSED, and that is a decision rather than an
 * omission. The reader's own three-step density, the one the rail's switcher
 * sets, reaches these rows already: it scales the spacing and size tokens the
 * table is built out of, so Compact tightens a panel table with nothing
 * passed. The table's own two-step `density` prop answers a different
 * question, which the package states at the rule that implements it: the
 * comfortable step is for a table inside a section card with 24px of inner
 * padding, where the compact step reads cramped against the chrome around it.
 * These panels pad their header by `--spacing-4` and their table by the
 * table's own inset, so the compact step is the one that lines up, and the
 * prop stays where the design system left it.
 */
export function RecordTable<T>({
  screen,
  table,
  rows,
  columns,
  defaultSort = null,
  ...rest
}: RecordTableProps<T>) {
  const choices = useTableChoices({
    screen,
    table,
    columns,
    defaultItemsPerPage: PANEL_PAGE_SIZE,
    defaultSort,
  });
  const { columns: byKey, order } = useMemo(() => tableColumns(columns), [columns]);
  // What the table is drawing, for the footer: the page's share of the rows,
  // and the last page's share is whatever is left rather than a full page.
  const onPage =
    choices.itemsPerPage === ALL_ITEMS
      ? rows.length
      : Math.max(
          0,
          Math.min(choices.itemsPerPage, rows.length - (choices.page - 1) * choices.itemsPerPage),
        );
  return (
    <>
      <DataTable<T>
        {...rest}
        {...choices}
        data={[...rows]}
        columns={byKey}
        defaultColumnOrder={order}
        variant="compact"
      />
      <TableFooter
        visibleCount={onPage}
        totalCount={rows.length}
        {...(rest.loading === undefined ? {} : { loading: rest.loading })}
      />
    </>
  );
}

/**
 * The rail a seat tile takes for what it is doing.
 *
 * Three, and `quiet` is deliberately absent rather than neutral: a seat with
 * nothing to say draws no mark, where a neutral rail would be a line every
 * idle seat carried and nobody could read past.
 */
const SEAT_RAIL: Record<string, "info" | "warning" | "danger" | undefined> = {
  working: "info",
  needs: "warning",
  broken: "danger",
};

/** A seat's name and handle, linked. The one way a person appears in a list. */
export function SeatChip({
  name,
  handle,
  human,
  size = "xs",
}: {
  name: string;
  handle?: string;
  human?: boolean;
  size?: "xs" | "sm";
}) {
  const target = handle || name;
  return (
    <a
      // `seat-chip`, not a bare link: a seat's name is IDENTITY, and the
      // accent is reserved for saying where the reader is. A name rendered in
      // the accent everywhere it appears is identity-colouring by accident.
      // The affordance is the hover state and the cursor.
      className="row seat-chip"
      style={{ gap: "var(--spacing-2)", minWidth: 0 }}
      href={href(["seats", target])}
    >
      <Avatar name={name} size={size} variant={human ? "dashed" : "solid"} decorative />
      <span className="truncate">{name}</span>
    </a>
  );
}

export function StateBadge({
  agent,
  sandboxes,
}: {
  agent: AgentRow | null | undefined;
  sandboxes: SandboxEntry[];
}) {
  const state = runState(agent, sandboxes);
  return (
    <Tag variant={toneOf(state)} dot>
      {stateLabel(state)}
    </Tag>
  );
}

export function SeatCard({
  seat,
  agent,
  sandboxes,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  sandboxes: SandboxEntry[];
}) {
  const now = useNow();
  const sandbox = sandboxes.find((s) => s.role === seat.name) ?? null;
  const tone = seat.kind === "human" ? "quiet" : seatTone(agent, sandboxes);
  const call = agent?.live_call;
  return (
    <Card
      href={href(seatPath(seat))}
      variant="subtle"

      /* The rail is what the seat is DOING, never who it is, and `quiet`
         takes none at all: an idle seat used to draw a glowing tile that
         read as activity. */
      {...(SEAT_RAIL[tone] ? { rail: SEAT_RAIL[tone] } : {})}
    >
      <Stack gap={2}>
        <div className="row">
          <Avatar
            name={seat.name}
            size="lg"
            variant={seat.kind === "human" ? "dashed" : "solid"}
            decorative
          />
          <div className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
            <strong className="truncate t-body">{seat.name}</strong>
            {seat.handle && <span className="truncate t-caption mono">@{seat.handle}</span>}
          </div>
          {seat.kind === "human" ? (
            <Tag appearance="outline">human</Tag>
          ) : (
            <StateBadge agent={agent} sandboxes={sandboxes} />
          )}
        </div>
        <div className="seat-line truncate">{statusLine(agent, { sandbox, seat })}</div>
        {call?.in_progress && (
          <div className="row gap-1">
            <Tag variant="info">{call.phase}</Tag>
            <span className="t-caption t-num">
              round {call.round_num >= 0 ? call.round_num + 1 : <EmptyValue label="Not reported" />}
            </span>
            <span className="spacer" />
            <RelativeTime className="t-caption" value={call.updated_at} now={now} />
          </div>
        )}
        {seat.unit && <div className="t-caption truncate">{seat.unit.name}</div>}
      </Stack>
    </Card>
  );
}

/**
 * One obligation.
 *
 * Every row says WHAT happened and WHAT IT COSTS to leave it — the second half
 * is the part a list of conditions usually omits, and it is the half that lets
 * a reader decide whether to act now.
 */
export function AttentionRow({ item }: { item: Attention }) {
  const now = useNow();
  const inner = (
    <>
      <span className="attention-icon">
        <item.icon size="sm" />
      </span>
      <span className="col" style={{ gap: 2, flex: 1, minWidth: 0 }}>
        <span className="t-body" style={{ fontWeight: "var(--font-weight-medium)" }}>
          {item.title}
        </span>
        <span className="t-caption">{item.detail}</span>
      </span>
      {item.at && <RelativeTime className="t-caption nowrap" value={item.at} now={now} />}
    </>
  );
  if (!item.path) {
    return (
      <div className="attention-row" data-severity={item.severity}>
        {inner}
      </div>
    );
  }
  return (
    <a className="attention-row" data-severity={item.severity} href={href(item.path, item.query)}>
      {inner}
    </a>
  );
}

/**
 * One row of the event log.
 *
 * Four fixed columns — when, who, what, where — so a run of rows scans as
 * columns rather than as prose. The category is a WORD, not a hue: eight
 * coloured category chips in one list, repeated on every row, is most of what
 * made the old feed unreadable.
 */
export function EventRow({
  event,
  onOpen,
  showDate,
  depth = 0,
  mark,
}: {
  event: FeedRow;
  onOpen?: () => void;
  showDate?: boolean;
  /**
   * How far in this row's actor sits, in nesting steps. A trace draws its
   * spans as a tree, and a row that carried its own copy of this grid was a
   * second four-column layout drifting from this one a column at a time.
   */
  depth?: number;
  /** Drawn under the summary: a trace's own span bar. */
  mark?: ReactNode;
}) {
  const now = useNow();
  return (
    <a
      className={cx("feed-row", event.failed && "failed")}
      href={href(["events", event.id])}
      onClick={onOpen}
    >
      <time
        className="feed-time"
        dateTime={event.timestamp}
        title={[fmtDateTime(event.timestamp), formatRelative(event.timestamp, now)]
          .filter(Boolean)
          .join(" · ")}
      >
        {showDate ? fmtDateTime(event.timestamp) : fmtTime(event.timestamp)}
      </time>
      <span className="feed-actor truncate" style={depth ? { paddingLeft: depth * 12 } : undefined}>
        {depth > 0 && <span className="muted">└ </span>}
        {event.actor || "engine"}
      </span>
      <span className="feed-what truncate">
        {event.failed && (
          <>
            <ErrorGlyph size="xs" className="feed-failed-mark" />
            <VisuallyHidden>Failed</VisuallyHidden>{" "}
          </>
        )}
        {event.summary || event.type}
        {mark}
      </span>
      <span className="feed-tail">
        {event.source && <span className="truncate">{event.source}</span>}
        <span className="muted">{humanize(event.category) || "system"}</span>
      </span>
    </a>
  );
}

/**
 * What an empty or failed answer means, said precisely.
 *
 * `no_event_store` and "nothing has happened yet" are the same empty list and
 * completely different problems; so are `unauthorized` and a company with no
 * seats. Every screen routes its failure through here so the distinction is
 * made once.
 */
export function QueryState({
  error,
  loading,
  empty,
  children,
}: {
  error: string | null;
  loading: boolean;
  empty?: { title: ReactNode; hint?: ReactNode };
  children?: ReactNode;
}) {
  if (error === "unauthorized") {
    return (
      <Callout variant="warning" icon={<KeyGlyph size="sm" />}>
        <span>
          This answer is auth-gated. It needs an API token matching one of your{" "}
          <InlineCode>api.auth.tokens</InlineCode> entries.
        </span>
        <span className="spacer" />
        {/* The banner used to say "set a token" and offer nothing that could.
            With anonymous reads allowed the socket is never refused, so the
            dialog's only other doors — a refusal, and the engine panel — both
            stay shut on exactly the screen that needs it. */}
        <Button variant="secondary" size="small" leadingIcon={<KeyGlyph />} onClick={requestToken}>
          Set token
        </Button>
      </Callout>
    );
  }
  if (error === "no_event_store") {
    return (
      <Callout variant="neutral" icon={<DatabaseGlyph size="sm" />}>
        <span>
          This node keeps no event log, so there is no history to read. Set{" "}
          <InlineCode>store.path</InlineCode> in <InlineCode>crewlet.yaml</InlineCode> to make it
          durable.
        </span>
      </Callout>
    );
  }
  if (error === "unknown_query") {
    return (
      <Callout variant="neutral" icon={<InfoGlyph size="sm" />}>
        <span>
          The engine does not serve this answer. The subsystem behind it is not running on this
          node.
        </span>
      </Callout>
    );
  }
  if (error === "timeout") {
    return (
      <Callout variant="warning" icon={<ScheduleGlyph size="sm" />}>
        <span>The engine did not answer within 10 seconds. It may be under load.</span>
      </Callout>
    );
  }
  if (error) {
    return (
      <Callout variant="danger" icon={<ErrorGlyph size="sm" />}>
        <span>The engine refused this query ({error}).</span>
      </Callout>
    );
  }
  if (loading) return null;
  if (empty) {
    return (
      <EmptyState
        size="compact"
        icon={<InboxGlyph />}
        title={empty.title}
        // WHY it is empty, and what would fill it. A caller that says nothing
        // gets the honest sentence rather than a blank line: the query
        // answered, and this is what it answered with.
        description={empty.hint ?? "The engine answered this query with nothing."}
      />
    );
  }
  return <>{children}</>;
}
