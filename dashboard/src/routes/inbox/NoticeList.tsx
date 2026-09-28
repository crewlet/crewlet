/**
 * The Inbox's left column: the scope, the chips, and the rows — "Needs a
 * decision" first, then the notices by the company's day.
 *
 * EVERY ROW IS ONE BUTTON that opens the pane beside it, with the same three
 * lines whatever it is: WHO (the person or seat behind it, never a token id),
 * WHAT (the question, the stop, the excerpt), and WHY (the pill). A row that
 * drew its kind in a different shape would make a reader learn four lists to
 * read one.
 */

import { firstLine } from "~/lib/format.ts";
import { useId, useMemo } from "react";
import { EmptyValue, FilterChip, FilterChipGroup, Skeleton, Tag } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { Segmented } from "~/ui/primitives.tsx";
import { PERIOD_ADJECTIVE } from "~/lib/budget.ts";
import { plainText } from "~/lib/markdown.ts";
import { ENGINE_SENTENCES, authorOf, nameAuthor } from "~/lib/work.ts";
import { fmtDateTime, humanize } from "~/lib/format.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import { INBOX_PAGE } from "~/lib/useInboxCounts.ts";
import { CHIPS, rowPill, type Chip, type DayGroup, type InboxRow, type Scope } from "./model.ts";
import type { WorkInboxNotice } from "~/protocol/index.ts";

/**
 * What a chip's count is, where it is not the whole: said on each chip, as its
 * description and its title. About what was LOADED rather than naming a page
 * size, because two reads fill the list — the notices, a page of fifty, and
 * the decisions, a page of their own — and either may be the one that stopped.
 */
export const PAGE_LOCAL_CHIPS = "Counted over what this page loaded — more lie past it.";

/** Who a row is about, as its badge and its first words draw it. */
export interface Who {
  name: string;
  kind: "agent" | "human";
  /** Null where the row is about no seat at all (the company's budget). */
  handle: string | null;
}

/** The seat behind a handle, named as the chart names it. */
export function whoOf(index: OrgIndex, handle: string | null | undefined): Who | null {
  const h = (handle ?? "").trim();
  if (!h) return null;
  const seat = index.byHandle.get(h);
  return { name: seat?.name ?? h, kind: seat?.kind ?? "agent", handle: h };
}

/**
 * Who a row is about.
 *
 * A NOTICE'S AUTHOR IS THE PERSON, NEVER THE CREDENTIAL: a change a person's
 * token made is authored by the token and carries `actor_seat`, the seat it was
 * bound to. An operator change with no bound seat names nobody — its actor is a
 * token id, which is a secret's name rather than a person's — so it is drawn as
 * "An operator".
 */
export function rowWho(row: InboxRow, index: OrgIndex): Who | null {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return whoOf(index, row.subject.ask.asked_by_seat || row.subject.ask.asked_by);
        case "run":
          return whoOf(index, row.subject.run.agent_handle);
        case "seat":
          return whoOf(index, row.subject.seat.row.handle ?? row.subject.seat.row.role);
      }
      break;
    case "condition":
      return whoOf(index, row.item.who);
    case "notice": {
      const n = row.notice;
      if (n.actor_seat) return whoOf(index, n.actor_seat);
      if (n.actor && n.actor_kind !== "operator") return whoOf(index, n.actor);
      return null;
    }
  }
  return null;
}

/** The work item a row is on, by key, or "". */
export function rowKey(row: InboxRow): string {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return row.subject.ask.key;
        case "run":
          return row.subject.run.work_item?.key ?? "";
        case "seat": {
          const r = row.subject.seat.row;
          return r.turn?.work_item?.key ?? r.live_call?.work_item?.key ?? "";
        }
      }
      break;
    case "condition":
      return "";
    case "notice":
      return row.notice.subject_key ?? "";
  }
  return "";
}

/**
 * What a notice SAYS, in this screen's words.
 *
 * The excerpt, flattened — and, where the engine composed it (a reorder of
 * the reader's priorities), with its author named as the row above it names them:
 * the engine writes a HANDLE, because a seat reads the same sentence, and the
 * row read "Maya Ops on LEAD-3" over "maya-ops put LEAD-3 at position 1 of
 * your priorities". The Inbox is the reader's own, so "your" is theirs.
 */
export function noticeText(notice: WorkInboxNotice, index: OrgIndex): string {
  const said = plainText(notice.excerpt ?? "");
  if (!ENGINE_SENTENCES.has(notice.kind)) return said;
  return nameAuthor(said, authorOf(notice), (h) => index.byHandle.get(h)?.name ?? h);
}

