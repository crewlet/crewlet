/**
 * The Knowledge tree: the column beside every Knowledge screen.
 *
 * # One column, three things a reader keeps while moving
 *
 * The approved layout puts the SEARCH, its MODE and every SPACE's pages in one
 * column that stays put while the pane beside it changes — a hit, a space, a
 * page. It is the workspace's `tree` renderer (`app/nav.ts`), drawn by the
 * shell and mounted once for the whole workspace, so a folder a reader opened
 * stays open while they read the pages in it. Leaving Knowledge unmounts it;
 * the open folders are kept for the tab in session storage, which is a
 * convenience a private window may refuse and the tree draws fine without.
 *
 * # The modes are offered as the engine serves them
 *
 * `knowledge` with no phrase is the seam's PROBE: it runs nothing and answers
 * which modes this backend serves as asked, and why the rest would degrade
 * (`knowledge.Searcher` in the engine). It is asked for `semantic`, the mode
 * that needs the most, so its `degraded` names the configuration reason — no
 * embeddings provider, or a backend with no such ranker — and a mode the
 * answer does not list is DISABLED WITH THAT REASON, written under the control
 * rather than only in a tooltip. Before the probe answers nothing is disabled:
 * "not known yet" is never a reason to take a mode away.
 *
 * # A level is read whole, and says when it is not
 *
 * A space's top level (`roots`) and one page's children (`parent`) are each a
 * `pages` listing of up to 500 (`PAGES_WINDOW`), with "Load more" from the
 * answer's own cursor and the TOTAL beside it. A row opens only where the
 * engine counted children the same listing would show, so an expander never
 * opens onto a folder of trashed pages.
 *
 * # On a phone the spaces fold
 *
 * Below the phone breakpoint the frame is one pane and this column stacks
 * ABOVE the screen, so a tree whose length is the company's page count would
 * stand between a reader and every page they open. The search and its mode
 * stay drawn; the spaces are one disclosure, closed on every arrival, and
 * nothing under it is read until it is opened.
 *
 * # The default mode is one the engine serves
 *
 * With no mode chosen the control checks `defaultSearchMode`: Hybrid where it
 * is served, else the first mode that is — never a segment it also draws as
 * unavailable.
 *
 * # Below the spaces, the workspace's sections
 *
 * The artboard's rows under the spaces are sections of this workspace with
 * addresses of their own. Agent skills is one, its count the TOOL-SKILL total
 * the engine answers (`pages{skills}.total`) — never the length of a window.
 * Agent diaries is the other, its count the agents `memory_overview` lists —
 * every agent seat in the chart, which is what its screen draws.
 */

import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { Button, cx, Input, Skeleton } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  BrainGlyph,
  ChevronDownGlyph,
  ChevronRightGlyph,
  FileTextGlyph,
  SearchGlyph,
  WandSparklesGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { Segmented } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtExact } from "~/lib/format.ts";
import { QueryState } from "~/components/common.tsx";
import { useMediaQuery } from "~/lib/media.ts";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { SEARCH_MODES, asSearchMode, defaultSearchMode, modeUnavailable } from "~/lib/search.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { PageContainer, PageSummary, SearchMode } from "~/protocol/index.ts";
import { usePagedPages } from "./usePagedPages.ts";
import { otherProjects, ownerSentence, useSpaceOwners, type SpaceOwners } from "./spaceOwners.ts";

/**
 * What the tree lists: the pages a person reads. Drafts are listed because a
 * draft is somebody's page in progress and its space is where it lives; the
 * trash is not. Tool-skill pages are left out: they are machinery a phase is
 * offered, and they get their own section.
 */
const TREE_PAGES = { status: "published,draft", skills: false } as const;

/**
 * How often a tree level re-reads: a minute, the containers' own cadence. A
 * tree is where a reader finds a page, not where they watch one change, and
 * every open level is a poll of its own.
 */
