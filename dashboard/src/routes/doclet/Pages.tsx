/**
 * One space: what it is for, who files into it, and its pages as an outline.
 *
 * # An outline, read one level at a time
 *
 * The space's top level is one `pages` listing (`roots`), and a page's level
 * is another (`parent`), read the moment its row opens and never before: a
 * space of four hundred pages costs one window until somebody opens a
 * folder. A row opens where the engine counted pages under it the same
 * listing would show, and on a FOLDER (a page with nothing written in it)
 * even when it holds none, so its Create row can be reached. Each row says
 * who last saved what the page says and when, which is the engine's
 * `saved_by` and `saved_at`, never `updated_at`: a label or a watch moves
 * that.
 *
 * # Create, at the end of every open level
 *
 * A Document goes to the composer, filed where the row is
 * (`#/doclet/new?space=…&parent=…`). A Folder is named in place and
 * written at once through `write_page` with an EMPTY BODY, which is what a
 * folder is: the engine derives it on every read and stores no kind for it,
 * so a folder somebody later writes in becomes a page with no migration.
 * Both are drawn for every reader and disabled with the reason for one who
 * cannot write.
 */

import { useCallback, useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  Card,
  EmptyState,
  Eyebrow,
  Input,
  KeyChip,
  Menu,
  PageHeader,
  Skeleton,
  TreeGrid,
  type TreeGridColumn,
  type TreeGridContext,
  type TreeInput,
} from "@crewlethq/ui";
import { FileTextGlyph, FolderGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { RefusalNote, pressable } from "~/components/WriteButton.tsx";
import { QueryState } from "~/components/common.tsx";
import { PAGE_TITLE_MAX_BYTES } from "~/contract/pages.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateCompact, fmtDateTime, plural, relTime, textBudget } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { PageContainer, PageSummary } from "~/protocol/index.ts";
import {
  Face,
  DocletBar,
  NewPageButton,
  PageGlyph,
  PageTags,
  SPACES_POLL_MS,
  SpacesRow,
  newPagePath,
  pagePath,
  usePeople,
  useSpaces,
} from "./parts.tsx";
import { otherProjects, ownerSentence, useSpaceOwners, type SpaceOwners } from "./spaceOwners.ts";
import { usePagedPages } from "./usePagedPages.ts";

/**
 * What the outline lists: the pages a person reads. A draft is somebody's page
 * in progress and its space is where it lives; the trash is not, and the tool
 * skills are machinery with a section of their own.
 */
const OUTLINE_PAGES = { status: "published,draft", skills: false } as const;

/**
 * How often an open level reads itself again: a minute, the spaces' own
 * cadence. An outline is where a reader finds a page, not where they watch one
 * change, and every open level is a poll of its own. A write made here reads
 * its level back at once (`contract/domains.ts`).
 */
const LEVEL_POLL_MS = 60_000;

/** The kinds of change that alter what a page says, which "Last written" is about. */
const WRITTEN_KINDS = "created,saved,renamed";

/** The top of the space, as a level's key. */
const TOP = "";

/**
 * The rows that hold no page, each keyed by the level it closes. A level not
 * yet read stands on one LOADING row, which is what lets a folder draw its
 * expander before its pages are known.
 */
const LOADING = "loading:";
const FAILED = "failed:";
const EMPTY = "empty:";
const MORE = "more:";
const CREATE = "create:";
const MARKS = [LOADING, FAILED, EMPTY, MORE, CREATE] as const;
type Mark = (typeof MARKS)[number];

/** The mark a row is and the level it closes, or null for a page. */
function markOf(id: string): { mark: Mark; parent: string } | null {
  for (const mark of MARKS) {
    if (id.startsWith(mark)) return { mark, parent: id.slice(mark.length) };
  }
  return null;
}

/** Whether a page opens onto a level: it holds pages, or it is a folder. */
const opens = (page: PageSummary): boolean => Boolean(page.folder) || (page.children ?? 0) > 0;

/** One level as its reader last published it. */
interface Level {
  ready: boolean;
  rows: PageSummary[];
  total: number | null;
  more: boolean;
  paging: boolean;
  error: QueryErrorCode | null;
  pageError: QueryErrorCode | null;
}

/**
 * The outline's columns: the title takes the room, and the face and the date
 * keep the widths every row shares, since each row is a grid of its own.
 */
const COLUMNS: TreeGridColumn[] = [
  { key: "title", header: "Title" },
  { key: "saved-by", header: "Last saved by", width: "var(--size-control-md)" },
  { key: "saved-at", header: "Last saved", width: "var(--knowledge-outline-when)" },
];

export function Pages({ container }: { container: string }) {
  const { spaces, read, error, refusal } = useSpaces();
  const space = spaces.find((s) => s.key === container);
  // THE TRAIL NAMES THE SPACE, as a page's own trail does.
  usePageLabels(space?.name ? { [container]: space.name } : {});

  let body: ReactNode;
  if (error === "unknown_query") {
    body = (
      <EmptyState
        icon={<FolderGlyph size="xl" />}
        title="This company's pages live in its wiki"
        description="Use the search in the bar."
      />
    );
  } else if (!read && error) {
    // A REFUSED READ IS SAID, never drawn as a space still loading: the
    // skeleton stood for ever over a read the engine had answered with a no.
    body = <QueryState error={error} refusal={refusal} loading={false} />;
  } else if (!read) {
    body = <Skeleton variant="text" rows={8} label="Loading the space" />;
  } else if (!space) {
    // AN ADDRESS THAT DID NOT RESOLVE, named: a stale link and a node that has
    // not caught up are told apart from a typo by the key itself.
    body = (
      <EmptyState
        icon={<FolderGlyph size="xl" />}
        title={`No space called “${container}”`}
        description="It may not exist yet, or this node is catching up."
      />
    );
  } else {
    body = (
      <>
        <SpaceHead space={space} />
        <SpaceOutline key={space.key} space={space} />
      </>
    );
  }

  return (
    <>
      <DocletBar>
        <NewPageButton space={container} />
      </DocletBar>
      <div className="knowledge-home">
        <SpacesRow current={container} />
        {body}
      </div>
    </>
  );
}

// ---------------------------------------------------------------------------
// The head
// ---------------------------------------------------------------------------

/** Who files into a space, as the phrase its facts read. */
function filedBy(owners: SpaceOwners): ReactNode {
  if (owners.state !== "read") return ownerSentence(owners);
  if (owners.units.length === 0) return "No unit names this space";
  return (
    <>
      Filed by <b>{owners.units.map((u) => u.name).join(", ")}</b>
    </>
  );
}

/**
 * The space's name and key, what it is for, and its facts: how many pages,
 * when somebody last wrote in it, who files into it, where its work goes, and
 * when it was made.
 */
function SpaceHead({ space }: { space: PageContainer }) {
  const now = useNow();
  const owners = useSpaceOwners()(space.key);
  // THE NEWEST CHANGE TO WHAT A PAGE SAYS, asked of the space's own change
  // log: exact, where the newest row of a listing ordered by title is not.
  const written = useQuery(
    "page_activity",
    { container: space.key, kinds: WRITTEN_KINDS, limit: 1 },
    { pollMs: SPACES_POLL_MS },
  );
  const last = written.data?.changes?.[0]?.at;
  const named = space.name || space.key;
  return (
    <header className="knowledge-space-head">
      <Eyebrow as="p">Space</Eyebrow>
      <PageHeader
        title={named}
        badges={<KeyChip>{space.key}</KeyChip>}
        description={space.purpose || undefined}
      />
      <ul className="knowledge-space-facts" role="list" aria-label={`About ${named}`}>
        <li>
          <b>{space.pages.toLocaleString()}</b> {space.pages === 1 ? "page" : "pages"}
        </li>
        <li>
          Last written{" "}
          {last ? (
            <time dateTime={last} title={fmtDateTime(last)}>
              <b>{relTime(last, now)}</b>
            </time>
          ) : (
            <b>{written.data ? "before this node's change log" : "not read yet"}</b>
          )}
        </li>
        <li>{filedBy(owners)}</li>
        {otherProjects(owners, space.key).map((project) => (
          <li key={project}>
            Work in <KeyChip>{project}</KeyChip>
          </li>
        ))}
        {space.created_at && (
          <li>
            Created{" "}
            <time dateTime={space.created_at} title={fmtDateTime(space.created_at)}>
              <b>{fmtDateCompact(space.created_at, now)}</b>
            </time>
          </li>
        )}
      </ul>
    </header>
  );
}

// ---------------------------------------------------------------------------
// The outline
// ---------------------------------------------------------------------------

/**
 * One level's listing, read while its row is open. It publishes what it read
 * to the outline, which draws every level in one grid; a component rather
 * than a hook in a loop, because levels open and close.
 */
function LevelReader({
  space,
  parent,
  onLevel,
  onMore,
}: {
  space: string;
  parent: string;
  onLevel: (parent: string, level: Level) => void;
  onMore: (parent: string, more: (() => void) | null) => void;
}) {
  const params = useMemo(
    () =>
      parent === TOP
        ? { ...OUTLINE_PAGES, container: space, roots: true }
        : { ...OUTLINE_PAGES, parent },
    [space, parent],
  );
  const level = usePagedPages(params, { pollMs: LEVEL_POLL_MS });
  const { rows, total, more, paging, error, pageError, loadMore } = level;
  const ready = level.data !== null || error !== null;
  useEffect(() => {
    onLevel(parent, { ready, rows, total, more, paging, error, pageError });
  }, [onLevel, parent, ready, rows, total, more, paging, error, pageError]);
  // THE NEXT WINDOW'S PRESS, held where the outline reaches it without a
  // render: a function is a new value on every render, and publishing it as
  // state would publish the level again for nothing.
  const latest = useRef(loadMore);
  useEffect(() => {
    latest.current = loadMore;
  });
  useEffect(() => {
    onMore(parent, () => latest.current());
    return () => onMore(parent, null);
  }, [onMore, parent]);
  return null;
}

function SpaceOutline({ space }: { space: PageContainer }) {
  const nav = useNavigator();
  const who = usePeople();
  const now = useNow();
  const [open, setOpen] = useState<ReadonlySet<string>>(() => new Set());
  const [levels, setLevels] = useState<ReadonlyMap<string, Level>>(() => new Map());
  // WHERE A FOLDER IS BEING NAMED: the level whose Create row became a field.
  const [naming, setNaming] = useState<string | null>(null);
  const more = useRef(new Map<string, () => void>());

  const onLevel = useCallback(
    (parent: string, level: Level) => setLevels((was) => new Map(was).set(parent, level)),
    [],
  );
  const onMore = useCallback((parent: string, press: (() => void) | null) => {
    if (press) more.current.set(parent, press);
    else more.current.delete(parent);
  }, []);

  const pages = useMemo(() => {
    const byID = new Map<string, PageSummary>();
    for (const level of levels.values()) for (const page of level.rows) byID.set(page.id, page);
    return byID;
  }, [levels]);
  const name = (parent: string) =>
    parent === TOP ? space.name || space.key : (pages.get(parent)?.title ?? "this page");

  const rows = useMemo<TreeInput[]>(() => {
    const level = (parent: string, above: ReadonlySet<string>): TreeInput[] => {
      const read = levels.get(parent);
      if (!read?.ready) return [{ id: LOADING + parent, label: "" }];
      if (read.error) return [{ id: FAILED + parent, label: "" }];
      const out: TreeInput[] = [];
      for (const page of read.rows) {
        // A PAGE IS NEVER ITS OWN ANCESTOR, and a level that claimed so is
        // drawn once rather than for ever.
        if (above.has(page.id)) continue;
        out.push(
          opens(page)
            ? {
                id: page.id,
                label: page.title,
                children: level(page.id, new Set([...above, page.id])),
              }
            : { id: page.id, label: page.title },
        );
      }
      if (read.rows.length === 0) out.push({ id: EMPTY + parent, label: "" });
      if (read.more) out.push({ id: MORE + parent, label: "" });
      out.push({ id: CREATE + parent, label: "" });
      return out;
    };
    return level(TOP, new Set());
  }, [levels]);

  // THE LEVELS BEING READ: the top, and every open row that opens onto one.
  const reading = useMemo(() => {
    const below = [...open].filter((id) => {
      const page = pages.get(id);
      return page !== undefined && opens(page);
    });
    return [TOP, ...below];
  }, [open, pages]);

  const created = (parent: string, folder: string | null) => {
    setNaming(null);
    // THE NEW FOLDER OPENS, so its own Create row is the next thing in view.
    if (folder) setOpen((was) => new Set([...was, ...(parent === TOP ? [] : [parent]), folder]));
  };

  const markCell = (
    id: string,
    mark: Mark,
    parent: string,
    column: number,
    grid: TreeGridContext,
  ) => {
    const tab = grid.tabStop(id, column) ? 0 : -1;
    const level = levels.get(parent);
    const where = name(parent);
    switch (mark) {
      case LOADING:
        return <span className="knowledge-note">Loading the pages&hellip;</span>;
      case FAILED:
        return (
          <span className="knowledge-note" role="alert">
            The pages here could not be read ({level?.error}).
          </span>
        );
      case EMPTY:
        return (
          <span className="knowledge-note">
            {parent === TOP
              ? "Nothing is filed in this space yet."
              : "Nothing is filed under it yet."}
          </span>
        );
      case MORE:
        return (
          <span className="knowledge-outline-more">
            <Button
              size="small"
              variant="ghost"
              tabIndex={tab}
              loading={level?.paging}
              onClick={() => more.current.get(parent)?.()}
            >
              Load more
            </Button>
            <span className="knowledge-note">
              {(level?.rows.length ?? 0).toLocaleString()} of {(level?.total ?? 0).toLocaleString()}{" "}
              in {where}
            </span>
            {level?.pageError && (
              <span className="knowledge-note" role="alert">
                The next pages could not be read ({level.pageError}).
              </span>
            )}
          </span>
        );
      case CREATE:
        return naming === parent ? (
          <FolderField
            space={space.key}
            parent={parent}
            where={where}
            tabIndex={tab}
            onDone={(folder) => created(parent, folder)}
          />
        ) : (
          <CreateMenu
            id={id}
            column={column}
            grid={grid}
            where={where}
            onDocument={() => {
              const to = newPagePath(space.key, parent || undefined);
              nav.to(to.path, to.query);
            }}
            onFolder={() => setNaming(parent)}
          />
        );
    }
  };

  const renderCell = (id: string, column: number, grid: TreeGridContext) => {
    const mark = markOf(id);
    if (mark) return markCell(id, mark.mark, mark.parent, column, grid);
    const page = pages.get(id);
    if (!page) return null;
    const saver = page.saved_by ?? page.author ?? "";
    const at = page.saved_at ?? page.updated_at;
    switch (column) {
      case 1:
        return (
          <a
            className="knowledge-outline-title"
            href={href(pagePath(page.id))}
            tabIndex={grid.tabStop(id, column) ? 0 : -1}
          >
            <PageGlyph folder={opens(page)} />
            <span className="truncate">{page.title}</span>
            <PageTags status={page.status} onboarding={page.onboarding} />
            {(page.children ?? 0) > 0 && (
              <span
                className="knowledge-outline-count"
                title={`${plural(page.children ?? 0, "page")} under it`}
              >
                {page.children}
              </span>
            )}
          </a>
        );
      case 2:
        return saver ? (
          <span className="knowledge-outline-face" title={`Last saved by ${who(saver).name}`}>
            <Face handle={saver} who={who} />
            <span className="sr-only">Last saved by {who(saver).name}</span>
          </span>
        ) : (
          <span className="sr-only">Written by the engine</span>
        );
      default:
        return (
          <time className="knowledge-outline-when" dateTime={at} title={fmtDateTime(at)}>
            {fmtDateCompact(at, now)}
          </time>
        );
    }
  };

  return (
    <Card padding="none" className="knowledge-outline">
      {reading.map((parent) => (
        <LevelReader
          key={parent || "top"}
          space={space.key}
          parent={parent}
          onLevel={onLevel}
          onMore={onMore}
        />
      ))}
      <TreeGrid
        label={`Pages in ${space.name || space.key}`}
        columns={COLUMNS}
        rows={rows}
        headless
        defaultExpanded="none"
        expanded={open}
        onExpandedChange={setOpen}
        renderCell={renderCell}
        cellHasControl={(id, column) => {
          const mark = markOf(id);
          if (mark) return mark.mark === CREATE || mark.mark === MORE;
          return column === 1;
        }}
        addRow={(id) => {
          const mark = markOf(id);
          if (!mark) return null;
          const where = name(mark.parent);
          const label: Record<Mark, string> = {
            [LOADING]: `Loading the pages in ${where}`,
            [FAILED]: `The pages in ${where} could not be read`,
            [EMPTY]: `Nothing is filed in ${where} yet`,
            [MORE]: `More pages in ${where}`,
            [CREATE]: `Create in ${where}`,
          };
          return { label: label[mark.mark], cells: 1 };
        }}
        onRowKey={(id, action) => {
          if (action !== "activate" || !pages.has(id)) return false;
          nav.to(pagePath(id));
          return true;
        }}
      />
      <Card.Footer variant="meta">{plural(space.pages, "page")}</Card.Footer>
    </Card>
  );
}

/**
 * Create, at the end of a level: a Document goes to the composer, a Folder is
 * named in place. Drawn for every reader; one who cannot write meets it
 * disabled with the reason.
 */
function CreateMenu({
  id,
  column,
  grid,
  where,
  onDocument,
  onFolder,
}: {
  id: string;
  column: number;
  grid: TreeGridContext;
  where: string;
  onDocument: () => void;
  onFolder: () => void;
}) {
  const can = useWriteAccess("write_page");
  const tab = grid.tabStop(id, column) ? 0 : -1;
  if (!can.can) {
    return (
      <Button
        size="small"
        variant="ghost"
        leadingIcon={<PlusGlyph size="sm" />}
        tabIndex={tab}
        disabledReason={can.reason}
        title={can.reason}
      >
        Create
      </Button>
    );
  }
  return (
    <Menu
      label={`Create in ${where}`}
      icon={<PlusGlyph size="sm" />}
      trigger="Create"
      triggerVariant="ghost"
      triggerTabIndex={tab}
      open={grid.menuOpen(id)}
      onOpenChange={(isOpen) => {
        if (isOpen) grid.opened(id, column);
        grid.setMenuOpen(id, isOpen);
      }}
      items={[
        {
          key: "document",
          label: "Document",
          icon: <FileTextGlyph size="sm" />,
          description: "A page to write and read",
          onSelect: onDocument,
        },
        {
          key: "folder",
          label: "Folder",
          icon: <FolderGlyph size="sm" />,
          description: "Groups pages under one name",
          onSelect: onFolder,
        },
      ]}
    />
  );
}

/**
 * A folder's name, typed where the folder will be. Enter writes it and Escape
 * lets it go. A name already taken in the space is the engine's refusal
 * (`exists`), drawn under the field it is about.
 */
function FolderField({
  space,
  parent,
  where,
  tabIndex,
  onDone,
}: {
  space: string;
  parent: string;
  where: string;
  tabIndex: number;
  onDone: (folder: string | null) => void;
}) {
  const write = useAct("write_page");
  const [draft, setDraft] = useState("");
  const field = useRef<HTMLInputElement>(null);
  const hintID = useId();
  useEffect(() => field.current?.focus(), []);
  const title = draft.trim();
  const budget = textBudget(draft, PAGE_TITLE_MAX_BYTES, "a folder's name");
  const blocked = !title ? "Give the folder a name." : budget.over ? budget.line : undefined;

  const create = async () => {
    const result = await write.run(
      { title, body: "", container: space, ...(parent ? { parent } : {}) },
      { done: `Created the folder “${title}”.` },
    );
    if (!result || (result.kind !== "applied" && result.kind !== "pending")) return;
    const id = (result.receipt as { id?: unknown } | null)?.id;
    onDone(typeof id === "string" && id ? id : null);
  };

  return (
    <span className="knowledge-folder-field">
      <FolderGlyph size="sm" />
      <Input
        ref={field}
        inputSize="sm"
        aria-label={`New folder in ${where}`}
        aria-describedby={hintID}
        placeholder="Folder name"
        value={draft}
        tabIndex={tabIndex}
        error={budget.over}
        disabled={write.busy}
        onChange={(event) => setDraft(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            // THE FIELD'S OWN ESCAPE: it lets the name go and nothing else.
            event.preventDefault();
            event.stopPropagation();
            onDone(null);
          }
          if (event.key === "Enter") {
            event.preventDefault();
            if (pressable(write, blocked)) void create();
          }
        }}
      />
      <span id={hintID} className="knowledge-note">
        {title && blocked ? blocked : "Enter creates it. Esc cancels."}
      </span>
      <RefusalNote write={write} />
    </span>
  );
}
