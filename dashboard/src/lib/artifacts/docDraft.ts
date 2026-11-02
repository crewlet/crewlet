/**
 * A person's edit of a doc: its tabs' text, titles and order, tabs added and
 * removed, the publish that saves it, and the merge a refused save is moved
 * onto the newer version by.
 *
 * ONE DRAFT FOR THE WHOLE DOC, so a save is one version however many tabs it
 * touched, based on the version the edit started from (D69). What it sends is
 * the least that says the change: an exact edit of each changed tab, of the
 * index when the tabs' list changed, a removal for each tab dropped, and the
 * bytes of each tab added (or of the index, the first time a doc of one file
 * gains a second tab, which then opens on `doc.json`).
 *
 * A MERGE BY TAB. A save refused because somebody published since is moved
 * onto their version tab by tab: a tab's text by line (`lib/merge.ts`, as a
 * Doclet page's), its title and the tabs' order each as one value, the
 * order through the deck's own rules (`orderMerge.ts`). What both changed
 * differently is the person's to settle before saving again; nothing is
 * guessed.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives.
 */

import { merge3, mergedText, type Merge, type Resolution } from "../merge.ts";
import { utf8Bytes } from "../format.ts";
import { DOC_MAX_TABS, DOC_TAB_TITLE_MAX_BYTES } from "~/contract/artifacts.ts";
import type { ArtifactDoc } from "~/protocol/index.ts";
import { editOf, type DocEdit } from "./doc.ts";
import { mergeOrder, placeAfter } from "./orderMerge.ts";

/** A tabbed doc's index, its entry (`docindex.IndexPath`). */
export const DOC_INDEX = "doc.json";

/** One tab as a draft holds it. */
export interface DraftTab {
  path: string;
  title: string;
  text: string;
}

/** A doc as an edit holds it. */
export interface DocDraft {
  /** The version the draft started from: what its save is based on. */
  base: number;
  /** Whether that version opens on `doc.json` rather than one Markdown file. */
  indexed: boolean;
  order: string[];
  tabs: Readonly<Record<string, DraftTab>>;
}

export function docDraftOf(doc: ArtifactDoc, version: number): DocDraft {
  const tabs: Record<string, DraftTab> = {};
  for (const t of doc.tabs) tabs[t.path] = { path: t.path, title: t.title, text: t.text };
  return { base: version, indexed: doc.indexed, order: doc.tabs.map((t) => t.path), tabs };
}

// ---- what changed ---------------------------------------------------------- //

export interface DocChanges {
  /** Tabs of the base whose text changed, in the draft's order. */
  texts: string[];
  added: string[];
  removed: string[];
  /** Whether the list of tabs changed: their order, a title, or which tabs there are. */
  index: boolean;
}

export function docChangesOf(base: DocDraft, draft: DocDraft): DocChanges {
  const texts = draft.order.filter(
    (p) => base.tabs[p] !== undefined && base.tabs[p]!.text !== draft.tabs[p]!.text,
  );
  const added = draft.order.filter((p) => base.tabs[p] === undefined);
  const removed = base.order.filter((p) => !draft.order.includes(p));
  const index =
    draft.order.join("\n") !== base.order.join("\n") ||
    draft.order.some(
      (p) => base.tabs[p] !== undefined && base.tabs[p]!.title !== draft.tabs[p]!.title,
    );
  return { texts, added, removed, index };
}

export function docChanged(c: DocChanges): boolean {
  return c.texts.length > 0 || c.added.length > 0 || c.removed.length > 0 || c.index;
}

// ---- the edits ------------------------------------------------------------ //

/** Why a tab title cannot be saved, or null: the engine's rules (`docindex.ParseIndex`), said at the field. */
export function tabTitleProblem(title: string): string | null {
  if (title.trim() === "") return "Give the tab a title.";
  const bytes = utf8Bytes(title);
  if (bytes > DOC_TAB_TITLE_MAX_BYTES) {
    return `The title is ${bytes} bytes and the most is ${DOC_TAB_TITLE_MAX_BYTES}.`;
  }
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f-\u009f]/.test(title)) return "A title holds no control characters.";
  return null;
}