const TREE_POLL_MS = 60_000;

/** Where the open folders are kept for the tab. */
const OPEN_KEY = STORAGE_KEYS.knowledgeOpen;

function readOpen(): Set<string> {
  try {
    const raw = sessionStorage.getItem(OPEN_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return new Set(Array.isArray(parsed) ? parsed.filter((v) => typeof v === "string") : []);
  } catch {
    return new Set();
  }
}

function writeOpen(open: Set<string>): void {
  try {
    sessionStorage.setItem(OPEN_KEY, JSON.stringify([...open]));
  } catch {
    // A private window or a blocked store: the tree works, it just forgets.
  }
}

const spaceNode = (key: string) => `space:${key}`;
const pageNode = (id: string) => `page:${id}`;

export function KnowledgeTree() {
  const route = useRoute();
  const where = resolve(route.path);
  const onSearch = where.resolved && where.screen === "knowledge";
  const currentPage = where.resolved && where.screen === "page" ? where.id : "";
  const currentSpace = where.resolved && where.screen === "container" ? where.key : "";

  const [open, setOpen] = useState<Set<string>>(readOpen);
  const toggle = (node: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(node)) next.delete(node);
      else next.add(node);
      writeOpen(next);
      return next;
    });
  const reveal = (nodes: string[]) =>
    setOpen((prev) => {
      if (nodes.every((n) => prev.has(n))) return prev;
      const next = new Set([...prev, ...nodes]);
      writeOpen(next);
      return next;
    });

  // ON A PHONE THE SPACES ARE FOLDED. The frame stacks this column ABOVE the
  // screen there (one pane), and a tree is as long as the company's pages are
  // many: opened to the page being read, a space's top level alone is up to
  // 500 rows, all of them between a reader who followed a link and the page
  // they followed it to. So the search and its mode stay — they are a few
  // rows and the workspace's way in — and the spaces are one disclosure,
  // closed on every arrival: a reader who opened it to go somewhere lands on
  // the page they picked, not on the tree again.
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  const [unfolded, setUnfolded] = useState(false);
  useEffect(() => setUnfolded(false), [route.path]);
  const shown = !phone || unfolded;

  // WHERE THE READER IS, OPENED TO: the space on its own screen, and the page's
  // space and every ancestor on a page's. Read once rather than polled — the
  // page's own screen polls the page; this only needs to know where it sits —
  // and only while the tree is drawn: a folded tree has nowhere to open to,
  // and unfolding it asks then, so it opens where the reader is.
  const here = useQuery("page", { id: currentPage }, { enabled: currentPage !== "" && shown });
  const hereID = here.data?.page.id;
  const hereSpace = here.data?.page.container;
  const hereAncestors = here.data?.ancestors;
  useEffect(() => {
    if (currentSpace) reveal([spaceNode(currentSpace)]);
  }, [currentSpace]);
  useEffect(() => {
    if (!hereID || !hereSpace) return;
    reveal([spaceNode(hereSpace), ...(hereAncestors ?? []).map((a) => pageNode(a.id))]);
  }, [hereID, hereSpace, hereAncestors]);

  return (
    <nav className="section-column ktree" aria-label="Knowledge">
      <TreeSearch onSearch={onSearch} />
      <Spaces
        open={open}
        toggle={toggle}
        currentPage={currentPage}
        currentSpace={currentSpace}
        fold={phone ? { unfolded, set: setUnfolded } : null}
      />
      <Sections current={where.resolved ? where.screen : ""} />
    </nav>
  );
}

/**
 * The search box and its mode: the workspace's search, wherever in it the
 * reader is. It navigates to the search screen with both in the address.
 */
