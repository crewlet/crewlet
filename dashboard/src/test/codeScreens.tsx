/**
 * The frame a Gitlet screen's suite mounts it in, and the engine it answers
 * from: every REST question routed by its path to the case's answers, every
 * URL asked kept for the case to read.
 */

import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { engineJson } from "~/test/engineAnswer.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";

export const CODE_POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 30 };

export const CODE_ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
}) as unknown as OrgProjection;

/** An answer as the engine writes one, its mark included ([engineJson]). */
export const jsonAnswer = (payload: unknown, status = 200) => engineJson(payload, status);

/**
 * The engine's REST answers: `answers` maps a question's path
 * (`/query/code_queue`) to its answer for a URL; any other path is 404, and
 * the session's own read never settles. Returns every URL asked.
 */
export function stubCodeEngine(answers: Record<string, (url: URL) => Response>): URL[] {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      const answer = answers[url.pathname];
      return Promise.resolve(answer ? answer(url) : jsonAnswer({}, 404));
    }),
  );
  return asked;
}

/**
 * Mount `screen` at `hash`, inside the frame every Gitlet screen reads through,
 * and answer the store its socket reports to — for a case that takes the
 * socket away and back.
 */
export function mountCode(hash: string, screen: ReactNode): Store {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(CODE_ORG);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(ADMIN_VIEWER) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>{screen}</Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  return store;
}

/**
 * Whether a grid's column, by its head's words, sits on the right edge: the
 * kit's rule for times and figures, which a time column left at the start breaks.
 */
export function onTheRight(grid: HTMLElement, head: string): boolean {
  const cell = Array.from(grid.querySelectorAll<HTMLElement>('[role="columnheader"]')).find(
    (h) => h.textContent?.trim() === head,
  );
  if (!cell) throw new Error(`no column head reads “${head}”`);
  return cell.className.includes("crewlet-data-grid__th--right");
}
