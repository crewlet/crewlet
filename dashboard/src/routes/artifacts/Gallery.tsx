/**
 * The Artifacts gallery: what the company and its agents made that this reader
 * can open, by section.
 *
 * # One listing, five questions
 *
 * Every section is the engine's `artifacts` question under one scope: For you
 * is every artifact the reader can see, the ones they opened most lately
 * first, with their pins above; Mine, Shared with you, Company and By agents
 * are the engine's own scopes. The engine filters the listing by the reader's
 * relations and decides every row by `get_artifact`, so nothing here filters
 * what the engine showed: a row this reader may not open is never in the
 * answer, and the admin path lists nothing it would have to record.
 *
 * # Every list is one grid
 *
 * The list shape is the frame's `DataGrid`, sorted by the engine (`sort=` is
 * its question, so a page of results is ordered across the whole set); the
 * card shape draws the same rows as the design system's `ArtifactCard`. The
 * kind, the phrase, the order and the shape are the workspace's own question,
 * kept across the sections.
 *
 * # The bar finds, the toolbar arranges
 *
 * The page bar holds what every workspace's bar holds: the find field and the
 * one primary action, New. How the answer is cut (the type, the order, the
 * trash and the shape) is the toolbar's, the row under the sections, as Work
 * and Agents draw theirs. All five in the bar stood the title alone on a line
 * at 1024 and ran off the screen's edge on a phone.
 *
 * # Who owns it
 *
 * The reader's own is "You", and a column that would say so on every row (the
 * owner and the role, on Mine) is not drawn. Anybody else is named by the
 * chart, and an owner who acts as no seat by their login's words
 * (`lib/artifacts/people.ts`), never the credential's spelling.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArtifactKindGlyph,
  Button,
  ButtonLink,
  Input,
  Menu,
  Meter,
  SegmentedControl,
  Select,
  Skeleton,
  StatCard,
  StatGroup,
  useToast,
} from "@crewlethq/ui";
import {
  CopyGlyph,
  EllipsisGlyph,
  LayoutGridGlyph,
  ListGlyph,
  PinGlyph,
  PlusGlyph,
  SearchGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import type { ArtifactSection } from "~/app/routes.ts";
import { QueryState, Section } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes, fmtCount, fmtDate, plural, relTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { artifactPath } from "~/lib/artifacts/api.ts";
import { usePinToggle, usePinsChanged } from "~/lib/artifacts/pins.ts";
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
import { ListedArtifactCard, copyAddress } from "./parts.tsx";
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

/** The scope each section asks the engine about. For you is everything the reader can see. */
const SCOPE: Readonly<Record<ArtifactSection, ArtifactListScope>> = {
  "for-you": "all",
  mine: "mine",
  shared: "shared",
  company: "company",
  agents: "agents",
};

/** Each section's own order when the reader chose none: what they opened lately, on For you. */
const DEFAULT_SORT: Readonly<Record<ArtifactSection, ArtifactListSort>> = {
  "for-you": "opened",
  mine: "updated",
  shared: "updated",
  company: "updated",
  agents: "updated",
};

/** What each order is called in the bar. */
const SORT_WORDS: Readonly<Record<ArtifactListSort, string>> = {
  updated: "Recently changed",
  created: "Newest",
  title: "Title, A to Z",
  opened: "Recently opened",
};

