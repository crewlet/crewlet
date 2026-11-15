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
 * # What All covers, said when it is empty
 *
 * The listing is the reader's own: what they own and what is shared with
 * them, their team or the company. The engine shows no row by the admin path
 * (an administrator's read of somebody's private artifact is an override it
 * records, one artifact at a time), so an administrator who acts as no seat
 * can hold an empty All in a company full of artifacts. The empty state says
 * what the list covers rather than that nothing exists, and gives an
 * administrator the way on they do have: the audit log's Artifacts trail,
 * every artifact's history, whose rows open the artifact with that read
 * recorded.
 *
 * What the company holds against its quotas is Settings › File storage's, and
 * the gallery points nowhere at it.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArtifactMakeRow,
  ButtonLink,
  Callout,
  LivePreviewBudgetProvider,
  Section,
} from "@crewlethq/ui";
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
import { useHeldGrants, useViewer } from "~/lib/viewer.ts";
import { offeredKinds, settingsSectionOpen } from "~/app/nav.ts";
import { useServicesRun } from "~/lib/services.ts";
import { needsWords } from "~/lib/access.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { KIND_WORDS, TRASH_KEPT, isArtifactKind, type MakeKind } from "~/lib/artifacts/words.ts";
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
import { NewFolderDialog, useNewFolderAccess } from "./FolderDialogs.tsx";
import { useFolderTree } from "~/lib/artifacts/folders.ts";
import type { ArtifactListScope, ArtifactListSort } from "~/protocol/index.ts";
import { BrandMakeChip } from "~/components/brand.tsx";

/** The scope each section asks the engine about. All is everything the reader can see. */
const SCOPE: Readonly<Record<ArtifactSection, ArtifactListScope>> = {
  all: "all",
  yours: "mine",
  shared: "shared",
};

/** The Make row's heading: what the reader does there, said plainly. */
export const MAKE_TITLE = "Start a new artifact";

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
  all: {
    title: "No artifacts are shared with you yet",
    hint: "Artifacts shared with you, your team or the company appear here.",
  },
  yours: {
    title: "You have not made an artifact yet",
    hint: "Ask an agent to make one for you, and it appears here.",
  },
  shared: {
    title: "Nothing is shared with you",
    hint: "When somebody shares an artifact with you, your team or the company, it appears here.",
  },
};

/**
 * What a section says when it holds nothing, and what to do about it.
 *
 * ALL NAMES WHAT IT COVERS. "Nothing here yet" read as "the company has no
 * artifacts", which was untrue for every reader whose own list was empty.
 */