function TreeSearch({ onSearch }: { onSearch: boolean }) {
  const route = useRoute();
  const nav = useNavigator();
  const box = useRef<HTMLInputElement>(null);
  useSearchTarget(box);
  const urlQ = onSearch ? (route.query.get("q") ?? "") : "";
  const urlMode = onSearch ? route.query.get("mode") : null;
  const [draft, setDraft] = useState(urlQ);
  // WHAT THE READER CHOSE, or null for "the default" — which is the probe's
  // to decide (`defaultSearchMode`), so it is resolved at render rather than
  // copied into state before the probe has answered.
  const [picked, setPicked] = useState<SearchMode | null>(urlMode ? asSearchMode(urlMode) : null);
  // THE ADDRESS WINS WHEN IT MOVES: Back to an earlier search, or a pasted
  // link, puts that search's phrase and mode in the box.
  useEffect(() => {
    if (onSearch) setDraft(urlQ);
  }, [onSearch, urlQ]);
  useEffect(() => {
    if (onSearch) setPicked(urlMode ? asSearchMode(urlMode) : null);
  }, [onSearch, urlMode]);

  const probe = useQuery("knowledge", { q: "", mode: "semantic" }, { pollMs: TREE_POLL_MS });
  // THE CHECKED SEGMENT IS NEVER ONE DRAWN AS UNAVAILABLE unless the reader
  // asked for it by name: with no embeddings provider the default is Keyword,
  // not a disabled Hybrid a search then silently degrades from.
  const mode = picked ?? defaultSearchMode(probe.data);
  const reasons = SEARCH_MODES.map((m) => ({ mode: m, why: modeUnavailable(probe.data, m.value) }));
  const reasonID = useId();
  const unavailable = reasons.filter((r) => r.why);

  const go = (phrase: string, how: SearchMode) => {
    const query: Record<string, string> = {};
    if (phrase) query.q = phrase;
    if (how !== "hybrid") query.mode = how;
    nav.to(["knowledge"], query);
  };

  return (
    <div className="ktree-search">
      <form
        role="search"
        onSubmit={(e) => {
          e.preventDefault();
          go(draft.trim(), mode);
        }}
      >
        <Input
          type="search"
          width="full"
          inputSize="sm"
          ref={box}
          aria-label="Search pages"
          placeholder="Search pages"
          leading={<SearchGlyph size="sm" />}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          clearLabel="Clear the search"
          onClear={() => {
            setDraft("");
            if (onSearch && urlQ) go("", mode);
          }}
        />
      </form>
      <Segmented
        size="sm"
        ariaLabel="Rank by"
        value={mode}
        onChange={(next) => {
          setPicked(next);
          // ON THE SEARCH SCREEN A MODE IS A FILTER OF WHAT IS SHOWN, so it
          // re-runs there; anywhere else it is the mode the next search runs.
          if (onSearch && urlQ) go(urlQ, next);
        }}
        options={reasons.map(({ mode: m, why }) => ({
          value: m.value,
          label: m.label,
          title: why ?? m.hint,
          disabled: why !== null,
          describedBy: why ? reasonID : undefined,
        }))}
      />
      {unavailable.length > 0 && (
        // WRITTEN, NOT ONLY HOVERED: a disabled control whose reason lives in
        // a tooltip is a reason a keyboard reader and a phone never see.
        <p id={reasonID} className="ktree-reason">
          {unavailable[unavailable.length - 1]!.why}
          {unavailable.length > 1 &&
            ` (${unavailable.map((r) => r.mode.label).join(" and ")} both need it).`}
        </p>
      )}
    </div>
  );
}

