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
 * Make row, as Worklet and Agents draw theirs.
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
import { ArtifactMakeRow, LivePreviewBudgetProvider, Section } from "@crewlethq/ui";
import { href, useNavigator, useParam, useRoute } from "~/app/router.tsx";
import { useKeymap } from "~/app/keymap.ts";
import {
  LEGACY_ARTIFACT_SECTIONS,
  artifactSectionPath,
  newArtifactPath,
  type ArtifactSection,
} from "~/app/routes.ts";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { useViewer } from "~/lib/viewer.ts";
import { offeredKinds } from "~/app/nav.ts";
import { useServicesRun } from "~/lib/services.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { TRASH_KEPT, isArtifactKind, type MakeKind } from "~/lib/artifacts/words.ts";
import { NewArtifactButton } from "./new/NewArtifactMenu.tsx";
import {
  DEFAULT_SORT,
  GalleryBar,
  GallerySelectionBar,
  GallerySkeleton,
  GalleryToolbar,
  Listing,
  listParams,
  noMatch,
  sortOf,
  useListing,
  useShape,
  type ListingView,
} from "./listing.tsx";
import { useGallerySelection } from "./selection.ts";
import { MoveDialog } from "./MoveDialog.tsx";
import type { ArtifactListScope } from "~/protocol/index.ts";
import { BrandMakeChip } from "~/components/brand.tsx";

/** The scope each section asks the engine about. All is everything the reader can see. */
const SCOPE: Readonly<Record<ArtifactSection, ArtifactListScope>> = {
  all: "all",
  yours: "mine",
  shared: "shared",
};

/** The Make row's heading: what the reader does there, said plainly. */
export const MAKE_TITLE = "Start a new artifact";

/** What each kind is called on the Make row: the kind's word, said as what one makes. */
const MAKE_WORDS: Readonly<Record<MakeKind, string>> = {
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
const MAKE_DETAILS: Readonly<Record<MakeKind, string>> = {
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
  const sort = sortOf(sortParam);
  const [shape, setShape] = useShape();
  const [trash, setTrash] = useParam("status", "");
  // THE TRASH IS THE OWNER'S: a reader's own artifacts are the ones they can
  // restore, so the trashed listing is offered on Yours.
  const trashed = section === "yours" && trash === "trashed";
  const params = useMemo(
    () => listParams({ scope: SCOPE[section], kind, q, sort, trashed }),
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

  // PICKING FOR A MOVE, over every card drawn (the pins above the rest), and
  // never in the trash, whose rows move nowhere until they are restored.
  const drawn = useMemo(
    () => [...(section === "all" && !trashed ? pinned.items : []), ...rows],
    [section, trashed, pinned.items, rows],
  );
  const selection = useGallerySelection(
    useMemo(() => drawn.map((i) => i.id), [drawn]),
    JSON.stringify(params),
  );
  const picked = useMemo(
    () => drawn.filter((i) => selection.selected.has(i.id)),
    [drawn, selection.selected],
  );
  const selectRef = useRef<HTMLButtonElement>(null);
  const [moving, setMoving] = useState(false);
  useKeymap({
    "gallery.select": { run: () => selection.setToggled(!selection.selecting), when: !trashed },
  });

  const view: ListingView = {
    trashed,
    own: section === "yours",
    folders: true,
    shape,
    people,
    viewer,
    now,
    onChanged: refetch,
    selection: trashed ? null : selection,
  };
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
        loadMore={listing.more ? listing.loadMore : undefined}
        loadingMore={listing.loadingMore}
        total={listing.first.data?.total}
      />
    ) : null;

  return (
    <LivePreviewBudgetProvider>
      <GalleryBar q={q} onQ={setQ}>
        <NewArtifactButton held={author.can ? null : author.reason} />
      </GalleryBar>
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
        select={
          trashed
            ? null
            : { on: selection.selecting, onChange: selection.setToggled, ref: selectRef }
        }
      />
      {!trashed && (
        <GallerySelectionBar
          selection={selection}
          picked={picked}
          author={author}
          onMove={() => setMoving(true)}
          returnFocusRef={selectRef}
        />
      )}
      {moving && (
        <MoveDialog
          subject={{ kind: "artifacts", items: picked }}
          onClose={() => setMoving(false)}
          onMoved={() => {
            setMoving(false);
            selection.clear();
            refetch();
          }}
        />
      )}
      {trashed && (
        <p className="artifacts-trash-note">
          Only you see your trash. An artifact stays here for {TRASH_KEPT}, then it is deleted for
          good.
        </p>
      )}
      {/* THE KIT'S SECTION HEADING for every titled part of the gallery,
          as every other page's sections take: three local titles here were
          16px, 16px and 14px at two weights. */}
      {showPinned && (
        <Section title="Pinned" className="artifacts-group">
          <Listing items={pinned.items} view={view} name="artifacts-pinned" />
        </Section>
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
            <Section title="Recent" className="artifacts-group">
              {rest}
            </Section>
          ) : (
            rest
          ))}
      </QueryState>
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
    // THE COMPANY'S BRAND beside the kinds it applies to, one press from
    // where it is set (`BrandMakeChip`).
    <Section title={MAKE_TITLE} actions={<BrandMakeChip />}>
      <ArtifactMakeRow
        aria-label={MAKE_TITLE}
        items={offeredKinds(runs).map((k) => ({
          kind: k,
          label: MAKE_WORDS[k],
          detail: MAKE_DETAILS[k],
          href: href(newArtifactPath(k)),
        }))}
      />
    </Section>
  );
}