/** The row's one line of what it is. */
export function rowLine(row: InboxRow, index: OrgIndex): string {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask": {
          const ask = row.subject.ask;
          return ask.decision?.question || firstLine(ask.body) || ask.title;
        }
        case "run":
          return row.subject.run.question
            ? `Coding run parked: ${row.subject.run.question}`
            : "Coding run parked on a question";
        case "seat": {
          const seat = row.subject.seat;
          const period = seat.window ? `${PERIOD_ADJECTIVE[seat.window.period]} ` : "";
          const onItem = rowKey(row) !== "";
          return `${onItem ? "Stopped mid-turn" : "Stopped"}: ${period}token budget exhausted`;
        }
      }
      break;
    case "condition":
      return row.item.title;
    case "notice":
      return noticeText(row.notice, index) || humanize(row.notice.kind);
  }
  return "";
}

/**
 * WHEN, as few characters as a row can spare: "now", "12m", "3h", then the
 * weekday inside a week and the date past it.
 *
 * A MOMENT, NOT AN AGE — which is why it is not `lib/seats.ts`' `shortAge`
 * ("2d"): a notice list is read as a timeline, where "Tue" places a row and
 * "4d" makes the reader count back.
 */
export function shortWhen(at: string | undefined, now: number): string {
  const t = Date.parse(at ?? "");
  if (!Number.isFinite(t)) return "";
  const minutes = Math.max(0, Math.floor((now - t) / 60_000));
  if (minutes < 1) return "now";
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  if (hours < 24 * 7) return new Intl.DateTimeFormat(undefined, { weekday: "short" }).format(t);
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(t);
}

/** Whether the row has something this person has not read. */
export function rowUnread(row: InboxRow): boolean {
  switch (row.kind) {
    case "notice":
      return !row.notice.read;
    case "decision":
      return row.notices.some((n) => !n.read);
    case "condition":
      return false;
  }
}

export function NoticeList({
  scope,
  onScope,
  unread,
  chip,
  onChip,
  counts,
  decisions,
  groups,
  selected,
  onOpen,
  index,
  now,
  loading,
  quiet,
  more,
  pageLocal,
  showDecisions,
  showNotices,
  refusal,
}: {
  scope: Scope;
  onScope: (scope: Scope) => void;
  /** Unread notices on the loaded page, for the Unread option; null when unknown. */
  unread: { count: number; floor: boolean } | null;
  chip: Chip;
  onChip: (chip: Chip) => void;
  counts: Record<Chip, number>;
  decisions: InboxRow[];
  groups: DayGroup[];
  selected: string;
  onOpen: (key: string) => void;
  index: OrgIndex;
  now: number;
  /** Nothing has answered yet. */
  loading: boolean;
  /** What to say instead of rows, or null where rows (or a refusal) are drawn. */
  quiet: { title: string; hint: string } | null;
  /** More notices lie past the page. */
  more: boolean;
  /**
   * The chip counts are the page's rather than the whole: the notices, or the
   * decisions, stopped with more behind them.
   */
  pageLocal: boolean;
  /** Whether the decisions group is part of this view (not under Snoozed). */
  showDecisions: boolean;
  /** Whether the notices are part of this view (a bound reader). */
  showNotices: boolean;
  /** A read's refusal, drawn where its rows would be. */
  refusal?: React.ReactNode;
}) {
  const chipsNoteID = useId();
  const chips = useMemo(
    () =>
      // Under Snoozed there are no decisions, so the two chips that are only
      // decisions have nothing to narrow and are not offered.
      CHIPS.filter((c) => showDecisions || (c.value !== "decisions" && c.value !== "reviews")),
    [showDecisions],
  );
  return (
    <div className="inbox-list">
      <div className="inbox-list-head">
        {/* THE UNREAD COUNT IS INSIDE ITS OWN OPTION, as the approved Inbox
            draws it ("Unread 5") — a number beside the group read as a fact
            about all three. It is the page's, so where the page stopped with
            more behind it the option says so (`50+`) and its title says why. */}
        <Segmented<Scope>
          value={scope}
          onChange={onScope}
          ariaLabel="Which notices"
          size="sm"
          options={[
            {
              value: "unread",
              label: "Unread",
              title: unread?.floor
                ? `What you have not read yet. The engine answers ${INBOX_PAGE} at a time, and more lie past this page.`
                : "What you have not read yet.",
              ...(unread ? { count: unread.count, capped: unread.floor } : {}),
            },
            { value: "all", label: "All", title: "Everything that reached you, read or not." },
            {
              value: "snoozed",
              label: "Snoozed",
              title: "Notices you put off, until they come back.",
            },
          ]}
        />
      </div>
      <div className="inbox-chips">
        {/* A PAGE IS A PAGE: the chip counts are over the rows LOADED. Where
            that is not every row — the notices stopped at the page with more
            behind them — each chip says so in its own description (read with
            it) and its title (shown on hover), rather than in a line of muted
            text under the row that read like a debugging aid. Where the page
            holds everything the counts are the whole, and nothing is said. */}
        {pageLocal && (
          <span id={chipsNoteID} className="sr-only">
            {PAGE_LOCAL_CHIPS}
          </span>
        )}
        <FilterChipGroup
          label="Show"
          hideLabel
          semantics="radio"
          activate="manual"
          value={chip}
          onValueChange={(value) => onChip((value ?? "") as Chip)}
        >
          {chips.map((c) => (
            <FilterChip
              key={c.value}
              value={c.value}
              count={c.value ? counts[c.value] : undefined}
              title={pageLocal && c.value ? PAGE_LOCAL_CHIPS : undefined}
              aria-describedby={pageLocal && c.value ? chipsNoteID : undefined}
            >
              {c.label}
            </FilterChip>
          ))}
        </FilterChipGroup>
      </div>

      <div className="inbox-rows">
        {refusal}
        {showDecisions && decisions.length > 0 && (
          <Group label="Needs a decision">
            {decisions.map((row) => (
              <Row
                key={row.key}
                row={row}
                index={index}
                now={now}
                selected={row.key === selected}
                onOpen={() => onOpen(row.key)}
              />
            ))}
          </Group>
        )}
        {groups.map((group) => (
          <Group key={group.label} label={group.label}>
            {group.rows.map((row) => (
              <Row
                key={row.key}
                row={row}
                index={index}
                now={now}
                selected={row.key === selected}
                onOpen={() => onOpen(row.key)}
              />
            ))}
          </Group>
        ))}
        {loading && <Skeleton variant="text" rows={5} label="Loading the inbox" />}
        {quiet && (
          <div className="inbox-quiet">
            <strong className="t-cell">{quiet.title}</strong>
            <span className="t-caption">{quiet.hint}</span>
          </div>
        )}
        {showNotices && more && (
          <p className="t-caption inbox-more">
            {`More notices exist beyond this page — the engine returns ${INBOX_PAGE} at a time. Mark these read and the next ones come up.`}
          </p>
        )}
      </div>
    </div>
  );
}