/** Why a tab cannot be added, or null. */
export function addTabProblem(draft: DocDraft): string | null {
  return draft.order.length >= DOC_MAX_TABS ? `A doc holds at most ${DOC_MAX_TABS} tabs.` : null;
}

/**
 * A new tab's file: `tabs/<words of its title>.md`, numbered past every path
 * in `taken`, which is every file of the version as well as every tab of the
 * draft, so a new tab never writes over a file the doc already holds.
 */
export function newTabPath(title: string, taken: Iterable<string>): string {
  const used = new Set(taken);
  const words =
    title
      .toLowerCase()
      .normalize("NFKD")
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 48) || "tab";
  for (let n = 1; ; n++) {
    const path = `tabs/${n === 1 ? words : `${words}-${n}`}.md`;
    if (!used.has(path)) return path;
  }
}

/** The draft with a tab added after `after` (last when absent), headed by its title. */
export function addTab(
  draft: DocDraft,
  title: string,
  taken: Iterable<string>,
  after?: string,
): { draft: DocDraft; path: string } {
  const path = newTabPath(title, [...taken, ...draft.order]);
  const order = [...draft.order];
  const at = after === undefined ? order.length : order.indexOf(after) + 1;
  order.splice(at, 0, path);
  const tabs = {
    ...draft.tabs,
    [path]: { path, title: title.trim(), text: `# ${title.trim()}\n` },
  };
  return { draft: { ...draft, order, tabs }, path };
}

export function renameTab(draft: DocDraft, path: string, title: string): DocDraft {
  const tab = draft.tabs[path];
  if (!tab) return draft;
  return { ...draft, tabs: { ...draft.tabs, [path]: { ...tab, title } } };
}

export function setTabText(draft: DocDraft, path: string, text: string): DocDraft {
  const tab = draft.tabs[path];
  if (!tab) return draft;
  return { ...draft, tabs: { ...draft.tabs, [path]: { ...tab, text } } };
}

/** The draft with its tabs in `order`, which must be a permutation of the draft's own. */
export function reorderTabs(draft: DocDraft, order: readonly string[]): DocDraft {
  if (order.length !== draft.order.length || order.some((p) => !draft.tabs[p])) return draft;
  return { ...draft, order: [...order] };
}

/** The draft without the tab at path. A doc keeps at least one tab, so the last is never removed. */
export function removeTab(draft: DocDraft, path: string): DocDraft {
  if (draft.order.length <= 1 || !draft.order.includes(path)) return draft;
  const tabs = { ...draft.tabs };
  delete tabs[path];
  return { ...draft, order: draft.order.filter((p) => p !== path), tabs };
}

/**
 * The draft with every tab it added moved off a path `taken` names: a new
 * tab's file is written whole, so one on a path the version already holds
 * (a file no tab lists) would replace that file.
 */
export function withFreePaths(base: DocDraft, draft: DocDraft, taken: Iterable<string>): DocDraft {
  const files = new Set(taken);
  const used = new Set([...files, ...draft.order]);
  let out = draft;
  for (const p of draft.order) {
    if (base.tabs[p] || !files.has(p)) continue;
    const to = newTabPath(draft.tabs[p]!.title, used);
    used.add(to);
    const tabs: Record<string, DraftTab> = { ...out.tabs, [to]: { ...out.tabs[p]!, path: to } };
    delete tabs[p];
    out = { ...out, order: out.order.map((q) => (q === p ? to : q)), tabs };
  }
  return out;
}

// ---- the publish ---------------------------------------------------------- //

/** The index a draft's tabs make, as `doc.json` holds it. */
export function indexSource(draft: DocDraft): string {
  const tabs = draft.order.map((p) => ({ path: p, title: draft.tabs[p]!.title }));
  return JSON.stringify({ v: 1, tabs }, null, 2) + "\n";
}

