/**
 * One place in the folder tree, drawn: the Folders section (the top level)
 * and a folder's page share it, each saying only what is its own (a folder's
 * header, its trail, a folder that is not there).
 *
 * # The folders, then the artifacts
 *
 * The folders directly in this place come first, as tiles A to Z, shorter
 * than an artifact's card and with no preview, so the two never look alike
 * (`FolderTile`); then the artifacts directly in it, in the order the reader
 * chose, under the gallery's own toolbar. At the top level that second group
 * is "Not in a folder": everything the reader can see that is filed in no
 * folder they can see, which is where organising starts. In the list shape the
 * two are bands of one grid ([PlaceList]).
 *
 * # Counts are the reader's own
 *
 * A tile says what is directly inside that THIS reader can open ("12
 * artifacts · 2 folders"), or "Nothing you can open yet", never "Empty": a
 * folder grants nothing, so it may hold what is not shared with them.
 *
 * # A search looks beneath
 *
 * With a phrase, the place's whole subtree is searched (the listing without a
 * delimiter), the folders beneath it whose names match are offered as tiles,
 * and each artifact says which folder it is in.
 */

import { useMemo, useRef, useState, type ReactNode } from "react";
import {
  ArtifactAccessLabel,
  ArtifactNameCell,
  Button,
  EmptyState,
  FolderTile,
  LivePreviewBudgetProvider,
  Section,
  describeFolderContents,
} from "@crewlethq/ui";
import { FolderGlyph, FolderOpenGlyph, FolderPlusGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useKeymap } from "~/app/keymap.ts";
import { artifactFolderPath, artifactFoldersPath, artifactPath } from "~/app/routes.ts";
import { QueryState } from "~/components/common.tsx";
import { ARTIFACT_FOLDER_MAX_DEPTH } from "~/contract/artifactsworkspace.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useNow } from "~/lib/clock.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import type { FolderIndex } from "~/lib/artifacts/folders.ts";
import type { ArtifactFolderItem, ArtifactListItem, ArtifactListSort } from "~/protocol/index.ts";
import { NewArtifactButton } from "./new/NewArtifactMenu.tsx";
import {
  DEFAULT_SORT,
  GalleryBar,
  GallerySelectionBar,
  EditedCell,
  FolderPathCell,
  GallerySkeleton,
  GalleryToolbar,
  Listing,
  OwnerCell,
  artifactCells,
  folderScopeOf,
  listParams,
  listingColumns,
  listingOrder,
  loadedNote,
  noMatch,
  quietCell,
  sortOf,
  useListing,
  useShape,
  type ListingCells,
  type ListingView,
} from "./listing.tsx";
import { useGallerySelection } from "./selection.ts";
import { MoveDialog } from "./MoveDialog.tsx";
import { NewFolderDialog, useNewFolderAccess, useVisibilityWords } from "./FolderDialogs.tsx";
import { FolderMenu } from "./FolderMenu.tsx";

/** The tiles of a place's folders: a gallery of `FolderTile`s, each with its own menu. */
export function FolderTiles({
  folders,
  index,
  label,
}: {
  folders: readonly ArtifactFolderItem[];
  index: FolderIndex;
  /** Names the list for a screen reader: "Folders in Marketing". */
  label: string;
}) {
  const words = useVisibilityWords();
  return (
    <ul className="artifacts-folders" role="list" aria-label={label}>
      {folders.map((folder) => {
        const said = words(folder);
        return (
          <li key={folder.id}>
            <FolderTile
              name={folder.name}
              href={href(artifactFolderPath(folder.id))}
              // WHO SEES IT is said on a top-level tile, where it is decided;
              // a subfolder is seen by whoever sees the folder it is in, which
              // the page's own header already says.
              visibility={
                folder.parent ? undefined : (
                  <ArtifactAccessLabel access={said.access} label={said.short} title={said.long} />
                )
              }
              contents={{ artifacts: folder.artifacts, folders: folder.folders }}
              actions={<FolderMenu folder={folder} index={index} />}
            />
          </li>
        );
      })}
    </ul>
  );
}

/** One row of a place's list: a folder, or an artifact in it. */
type PlaceRow =
  { folder: ArtifactFolderItem; item?: undefined } | { folder?: undefined; item: ArtifactListItem };

