/**
 * What every listing of artifacts shares, the gallery's sections and the
 * folders' pages alike: the listing asked a page at a time, the shape a
 * browser draws it in, the page bar's field, the toolbar, the cards and the
 * rows, the selection's bar, and what a search that found nothing says.
 *
 * ONE LISTING, MANY QUESTIONS. All, Yours and Shared with you ask the engine's
 * `artifacts` question under a scope; a folder's page asks it for one folder
 * (`folder`, one level with `delimiter=/`), and the Folders section for the
 * top level. Each screen is its own file and says what is particular to it;
 * how a listing is fetched, cut and drawn is said once, here.
 */

import { useCallback, useEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import {
  ArtifactAccessLabel,
  ArtifactCardSkeleton,
  ArtifactKindGlyph,
  ArtifactNameCell,
  Button,
  Input,
  PrincipalAvatar,
  SegmentedControl,
  Select,
  SelectionBar,
  Skeleton,
  TableEntityCell,
  type DataGridColumn,
} from "@crewlethq/ui";
import {
  CheckGlyph,
  FolderInputGlyph,
  LayoutGridGlyph,
  ListGlyph,
  SearchGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { useKeymap } from "~/app/keymap.ts";
import { artifactPath } from "~/app/routes.ts";
import { ARTIFACT_LIST_DELIMITER, ARTIFACT_LIST_SORTS } from "~/contract/artifacts.ts";
import { fmtCount, fmtDate, relTime } from "~/lib/format.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useClient } from "~/lib/store-hooks.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import { usePinsChanged } from "~/lib/artifacts/pins.ts";
import { useFoldersChanged, pathWords, TOP_LEVEL } from "~/lib/artifacts/folders.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import {
  KIND_WORDS,
  KINDS,
  ROLE_WORDS,
  TRASH_KEPT,
  purgeAt,
  purgeSentence,
} from "~/lib/artifacts/words.ts";
import type {
  ArtifactKind,
  ArtifactListAnswer,
  ArtifactListItem,
  ArtifactListScope,
  ArtifactListSort,
} from "~/protocol/index.ts";
import { ListedArtifactCard, accessOf } from "./parts.tsx";
import { ArtifactMenu } from "./GalleryMenu.tsx";
import type { GallerySelection } from "./selection.ts";

/** How often a listing reads itself again: no push moves a listing, and a minute keeps a left-open tab current. */
export const GALLERY_POLL_MS = 60_000;

/** A page of the listing: the engine's own default, and the most a page asks for. */
export const PAGE = 50;

/**
 * The order a listing is in when the reader chose none: the latest activity
 * first, which is the time every card says ("Edited 20h ago"), so a grid reads
 * top to bottom as a timeline. What the reader opened lately is one choice
 * away, as Recently opened.
 */
export const DEFAULT_SORT: ArtifactListSort = "updated";

/** What each order is called in the bar. */
const SORT_WORDS: Readonly<Record<ArtifactListSort, string>> = {
  updated: "Recent activity",
  created: "Newest",
  title: "Title, A to Z",
  opened: "Recently opened",
};

/** The order the address names, or the default for none or one the engine does not take. */
export function sortOf(raw: string): ArtifactListSort {
  return (ARTIFACT_LIST_SORTS as readonly string[]).includes(raw)
    ? (raw as ArtifactListSort)
    : DEFAULT_SORT;
}

/**
 * What a search or a type that found nothing says: the phrase it looked for,
 * and only the change that could find more. It said "Change the phrase or the
 * type" with no type chosen.
 */
export function noMatch(q: string, kind: ArtifactKind | ""): { title: string; hint: string } {
  if (q) {
    return {
      title: `Nothing matches “${q}”`,
      hint: kind
        ? `Only ${KIND_WORDS[kind]} artifacts were searched. Try another phrase, or choose Every type.`
        : "Try another phrase.",
    };
  }
  if (kind) {
    return {
      title: `No ${KIND_WORDS[kind]} artifacts here`,
      hint: "Choose Every type to see the rest.",
    };
  }
  return { title: "Nothing matches", hint: "Try another phrase." };
}

/**
 * The listing's question as the engine reads it (S3's ListObjectsV2 terms).
 * `folder` names one folder by id (`""` for the top level) and `level` asks
 * only what is directly in it, with its child folders as `common_prefixes`;
 * left out, the question is flat, every folder at once.
 */
export function listParams(ask: {
  scope: ArtifactListScope;
  kind: string;
  q: string;
  sort: ArtifactListSort;
  trashed?: boolean;
  folder?: string | undefined;
  level?: boolean;
}): Record<string, unknown> {
  return {
    scope: ask.scope,
    sort: ask.sort,
    "max-keys": PAGE,
    ...(ask.folder ? { folder: ask.folder } : {}),
    ...(ask.level ? { delimiter: ARTIFACT_LIST_DELIMITER } : {}),
    ...(ask.kind ? { kind: ask.kind } : {}),
    ...(ask.q.trim() ? { q: ask.q.trim() } : {}),
    ...(ask.trashed ? { status: "trashed" } : {}),
  };
}

/**
 * One listing, its first page live and the pages after it fetched on demand,
 * through the pager every paged list shares (`lib/usePaged.ts`). A change of
 * question drops the pages after the first: they answered another question.
 * The next page starts after the last row the first was answered with, and an
 * artifact made or moved into its range pushes that one off the first page's
 * foot when it is read again on its poll; the pager joins the moved first page
 * to the pages after it, so the row is on neither only for a read, never on
 * screen. A pin, a filing, a rename, the trash or a restore anywhere in the
 * tab moves a row on any page loaded, so each reads every page again in place
 * ([Paged.refresh]).
 */
export function useListing(params: Record<string, unknown>, enabled = true) {
  const { socket } = useClient();
  const first = useQuery("artifacts", params, {
    pollMs: GALLERY_POLL_MS,
    enabled,
  });
  const paged = usePaged(
    first,
    params,
    (token) => socket.query("artifacts", { ...params, "continuation-token": token }),
    contentsOf,
    artifactIdOf,
    tokenAfter,
  );
  // EVERY PAGE, not the first alone: the row a press moved may be on any of
  // them. Called with no argument whatever the caller hands a callback.
  const { refresh } = paged;
  const rereadAll = useCallback(() => refresh(), [refresh]);
  // A PIN MOVES A ROW between the pinned list and the rest, and marks it.
  usePinsChanged(rereadAll);
  // A MOVE takes a row out of one folder's listing and into another's.
  useFoldersChanged(rereadAll);
  // A PAGE THAT DID NOT COME is asked for again by the same button: the rows
  // already drawn stay, and nothing is claimed about the rest.
  return {
    first,
    items: paged.items,
    more: paged.more,
    loadMore: paged.next,
    loadingMore: paged.paging,
    refetch: rereadAll,
  };
}

/** A page's artifacts, each one's id, and the token to the page after. */
const contentsOf = (a: ArtifactListAnswer) => a.contents ?? [];
const artifactIdOf = (item: ArtifactListItem) => item.id;
const tokenAfter = (a: ArtifactListAnswer) => a.next_continuation_token ?? "";

/** How a listing is drawn. */
export type Shape = "grid" | "list";

/**
 * The shape this browser draws a listing in: the grid unless the reader chose
 * the list here before. A preference that could not be read or kept (a
 * private window) is the grid, every time. Every listing shares it, so a
 * reader who reads lists reads one in every folder too.
 */
export function useShape(): [Shape, (next: Shape) => void] {
  const [shape, setShape] = useState<Shape>(() => {
    try {
      return localStorage.getItem(STORAGE_KEYS.artifactsShape) === "list" ? "list" : "grid";
    } catch {
      return "grid";
    }
  });
  const choose = useCallback((next: Shape) => {
    setShape(next);
    try {
      if (next === "grid") localStorage.removeItem(STORAGE_KEYS.artifactsShape);
      else localStorage.setItem(STORAGE_KEYS.artifactsShape, next);
    } catch {
      // A preference the browser will not keep holds for this visit only.
    }
  }, []);
  return [shape, choose];
}

/** How many card placeholders the grid draws while its first page loads: two rows at three across. */
const SKELETON_CARDS = 6;

/** The listing's placeholder, in the shape it will be drawn in, said once. */
export function GallerySkeleton({ shape }: { shape: Shape }) {
  if (shape === "list") return <Skeleton variant="list" rows={6} label="Loading artifacts" />;
  return (
    <>
      <ul className="artifacts-cards" role="list" aria-hidden="true">
        {Array.from({ length: SKELETON_CARDS }, (_, i) => (
          <li key={i}>
            <ArtifactCardSkeleton />
          </li>
        ))}
      </ul>
      <Skeleton variant="box" width={0} height={0} label="Loading artifacts" />
    </>
  );
}

/** How long the phrase waits for the reader to pause before it is asked. */
const SEARCH_PAUSE_MS = 300;

/**
 * A listing's page bar: the phrase, then the page's way to make something
 * (`children`: New, or New folder).
 */
export function GalleryBar({
  q,
  onQ,
  placeholder = "Search artifacts",
  children,
}: {
  q: string;
  onQ: (q: string) => void;
  /** What the field says it searches: "Search Q4 review" in a folder. */
  placeholder?: string;
  children?: ReactNode;
}) {
  const box = useRef<HTMLInputElement>(null);
  useSearchTarget(box);
  // THE PHRASE IS TYPED HERE and sent when the reader pauses, so a listing is
  // not asked for once per keystroke.
  const [typed, setTyped] = useState(q);
  useEffect(() => setTyped(q), [q]);
  useEffect(() => {
    if (typed === q) return undefined;
    const t = setTimeout(() => onQ(typed), SEARCH_PAUSE_MS);
    return () => clearTimeout(t);
  }, [typed, q, onQ]);
  return (
    <PageActions>
      <span className="artifacts-find page-find">
        <Input
          ref={box}
          type="search"
          inputSize="sm"
          width="full"
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          leading={<SearchGlyph size="sm" />}
          placeholder={placeholder}
          aria-label={placeholder}
        />
      </span>
      {children}
    </PageActions>
  );
}

/** Whose artifacts a folder's listing shows: a filter in its bar, never a section. */
const SCOPE_WORDS = {
  all: "Anyone's",
  mine: "Yours",
  shared: "Shared with you",
} as const satisfies Partial<Record<ArtifactListScope, string>>;

/** The scopes a folder's bar offers, the widest first. */
export type FolderScope = keyof typeof SCOPE_WORDS;

/** The scope the address names, or Anyone's for none or one a folder does not offer. */
export function folderScopeOf(raw: string): FolderScope {
  return Object.hasOwn(SCOPE_WORDS, raw) ? (raw as FolderScope) : "all";
}

/**
 * A listing's toolbar: the type and the order it is asked in, whose it is (in
 * a folder), the trash on Yours, Select, and the shape it is drawn in: quiet
 * controls on a line of their own, since the cards are what the page is for.
 *
 * `toolbar` FIRST, as every screen's second row: the scroller's
 * `:has(.toolbar)` publishes `--sticky-top`, which the grid's column heads
 * offset themselves by.
 */
export function GalleryToolbar({
  kind,
  onKind,
  sort,
  onSort,
  shape,
  onShape,
  scope,
  trash,
  select,
}: {
  kind: string;
  onKind: (kind: string) => void;
  sort: ArtifactListSort;
  onSort: (sort: ArtifactListSort) => void;
  shape: Shape;
  onShape: (shape: Shape) => void;
  /** Whose artifacts a folder shows, or null where the section already says. */
  scope?: { value: FolderScope; onChange: (scope: FolderScope) => void } | null;
  /** The trash toggle, on the section whose listing has one, or null. */
  trash?: { on: boolean; onChange: (on: boolean) => void } | null;
  /** The Select toggle, where the listing's rows can be picked, or null. */
  select?: {
    on: boolean;
    onChange: (on: boolean) => void;
    ref: RefObject<HTMLButtonElement | null>;
  } | null;
}) {
  return (
    <div className="toolbar">
      <Select
        size="sm"
        width="auto"
        appearance="quiet"
        value={kind}
        onChange={(value) => onKind(String(value))}
        options={[
          { value: "", label: "Every type" },
          ...KINDS.map((k) => ({
            value: k,
            label: KIND_WORDS[k],
            leading: <ArtifactKindGlyph kind={k} size="sm" />,
          })),
        ]}
        ariaLabel="Type"
        placeholder="Every type"
        active={kind !== ""}
      />
      <Select
        size="sm"
        width="auto"
        appearance="quiet"
        value={sort}
        onChange={(value) => onSort(String(value) as ArtifactListSort)}
        options={ARTIFACT_LIST_SORTS.map((s) => ({ value: s, label: SORT_WORDS[s] }))}
        ariaLabel="Order"
      />
      {scope && (
        <Select
          size="sm"
          width="auto"
          appearance="quiet"
          value={scope.value}
          onChange={(value) => scope.onChange(folderScopeOf(String(value)))}
          options={(Object.keys(SCOPE_WORDS) as FolderScope[]).map((s) => ({
            value: s,
            label: SCOPE_WORDS[s],
          }))}
          ariaLabel="Whose"
          active={scope.value !== "all"}
        />
      )}
      {/* THE TRASH, SELECT AND THE SHAPE ARE ONE CLUSTER at the far end, so a
          narrow row moves them down together and keeps them at the end. */}
      <span className="artifacts-bar-end">
        {trash && (
          // A TOGGLE, SO ONE NAME: pressed, it is the trash's listing, and
          // pressed again the reader's own.
          <Button
            size="small"
            variant="ghost"
            leadingIcon={<TrashGlyph size="sm" />}
            pressed={trash.on}
            onClick={() => trash.onChange(!trash.on)}
          >
            Trash
          </Button>
        )}
        {select && (
          <Button
            ref={select.ref}
            size="small"
            variant="ghost"
            leadingIcon={<CheckGlyph size="sm" />}
            pressed={select.on}
            onClick={() => select.onChange(!select.on)}
          >
            Select
          </Button>
        )}
        <SegmentedControl<Shape>
          semantics="radio"
          size="sm"
          label="Show as"
          value={shape}
          onValueChange={onShape}
          options={[
            {
              value: "grid",
              icon: <LayoutGridGlyph size="sm" />,
              srLabel: "Grid",
              title: "Grid",
            },
            { value: "list", icon: <ListGlyph size="sm" />, srLabel: "List", title: "List" },
          ]}
        />
      </span>
    </div>
  );
}

/**
 * The bar over a listing while its reader picks: the count, Move to… and
 * Clear. `picked` are the rows picked that are drawn; `onMove` opens the
 * Move dialog for them.
 *
 * MOVE TO… IS HELD ONLY BY THE ARTIFACTS APP, never by a pick's role. The
 * engine decides each artifact on its own: an editor files it anywhere they
 * may file into, and a folder's manager takes anything in it they can read
 * to the top level, though they could not edit it, which no role on a row
 * says. So the whole selection is sent, every pick the engine refuses stays
 * where it was, and the move's toast names it and why (`useMoveArtifacts`).
 */
export function GallerySelectionBar({
  selection,
  picked,
  author,
  onMove,
  returnFocusRef,
}: {
  selection: GallerySelection;
  picked: readonly ArtifactListItem[];
  author: WriteAccess;
  onMove: () => void;
  returnFocusRef: RefObject<HTMLButtonElement | null>;
}) {
  const hold = author.can ? undefined : author.reason;
  // THE BINDINGS FOLLOW THE BAR: `m` moves what is picked while the bar is
  // up and the move is allowed, and nothing otherwise.
  useKeymap({
    "gallery.move": { run: onMove, when: picked.length > 0 && hold === undefined },
  });
  return (
    <SelectionBar
      count={selection.selected.size}
      onClear={selection.clear}
      returnFocusRef={returnFocusRef}
      className="artifacts-selection"
    >
      <Button
        size="small"
        variant="primary"
        leadingIcon={<FolderInputGlyph size="sm" />}
        disabledReason={hold}
        title={hold}
        onClick={onMove}
      >
        Move to…
      </Button>
    </SelectionBar>
  );
}

/**
 * The fact columns' tracks, FIXED rather than sized to their contents: All
 * draws Pinned and the rest as two grids one above the other, and a page
 * loaded later is the same grid grown, so a track sized to what one grid holds
 * put a column at two places on one screen and moved it when a page came in.
 * Each is its widest value at the body size ("Company" beside its glyph,
 * "Nov 18, 2026", "Can comment"); a longer owner's name or folder path is
 * cut, with the whole of it on hover.
 */
const TRACK = {
  access: "7.5rem",
  owner: "11rem",
  folder: "10rem",
  when: "7.5rem",
  role: "8rem",
  purge: "10rem",
} as const;

/** What every listing is drawn for: its shape, its reader and what its rows can do. */
export interface ListingView {
  /** The trash's listing, on Yours. */
  trashed: boolean;
  /** Every row is the reader's own (Yours): no owner and no role column. */
  own: boolean;
  /**
   * Each row's folder is drawn: a flat question, or a search through a
   * folder and everything beneath it. One folder's own level leaves it out,
   * where every row would name the folder its page is titled by.
   */
  folders: boolean;
  shape: Shape;
  people: ArtifactPeople;
  viewer: Pick<ViewerState, "owner">;
  now: number;
  /** A row's menu moved the listing: a rename, a move, the trash or a restore. */
  onChanged: () => void;
  /** The reader's picks, where the listing offers Select. */
  selection: GallerySelection | null;
}

/**
 * Who a row names, as the reader knows them: "You" for the reader's own
 * record (`ViewerState.owner`, the name an owner is recorded under), else the
 * chart's name or the login's words.
 */
function whoWords(
  name: string,
  viewer: Pick<ViewerState, "owner">,
  people: ArtifactPeople,
): string {
  return viewer.owner !== "" && name === viewer.owner ? "You" : people.seatName(name);
}

/** An owner's cell, as every listing draws one: the name the reader knows and the badge. */
export function OwnerCell({
  owner,
  viewer,
  people,
}: {
  owner: string;
  viewer: Pick<ViewerState, "owner">;
  people: ArtifactPeople;
}) {
  // THE OWNER'S BADGE, as the chart draws them: a person's circle and an
  // agent's character, so the column reads by who as well as by name.
  return (
    <TableEntityCell
      name={whoWords(owner, viewer, people)}
      badge={<PrincipalAvatar principal={people.seatPrincipal(owner)} size="xs" />}
    />
  );
}

/** Where a row is kept, in words, cut with the whole of it on hover; the top level when blank. */
export function FolderPathCell({ words }: { words: string }) {
  return (
    <span className="artifacts-folder" title={words || TOP_LEVEL}>
      {words || TOP_LEVEL}
    </span>
  );
}

/** When a row last changed, relative, with the instant on hover. */
export function EditedCell({ at, now }: { at: string; now: number }) {
  return (
    <time className="artifacts-when" dateTime={at} title={at}>
      {relTime(at, now)}
    </time>
  );
}

/**
 * A cell a row has nothing to say in: a folder holds no role of its own, so
 * its row leaves the Your role cell quiet and empty rather than drawing a
 * word or a control that does nothing.
 */
export const quietCell = (): ReactNode => null;

/** What a row says in each fact column. */
export interface ListingFactCells<T> {
  access: (row: T) => ReactNode;
  owner: (row: T) => ReactNode;
  folder: (row: T) => ReactNode;
  updated: (row: T) => ReactNode;
  role: (row: T) => ReactNode;
  trashed: (row: T) => ReactNode;
  purge: (row: T) => ReactNode;
}

/**
 * THE FACT COLUMNS, ONE SET FOR EVERY GRID OF A PLACE. A place draws its
 * folders' rows and its artifacts' rows as two grids, one above the other, and
 * the two used to choose their own columns: the folders' Access track was
 * wider, they kept an Owner column on Yours and never a Your role, so Access,
 * Owner and Edited stood at different places in the two. Both grids now take
 * their columns from here, for the same view, and differ only in what each
 * row says in a cell.
 *
 * ON YOURS EVERY ROW IS THE READER'S, so its owner and their role would say
 * "You" and "Owner" on every row; the trash's rows say when each went in and
 * when it goes for good in their place.
 */
export function listingFactColumns<T>(
  view: Pick<ListingView, "trashed" | "own" | "folders">,
  cells: ListingFactCells<T>,
): DataGridColumn<T>[] {
  const { trashed, own } = view;
  return [
    // WHO CAN OPEN IT, but not in the trash: a trashed artifact opens for
    // nobody until it is restored, and its rows say when it goes instead.
    ...(trashed
      ? []
      : [
          {
            key: "access",
            header: "Access",
            width: TRACK.access,
            drop: 3,
            // ON A PHONE the lead already says what it is; the line under it
            // is when it was edited, and who can open it is its page's to say.
            phoneOmit: true,
            cell: cells.access,
          },
        ]),
    ...(own
      ? []
      : [
          {
            key: "owner",
            header: "Owner",
            width: TRACK.owner,
            drop: 2,
            // ON A PHONE the row's line is the type and the time: the owner
            // and the role are the row's own page's to say.
            phoneOmit: true,
            cell: cells.owner,
          },
        ]),
    // WHERE IT IS KEPT, as the reader sees it: the folder's path in words, or
    // the top level for one in no folder (or in a folder the reader cannot
    // see, which is the top level to them). A folder grants nothing, so the
    // path is the reader's own view of the tree and never a fact about access.
    ...(view.folders && !trashed
      ? [
          {
            key: "folder",
            header: "Folder",
            width: TRACK.folder,
            drop: 2,
            phoneOmit: true,
            cell: cells.folder,
          },
        ]
      : []),
    trashed
      ? { key: "trashed", header: "Trashed", width: TRACK.when, cell: cells.trashed }
      : { key: "updated", header: "Edited", width: TRACK.when, cell: cells.updated },
    ...(trashed
      ? [
          {
            key: "purge",
            header: "Deleted for good",
            width: TRACK.purge,
            drop: 1,
            cell: cells.purge,
          },
        ]
      : own
        ? []
        : [
            {
              key: "role",
              header: "Your role",
              width: TRACK.role,
              drop: 1,
              phoneOmit: true,
              cell: cells.role,
            },
          ]),
  ];
}

/** One listing, drawn in the shape the reader chose. */
export function Listing({
  items,
  view,
  name,
  loadMore,
  loadingMore,
  total,
}: {
  items: ArtifactListItem[];
  view: ListingView;
  name: string;
  loadMore?: (() => void) | undefined;
  loadingMore?: boolean;
  total?: number | undefined;
}) {
  const note =
    total !== undefined && total > items.length
      ? `${fmtCount(items.length)} of ${fmtCount(total)}`
      : undefined;
  const { people, viewer, now, onChanged, selection } = view;
  if (view.shape === "grid") {
    return (
      <>
        <ul className="artifacts-cards" role="list">
          {items.map((item) => (
            <li key={item.id}>
              <ListedArtifactCard
                item={item}
                people={people}
                now={now}
                actions={<ArtifactMenu item={item} onChanged={onChanged} />}
                selection={
                  selection
                    ? {
                        selecting: selection.selecting,
                        selected: selection.selected.has(item.id),
                        onSelectedChange: (on, change) => selection.pick(item.id, on, change.range),
                      }
                    : undefined
                }
              />
            </li>
          ))}
        </ul>
        {loadMore && (
          <div className="artifacts-more">
            <Button size="small" variant="secondary" onClick={loadMore} loading={loadingMore}>
              Load more
            </Button>
            {note && <span className="artifacts-more-note">{note}</span>}
          </div>
        )}
      </>
    );
  }
  // THE FACT COLUMNS ARE THE PLACE'S ONE SET (`listingFactColumns`), shared
  // with the folders' rows above.
  const columns: DataGridColumn<ArtifactListItem>[] = [
    {
      key: "title",
      header: "Name",
      floor: "14rem",
      phoneLead: true,
      // THE KIND IS PRINTED UNDER THE TITLE, beside the description, so the
      // list keeps no Type column of its own: the mark and its word are the
      // grid card's badge, said in a row.
      cell: (item) => (
        <ArtifactNameCell
          kind={item.kind}
          title={item.title}
          detail={item.description}
          pinned={item.pinned ?? false}
          trashed={item.status === "trashed"}
          labels={{ kinds: KIND_WORDS }}
        />
      ),
    },
    ...listingFactColumns<ArtifactListItem>(view, {
      access: (item) => <ArtifactAccessLabel access={accessOf(item)} />,
      owner: (item) => <OwnerCell owner={item.owner} viewer={viewer} people={people} />,
      folder: (item) => <FolderPathCell words={pathWords(item.folder_path)} />,
      updated: (item) => <EditedCell at={item.updated_at} now={now} />,
      role: (item) => <span className="artifacts-role">{ROLE_WORDS[item.role]}</span>,
      trashed: (item) => (
        <time dateTime={item.trashed_at} title={item.trashed_at}>
          {relTime(item.trashed_at, now)}
        </time>
      ),
      purge: (item) => {
        const at = purgeAt(item.trashed_at);
        return at ? (
          <time dateTime={at} title={purgeSentence(item.trashed_at)}>
            {fmtDate(at)}
          </time>
        ) : (
          `${TRASH_KEPT} after`
        );
      },
    }),
    {
      key: "actions",
      header: "",
      action: true,
      shrink: true,
      cell: (item) => <ArtifactMenu item={item} onChanged={onChanged} />,
    },
  ];
  return (
    <div className="artifacts-list">
      <DataGrid
        rows={items}
        columns={columns}
        rowKey={(item) => item.id}
        rowHref={(item) => href(artifactPath(item.id))}
        serverSorted
        name={name}
        phoneRows="compact"
        onLoadMore={loadMore}
        loadedNote={note}
        // THE BOXES ARE A MODE, as on the cards: drawn while the reader is
        // choosing, and a list of names otherwise.
        selection={
          selection?.selecting
            ? {
                selected: selection.selected,
                onChange: selection.replace,
                rowLabel: (item) => `Select ${item.title}`,
              }
            : undefined
        }
      />
    </div>
  );
}