/** What a save sends: the edits, the removals, the files whose bytes it uploads, and a new entry. */
export interface DocPublishPlan {
  edits: DocEdit[];
  removes: string[];
  uploads: { path: string; text: string }[];
  /** `doc.json`, the first time a doc of one file gains an index. */
  entry?: string;
}

/**
 * The publish that saves `draft` against `base`. `indexRaw` is the base's
 * `doc.json` as it stands, which an edit of it must quote; null for a base of
 * one file.
 *
 * A changed tab goes as its exact edit, or whole where no edit can address it
 * (an empty tab, which holds no text to replace).
 */
export function docPublishPlan(
  base: DocDraft,
  draft: DocDraft,
  indexRaw: string | null,
): DocPublishPlan {
  const c = docChangesOf(base, draft);
  const plan: DocPublishPlan = { edits: [], removes: c.removed, uploads: [] };
  for (const p of c.texts) {
    const e = editOf(p, base.tabs[p]!.text, draft.tabs[p]!.text);
    if (e) plan.edits.push(e);
    else plan.uploads.push({ path: p, text: draft.tabs[p]!.text });
  }
  for (const p of c.added) plan.uploads.push({ path: p, text: draft.tabs[p]!.text });
  const membership = c.added.length > 0 || c.removed.length > 0;
  if (c.index || membership) {
    const next = indexSource(draft);
    if (base.indexed) {
      if (indexRaw === null)
        throw new Error("the doc's index was not read, so its edit has nothing to replace");
      const e = editOf(DOC_INDEX, indexRaw, next);
      if (e) plan.edits.push(e);
    } else {
      plan.uploads.push({ path: DOC_INDEX, text: next });
      plan.entry = DOC_INDEX;
    }
  }
  return plan;
}

// ---- the merge ------------------------------------------------------------ //

/** One thing both sides changed differently, for the person to settle. */
export type DocConflict =
  /** Both changed the same lines of a tab: settled clash by clash. */
  | { kind: "text"; path: string; title: string; merge: Merge }
  /** One side removed a tab the other changed. */
  | { kind: "tab"; path: string; title: string }
  | { kind: "title"; path: string; title: string }
  | { kind: "order" };

/** A draft moved onto a newer version. */
export interface DocMerge {
  /** The merged draft, based on the newer version; a conflict holds the person's own side until settled. */
  draft: DocDraft;
  /** The newer version, as its own draft: the base the merged draft's save is against. */
  live: DocDraft;
  /** The titles of the tabs the newer version changed, added or removed, which the person is told about. */
  theirs: string[];
  conflicts: DocConflict[];
}

const sameTab = (a: DraftTab | undefined, b: DraftTab | undefined) =>
  a === b || (!!a && !!b && a.text === b.text && a.title === b.title);