/**
 * A place's folders in the order the listing beside them is in, so the head
 * that marks the order is true of every row. The tree is the reader's whole
 * answer, so this orders all of them. An order the tree cannot answer (when
 * the reader last opened one) leaves them A to Z, as the tree keeps them.
 */
export function foldersInOrder(
  folders: readonly ArtifactFolderItem[],
  sort: ArtifactListSort,
): readonly ArtifactFolderItem[] {
  const at = sort === "updated" ? "updated_at" : sort === "created" ? "created_at" : null;
  return at ? [...folders].sort((a, b) => b[at].localeCompare(a[at])) : folders;
}

/**
 * THE LIST SHAPE OF A PLACE: its folders and its artifacts as bands of one
 * grid, for a reader who reads lists. One head and one track list, so Access,
 * Owner and Edited stand in one place down every row. A folder's row is its
 * glyph and name over what is inside (`ArtifactNameCell kind="folder"`), a link
 * to its page; it never expands in place, and it holds no role of its own.
 */
function PlaceList({
  folders,
  index,
  artifactsLabel,
  view,
  items,
  total,
  loadMore,
  name,
}: {
  folders: readonly ArtifactFolderItem[];
  index: FolderIndex;
  /** What the artifacts' band is called: "Not in a folder" at the top level. */
  artifactsLabel: string;
  view: ListingView;
  items: ArtifactListItem[];
  total: number | undefined;
  loadMore: (() => void) | undefined;
  name: string;
}) {
  const words = useVisibilityWords();
  const { people, viewer, now, selection } = view;
  const ofArtifact = artifactCells(view);
  const ofFolder: ListingCells<ArtifactFolderItem> = {
    name: (folder) => (
      <ArtifactNameCell
        kind="folder"
        title={folder.name}
        detail={describeFolderContents({ artifacts: folder.artifacts, folders: folder.folders })}
      />
    ),
    access: (folder) => {
      const said = words(folder);
      return <ArtifactAccessLabel access={said.access} label={said.short} title={said.long} />;
    },
    owner: (folder) => <OwnerCell owner={folder.owner} viewer={viewer} people={people} />,
    // WHERE A FOLDER IS KEPT is the folder it is in, drawn while a search
    // reaches beneath this one.
    folder: (folder) => <FolderPathCell words={folder.parent ? index.label(folder.parent) : ""} />,
    updated: (folder) => <EditedCell at={folder.updated_at} now={now} />,
    role: quietCell,
    // A folder is never in the trash: a removed folder's contents move up.
    trashed: quietCell,
    purge: quietCell,
    menu: (folder) => <FolderMenu folder={folder} index={index} />,
  };
  const cell =
    (key: keyof ListingCells<ArtifactListItem>) =>
    (row: PlaceRow): ReactNode =>
      row.folder ? ofFolder[key](row.folder) : ofArtifact[key](row.item);
  const folderRows = foldersInOrder(folders, view.sort).map((folder) => ({ folder }));
  const artifactRows = items.map((item) => ({ item }));
  return (
    <DataGrid<PlaceRow>
      bands={[
        ...(folderRows.length > 0 ? [{ key: "folders", label: "Folders", rows: folderRows }] : []),
        ...(artifactRows.length > 0
          ? [{ key: "artifacts", label: artifactsLabel, rows: artifactRows, total }]
          : []),
      ]}
      columns={listingColumns<PlaceRow>(view, {
        name: cell("name"),
        access: cell("access"),
        owner: cell("owner"),
        folder: cell("folder"),
        updated: cell("updated"),
        role: cell("role"),
        trashed: cell("trashed"),
        purge: cell("purge"),
        menu: cell("menu"),
      })}
      rowKey={(row) => (row.folder ? row.folder.id : row.item.id)}
      rowHref={(row) =>
        href(row.folder ? artifactFolderPath(row.folder.id) : artifactPath(row.item.id))
      }
      {...listingOrder(view)}
      name={name}
      phoneRows="compact"
      onLoadMore={loadMore}
      loadedNote={loadedNote(items.length, total)}
      // THE BOXES PICK ARTIFACTS: Move to… moves artifacts, and a folder moves
      // from its own menu, so a folder's box is drawn and held.
      selection={
        selection?.selecting
          ? {
              selected: selection.selected,
              onChange: selection.replace,
              rowLabel: (row) => `Select ${row.folder ? row.folder.name : row.item.title}`,
              isSelectable: (row) => row.folder === undefined,
            }
          : undefined
      }
    />
  );
}