function Spaces({
  open,
  toggle,
  currentPage,
  currentSpace,
  fold,
}: {
  open: Set<string>;
  toggle: (node: string) => void;
  currentPage: string;
  currentSpace: string;
  /** The phone's disclosure, or null where the spaces are always drawn. */
  fold: { unfolded: boolean; set: (unfolded: boolean) => void } | null;
}) {
  const bodyID = useId();
  const containers = useQuery("containers", undefined, { pollMs: TREE_POLL_MS });
  const owners = useSpaceOwners();
  const spaces = useMemo(
    () =>
      [...(containers.data?.containers ?? [])].sort((a, b) =>
        (a.name || a.key).localeCompare(b.name || b.key),
      ),
    [containers.data],
  );

  let body: ReactNode;
  if (containers.error === "unknown_query") {
    // NOT AN ERROR: a company whose knowledge lives in a vendor's wiki has no
    // pages on this node to browse, by design. Search reaches that wiki.
    body = (
      <p className="ktree-note">
        This company&rsquo;s pages live in its wiki rather than on this engine, so there is no tree
        to browse here — search reaches them.
      </p>
    );
  } else if (containers.error) {
    body = <p className="ktree-note">The spaces could not be read ({containers.error}).</p>;
  } else if (!containers.data) {
    body = <Skeleton variant="text" rows={4} label="Loading the spaces" />;
  } else if (spaces.length === 0) {
    body = (
      <p className="ktree-note">
        No spaces yet. A space is created the first time somebody writes into it — give a unit a{" "}
        <code className="inline">space</code> and its agents have somewhere to file what they learn.
      </p>
    );
  } else {
    body = (
      <ul className="ktree-list" role="list">
        {spaces.map((space) => (
          <SpaceRow
            key={space.key}
            space={space}
            owners={owners(space.key)}
            open={open}
            toggle={toggle}
            currentPage={currentPage}
            current={currentSpace === space.key}
          />
        ))}
      </ul>
    );
  }

  if (fold) {
    // HOW MANY, ON THE CLOSED DISCLOSURE, so a reader knows whether opening it
    // is worth it without opening it.
    const count = containers.data ? spaces.length : null;
    return (
      <div className="ktree-spaces">
        <h2 className="ktree-heading">
          <button
            type="button"
            className="ktree-fold"
            aria-expanded={fold.unfolded}
            aria-controls={fold.unfolded ? bodyID : undefined}
            onClick={() => fold.set(!fold.unfolded)}
          >
            {fold.unfolded ? (
              <ChevronDownGlyph size="xs" aria-hidden="true" />
            ) : (
              <ChevronRightGlyph size="xs" aria-hidden="true" />
            )}
            <span>Spaces</span>
            {count !== null && <span className="ktree-fold-count">{fmtExact(count)}</span>}
          </button>
        </h2>
        {fold.unfolded && <div id={bodyID}>{body}</div>}
      </div>
    );
  }

  return (
    <div className="ktree-spaces">
      <h2 className="ktree-heading">Spaces</h2>
      {body}
    </div>
  );
}

/**
 * The workspace's sections under the spaces: Agent skills, with the engine's
 * count of tool skills, and Agent diaries, with how many agents keep one. Agent
 * skills is not drawn where the pages are a vendor wiki's — there is no skill
 * page on this engine to count; a diary is the engine's own whatever the wiki.
 */
function Sections({ current }: { current: string }) {
  const skills = useQuery(
    "pages",
    { skills: true, status: "published", limit: 1 },
    {
      pollMs: TREE_POLL_MS,
    },
  );
  const diaries = useQuery("memory_overview", undefined, { pollMs: TREE_POLL_MS });
  return (
    <ul className="ktree-list ktree-sections" role="list">
      {skills.error !== "unknown_query" && (
        <SectionRow
          path={["knowledge", "skills"]}
          here={current === "skills"}
          mark={<WandSparklesGlyph size="sm" className="ktree-mark" />}
          label="Agent skills"
          count={skills.data?.total}
          countTitle="Tool-skill pages"
        />
      )}
      {diaries.error !== "unknown_query" && (
        <SectionRow
          path={["knowledge", "diaries"]}
          // THE LIST AND ONE AGENT'S DIARY BOTH SIT UNDER THIS ROW.
          here={current === "diaries" || current === "diary"}
          mark={<BrainGlyph size="sm" className="ktree-mark" />}
          label="Agent diaries"
          count={diaries.data?.seats.length}
          countTitle="Agents"
        />
      )}
    </ul>
  );
}

