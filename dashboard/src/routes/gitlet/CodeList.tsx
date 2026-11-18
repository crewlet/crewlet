/**
 * Gitlet's lists and timelines: rows of sentences, keyed and peeked as the grid's rows are.
 * Keeps the grid's j, k and Enter, its peek, a phone row, Load more and the address.
 */

import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ComponentType,
  type ReactNode,
  type RefObject,
} from "react";
import { Button, cx, peekClickHandler, useDataGridDriving } from "@crewlethq/ui";
import { RotateCcwClockGlyph, type GlyphProps } from "@crewlethq/icons/glyphs";
import { useOneReaderPerKey } from "~/app/frame/DataGrid.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { peekHref, usePeek, usePeekControls } from "~/app/frame/peekAddress.ts";
import { useKeymap } from "~/app/keymap.ts";
import { useParam } from "~/app/router.tsx";
import { PageFailed } from "~/components/common.tsx";
import { useClock } from "~/lib/clock.ts";
import { civilAt, readerDay } from "~/lib/format.ts";
import type { Paged } from "~/lib/usePaged.ts";
import "~/styles/gitlet.css";

/** A run of rows under one heading: a day of a timeline, or a queue's box later. */
export interface CodeListGroup<T> {
  key: string;
  heading: ReactNode;
  /** Beside the heading: "2 of 13". */
  aside?: ReactNode;
  rows: readonly T[];
}

export interface CodeListProps<T> {
  /** The rows, or the groups they are drawn under: exactly one of the two. */
  rows?: readonly T[];
  groups?: readonly CodeListGroup<T>[];
  rowKey: (row: T) => string;
  /** What the list is, said by assistive technology: "History of main". */
  "aria-label": string;
  /** `card` (default): one card of rows. `timeline`: each group a heading on a spine over its card. */
  look?: "card" | "timeline";
  /** The glyph on the spine at each heading (a commit's, a push's). */
  spine?: ComponentType<GlyphProps>;
  /** The groups' heading level under the page's own headings. Three by default. */
  headingLevel?: 2 | 3 | 4;
  /** The row's link, its title stretched over the row. Undefined: a row with no link. */
  rowHref?: (row: T) => string | undefined;
  /** The object a plain press or Enter peeks at; its page is the link where rowHref is absent. */
  peek?: (row: T) => ObjectRef | undefined;
  /** The row the reader is on that is no peek (a pane beside the list). */
  isSelected?: (row: T) => boolean;
  /** A plain press on the row's link, or Enter on its row: the host opens it in place (a pane). */
  onOpen?: (row: T) => void;
  /** The row's parts, in reading order. */
  lead?: (row: T) => ReactNode;
  title: (row: T) => ReactNode;
  titleAfter?: (row: T) => ReactNode;
  /** A line under the title: a repository's description. */
  summary?: (row: T) => ReactNode;
  meta?: (row: T) => ReactNode;
  facts?: (row: T) => ReactNode;
  below?: (row: T) => ReactNode;
  density?: "default" | "compact";
  /** A band at the card's head: a state bar, pickers, a sort; drawn inside the card. */
  head?: ReactNode;
  /** Drawn where there is no row, under the head: the list's loading, empty or refused state. */
  placeholder?: ReactNode;
  /** False leaves `[` and `]` to the host, which publishes one order across its lists. */
  neighbours?: boolean;
  /** True: j and k drive this list only once the reader points or focuses in it (a box above the page's list). */
  keysOnceClaimed?: boolean;
  /** Load more, as usePaged answers it. */
  paged?: Pick<Paged<unknown, unknown>, "more" | "next" | "paging" | "pageError">;
  /** The press's words: "Load older commits". */
  moreLabel?: string;
  /** Under the rows, paged or not: "7 commits of main loaded, newest first". */
  foot?: ReactNode;
}

/** A reader's day: its `YYYY-MM-DD` key, and how many days before the reader's today it is. */
export interface ListDay {
  key: string;
  ago: number;
}

