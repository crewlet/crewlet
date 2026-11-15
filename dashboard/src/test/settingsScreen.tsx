/**
 * One Settings screen mounted as the frame mounts it, for a suite about what
 * the screen WRITES: a store with a health push and an org, a socket whose
 * queries the suite answers, and the REST surface stubbed at `fetch`.
 */

import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { InertWebSocket } from "./client.tsx";
import { ENGINE_HEADERS } from "./engineAnswer.ts";
import { healthFrame } from "./health.ts";
import { withDerived } from "./org.ts";
import { ADMIN_VIEWER } from "./viewer.ts";

/** A signed-in reader who holds no grant that writes the company document. */
export const READER_VIEWER = { ...ADMIN_VIEWER, login: "reader", grants: ["worklet"] };

/**
 * Mounts `screen` over `answers`, the socket's answer per question (a value,
 * or a function of the question's parameters). A question the suite does not
 * answer stays in flight.
 */
export function mountSettings(
  screen: ReactElement,
  answers: Record<string, unknown>,
  {
    hash = "#/settings",
    viewer = ADMIN_VIEWER,
    org = { timezone: "UTC", roles: [] },
  }: { hash?: string; viewer?: Record<string, unknown>; org?: Record<string, unknown> } = {},
): { store: Store; asked: string[] } {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = hash;
  const store = new Store();
  store.applyHealth(healthFrame({ applied_epoch: 4 }));
  store.setConnected(true);
  store.applyOrg(withDerived(org as Parameters<typeof withDerived>[0]));
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string, params?: unknown) => Promise<unknown> }).query = (
    what,
    params,
  ) => {
    asked.push(what);
    if (what === "viewer") return Promise.resolve(viewer);
    if (!(what in answers)) return new Promise(() => {});
    const answer = answers[what];
    return Promise.resolve(typeof answer === "function" ? answer(params) : answer);
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ToastProvider>
        <LayerHost>
          <FrameReadings>
            <Router>{screen}</Router>
          </FrameReadings>
        </LayerHost>
      </ToastProvider>
    </ClientContext.Provider>,
  );
  return { store, asked };
}

/** One request a screen sent. */
export interface EngineCall {
  method: string;
  /** The path and query, from `/config` on. */
  path: string;
  /** The body as sent: parsed where it is JSON, else the text. */
  body: unknown;
  headers: Headers;
}

/** Stubs the REST surface; `respond` answers each request. Returns the calls, in order. */
export function stubEngine(respond: (call: EngineCall) => Response): EngineCall[] {
  const calls: EngineCall[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const raw = typeof init.body === "string" ? init.body : undefined;
      let body: unknown = raw;
      try {
        if (raw !== undefined) body = JSON.parse(raw);
      } catch {
        // Not JSON: a YAML document travels as the text it is.
      }
      const call: EngineCall = {
        method: init.method ?? "GET",
        path: String(url).replace(/^[a-z]+:\/\/[^/]+/, ""),
        body,
        headers: new Headers(init.headers),
      };
      calls.push(call);
      return respond(call);
    }),
  );
  return calls;
}

/** A JSON answer the engine wrote. */
export const engineAnswer = (
  payload: unknown,
  status: number,
  headers: Record<string, string> = {},
): Response =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", ...headers },
  });

/** A dry run's `200`: valid against `r1`, with `warnings`. */
export const validAnswer = (warnings: unknown[] = []): Response =>
  engineAnswer({ valid: true, base_revision_id: "r1", warnings, derived: null }, 200);

/** A write's `201`: stored as `r2`, activated at epoch 5. */
export const savedAnswer = (): Response =>
  engineAnswer({ revision_id: "r2", epoch: 5, warnings: [], derived: null }, 201);
