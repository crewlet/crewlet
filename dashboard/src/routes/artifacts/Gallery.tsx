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
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArtifactKindGlyph,
  Button,
  ButtonLink,
  Input,
  Menu,
  Meter,
  Select,
  Skeleton,
  Tag,
  useToast,
} from "@crewlethq/ui";
import {
  EllipsisGlyph,
  PinGlyph,
  PlusGlyph,
  SearchGlyph,
  CopyGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import type { ArtifactSection } from "~/app/routes.ts";
import { QueryState, Section } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes, fmtCount, relTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { artifactPath } from "~/lib/artifacts/api.ts";
import { usePinToggle, usePinsChanged } from "~/lib/artifacts/pins.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople, type ArtifactPeople } from "~/lib/artifacts/people.ts";
import { KIND_WORDS, KINDS, ROLE_WORDS, isArtifactKind } from "~/lib/artifacts/words.ts";
import { ListedArtifactCard, copyAddress } from "./parts.tsx";
import { ARTIFACT_LIST_SORTS } from "~/contract/artifacts.ts";
import {
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

/** The owner of a row as the reader knows them. */
function ownerCell(item: ArtifactListItem, people: ArtifactPeople) {
  return people.seatName(item.owner);
}

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
  const [shape, setShape] = useParam("shape", "list");
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

  return (
    <>
      <GalleryBar
        q={q}
        onQ={setQ}
        kind={kind}
        onKind={setKind}
        sort={sort}
        onSort={(next) => setSort(next === DEFAULT_SORT[section] ? "" : next)}
        shape={shape === "cards" ? "cards" : "list"}
        onShape={(next) => setShape(next === "list" ? "" : next)}
        canAuthor={author.can ? null : author.reason}
      />
      {section === "mine" && (
        <div className="artifacts-trash-toggle">
          <Button
            size="small"
            variant={trashed ? "secondary" : "ghost"}
            leadingIcon={<TrashGlyph size="sm" />}
            aria-pressed={trashed}
            onClick={() => setTrash(trashed ? "" : "trashed")}
          >
            {trashed ? "Back to your artifacts" : "In the trash"}
          </Button>
        </div>
      )}
      {section === "for-you" && pinned.items.length > 0 && (
        <Section title="Pinned" hint="Your own pins, on every device you sign in on">
          <Listing
            items={pinned.items}
            shape={shape === "cards" ? "cards" : "list"}
            people={people}
            now={now}
            name="artifacts-pinned"
          />
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
                  hint: "An artifact you trash stays here for 30 days, then is deleted.",
                }
              : q || kind
                ? { title: "Nothing matches", hint: "Change the phrase or the type to see more." }
                : EMPTY[section]
            : undefined
        }
      >
        {rows.length > 0 && (
          <Listing
            items={rows}
            shape={shape === "cards" ? "cards" : "list"}
            people={people}
            now={now}
            name={`artifacts-${section}`}
            loadMore={listing.after ? listing.loadMore : undefined}
            loadingMore={listing.loadingMore}
            total={listing.first.data?.total}
          />
        )}
      </QueryState>
      {viewer.admin && <Usage />}
    </>
  );
}

/** The gallery's bar: the phrase, the kind, the order, the shape and New. */
function GalleryBar({
  q,
  onQ,
  kind,
  onKind,
  sort,
  onSort,
  shape,
  onShape,
  canAuthor,
}: {
  q: string;
  onQ: (q: string) => void;
  kind: string;
  onKind: (kind: string) => void;
  sort: ArtifactListSort;
  onSort: (sort: ArtifactListSort) => void;
  shape: "list" | "cards";
  onShape: (shape: "list" | "cards") => void;
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
      <Select
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
        width="auto"
        value={sort}
        onChange={(value) => onSort(String(value) as ArtifactListSort)}
        options={ARTIFACT_LIST_SORTS.map((s) => ({ value: s, label: SORT_WORDS[s] }))}
        ariaLabel="Order"
      />
      <Select
        width="auto"
        value={shape}
        onChange={(value) => onShape(value === "cards" ? "cards" : "list")}
        options={[
          { value: "list", label: "List" },
          { value: "cards", label: "Cards" },
        ]}
        ariaLabel="Shape"
      />
      {canAuthor === null ? (
        <ButtonLink
          size="small"
          variant="primary"
          href={href(["artifacts", "new"])}
          leadingIcon={<PlusGlyph size="sm" />}
        >
          New sheet
        </ButtonLink>
      ) : (
        <Button
          size="small"
          variant="primary"
          leadingIcon={<PlusGlyph size="sm" />}
          disabledReason={canAuthor}
          title={canAuthor}
        >
          New sheet
        </Button>
      )}
    </PageActions>
  );
}

/** How long the phrase waits for the reader to pause before it is asked. */
const SEARCH_PAUSE_MS = 300;

/** One listing, drawn in the shape the reader chose. */
function Listing({
  items,
  shape,
  people,
  now,
  name,
  loadMore,
  loadingMore,
  total,
}: {
  items: ArtifactListItem[];
  shape: "list" | "cards";
  people: ArtifactPeople;
  now: number;
  name: string;
  loadMore?: (() => void) | undefined;
  loadingMore?: boolean;
  total?: number | undefined;
}) {
  const note =
    total !== undefined && total > items.length
      ? `${fmtCount(items.length)} of ${fmtCount(total)}`
      : undefined;
  if (shape === "cards") {
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
  const columns: GridColumn<ArtifactListItem>[] = [
    {
      key: "title",
      header: "Name",
      floor: "12rem",
      phoneLead: true,
      cell: (item) => (
        <span className="artifacts-name">
          <ArtifactKindGlyph kind={item.kind} size="sm" />
          <span className="truncate">{item.title}</span>
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
      shrink: true,
      cell: (item) => KIND_WORDS[item.kind],
    },
    {
      key: "owner",
      header: "Owner",
      cell: (item) => ownerCell(item, people),
      drop: 2,
    },
    {
      key: "updated",
      header: "Updated",
      shrink: true,
      cell: (item) => (
        <time dateTime={item.updated_at} title={item.updated_at}>
          {relTime(item.updated_at, now)}
        </time>
      ),
    },
    {
      key: "role",
      header: "Your role",
      shrink: true,
      drop: 1,
      cell: (item) =>
        item.status === "trashed" ? (
          <Tag variant="warning" appearance="outline">
            In the trash
          </Tag>
        ) : (
          ROLE_WORDS[item.role]
        ),
    },
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

/** A row's own actions: pin or unpin, and copy its address. */
function RowMenu({ item }: { item: ArtifactListItem }) {
  const toast = useToast();
  const pin = usePinToggle(item.id, item.pinned ?? false);
  return (
    <Menu
      label={`More for “${item.title}”`}
      icon={<EllipsisGlyph size="sm" />}
      triggerVariant="ghost"
      align="end"
      items={[
        {
          key: "pin",
          label: pin.label,
          icon: <PinGlyph size="sm" />,
          onSelect: () => void pin.toggle(),
        },
        {
          key: "copy",
          label: "Copy link",
          icon: <CopyGlyph size="sm" />,
          onSelect: () => void copyAddress(item.id, toast),
        },
      ]}
    />
  );
}

/** The company's artifacts and datastores against the plan's quotas: an administrator's. */
function Usage() {
  const artifacts = useQuery("artifact_usage", undefined, { pollMs: GALLERY_POLL_MS });
  const data = useQuery("artifact_data_usage", undefined, { pollMs: GALLERY_POLL_MS });
  const a = artifacts.data;
  const d = data.data;
  return (
    <Section
      title="Storage"
      hint="The company's artifacts against the quotas this deployment enforces"
    >
      <QueryState
        error={artifacts.error ?? data.error}
        refusal={artifacts.refusal ?? data.refusal}
        loading={artifacts.loading || data.loading}
      >
        {a && d && (
          <div className="artifacts-usage">
            <Meter
              label="Files"
              value={a.usage.bytes}
              max={a.limits.company_bytes}
              valueText={`${fmtBytes(a.usage.bytes)} of ${fmtBytes(a.limits.company_bytes)}`}
            />
            <Meter
              label="Artifacts"
              value={a.usage.artifacts}
              max={a.limits.artifacts}
              valueText={`${fmtCount(a.usage.artifacts)} of ${fmtCount(a.limits.artifacts)}`}
            />
            <Meter
              label="Data"
              value={d.usage.footprint}
              max={d.data_bytes}
              valueText={`${fmtBytes(d.usage.footprint)} of ${fmtBytes(d.data_bytes)} in ${fmtCount(d.usage.stores)} sheets and pages`}
            />
          </div>
        )}
      </QueryState>
    </Section>
  );
}