const DAY_MS = 86_400_000;

/**
 * The rows cut into runs of one reader's day, in the order given: a day met again
 * after another opens its own group, so a source's order is never re-sorted.
 */
export function useDayGroups<T>(
  rows: readonly T[],
  at: (row: T) => string | undefined,
  heading: (day: ListDay | null, first: T) => ReactNode,
): CodeListGroup<T>[] {
  // RE-RENDERED ONLY WHEN THE READER'S DAY TURNS, so "Today" moves at midnight.
  const today = useClock((now) => readerDay(now));
  return useMemo(() => {
    const todayAt = civilAt(today);
    const runs: { day: string; rows: T[] }[] = [];
    for (const row of rows) {
      const ms = Date.parse(at(row) ?? "");
      const day = Number.isNaN(ms) ? "" : readerDay(ms);
      const last = runs[runs.length - 1];
      if (last && last.day === day) last.rows.push(row);
      else runs.push({ day, rows: [row] });
    }
    return runs.map((run, index) => {
      const dayAt = civilAt(run.day);
      const day =
        dayAt === null || todayAt === null
          ? null
          : { key: run.day, ago: Math.round((todayAt - dayAt) / DAY_MS) };
      return { key: `${run.day}#${index}`, heading: heading(day, run.rows[0]!), rows: run.rows };
    });
  }, [rows, today, at, heading]);
}

/** The list's sort in the address (`sort=`, or `sort.<name>=`), holding only the reader's choice. */
export function useListSort(name?: string): [sort: string, setSort: (next: string) => void] {
  const key = name ? `sort.${name}` : "sort";
  useOneReaderPerKey([key]);
  return useParam(key, "");
}

/** The row element a container draws under a key: any element, an `li` or a table's `tr`. */
function rowIn(root: HTMLElement | null, key: string): HTMLElement | null {
  if (!root) return null;
  for (const el of root.querySelectorAll<HTMLElement>("[data-key]")) {
    if (el.getAttribute("data-key") === key) return el;
  }
  return null;
}

/** j, k and Enter over rows a container draws, the list the reader last pointed at or focused. */
export function useListKeys(opts: {
  root: RefObject<HTMLElement | null>;
  keys: readonly string[];
  onOpen?: (key: string, e: KeyboardEvent) => void;
  /** Take part in the keys only once claimed, so another list drives them on landing. */
  onceClaimed?: boolean;
}): { cursor: string | null; claim: () => void; focusRow: (key: string) => void } {
  const { root, keys, onOpen, onceClaimed = false } = opts;
  const [held, setHeld] = useState<string | null>(null);
  const [claimed, setClaimed] = useState(false);
  // BY KEY, so a row gained at the top keeps the reader on theirs; a key that left is none.
  const cursor = held !== null && keys.includes(held) ? held : null;
  const drivable = keys.length > 0;
  const { driving, claim: claimKeys } = useDataGridDriving(drivable && (!onceClaimed || claimed));
  const claim = useCallback(() => {
    setClaimed(true);
    claimKeys();
  }, [claimKeys]);

  const focusRow = useCallback(
    (key: string) => {
      const row = rowIn(root.current, key);
      if (!row) return;
      const target = row.querySelector<HTMLElement>("[data-list-link]") ?? row;
      target.focus({ preventScroll: true });
      target.scrollIntoView({ block: "nearest" });
    },
    [root],
  );

  const step = (by: number) => {
    if (keys.length === 0) return;
    const from = cursor === null ? -1 : keys.indexOf(cursor);
    const next = keys[Math.max(0, Math.min(keys.length - 1, from + by))]!;
    setHeld(next);
    focusRow(next);
  };

  // A POINTER OR FOCUS INSIDE says which list the reader is in; focus also moves the cursor.
  useEffect(() => {
    const el = root.current;
    if (!el || !drivable) return undefined;
    const onFocus = (e: FocusEvent) => {
      claim();
      const row = (e.target as Element | null)?.closest<HTMLElement>("[data-key]");
      if (row && el.contains(row)) setHeld(row.getAttribute("data-key"));
    };
    el.addEventListener("focusin", onFocus);
    el.addEventListener("pointerdown", claim);
    return () => {
      el.removeEventListener("focusin", onFocus);
      el.removeEventListener("pointerdown", claim);
    };
  }, [root, claim, drivable]);

  useKeymap({
    "list.next": { run: () => step(1), when: driving },
    "list.previous": { run: () => step(-1), when: driving },
    // FOCUS ON THE ROW'S LINK leaves Enter to the browser (lib/keys.ts); this is Enter elsewhere.
    "list.open": {
      when: driving && cursor !== null,
      run: (e) => {
        if (cursor === null) return;
        const event = e as KeyboardEvent;
        const modified = event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
        // A MODIFIED ENTER is the link's, as a modified click is: a new tab or window.
        if (onOpen && !modified) {
          onOpen(cursor, event);
          return;
        }
        const link = rowIn(root.current, cursor)?.querySelector("a[data-list-link]");
        link?.dispatchEvent(
          new MouseEvent("click", {
            bubbles: true,
            cancelable: true,
            metaKey: event.metaKey,
            ctrlKey: event.ctrlKey,
            shiftKey: event.shiftKey,
            altKey: event.altKey,
          }),
        );
      },
    },
  });

  return { cursor, claim, focusRow };
}