/** What a section says when it holds nothing, and what to do about it. */
const EMPTY: Readonly<Record<ArtifactSection, { title: string; hint: string }>> = {
  "for-you": {
    title: "Nothing here yet",
    hint: "Artifacts you open, or that are shared with you, appear here.",
  },
  mine: {
    title: "You have not made an artifact yet",
    hint: "Start a sheet, or ask an agent to publish a page or a document.",
  },
  shared: {
    title: "Nothing is shared with you",
    hint: "When somebody shares an artifact with you, your team or the company, it appears here.",
  },
  company: {
    title: "Nothing is shared with the whole company",
    hint: "An artifact's owner shares it with everyone from its Share dialog.",
  },
  agents: {
    title: "No agent has published an artifact you can read",
    hint: "When an agent shares a page, a document or a sheet with you, it appears here.",
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
  return { first, items, after, loadMore, loadingMore };
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
type Shape = "list" | "cards";

/**
 * The fact columns' tracks, FIXED rather than sized to their contents: For you
 * draws Pinned and the rest as two grids one above the other, and a page
 * loaded later is the same grid grown, so a track sized to what one grid holds
 * put "Type" at two places on one screen and moved it when a page came in.
 * Each is its widest value at the body size ("Dashboard", "Nov 18, 2026",
 * "Can comment"); a longer owner's name is cut, with the whole of it on hover.
 */
const TRACK = {
  kind: "7rem",
  owner: "11rem",
  when: "7.5rem",
  role: "8rem",
  purge: "10rem",
} as const;

export function ArtifactGallery({ section }: { section: ArtifactSection }) {
  const people = useArtifactPeople();
  const now = useNow();
  const viewer = useViewer();
  const [kindParam, setKind] = useParam("kind", "");
  const kind = isArtifactKind(kindParam) ? kindParam : "";
  const [q, setQ] = useParam("q", "");
  const [sortParam, setSort] = useParam("sort", "");
  const sort: ArtifactListSort = (ARTIFACT_LIST_SORTS as readonly string[]).includes(sortParam)
    ? (sortParam as ArtifactListSort)
    : DEFAULT_SORT[section];
  const [shapeParam, setShape] = useParam("shape", "list");
  const shape: Shape = shapeParam === "cards" ? "cards" : "list";
  const [trash, setTrash] = useParam("status", "");
  // THE TRASH IS THE OWNER'S: a reader's own artifacts are the ones they can
  // restore, so the trashed listing is offered on Mine.
  const trashed = section === "mine" && trash === "trashed";
  const params = useMemo(
    () => listParams(SCOPE[section], kind, q, sort, trashed),
    [section, kind, q, sort, trashed],
  );
  const listing = useListing(params);
  // PINNED FIRST, on For you: the reader's own pins, which follow them across
  // devices, above what they opened lately.
  const pinnedParams = useMemo(() => ({ ...params, pinned: true, sort: "updated" }), [params]);
  const pinned = useListing(pinnedParams, section === "for-you" && !trashed);
  const author = useArtifactAccess({ app: true });

  const rows = useMemo(() => {
    if (section !== "for-you") return listing.items;
    const pins = new Set(pinned.items.map((i) => i.id));
    return listing.items.filter((i) => !pins.has(i.id));
  }, [section, listing.items, pinned.items]);

  const view: ListingView = { section, trashed, shape, people, viewer, now };
  const showPinned = section === "for-you" && pinned.items.length > 0;
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
    <>
      <GalleryBar q={q} onQ={setQ} canAuthor={author.can ? null : author.reason} />
      <GalleryToolbar
        kind={kind}
        onKind={setKind}
        sort={sort}
        onSort={(next) => setSort(next === DEFAULT_SORT[section] ? "" : next)}
        shape={shape}
        onShape={(next) => setShape(next === "list" ? "" : next)}
        trash={
          section === "mine"
            ? { on: trashed, onChange: (on) => setTrash(on ? "trashed" : "") }
            : null
        }
      />
      {showPinned && (
        <Section title="Pinned" hint="Your own pins, on every device you sign in on">
          <Listing items={pinned.items} view={view} name="artifacts-pinned" />
        </Section>
      )}
      {listing.first.loading && <Skeleton variant="list" rows={6} label="Loading artifacts" />}
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
              : q || kind
                ? noMatch(q.trim(), kind)
                : EMPTY[section]
            : undefined
        }
      >
        {rest &&
          // UNDER PINNED, THE REST IS A SECTION TOO: a titled list over an
          // untitled one read as the pins' list running on.
          (showPinned ? <Section title="Other artifacts">{rest}</Section> : rest)}
      </QueryState>
      {/* THE COMPANY'S STORAGE IS AN ADMINISTRATOR'S OVERVIEW, said once on
          For you rather than under every section, the trash and a search. */}
      {viewer.admin && section === "for-you" && !trashed && !q && !kind && <Usage />}
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
      <span className="artifacts-find">
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
        <ButtonLink
          size="small"
          variant="primary"
          href={href(["artifacts", "new"])}
          leadingIcon={<PlusGlyph size="sm" />}
        >
          New
        </ButtonLink>
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
 * trash on Mine, and the shape it is drawn in.
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
    <div className="toolbar artifacts-bar">
      <Select
        size="sm"
        width="auto"
        value={kind}
        onChange={(value) => onKind(String(value))}
        options={[
          { value: "", label: "Every type" },
          ...KINDS.map((k) => ({ value: k, label: KIND_WORDS[k] })),
        ]}
        ariaLabel="Type"
        placeholder="Every type"
        active={kind !== ""}
      />
      <Select
        size="sm"
        width="auto"
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
          // pressed again the reader's own. "Back to your artifacts" beside a
          // trash glyph named the way out with the way in's picture.
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
            { value: "list", icon: <ListGlyph size="sm" />, srLabel: "List", title: "List" },
            {
              value: "cards",
              icon: <LayoutGridGlyph size="sm" />,
              srLabel: "Cards",
              title: "Cards",
            },
          ]}
        />
      </span>
    </div>
  );
}

