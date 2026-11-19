/**
 * One engine line naming a task, a page, an artifact, a change and a task that
 * is gone, and a client whose engine resolves them (`references`), for every
 * surface's suite to hold to one rule (`components/NamedRef.tsx`): what the
 * reader may open is a marked link to its page with its title on hover, and
 * what they may not, or what no longer exists, is its marked words, never a
 * link.
 */

import type { ReactNode } from "react";
import { expect } from "vitest";
import { ClientContext, type Client } from "~/lib/store-hooks.ts";
import { Store, type NamedRef, type ReferencesAnswer } from "~/protocol/index.ts";

export const NAMED_REFS: NamedRef[] = [
  { kind: "work_item", id: "0198-task", text: "ENG-287" },
  { kind: "page", id: "page-1", text: "Runbook" },
  { kind: "artifact", id: "art-1", text: "Launch plan" },
  { kind: "code_change", id: "web!42", text: "web!42" },
  { kind: "work_item", id: "0198-gone", text: "ENG-1" },
];

/** The line the refs are runs of, in order. */
export const NAMED_LINE = "handed ENG-287 with Runbook, Launch plan and web!42, after ENG-1";

/** What the engine answers: every ref but the gone task is open. */
const OPEN: ReferencesAnswer["references"] = [
  { kind: "work_item", id: "0198-task", title: "Rotate keys" },
  { kind: "page", id: "page-1", title: "Deploy runbook" },
  { kind: "artifact", id: "art-1", title: "Launch plan v2" },
  { kind: "code_change", id: "web!42", title: "Fix login" },
];

/**
 * A client whose socket answers `references` as an engine would, for the
 * prose fallback's guess `api!7` too, and says what it was asked.
 */
export function namedClient(): { client: Client; asks: string[][] } {
  const asks: string[][] = [];
  const socket = {
    query: (what: string, params: { refs: string[] }) => {
      if (what !== "references") return Promise.reject(new Error(`unexpected ${what}`));
      asks.push(params.refs);
      return Promise.resolve(namedAnswer(params));
    },
  };
  return { client: { store: new Store(), socket: socket as unknown as Client["socket"] }, asks };
}

/** The `references` answer an engine gives for [NAMED_REFS] and the fallback's guesses. */
export function namedAnswer(params: Record<string, unknown>): ReferencesAnswer {
  const asked = Array.isArray(params["refs"]) ? (params["refs"] as string[]) : [];
  const open = [...OPEN, { kind: "code_change", id: "api!7", title: "Retry" }];
  return { references: open.filter((r) => asked.includes(`${r.kind}:${r.id}`)), unresolved: [] };
}

export function WithClient({ client, children }: { client: Client; children: ReactNode }) {
  return <ClientContext.Provider value={client}>{children}</ClientContext.Provider>;
}

/** The reference drawn with `words` inside `root`. */
export function drawnRef(root: ParentNode, words: string): HTMLElement {
  const found = [...root.querySelectorAll<HTMLElement>(".crewlet-inline-ref")].find(
    (r) => r.querySelector(".crewlet-inline-ref__text")?.textContent === words,
  );
  if (!found) throw new Error(`no reference reads ${words}`);
  return found;
}

/**
 * The rule every surface keeps: the task, the page, the artifact and the
 * change are marked links to their pages with their titles, and the gone task
 * is its marked words with no link.
 */
export function expectNamedLinks(root: ParentNode): void {
  for (const [words, href, title] of [
    ["ENG-287", "#/worklet/0198-task", "Rotate keys"],
    ["Runbook", "#/doclet/pages/page-1", "Deploy runbook"],
    ["Launch plan", "#/artifacts/art-1", "Launch plan v2"],
    ["web!42", "#/gitlet/repos/web/changes/42", "Fix login"],
  ] as const) {
    const ref = drawnRef(root, words);
    expect(ref.tagName, words).toBe("A");
    expect(ref.getAttribute("href"), words).toBe(href);
    expect(ref.getAttribute("title"), words).toBe(title);
    expect(ref.querySelector(".crewlet-inline-ref__mark"), words).not.toBeNull();
  }
  const gone = drawnRef(root, "ENG-1");
  expect(gone.tagName).toBe("SPAN");
  expect(gone.querySelector(".crewlet-inline-ref__mark")).not.toBeNull();
  expect(gone.closest("a")?.classList.contains("crewlet-inline-ref")).not.toBe(true);
}