function Group({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <section className="inbox-group" aria-label={label}>
      <h2 className="inbox-group-label">{label}</h2>
      <ul className="inbox-group-rows">{children}</ul>
    </section>
  );
}

const RING = { warning: "warning", danger: "danger", neutral: undefined } as const;

function Row({
  row,
  index,
  now,
  selected,
  onOpen,
}: {
  row: InboxRow;
  index: OrgIndex;
  now: number;
  selected: boolean;
  onOpen: () => void;
}) {
  const who = rowWho(row, index);
  const key = rowKey(row);
  const pill = rowPill(row);
  const unread = rowUnread(row);
  const ring = row.kind === "notice" ? undefined : RING[pill.tone];
  const snoozedUntil = row.kind === "notice" && row.notice.snoozed ? row.notice.snoozed_until : "";
  const where = key ? `on ${key}` : row.kind === "notice" ? "" : "engine";
  return (
    <li>
      <button
        type="button"
        className="inbox-row"
        aria-current={selected || undefined}
        data-unread={unread || undefined}
        onClick={onOpen}
      >
        {who ? (
          <SeatAvatar name={who.name} kind={who.kind} size={30} ring={ring} decorative />
        ) : row.kind === "condition" ? (
          <span className="attention-icon inbox-row-mark" data-severity={row.item.severity}>
            <Mark name={row.item.icon} size="sm" />
          </span>
        ) : (
          <span className="inbox-row-mark inbox-row-nobody" aria-hidden="true">
            <Mark name="key" size="sm" />
          </span>
        )}
        <span className="inbox-row-main">
          <span className="inbox-row-head">
            <span className="inbox-row-who">
              {who?.name ?? (row.kind === "condition" ? "The company" : "An operator")}
            </span>
            <span className="inbox-row-where">{where}</span>
            <span className="inbox-row-age t-num">
              {shortWhen(row.at, now) || <EmptyValue label="No time recorded" />}
            </span>
          </span>
          <span className="inbox-row-line">{rowLine(row, index)}</span>
          <span className="inbox-row-foot">
            <Tag size="xs" variant={pill.tone} appearance="soft">
              {pill.label}
            </Tag>
            {snoozedUntil && (
              <span className="t-caption">{`Snoozed until ${fmtDateTime(snoozedUntil)}`}</span>
            )}
            <span className="spacer" />
            {/* A FIXED CELL, filled or not: a dot that came and went as a poll
                landed stepped every row's text sideways with it. */}
            <i className="inbox-unread" data-on={unread || undefined} aria-hidden="true" />
            {unread && <span className="sr-only">Unread</span>}
          </span>
        </span>
      </button>
    </li>
  );
}
