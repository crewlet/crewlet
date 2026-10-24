/**
 * The Artifacts gallery: what the company and its agents made that this reader
 * can open, as pictures of the artifacts themselves.
 *
 * # Three questions
 *
 * All is everything the reader can see, the latest activity first, with their
 * pins above it; Yours is what they own, with their trash beside it; Shared
 * with you is what somebody else shared with them, their team or the whole
 * company. Each is the engine's `artifacts` question under one scope. The
 * engine filters the listing by the reader's relations and decides every row
 * by `get_artifact`, so nothing here filters what the engine showed: a row
 * this reader may not open is never in the answer.
 *
 * An earlier gallery had five sections; an address of one is rewritten in
 * place to the section it became (`LEGACY_ARTIFACT_SECTIONS`).
 *
 * # Start a new artifact, then the work
 *
 * All and Yours open on a quiet tray of the kinds a reader can start, each
 * its glyph on its own tint, its name and what it is for (`ArtifactMakeRow`),
 * a toolbar for making things rather than a hero, and then the artifacts as
 * a grid of cards whose previews are the artifacts' own content drawn small
 * (`GalleryPreview`). The list is the second shape, kept for a reader who
 * scans names, dates and roles: each row the kind's mark on its tint beside
 * the title, the kind's word and the description under it
 * (`ArtifactNameCell`), who can open it (`ArtifactAccessLabel`), its owner's
 * badge and name, when it was edited and the reader's role, so a list reads
 * as the grid's cards do rather than as a ledger; which one a browser shows is remembered in
 * the browser (`STORAGE_KEYS.artifactsShape`) rather than the address, so a
 * link somebody pastes opens in the reader's own shape.
 *
 * # The bar finds, the toolbar arranges
 *
 * The page bar holds the find field and New. How the answer is cut (the type,
 * the order, the trash and the shape) is the toolbar's, quiet and under the
 * Make row, as Work and Agents draw theirs.
 *
 * # Who owns it
 *
 * The reader's own is "You", and a column that would say so on every row (the
 * owner and the role, on Yours) is not drawn. Anybody else is named by the
 * chart, and an owner who acts as no seat by their login's words
 * (`lib/artifacts/people.ts`), never the credential's spelling.
 *
 * # Storage
 *
 * What the company holds against its quotas is an administrator's overview,
 * not a reader's question, so it is one quiet line at the foot of All rather
 * than a row of tiles above the work.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArtifactAccessLabel,
  ArtifactCardSkeleton,
  ArtifactKindGlyph,
  ArtifactMakeRow,
  ArtifactNameCell,
  Button,
  Input,
  LivePreviewBudgetProvider,
  Meter,
  SegmentedControl,
  Select,
  Skeleton,
  TableEntityCell,
  PrincipalAvatar,
} from "@crewlethq/ui";
import {
  LayoutGridGlyph,
  ListGlyph,
  PinGlyph,
  PlusGlyph,
  SearchGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam, useRoute } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import {
  LEGACY_ARTIFACT_SECTIONS,
  artifactPath,
  artifactSectionPath,
  newArtifactPath,
  type ArtifactSection,
} from "~/app/routes.ts";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes, fmtCount, fmtDate, relTime } from "~/lib/format.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { offeredKinds } from "~/app/nav.ts";
import { useServicesRun } from "~/lib/services.ts";
import { usePinsChanged } from "~/lib/artifacts/pins.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople, type ArtifactPeople } from "~/lib/artifacts/people.ts";
import {
  KIND_WORDS,
  KINDS,
  ROLE_WORDS,
  TRASH_KEPT,
  isArtifactKind,
  purgeAt,
  purgeSentence,
} from "~/lib/artifacts/words.ts";
import { ListedArtifactCard, accessOf } from "./parts.tsx";
import { ArtifactMenu } from "./GalleryMenu.tsx";
import { NewArtifactMenu } from "./new/NewArtifactMenu.tsx";
import { ARTIFACT_LIST_SORTS } from "~/contract/artifacts.ts";
import {
  type ArtifactKind,
  type ArtifactListAnswer,
  type ArtifactListItem,
  type ArtifactListScope,
  type ArtifactListSort,
} from "~/protocol/index.ts";

/** How often a gallery reads itself again: no push moves a listing, and a minute keeps a left-open tab current. */
const GALLERY_POLL_MS = 60_000;