const EMPTY: Readonly<Record<ArtifactSection, { title: string; hint: string }>> = {
  all: {
    title: "Nothing is yours or shared with you yet",
    hint: "Start one above, or ask an agent to make one. Artifacts shared with you, your team or the company appear here too.",
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

/** Where an administrator reads every artifact's history: the audit log, cut to its Artifacts trail. */
export const ARTIFACT_TRAIL_PATH: readonly string[] = ["settings", "audit"];
export const ARTIFACT_TRAIL_QUERY: Readonly<Record<string, string>> = { kind: "artifacts" };

/**
 * An empty All for an administrator: the same claim, and the company's other
 * artifacts where they can be reached. The link is offered by the audit log's
 * own lock (`settingsSectionOpen`), so it is drawn for exactly the readers
 * that page opens for.
 */
function emptyAllForAdmin(base: { title: string; hint: string }) {
  return {
    title: base.title,
    hint: (
      <>
        {base.hint} As an administrator you can open any other artifact in the company from the
        audit log, and the engine records each one you open.
      </>
    ),
    action: (
      <ButtonLink variant="secondary" href={href(ARTIFACT_TRAIL_PATH, ARTIFACT_TRAIL_QUERY)}>
        See every artifact in the audit log
      </ButtonLink>
    ),
  };
}

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
  const chooseSort = (next: ArtifactListSort) => setSort(next === DEFAULT_SORT ? "" : next);
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
  // devices, above everything else, in the order the reader chose.
  const pinnedParams = useMemo(() => ({ ...params, pinned: true }), [params]);
  const pinned = useListing(pinnedParams, section === "all" && !trashed);
  const author = useArtifactAccess({ app: true });
  // WHETHER THE AUDIT LOG OPENS FOR THIS READER, which is who an empty All
  // can send on to every artifact's history.
  const trail = settingsSectionOpen("audit", useHeldGrants() ?? []);
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
  // A FOLDER IS MADE FROM NEW HERE TOO, at the top level, where the reader
  // may make one: the gallery is where most readers look for it first.
  const folderHeld = useNewFolderAccess(null);
  const [newFolder, setNewFolder] = useState(false);
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
    sort,
    onSort: chooseSort,
  };
  const showPinned = section === "all" && !trashed && pinned.items.length > 0;
  const filtered = q.trim() !== "" || kind !== "";
  // THE MAKE ROW opens the reader's own sections, and only for a reader who
  // may start one: six tiles that each said "you cannot" would be the loudest
  // thing on a page the reader came to read. A search is a question about
  // what exists, so the row steps aside for it.
  const showMake = section !== "shared" && !trashed && !filtered && author.can;
  const total = listing.first.data?.total;
  // THE LIST SHAPE IS ONE GRID: Pinned and Recent are bands under one head, so
  // a column stands in one place down both. Cards take a titled group each.
  const bands =
    shape === "list" && showPinned
      ? [
          { key: "pinned", label: "Pinned", rows: pinned.items },
          ...(rows.length > 0 ? [{ key: "recent", label: "Recent", rows }] : []),
        ]
      : undefined;
  const rest =
    rows.length > 0 || bands ? (
      <Listing
        items={rows}
        bands={bands}
        view={view}
        name={`artifacts-${section}`}
        loadMore={listing.more ? listing.loadMore : undefined}
        loadingMore={listing.loadingMore}
        // THE PINS ARE ROWS OF THE ANSWER'S TOTAL, drawn apart: counted in, a
        // list with every row on screen read "7 of 9".
        total={showPinned && total !== undefined ? total - pinned.items.length : total}
      />
    ) : null;

  return (
    <LivePreviewBudgetProvider>
      <GalleryBar q={q} onQ={setQ}>
        <NewArtifactButton
          held={author.can ? null : author.reason}
          {...(folderHeld === null ? { onNewFolder: () => setNewFolder(true) } : {})}
        />
      </GalleryBar>
      {newFolder && <NewTopFolder onClose={() => setNewFolder(false)} />}
      {!author.can && author.block === "no_access" && <MakeHeld />}
      {showMake && <MakeRow />}
      <GalleryToolbar
        kind={kind}
        onKind={setKind}
        sort={sort}
        onSort={chooseSort}
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
      {showPinned && shape === "grid" && (
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
              ? // THE NOTE ABOVE SAYS WHO SEES THE TRASH AND HOW LONG AN
                // ARTIFACT STAYS, so the empty state says how one gets here.
                {
                  title: "Nothing in the trash",
                  hint: "An artifact you move to the trash from its More menu waits here, ready to restore.",
                }
              : filtered
                ? noMatch(q.trim(), kind)
                : // THE COPY FOLLOWS THE ROW: offered to make one only where
                  // the Make row is drawn, by the same access check.
                  section === "all" && trail
                  ? emptyAllForAdmin((author.can ? EMPTY : EMPTY_READER).all)
                  : (author.can ? EMPTY : EMPTY_READER)[section]
            : undefined
        }
      >
        {rest &&
          // UNDER PINNED, THE REST IS A GROUP TOO: a titled grid over an
          // untitled one read as the pins' grid running on.
          (showPinned && shape === "grid" ? (
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
/**
 * New folder from the gallery's New: a top-level folder, opened once it is
 * made so the reader lands in it. The tree is read only while the dialog is
 * open, for the names already taken at the top level.
 */
function NewTopFolder({ onClose }: { onClose: () => void }) {
  const tree = useFolderTree();
  return <NewFolderDialog parent={null} index={tree.index} onClose={onClose} open />;
}

/**
 * WHAT THE MISSING ACCESS HOLDS BACK, said on the page: making an artifact.
 * The gallery stays open to a reader without Artifacts access, since a share
 * alone admits them to what was shared, so neither the sidebar nor the page
 * is locked; but New held with only a pointer's title was the one sign, where
 * Doclet and Gitlet say "You need … access" in words on the page. One quiet
 * line, in the same words every held control uses.
 */
function MakeHeld() {
  return (
    <Callout variant="info" appearance="quiet">
      {needsWords("Making an artifact", ["artifacts"])} What is shared with you opens here all the
      same.
    </Callout>
  );
}

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
          // ONE NAME PER KIND, the New menu's and every card's (`KIND_WORDS`):
          // a second table here called a doc "Docs" beside the menu's "Doc".
          label: KIND_WORDS[k],
          detail: MAKE_DETAILS[k],
          href: href(newArtifactPath(k)),
        }))}
      />
    </Section>
  );
}