function SectionRow({
  path,
  here,
  mark,
  label,
  count,
  countTitle,
}: {
  path: string[];
  here: boolean;
  mark: ReactNode;
  label: string;
  count: number | undefined;
  countTitle: string;
}) {
  return (
    <li>
      <div className={cx("ktree-row", here && "is-current")} style={depthStyle(0)}>
        <a className="ktree-link" href={href(path)} aria-current={here ? "page" : undefined}>
          {mark}
          <span className="ktree-title">{label}</span>
          {count !== undefined && (
            <span className="ktree-count" title={countTitle}>
              {fmtExact(count)}
            </span>
          )}
        </a>
      </div>
    </li>
  );
}

function SpaceRow({
  space,
  owners,
  open,
  toggle,
  currentPage,
  current,
}: {
  space: PageContainer;
  owners: SpaceOwners;
  open: Set<string>;
  toggle: (node: string) => void;
  currentPage: string;
  current: boolean;
}) {
  const node = spaceNode(space.key);
  const expanded = open.has(node);
  const childrenID = useId();
  const label = space.name || space.key;
  const who = ownerSentence(owners);
  const projects = otherProjects(owners, space.key);
  return (
    <li>
      <div className={cx("ktree-row", current && "is-current")} style={depthStyle(0)}>
        <Expander
          label={label}
          expanded={expanded}
          controls={childrenID}
          onToggle={() => toggle(node)}
          // A SPACE WITH NO PAGES STILL OPENS ONTO ITS OWN SENTENCE, but it
          // says how many it holds so nobody has to open it to find out.
          empty={space.pages === 0}
        />
        <a
          className="ktree-link"
          href={href(["knowledge", space.key])}
          aria-current={current ? "page" : undefined}
          title={space.purpose || undefined}
        >
          <span className="ktree-title">{label}</span>
        </a>
        <span className="ktree-key mono" title={who}>
          {space.key}
          <span className="sr-only">. {who}.</span>
        </span>
        {/* THE PROJECT, DRAWN — not only in the key's tooltip, which a touch
            reader never sees — where it is one the space key does not
            already name. */}
        {projects.map((p) => (
          <span
            key={p}
            className="ktree-key ktree-proj mono"
            title={`Work filed from here is in ${p}`}
          >
            <span className="sr-only">tracker project </span>
            {p}
          </span>
        ))}
      </div>
      {expanded && (
        <div id={childrenID}>
          <Level
            params={{ container: space.key, roots: true }}
            depth={1}
            open={open}
            toggle={toggle}
            currentPage={currentPage}
            emptyNote="Nothing is filed in this space."
          />
        </div>
      )}
    </li>
  );
}

/**
 * One level of the tree: a space's top, or one page's children, read whole in
 * windows of 500 with "Load more" from the answer's cursor.
 */