/** A page of the listing: the engine's own default, and the most a page asks for. */
const PAGE = 50;

/** The scope each section asks the engine about. All is everything the reader can see. */
const SCOPE: Readonly<Record<ArtifactSection, ArtifactListScope>> = {
  all: "all",
  yours: "mine",
  shared: "shared",
};

/**
 * The order a section is listed in when the reader chose none: the latest
 * activity first, which is the time every card says ("Edited 20h ago"), so a
 * grid reads top to bottom as a timeline. What the reader opened lately is
 * one choice away, as Recently opened.
 */
const DEFAULT_SORT: ArtifactListSort = "updated";

/** What each order is called in the bar. */
const SORT_WORDS: Readonly<Record<ArtifactListSort, string>> = {
  updated: "Recent activity",
  created: "Newest",
  title: "Title, A to Z",
  opened: "Recently opened",
};

/** The Make row's heading: what the reader does there, said plainly. */
export const MAKE_TITLE = "Start a new artifact";

/** What each kind is called on the Make row: the kind's word, said as what one makes. */
const MAKE_WORDS: Readonly<Record<ArtifactKind, string>> = {
  doc: "Docs",
  slides: "Slides",
  design: "Design",
  dashboard: "Dashboard",
  html: "HTML",
  sheet: "Sheet",
};

/**
 * What each kind is for, in a few words under its name on the Make row: what
 * a reader gets, never who makes it, since most kinds are written by a person
 * or asked of an agent alike.
 */
const MAKE_DETAILS: Readonly<Record<ArtifactKind, string>> = {
  doc: "Write and print",
  slides: "A deck to present",
  design: "Screens on a canvas",
  dashboard: "Live figures",
  html: "Any web page",
  sheet: "A table to fill in",
};

/**
 * What a section says when it holds nothing to a reader who cannot make an
 * artifact: no Make row is drawn for them, and "Start one above" pointed at
 * nothing.
 */
const EMPTY_READER: Readonly<Record<ArtifactSection, { title: string; hint: string }>> = {
  all: { title: "Nothing here yet", hint: "Artifacts shared with you appear here." },
  yours: {
    title: "You have not made an artifact yet",
    hint: "Ask an agent to make one for you, and it appears here.",
  },
  shared: {
    title: "Nothing is shared with you",
    hint: "When somebody shares an artifact with you, your team or the company, it appears here.",
  },
};

/** What a section says when it holds nothing, and what to do about it. */
const EMPTY: Readonly<Record<ArtifactSection, { title: string; hint: string }>> = {
  all: {
    title: "Nothing here yet",
    hint: "Start one above, or ask an agent to make one. Artifacts shared with you appear here too.",
  },
  yours: {
    title: "You have not made an artifact yet",
    hint: "Start a doc, a deck or a sheet above, or ask an agent to publish a page, a design or a dashboard for you.",
  },
  shared: {
    title: "Nothing is shared with you",
    hint: "When somebody shares an artifact with you, your team or the company, it appears here.",
  },
};

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

/** The listing's parameters as the engine reads them. */
function listParams(
  scope: ArtifactListScope,
  kind: string,
  q: string,
  sort: ArtifactListSort,
  trashed: boolean,
): Record<string, unknown> {
  return {
    scope,
    sort,
    limit: PAGE,
    ...(kind ? { kind } : {}),
    ...(q.trim() ? { q: q.trim() } : {}),
    ...(trashed ? { status: "trashed" } : {}),
  };
}

/**
 * One listing, its first page live and the pages after it fetched on demand.
 * A change of question drops the pages after the first: they answered another
 * question.
 */