/** What every listing on the gallery is drawn for: its section, its shape and its reader. */
interface ListingView {
  section: ArtifactSection;
  /** The trash's listing, on Mine. */
  trashed: boolean;
  shape: Shape;
  people: ArtifactPeople;
  viewer: Pick<ViewerState, "owner">;
  now: number;
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
  const { section, trashed, people, viewer, now } = view;
  if (view.shape === "cards") {
    return (
      <>
        <ul className="artifacts-cards" role="list">
          {items.map((item) => (
            <li key={item.id}>
              <ListedArtifactCard item={item} people={people} now={now} />
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
  // ON MINE EVERY ROW IS THE READER'S, so its owner and their role would say
  // "You" and "Owner" on every row; the trash's rows say when each went in
  // and when it goes for good in their place.
  const own = section === "mine";
  const columns: GridColumn<ArtifactListItem>[] = [
    {
      key: "title",
      header: "Name",
      floor: "12rem",
      phoneLead: true,
      cell: (item) => (
        <span className="artifacts-name">
          <ArtifactKindGlyph kind={item.kind} size="sm" />
          <span className="truncate" title={item.title}>
            {item.title}
          </span>
          {item.pinned && (
            <span className="artifacts-pin" title="Pinned">
              <PinGlyph size="xs" aria-hidden="true" />
              <span className="sr-only">Pinned</span>
            </span>
          )}
        </span>
      ),
    },
    {
      key: "kind",
      header: "Type",
      width: TRACK.kind,
      cell: (item) => KIND_WORDS[item.kind],
    },
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
            cell: (item: ArtifactListItem) => {
              const who = whoWords(item.owner, viewer, people);
              return (
                <span className="truncate" title={who}>
                  {who}
                </span>
              );
            },
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
          header: "Updated",
          width: TRACK.when,
          cell: (item: ArtifactListItem) => (
            <time dateTime={item.updated_at} title={item.updated_at}>
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
              cell: (item: ArtifactListItem) => ROLE_WORDS[item.role],
            },
          ]),
    {
      key: "actions",
      header: "",
      action: true,
      shrink: true,
      cell: (item) => <RowMenu item={item} />,
    },
  ];
  return (
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
  );
}

/**
 * A row's own actions: pin or unpin, and copy its address. A row in the trash
 * is not pinned: a pin is a way back to something open, and the trash's row
 * opens the page that restores it.
 */
function RowMenu({ item }: { item: ArtifactListItem }) {
  const toast = useToast();
  const pin = usePinToggle(item.id, item.pinned ?? false);
  const copy = {
    key: "copy",
    label: "Copy link",
    icon: <CopyGlyph size="sm" />,
    onSelect: () => void copyAddress(item.id, toast),
  };
  return (
    <Menu
      label={`More for “${item.title}”`}
      icon={<EllipsisGlyph size="sm" />}
      triggerVariant="ghost"
      align="end"
      items={
        item.status === "trashed"
          ? [copy]
          : [
              {
                key: "pin",
                label: pin.label,
                icon: <PinGlyph size="sm" />,
                onSelect: () => void pin.toggle(),
              },
              copy,
            ]
      }
    />
  );
}

/**
 * The company's artifacts and datastores against the plan's quotas: an
 * administrator's. Each quota is the design system's stat tile with its bar
 * under the number, the bar being the reading, on the one card a row of them
 * draws (a meter is drawn on a card).
 *
 * THE COUNT IS THE COMPANY'S, not the listing's: it holds every artifact the
 * quota is charged for, the ones in anybody's trash until they are deleted for
 * good and the private ones no listing shows this reader, so it says so
 * rather than leaving "12 of 50k" over eleven rows to read as a mistake.
 */
function Usage() {
  const artifacts = useQuery("artifact_usage", undefined, { pollMs: GALLERY_POLL_MS });
  const data = useQuery("artifact_data_usage", undefined, { pollMs: GALLERY_POLL_MS });
  const a = artifacts.data;
  const d = data.data;
  return (
    <Section title="Storage" hint="What the company holds against this deployment's quotas">
      <QueryState
        error={artifacts.error ?? data.error}
        refusal={artifacts.refusal ?? data.refusal}
        loading={artifacts.loading || data.loading}
      >
        {a && d && (
          <StatGroup columns={3}>
            <StatCard
              flush
              label="Files"
              value={fmtBytes(a.usage.bytes)}
              unit={`of ${fmtBytes(a.limits.company_bytes)}`}
              sub={`Every version of ${plural(a.usage.artifacts, "artifact")}`}
              trendPlacement="below"
              trend={
                <Meter
                  hideLabel
                  label="Files"
                  value={a.usage.bytes}
                  max={a.limits.company_bytes}
                  valueText={`${fmtBytes(a.usage.bytes)} of ${fmtBytes(a.limits.company_bytes)}`}
                />
              }
            />
            <StatCard
              flush
              label="Artifacts"
              value={fmtCount(a.usage.artifacts)}
              unit={`of ${fmtCount(a.limits.artifacts)}`}
              sub="Counts private ones and the trash"
              trendPlacement="below"
              trend={
                <Meter
                  hideLabel
                  label="Artifacts"
                  value={a.usage.artifacts}
                  max={a.limits.artifacts}
                  valueText={`${fmtCount(a.usage.artifacts)} of ${fmtCount(a.limits.artifacts)}`}
                />
              }
            />
            <StatCard
              flush
              label="Data"
              value={fmtBytes(d.usage.footprint)}
              unit={`of ${fmtBytes(d.data_bytes)}`}
              sub={`In ${plural(d.usage.stores, "sheet or page", "sheets and pages")}`}
              trendPlacement="below"
              trend={
                <Meter
                  hideLabel
                  label="Data"
                  value={d.usage.footprint}
                  max={d.data_bytes}
                  valueText={`${fmtBytes(d.usage.footprint)} of ${fmtBytes(d.data_bytes)}`}
                />
              }
            />
          </StatGroup>
        )}
      </QueryState>
    </Section>
  );
}