function Level({
  params,
  depth,
  open,
  toggle,
  currentPage,
  emptyNote,
}: {
  params: Record<string, unknown>;
  depth: number;
  open: Set<string>;
  toggle: (node: string) => void;
  currentPage: string;
  emptyNote: string;
}) {
  const asked = useMemo(() => ({ ...TREE_PAGES, ...params }), [params]);
  const level = usePagedPages(asked, { pollMs: TREE_POLL_MS });
  if (level.error) {
    return (
      <p className="ktree-note" style={depthStyle(depth)}>
        This level could not be read ({level.error}).
      </p>
    );
  }
  if (!level.data) {
    return (
      <div style={depthStyle(depth)}>
        <Skeleton variant="text" rows={2} label="Loading pages" />
      </div>
    );
  }
  if (level.rows.length === 0) {
    return (
      <p className="ktree-note" style={depthStyle(depth)}>
        {emptyNote}
      </p>
    );
  }
  return (
    <>
      <ul className="ktree-list" role="list">
        {level.rows.map((page) => (
          <PageRow
            key={page.id}
            page={page}
            depth={depth}
            open={open}
            toggle={toggle}
            currentPage={currentPage}
          />
        ))}
      </ul>
      {level.more && (
        <div className="ktree-more" style={depthStyle(depth)}>
          <Button variant="ghost" size="small" onClick={level.loadMore} loading={level.paging}>
            Load more
          </Button>
          {/* A WINDOW LABELLED AS A WINDOW: how many are drawn of how many
              there are, never the drawn count as if it were the level. */}
          <span className="t-caption">
            {fmtExact(level.rows.length)} of {fmtExact(level.total ?? 0)}
          </span>
          {level.pageFailure && (
            // THE FAILURE WHOLE, as every page of older rows says it: the
            // grant a refusal named, or that the state log will not lift it.
            <QueryState
              error={level.pageFailure.error}
              refusal={level.pageFailure.refusal}
              detail={level.pageFailure.detail ?? undefined}
              loading={false}
            />
          )}
        </div>
      )}
    </>
  );
}

function PageRow({
  page,
  depth,
  open,
  toggle,
  currentPage,
}: {
  page: PageSummary;
  depth: number;
  open: Set<string>;
  toggle: (node: string) => void;
  currentPage: string;
}) {
  const node = pageNode(page.id);
  const folder = (page.children ?? 0) > 0;
  const expanded = folder && open.has(node);
  const current = currentPage === page.id;
  const childrenID = useId();
  const params = useMemo(() => ({ parent: page.id }), [page.id]);
  return (
    <li>
      <div className={cx("ktree-row", current && "is-current")} style={depthStyle(depth)}>
        {folder ? (
          <Expander
            label={page.title}
            expanded={expanded}
            controls={childrenID}
            onToggle={() => toggle(node)}
          />
        ) : (
          <span className="ktree-spacer" aria-hidden="true" />
        )}
        <a
          className="ktree-link"
          href={href(["knowledge", "pages", page.id])}
          aria-current={current ? "page" : undefined}
        >
          {/* A PAGE THAT HOLDS PAGES IS DRAWN AS A BOOK, a leaf as a sheet —
              the artboard's two marks, and the one hint a closed row gives
              about what is under it. */}
          {folder ? (
            <BookOpenGlyph size="sm" className="ktree-mark" aria-hidden="true" />
          ) : (
            <FileTextGlyph size="sm" className="ktree-mark" aria-hidden="true" />
          )}
          <span className="ktree-title">{page.title}</span>
          {page.status === "draft" && <span className="ktree-draft">draft</span>}
        </a>
      </div>
      {expanded && (
        <div id={childrenID}>
          <Level
            params={params}
            depth={depth + 1}
            open={open}
            toggle={toggle}
            currentPage={currentPage}
            emptyNote="Nothing is filed under it any more."
          />
        </div>
      )}
    </li>
  );
}

/** The chevron that opens a row: a button of its own, so the title stays a link. */
function Expander({
  label,
  expanded,
  controls,
  onToggle,
  empty,
}: {
  label: string;
  expanded: boolean;
  controls: string;
  onToggle: () => void;
  empty?: boolean;
}) {
  return (
    <button
      type="button"
      className="ktree-expander"
      aria-expanded={expanded}
      aria-controls={expanded ? controls : undefined}
      aria-label={`Pages in ${label}${empty ? " (none)" : ""}`}
      onClick={onToggle}
    >
      {expanded ? (
        <ChevronDownGlyph size="xs" aria-hidden="true" />
      ) : (
        <ChevronRightGlyph size="xs" aria-hidden="true" />
      )}
    </button>
  );
}

/** A row's indent: one step per level, the artboard's 14px. */
function depthStyle(depth: number) {
  return { "--ktree-depth": depth } as React.CSSProperties;
}