function useListing(params: Record<string, unknown>, enabled = true) {
  const { socket } = useClient();
  const first = useQuery("artifacts", params, { pollMs: GALLERY_POLL_MS, enabled });
  // A PIN MOVES A ROW between the pinned list and the rest, and marks it.
  usePinsChanged(first.refetch);
  const key = JSON.stringify(params);
  const [more, setMore] = useState<{ key: string; items: ArtifactListItem[]; after: string }>({
    key,
    items: [],
    after: "",
  });
  const [loadingMore, setLoadingMore] = useState(false);
  const extra = more.key === key ? more : { key, items: [], after: "" };
  const after = extra.items.length > 0 ? extra.after : (first.data?.after ?? "");
  const loadMore = useCallback(() => {
    if (!after || loadingMore) return;
    setLoadingMore(true);
    socket
      .query("artifacts", { ...params, cursor: after })
      .then((page: ArtifactListAnswer) =>
        setMore((was) => ({
          key,
          items: [...(was.key === key ? was.items : []), ...page.items],
          after: page.after ?? "",
        })),
      )
      .catch(() => {
        // A PAGE THAT DID NOT COME is asked for again by the same button:
        // the rows already drawn stay, and nothing is claimed about the rest.
      })
      .finally(() => setLoadingMore(false));
  }, [after, loadingMore, socket, params, key]);
  const items = useMemo(() => {
    const seen = new Set<string>();
    const out: ArtifactListItem[] = [];
    for (const item of [...(first.data?.items ?? []), ...extra.items]) {
      if (seen.has(item.id)) continue;
      seen.add(item.id);
      out.push(item);
    }
    return out;
  }, [first.data, extra.items]);
  // A RENAME, THE TRASH OR A RESTORE moves rows on any page: the pages after
  // the first are dropped with the first read again, as a new question would.
  const refetch = useCallback(() => {
    setMore({ key, items: [], after: "" });
    first.refetch();
  }, [first, key]);
  return { first, items, after, loadMore, loadingMore, refetch };
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

/** How a listing is drawn. */
type Shape = "grid" | "list";

/**
 * The shape this browser draws the gallery in: the grid unless the reader
 * chose the list here before. A preference that could not be read or kept
 * (a private window) is the grid, every time.
 */
function useShape(): [Shape, (next: Shape) => void] {
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

/**
 * The fact columns' tracks, FIXED rather than sized to their contents: All
 * draws Pinned and the rest as two grids one above the other, and a page
 * loaded later is the same grid grown, so a track sized to what one grid holds
 * put a column at two places on one screen and moved it when a page came in.
 * Each is its widest value at the body size ("Company" beside its glyph,
 * "Nov 18, 2026", "Can comment"); a longer owner's name is cut, with the
 * whole of it on hover.
 */
const TRACK = {
  access: "7.5rem",
  owner: "11rem",
  when: "7.5rem",
  role: "8rem",
  purge: "10rem",
} as const;

/** How many card placeholders the grid draws while its first page loads: two rows at three across. */
const SKELETON_CARDS = 6;

export function ArtifactGallery({ section }: { section: ArtifactSection }) {
  const people = useArtifactPeople();
  const now = useNow();
  const viewer = useViewer();
  const route = useRoute();
  const nav = useNavigator();
  // AN EARLIER GALLERY'S ADDRESS (`#/artifacts/mine`), read as the section it
  // became and rewritten in place, so Back never lands on the old spelling.
  const spelled = route.path[1];
  useEffect(() => {
    if (spelled !== undefined && Object.hasOwn(LEGACY_ARTIFACT_SECTIONS, spelled)) {
      nav.replace(artifactSectionPath(section), route.query);
    }
  }, [spelled, section, nav, route.query]);
  const [kindParam, setKind] = useParam("kind", "");
  const kind = isArtifactKind(kindParam) ? kindParam : "";
  const [q, setQ] = useParam("q", "");
  const [sortParam, setSort] = useParam("sort", "");
  const sort: ArtifactListSort = (ARTIFACT_LIST_SORTS as readonly string[]).includes(sortParam)
    ? (sortParam as ArtifactListSort)
    : DEFAULT_SORT;
  const [shape, setShape] = useShape();
  const [trash, setTrash] = useParam("status", "");
  // THE TRASH IS THE OWNER'S: a reader's own artifacts are the ones they can
  // restore, so the trashed listing is offered on Yours.
  const trashed = section === "yours" && trash === "trashed";
  const params = useMemo(
    () => listParams(SCOPE[section], kind, q, sort, trashed),
    [section, kind, q, sort, trashed],
  );
  const listing = useListing(params);
  // PINNED FIRST, on All: the reader's own pins, which follow them across
  // devices, above everything else.
  const pinnedParams = useMemo(() => ({ ...params, pinned: true, sort: "updated" }), [params]);
  const pinned = useListing(pinnedParams, section === "all" && !trashed);
  const author = useArtifactAccess({ app: true });
  const refetch = useCallback(() => {
    listing.refetch();
    pinned.refetch();
  }, [listing, pinned]);

  const rows = useMemo(() => {
    if (section !== "all") return listing.items;
    const pins = new Set(pinned.items.map((i) => i.id));
    return listing.items.filter((i) => !pins.has(i.id));
  }, [section, listing.items, pinned.items]);

  const view: ListingView = { section, trashed, shape, people, viewer, now, onChanged: refetch };
  const showPinned = section === "all" && !trashed && pinned.items.length > 0;
  const filtered = q.trim() !== "" || kind !== "";
  // THE MAKE ROW opens the reader's own sections, and only for a reader who
  // may start one: six tiles that each said "you cannot" would be the loudest
  // thing on a page the reader came to read. A search is a question about
  // what exists, so the row steps aside for it.
  const showMake = section !== "shared" && !trashed && !filtered && author.can;
  const rest =
    rows.length > 0 ? (
      <Listing
        items={rows}
        view={view}
        name={`artifacts-${section}`}
        loadMore={listing.after ? listing.loadMore : undefined}
        loadingMore={listing.loadingMore}
        total={listing.first.data?.total}
      />
    ) : null;

  return (
    <LivePreviewBudgetProvider>
      <GalleryBar q={q} onQ={setQ} canAuthor={author.can ? null : author.reason} />
      {showMake && <MakeRow />}
      <GalleryToolbar
        kind={kind}
        onKind={setKind}
        sort={sort}
        onSort={(next) => setSort(next === DEFAULT_SORT ? "" : next)}
        shape={shape}
        onShape={setShape}
        trash={
          section === "yours"
            ? { on: trashed, onChange: (on) => setTrash(on ? "trashed" : "") }
            : null
        }
      />
      {trashed && (
        <p className="artifacts-trash-note">
          Only you see your trash. An artifact stays here for {TRASH_KEPT}, then it is deleted for
          good.
        </p>
      )}
      {showPinned && (
        <section className="artifacts-group" aria-labelledby="artifacts-pinned">
          <h2 className="artifacts-group-title" id="artifacts-pinned">
            <PinGlyph size="sm" aria-hidden="true" />
            Pinned
          </h2>
          <Listing items={pinned.items} view={view} name="artifacts-pinned" />
        </section>
      )}
      {listing.first.loading && <GallerySkeleton shape={shape} />}
      <QueryState
        error={listing.first.error}
        refusal={listing.first.refusal}
        loading={listing.first.loading}
        empty={
          rows.length === 0 && pinned.items.length === 0
            ? trashed
              ? {
                  title: "Nothing in the trash",
                  hint: `An artifact you trash stays here for ${TRASH_KEPT}, then is deleted.`,
                }
              : filtered
                ? noMatch(q.trim(), kind)
                : // THE COPY FOLLOWS THE ROW: offered to make one only where
                  // the Make row is drawn, by the same access check.
                  (author.can ? EMPTY : EMPTY_READER)[section]
            : undefined
        }
      >
        {rest &&
          // UNDER PINNED, THE REST IS A GROUP TOO: a titled grid over an
          // untitled one read as the pins' grid running on.
          (showPinned ? (
            <section className="artifacts-group" aria-labelledby="artifacts-recent">
              <h2 className="artifacts-group-title" id="artifacts-recent">
                Recent
              </h2>
              {rest}
            </section>
          ) : (
            rest
          ))}
      </QueryState>
      {viewer.admin && section === "all" && !trashed && !filtered && <Usage />}
    </LivePreviewBudgetProvider>
  );
}

/**
 * The Make row: one item per kind, each opening that kind's own create page,
 * under a heading as quiet as the groups' below it.
 */
function MakeRow() {
  const runs = useServicesRun();
  return (
    <section className="artifacts-make" aria-labelledby="artifacts-make-title">
      <h2 className="artifacts-make-title" id="artifacts-make-title">
        {MAKE_TITLE}
      </h2>
      <ArtifactMakeRow
        aria-labelledby="artifacts-make-title"
        items={offeredKinds(runs).map((k) => ({
          kind: k,
          label: MAKE_WORDS[k],
          detail: MAKE_DETAILS[k],
          href: href(newArtifactPath(k)),
        }))}
      />
    </section>
  );
}

/** The listing's placeholder, in the shape it will be drawn in, said once. */
function GallerySkeleton({ shape }: { shape: Shape }) {
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

/** The gallery's page bar: the phrase and New. */
function GalleryBar({
  q,
  onQ,
  canAuthor,
}: {
  q: string;
  onQ: (q: string) => void;
  /** Why the reader may not start one, or null when they may. */
  canAuthor: string | null;
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
          placeholder="Search artifacts"
          aria-label="Search artifacts"
        />
      </span>
      {canAuthor === null ? (
        <NewArtifactMenu />
      ) : (
        <Button
          size="small"
          variant="primary"
          leadingIcon={<PlusGlyph size="sm" />}
          disabledReason={canAuthor}
          title={canAuthor}
        >
          New
        </Button>
      )}
    </PageActions>
  );
}

/** How long the phrase waits for the reader to pause before it is asked. */
const SEARCH_PAUSE_MS = 300;

/**
 * The gallery's toolbar: the type and the order the listing is asked in, the
 * trash on Yours, and the shape it is drawn in: quiet controls on a line of
 * their own, since the cards are what the page is for.
 *
 * `toolbar` FIRST, as every screen's second row: the scroller's
 * `:has(.toolbar)` publishes `--sticky-top`, which the grid's column heads
 * offset themselves by.
 */
function GalleryToolbar({
  kind,
  onKind,
  sort,
  onSort,
  shape,
  onShape,
  trash,
}: {
  kind: string;
  onKind: (kind: string) => void;
  sort: ArtifactListSort;
  onSort: (sort: ArtifactListSort) => void;
  shape: Shape;
  onShape: (shape: Shape) => void;
  /** The trash toggle, on the section whose listing has one, or null. */
  trash: { on: boolean; onChange: (on: boolean) => void } | null;
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
      {/* THE TRASH AND THE SHAPE ARE ONE CLUSTER at the far end, so a narrow
          row moves them down together and keeps them at the end. */}
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

/** What every listing on the gallery is drawn for: its section, its shape and its reader. */
interface ListingView {
  section: ArtifactSection;
  /** The trash's listing, on Yours. */
  trashed: boolean;
  shape: Shape;
  people: ArtifactPeople;
  viewer: Pick<ViewerState, "owner">;
  now: number;
  /** A row's menu moved the listing: a rename, the trash or a restore. */
  onChanged: () => void;
}

/** One listing, drawn in the shape the reader chose. */
function Listing({
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
  const { section, trashed, people, viewer, now, onChanged } = view;
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
  // ON YOURS EVERY ROW IS THE READER'S, so its owner and their role would say
  // "You" and "Owner" on every row; the trash's rows say when each went in
  // and when it goes for good in their place.
  const own = section === "yours";
  const columns: GridColumn<ArtifactListItem>[] = [
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
            cell: (item: ArtifactListItem) => <ArtifactAccessLabel access={accessOf(item)} />,
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
            // and the role are the artifact's own page's to say.
            phoneOmit: true,
            // THE OWNER'S BADGE, as the chart draws them: a person's circle
            // and an agent's character, so the column reads by who as well
            // as by name.
            cell: (item: ArtifactListItem) => (
              <TableEntityCell
                name={whoWords(item.owner, viewer, people)}
                badge={<PrincipalAvatar principal={people.seatPrincipal(item.owner)} size="xs" />}
              />
            ),
          },
        ]),
    trashed
      ? {
          key: "trashed",
          header: "Trashed",
          width: TRACK.when,
          cell: (item: ArtifactListItem) => (
            <time dateTime={item.trashed_at} title={item.trashed_at}>
              {relTime(item.trashed_at, now)}
            </time>
          ),
        }
      : {
          key: "updated",
          header: "Edited",
          width: TRACK.when,
          cell: (item: ArtifactListItem) => (
            <time className="artifacts-when" dateTime={item.updated_at} title={item.updated_at}>
              {relTime(item.updated_at, now)}
            </time>
          ),
        },
    ...(trashed
      ? [
          {
            key: "purge",
            header: "Deleted for good",
            width: TRACK.purge,
            drop: 1,
            cell: (item: ArtifactListItem) => {
              const at = purgeAt(item.trashed_at);
              return at ? (
                <time dateTime={at} title={purgeSentence(item.trashed_at)}>
                  {fmtDate(at)}
                </time>
              ) : (
                `${TRASH_KEPT} after`
              );
            },
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
              cell: (item: ArtifactListItem) => (
                <span className="artifacts-role">{ROLE_WORDS[item.role]}</span>
              ),
            },
          ]),
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
      />
    </div>
  );
}

/**
 * The company's artifacts and datastores against the plan's quotas: an
 * administrator's, as one quiet line at the foot of All, each quota a figure
 * with a thin bar under it.
 *
 * THE COUNT IS THE COMPANY'S, not the listing's: it holds every artifact the
 * quota is charged for, the ones in anybody's trash until they are deleted for
 * good and the private ones no listing shows this reader, so it says so
 * rather than leaving "12 of 50k" over eleven cards to read as a mistake.
 */
function Usage() {
  const artifacts = useQuery("artifact_usage", undefined, { pollMs: GALLERY_POLL_MS });
  const data = useQuery("artifact_data_usage", undefined, { pollMs: GALLERY_POLL_MS });
  const a = artifacts.data;
  const d = data.data;
  if (artifacts.error || data.error || !a || !d) return null;
  const quotas = [
    {
      key: "files",
      label: "Files",
      value: a.usage.bytes,
      max: a.limits.company_bytes,
      text: `${fmtBytes(a.usage.bytes)} of ${fmtBytes(a.limits.company_bytes)}`,
    },
    {
      key: "artifacts",
      label: "Artifacts",
      value: a.usage.artifacts,
      max: a.limits.artifacts,
      text: `${fmtCount(a.usage.artifacts)} of ${fmtCount(a.limits.artifacts)}`,
    },
    {
      key: "data",
      label: "Data",
      value: d.usage.footprint,
      max: d.data_bytes,
      text: `${fmtBytes(d.usage.footprint)} of ${fmtBytes(d.data_bytes)}`,
    },
  ];
  return (
    <footer className="artifacts-storage" aria-labelledby="artifacts-storage-title">
      <h2 className="artifacts-storage-title" id="artifacts-storage-title">
        Company storage
      </h2>
      <dl className="artifacts-storage-quotas">
        {quotas.map((quota) => (
          <div key={quota.key} className="artifacts-storage-quota">
            <dt>{quota.label}</dt>
            <dd>
              <span className="artifacts-storage-figure">{quota.text}</span>
              <Meter
                hideLabel
                size="compact"
                label={quota.label}
                value={quota.value}
                max={quota.max}
                valueText={quota.text}
              />
            </dd>
          </div>
        ))}
      </dl>
      <p className="artifacts-storage-note">
        Every version of every artifact the company holds, private ones and the trash included.
      </p>
    </footer>
  );
}