export function FolderView({
  folder,
  index,
  topLevel,
  header,
}: {
  /** The folder drawn, or null for the top level (the Folders section). */
  folder: ArtifactFolderItem | null;
  index: FolderIndex;
  /** How many artifacts the reader can see in no folder they can see, from the tree. */
  topLevel: number;
  /** The folder's own header, above the toolbar. */
  header?: ReactNode;
}) {
  const people = useArtifactPeople();
  const now = useNow();
  const viewer = useViewer();
  const nav = useNavigator();
  const [kindParam, setKind] = useParam("kind", "");
  const kind = isArtifactKind(kindParam) ? kindParam : "";
  const [q, setQ] = useParam("q", "");
  const [sortParam, setSort] = useParam("sort", "");
  const sort = sortOf(sortParam);
  const chooseSort = (next: ArtifactListSort) => setSort(next === DEFAULT_SORT ? "" : next);
  const [scopeParam, setScope] = useParam("scope", "");
  const scope = folderScopeOf(scopeParam);
  const [shape, setShape] = useShape();
  const phrase = q.trim();
  const searching = phrase !== "";
  const id = folder?.id ?? "";

  // ONE LEVEL, OR EVERYTHING BENEATH: a place's own artifacts, or, for a
  // search, its whole subtree (at the top level, every folder at once).
  const params = useMemo(
    () => listParams({ scope, kind, q, sort, folder: id || undefined, level: !searching }),
    [scope, kind, q, sort, id, searching],
  );
  const listing = useListing(params);
  const author = useArtifactAccess({ app: true });
  const folderHeld = useNewFolderAccess(folder);
  const [newFolder, setNewFolder] = useState(false);

  const tiles = useMemo(() => {
    if (!searching) return index.children(id);
    const needle = phrase.toLocaleLowerCase();
    const beneath = id ? index.subtree(id) : null;
    return index.folders.filter(
      (f) =>
        f.id !== id &&
        (beneath === null || beneath.has(f.id)) &&
        f.name.toLocaleLowerCase().includes(needle),
    );
  }, [index, id, searching, phrase]);

  const selection = useGallerySelection(
    useMemo(() => listing.items.map((i) => i.id), [listing.items]),
    JSON.stringify(params),
  );
  const picked = useMemo(
    () => listing.items.filter((i) => selection.selected.has(i.id)),
    [listing.items, selection.selected],
  );
  const selectRef = useRef<HTMLButtonElement>(null);
  const [moving, setMoving] = useState(false);
  useKeymap({
    "gallery.select": () => selection.setToggled(!selection.selecting),
    "gallery.folder": { run: () => setNewFolder(true), when: folderHeld === null },
    "gallery.up": {
      run: () => nav.to(folder?.parent ? artifactFolderPath(folder.parent) : artifactFoldersPath()),
      when: folder !== null,
    },
  });

  const view: ListingView = {
    trashed: false,
    own: scope === "mine",
    folders: searching,
    shape,
    people,
    viewer,
    now,
    onChanged: listing.refetch,
    selection,
    sort,
    onSort: chooseSort,
  };
  const filtered = searching || kind !== "" || scope !== "all";
  const name = folder?.name ?? "";
  const nothingHere = tiles.length === 0 && listing.items.length === 0;
  const artifactsTitle = searching || folder ? "Artifacts" : "Not in a folder";
  // THE TREE'S COUNT IS OF EVERYTHING at the top level, so it is said only
  // while the listing is not cut by a type or whose.
  const artifactsHeading =
    !searching && !folder && topLevel > 0 && !filtered
      ? `${artifactsTitle} · ${topLevel.toLocaleString()}`
      : artifactsTitle;

  const empty = (): ReactNode => {
    if (filtered) {
      const said = noMatch(phrase, kind);
      return <EmptyState size="compact" title={said.title} description={said.hint} />;
    }
    if (folder === null) {
      return (
        <EmptyState
          icon={<FolderGlyph size="xl" />}
          title="No folders yet"
          // THE NESTING BOUND A NEW TREE IS GIVEN, said before the reader
          // plans one: a folder records its own bound when it is made.
          description={`Folders keep the company's artifacts in order, nested up to ${ARTIFACT_FOLDER_MAX_DEPTH} deep. Moving an artifact into a folder never changes who can open it.`}
          action={
            folderHeld === null ? (
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<FolderPlusGlyph size="sm" />}
                onClick={() => setNewFolder(true)}
              >
                New folder
              </Button>
            ) : undefined
          }
        />
      );
    }
    // A READER WHO CAN FILE HERE is told it is empty and how to fill it; one
    // who cannot is told only that nothing in it is shared with them, which
    // claims nothing about what it holds.
    return folderHeld === null ? (
      <EmptyState
        icon={<FolderOpenGlyph size="xl" />}
        title="This folder is empty"
        description="Start an artifact here, or move one in from its menu."
        action={
          <span className="artifacts-empty-actions">
            <NewArtifactButton held={author.can ? null : author.reason} folder={id} />
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<FolderPlusGlyph size="sm" />}
              onClick={() => setNewFolder(true)}
            >
              New folder
            </Button>
          </span>
        }
      />
    ) : (
      <EmptyState
        icon={<FolderOpenGlyph size="xl" />}
        title="Nothing here is shared with you"
        description="Artifacts in this folder appear here when somebody shares them with you."
      />
    );
  };

  return (
    <LivePreviewBudgetProvider>
      <GalleryBar
        q={q}
        onQ={setQ}
        placeholder={folder ? `Search ${name}` : "Search folders and artifacts"}
      >
        {folder ? (
          <NewArtifactButton
            held={author.can ? null : author.reason}
            folder={id}
            {...(folderHeld === null ? { onNewFolder: () => setNewFolder(true) } : {})}
          />
        ) : (
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<FolderPlusGlyph size="sm" />}
            disabledReason={folderHeld ?? undefined}
            title={folderHeld ?? undefined}
            onClick={() => setNewFolder(true)}
          >
            New folder
          </Button>
        )}
      </GalleryBar>
      {header}
      <GalleryToolbar
        kind={kind}
        onKind={setKind}
        sort={sort}
        onSort={chooseSort}
        scope={{ value: scope, onChange: (next) => setScope(next === "all" ? "" : next) }}
        shape={shape}
        onShape={setShape}
        select={{ on: selection.selecting, onChange: selection.setToggled, ref: selectRef }}
      />
      <GallerySelectionBar
        selection={selection}
        picked={picked}
        author={author}
        onMove={() => setMoving(true)}
        returnFocusRef={selectRef}
      />
      {/* NO FOLDERS YET, BUT THINGS TO FILE: the top level says so above
          what is in no folder, with the way to make the first one, rather
          than opening on a list that reads as the whole gallery. */}
      {folder === null && tiles.length === 0 && !filtered && listing.items.length > 0 && empty()}
      {shape === "grid" && tiles.length > 0 && (
        <Section title="Folders" className="artifacts-group">
          <FolderTiles
            folders={tiles}
            index={index}
            label={folder ? `Folders in ${name}` : "Folders"}
          />
        </Section>
      )}
      {listing.first.loading && <GallerySkeleton shape={shape} />}
      <QueryState
        error={listing.first.error}
        refusal={listing.first.refusal}
        loading={listing.first.loading}
      >
        {nothingHere ? (
          empty()
        ) : shape === "list" ? (
          <PlaceList
            folders={tiles}
            index={index}
            artifactsLabel={artifactsTitle}
            view={view}
            items={listing.items}
            total={listing.first.data?.total}
            loadMore={listing.more ? listing.loadMore : undefined}
            name={folder ? "artifacts-folder" : "artifacts-top-level"}
          />
        ) : listing.items.length > 0 ? (
          <Section title={artifactsHeading} className="artifacts-group">
            <Listing
              items={listing.items}
              view={view}
              name={folder ? "artifacts-folder" : "artifacts-top-level"}
              loadMore={listing.more ? listing.loadMore : undefined}
              loadingMore={listing.loadingMore}
              total={listing.first.data?.total}
            />
          </Section>
        ) : null}
      </QueryState>
      {moving && (
        <MoveDialog
          subject={{ kind: "artifacts", items: picked }}
          within={id}
          onClose={() => setMoving(false)}
          onMoved={() => {
            setMoving(false);
            selection.clear();
            listing.refetch();
          }}
        />
      )}
      {newFolder && (
        <NewFolderDialog parent={folder} index={index} onClose={() => setNewFolder(false)} />
      )}
    </LivePreviewBudgetProvider>
  );
}