/** Publishes the list's order to `[` and `]`, only for a list that peeks. */
function Neighbours({ refs }: { refs: ObjectRef[] }) {
  usePeekNeighbours(refs);
  return null;
}

export function CodeList<T>({
  rows,
  groups,
  rowKey,
  "aria-label": label,
  look = "card",
  spine: Spine,
  headingLevel = 3,
  rowHref,
  peek,
  isSelected,
  onOpen,
  lead,
  title,
  titleAfter,
  summary,
  meta,
  facts,
  below,
  density = "default",
  head,
  placeholder,
  neighbours = true,
  keysOnceClaimed = false,
  paged,
  moreLabel = "Load more",
  foot,
}: CodeListProps<T>) {
  if ((rows === undefined) === (groups === undefined)) {
    throw new Error("CodeList draws rows or groups: hand it exactly one of the two.");
  }
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  const all = useMemo(
    () => (groups ? groups.flatMap((g) => g.rows) : (rows ?? [])),
    [groups, rows],
  );
  const keys = useMemo(() => all.map(rowKey), [all, rowKey]);
  const openKey = useMemo(() => {
    if (!onOpen) return undefined;
    const byKey = new Map(all.map((r) => [rowKey(r), r]));
    return (key: string) => {
      const item = byKey.get(key);
      if (item !== undefined) onOpen(item);
    };
  }, [all, rowKey, onOpen]);
  const { cursor, focusRow } = useListKeys({
    root,
    keys,
    onOpen: openKey,
    onceClaimed: keysOnceClaimed,
  });

  const { open } = usePeekControls();
  const peeked = usePeek();
  const peekedHref = peeked ? peekHref(peeked) : "";
  const refs = useMemo(
    () => (peek ? all.map(peek).filter((r): r is ObjectRef => r !== undefined) : []),
    [all, peek],
  );

  // A PRESS IS HELD UNTIL ITS PAGE ANSWERS; one that took the button away moves focus to the
  // first row it added, else the last row or the foot, so focus never falls out of the list.
  const pressedAt = useRef<number | null>(null);
  const note = useRef<HTMLParagraphElement>(null);
  const more = paged?.more ?? false;
  const paging = paged?.paging ?? false;
  useEffect(() => {
    const from = pressedAt.current;
    if (from === null || paging) return;
    const added = keys.length > from;
    if (!added && more) return;
    pressedAt.current = null;
    const dropped = document.activeElement === null || document.activeElement === document.body;
    if (more || !dropped) return;
    const to = added ? keys[from] : keys.at(-1);
    if (to !== undefined) focusRow(to);
    else note.current?.focus({ preventScroll: true });
  }, [keys, more, paging, focusRow]);

  const row = (item: T) => {
    const key = rowKey(item);
    const ref = peek?.(item);
    const to = rowHref?.(item) ?? (ref ? peekHref(ref) : undefined);
    const current = Boolean(isSelected?.(item)) || (peekedHref !== "" && to === peekedHref);
    const words = title(item);
    return (
      <li
        key={key}
        className="code-list__row"
        data-key={key}
        data-cursor={cursor === key ? "" : undefined}
        data-current={current ? "" : undefined}
        // A ROW WITH NO LINK takes focus itself, by script and never by Tab.
        tabIndex={to ? undefined : -1}
      >
        {lead && <span className="code-list__lead">{lead(item)}</span>}
        <div className="code-list__body">
          <p className="code-list__title">
            {to ? (
              <a
                className="code-list__link"
                data-list-link=""
                href={to}
                aria-current={current ? "true" : undefined}
                onClick={peekClickHandler(
                  ref ? () => open(ref) : onOpen ? () => onOpen(item) : undefined,
                )}
              >
                {words}
              </a>
            ) : (
              <span className="code-list__words">{words}</span>
            )}
            {titleAfter?.(item)}
          </p>
          {summary && <p className="code-list__summary">{summary(item)}</p>}
          {meta && <div className="code-list__meta">{meta(item)}</div>}
        </div>
        {facts && <div className="code-list__facts">{facts(item)}</div>}
        {below && <div className="code-list__below">{below(item)}</div>}
      </li>
    );
  };

  const Heading = `h${headingLevel}` as const;
  const busy = paged?.paging ? "true" : "false";
  const card = !groups;
  const cardRows = (
    <ol className="code-list__rows" aria-label={label} aria-busy={busy}>
      {all.map(row)}
    </ol>
  );
  const held = placeholder ? <div className="code-list__placeholder">{placeholder}</div> : null;
  return (
    <div
      ref={root}
      className={cx(
        "code-list",
        look === "timeline" && "code-list--timeline",
        lead && "code-list--lead",
        density === "compact" && "code-list--compact",
      )}
      role={card ? undefined : "group"}
      aria-label={card ? undefined : label}
      aria-busy={card ? undefined : busy}
    >
      {peek && neighbours && <Neighbours refs={refs} />}
      {card && head ? (
        <div className="code-list__card">
          <div className="code-list__head">{head}</div>
          {all.length > 0 ? cardRows : held}
        </div>
      ) : (
        head && <div className="code-list__head">{head}</div>
      )}
      {card && !head && (all.length > 0 ? cardRows : held)}
      {!card &&
        groups.map((group, index) => (
          <div key={group.key} className="code-list__group">
            <Heading className="code-list__heading" id={`${id}-${index}`}>
              {Spine && (
                <span className="code-list__node" aria-hidden="true">
                  <Spine size="sm" />
                </span>
              )}
              {group.heading}
              {group.aside && <span className="code-list__aside">{group.aside}</span>}
            </Heading>
            <ol className="code-list__rows" aria-labelledby={`${id}-${index}`}>
              {group.rows.map(row)}
            </ol>
          </div>
        ))}
      {(paged?.more || paged?.pageError || foot) && (
        <div className="code-list__foot">
          {paged?.more && (
            <Button
              variant="secondary"
              leadingIcon={<RotateCcwClockGlyph />}
              loading={paged.paging}
              onClick={() => {
                pressedAt.current = keys.length;
                paged.next();
              }}
            >
              {moreLabel}
            </Button>
          )}
          {paged?.pageError && <PageFailed error={paged.pageError} />}
          {foot && (
            <p ref={note} className="code-list__note" role="status" tabIndex={-1}>
              {foot}
            </p>
          )}
        </div>
      )}
    </div>
  );
}
