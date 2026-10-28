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
 * folder they can see, which is where organising starts.
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
  type DataGridColumn,
} from "@crewlethq/ui";
import { FolderGlyph, FolderOpenGlyph, FolderPlusGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useKeymap } from "~/app/keymap.ts";
import { artifactFolderPath, artifactFoldersPath } from "~/app/routes.ts";
import { QueryState } from "~/components/common.tsx";
import { ARTIFACT_FOLDER_MAX_DEPTH } from "~/contract/artifacts.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useNow } from "~/lib/clock.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import type { FolderIndex } from "~/lib/artifacts/folders.ts";
import type { ArtifactFolderItem } from "~/protocol/index.ts";
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
  folderScopeOf,
  listParams,
  listingFactColumns,
  noMatch,
  quietCell,
  sortOf,
  useListing,
  useShape,
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

/**
 * The rows of a place's folders, for a reader who reads lists: the folder
 * glyph and its name over what is inside (`ArtifactNameCell kind="folder"`),
 * then the place's fact columns, then its menu, each row a link to the
 * folder's page. A folder row opens; it never expands in place.
 *
 * THE PLACE'S ONE COLUMN SET (`listingFactColumns`), for the same view as the
 * artifacts' grid under it, so Access, Owner and Edited stand in one place in
 * both. A folder holds no role of its own, so its Your role cell is quiet.
 */
export function FolderRows({
  folders,
  index,
  label,
  view,
}: {
  folders: readonly ArtifactFolderItem[];
  index: FolderIndex;
  label: string;
  /** The view the artifacts' grid under these rows is drawn for. */
  view: ListingView;
}) {
  const words = useVisibilityWords();
  const { people, viewer, now } = view;
  const columns: DataGridColumn<ArtifactFolderItem>[] = [
    {
      key: "name",
      header: "Name",
      floor: "14rem",
      phoneLead: true,
      cell: (folder) => (
        <ArtifactNameCell
          kind="folder"
          title={folder.name}
          detail={describeFolderContents({ artifacts: folder.artifacts, folders: folder.folders })}
        />
      ),
    },
    ...listingFactColumns<ArtifactFolderItem>(view, {
      access: (folder) => {
        const said = words(folder);
        return <ArtifactAccessLabel access={said.access} label={said.short} title={said.long} />;
      },
      owner: (folder) => <OwnerCell owner={folder.owner} viewer={viewer} people={people} />,
      // WHERE A FOLDER IS KEPT is the folder it is in, drawn while a search
      // reaches beneath this one.
      folder: (folder) => (
        <FolderPathCell words={folder.parent ? index.label(folder.parent) : ""} />
      ),
      updated: (folder) => <EditedCell at={folder.updated_at} now={now} />,
      role: quietCell,
      // A folder is never in the trash: a removed folder's contents move up.
      trashed: quietCell,
      purge: quietCell,
    }),
    {
      key: "actions",
      header: "",
      action: true,
      shrink: true,
      cell: (folder) => <FolderMenu folder={folder} index={index} />,
    },
  ];
  return (
    <div className="artifacts-list" role="group" aria-label={label}>
      <DataGrid
        rows={[...folders]}
        columns={columns}
        rowKey={(folder) => folder.id}
        rowHref={(folder) => href(artifactFolderPath(folder.id))}
        serverSorted
        name="artifact-folders"
        phoneRows="compact"
      />
    </div>
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
  };
  const filtered = searching || kind !== "" || scope !== "all";
  const name = folder?.name ?? "";
  const nothingHere = tiles.length === 0 && listing.items.length === 0;

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
        onSort={(next) => setSort(next === DEFAULT_SORT ? "" : next)}
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
      {tiles.length > 0 && (
        <Section title="Folders" className="artifacts-group">
          {/* THE READER'S SHAPE, for folders as for artifacts: tiles in the
              grid, rows in the list. */}
          {shape === "grid" ? (
            <FolderTiles
              folders={tiles}
              index={index}
              label={folder ? `Folders in ${name}` : "Folders"}
            />
          ) : (
            <FolderRows
              folders={tiles}
              index={index}
              label={folder ? `Folders in ${name}` : "Folders"}
              view={view}
            />
          )}
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
        ) : listing.items.length > 0 ? (
          <Section
            // THE TREE'S COUNT IS OF EVERYTHING at the top level, so it is
            // said only while the listing is not cut by a type or whose.
            title={
              searching || folder
                ? "Artifacts"
                : `Not in a folder${topLevel > 0 && !filtered ? ` · ${topLevel.toLocaleString()}` : ""}`
            }
            className="artifacts-group"
          >
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