/** Move mine (started from base) onto live, the newer version. */
export function mergeDoc(base: DocDraft, mine: DocDraft, live: DocDraft): DocMerge {
  const conflicts: DocConflict[] = [];
  const tabs: Record<string, DraftTab> = { ...live.tabs };
  const theirs = [
    ...live.order
      .filter((p) => !sameTab(base.tabs[p], live.tabs[p]))
      .map((p) => live.tabs[p]!.title),
    ...base.order.filter((p) => !live.order.includes(p)).map((p) => base.tabs[p]!.title),
  ];
  const deleted = new Set<string>();
  // A TAB MINE KEEPS THAT LIVE REMOVED, held until the person settles it.
  const kept: string[] = [];
  // A TAB BOTH SIDES ADDED AT ONE PATH is mine moved to a path of its own.
  const renamed = new Map<string, string>();
  for (const p of mine.order) {
    if (!base.tabs[p] && live.tabs[p]) {
      renamed.set(
        p,
        newTabPath(mine.tabs[p]!.title, [...live.order, ...mine.order, ...renamed.values()]),
      );
    }
  }
  const mineOrder = mine.order.map((p) => renamed.get(p) ?? p);
  const mineTab = (p: string): DraftTab | undefined => {
    const from = [...renamed].find(([, to]) => to === p)?.[0] ?? p;
    const t = mine.order.includes(from) ? mine.tabs[from] : undefined;
    return t && { ...t, path: p };
  };

  for (const p of new Set([...base.order, ...mineOrder])) {
    const b = base.tabs[p];
    const m = mineTab(p);
    const l = live.order.includes(p) ? live.tabs[p] : undefined;
    if (!b) {
      if (m) tabs[p] = m;
      continue;
    }
    if (sameTab(b, m)) continue;
    if (!m) {
      if (sameTab(b, l)) deleted.add(p);
      else if (l) {
        conflicts.push({ kind: "tab", path: p, title: l.title });
        deleted.add(p);
      }
      continue;
    }
    if (!l) {
      conflicts.push({ kind: "tab", path: p, title: m.title });
      tabs[p] = m;
      kept.push(p);
      continue;
    }
    let text = l.text;
    if (m.text !== b.text) {
      if (l.text === b.text || l.text === m.text) text = m.text;
      else {
        const merge = merge3(b.text, m.text, l.text);
        if (merge.conflicts === 0) text = mergedText(merge);
        else {
          conflicts.push({ kind: "text", path: p, title: m.title, merge });
          text = m.text;
        }
      }
    }
    let title = l.title;
    if (m.title !== b.title) {
      if (l.title !== b.title && l.title !== m.title)
        conflicts.push({ kind: "title", path: p, title: m.title });
      title = m.title;
    }
    tabs[p] = { path: p, title, text };
  }

  const merged = mergeOrder(base.order, mineOrder, live.order, deleted);
  if (merged.conflict) conflicts.push({ kind: "order" });
  const order = merged.order;
  for (const p of kept) {
    if (!order.includes(p))
      order.splice(placeAfter(order, mineOrder.slice(0, mineOrder.indexOf(p))), 0, p);
  }
  for (const p of Object.keys(tabs)) if (!order.includes(p)) delete tabs[p];
  return {
    draft: { base: live.base, indexed: live.indexed, order, tabs },
    live,
    theirs,
    conflicts,
  };
}

/** How a person settled one conflict that is not a text clash. */
export type DocResolution = "mine" | "theirs";

/**
 * A merge with one conflict settled: a text clash by its choices, place by
 * place; anything else by keeping the person's side or taking the newer
 * version's.
 */
export function resolveDoc(
  merge: DocMerge,
  conflict: DocConflict,
  choice: DocResolution | Resolution[],
): DocMerge {
  const rest = merge.conflicts.filter((c) => c !== conflict);
  const d = merge.draft;
  const live = merge.live;
  if (conflict.kind === "text") {
    if (!Array.isArray(choice)) return merge;
    const draft = setTabText(d, conflict.path, mergedText(conflict.merge, choice));
    return { ...merge, draft, conflicts: rest };
  }
  if (choice === "mine") return { ...merge, conflicts: rest };
  switch (conflict.kind) {
    case "tab": {
      const tabs = { ...d.tabs };
      let order = d.order.filter((p) => p !== conflict.path);
      const theirs = live.order.includes(conflict.path) ? live.tabs[conflict.path] : undefined;
      if (theirs) {
        tabs[conflict.path] = theirs;
        order = [...order];
        order.splice(
          placeAfter(order, live.order.slice(0, live.order.indexOf(conflict.path))),
          0,
          conflict.path,
        );
      } else {
        delete tabs[conflict.path];
      }
      return { ...merge, draft: { ...d, order, tabs }, conflicts: rest };
    }
    case "title": {
      const theirs = live.tabs[conflict.path];
      return {
        ...merge,
        draft: theirs ? renameTab(d, conflict.path, theirs.title) : d,
        conflicts: rest,
      };
    }
    case "order": {
      const order = live.order.filter((p) => d.tabs[p]);
      for (const p of d.order) {
        if (!order.includes(p))
          order.splice(placeAfter(order, d.order.slice(0, d.order.indexOf(p))), 0, p);
      }
      return { ...merge, draft: { ...d, order }, conflicts: rest };
    }
  }
}
