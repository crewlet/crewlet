/**
 * The atlas's screens mounted over a socket that answers its questions from a
 * fixture, as the engine would: every ask recorded, the reader's `viewer`
 * answered where a suite names one, and every other answer a suite brings.
 */

import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { Announcer, LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import type { AtlasEntryAnswer } from "~/contract/atlas.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import {
  activityAnswer,
  entriesAnswer,
  entryAnswer,
  entryWorkAnswer,
  facetsAnswer,
  logAnswer,
  mapAnswer,
  namedAnswer,
  traceAnswer,
  workAnswer,
  type AtlasFixture,
} from "./atlas.ts";
import { InertWebSocket } from "./client.tsx";
import { healthFrame } from "./health.ts";

export type Asked = { what: string; params: Record<string, unknown> };

export interface AtlasMountOptions {
  /** One entry's own answer, where the fixture's summary is not enough. */
  entry?: (id: string) => AtlasEntryAnswer | undefined;
  /** Answers by question, read before the fixture's: `viewer`, `atlas_review`, `atlas_work`, `pages`. */
  answers?: Record<string, unknown>;
}

/** Mount `children` at `hash` over `fixture`, recording each ask. */
export function mountAtlas(
  hash: string,
  children: ReactNode,
  fixture: AtlasFixture,
  { entry = () => undefined, answers = {} }: AtlasMountOptions = {},
) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = hash;
  const asked: Asked[] = [];
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string, params?: unknown) => Promise<unknown> }).query = (
    what,
    raw,
  ) => {
    const params = (typeof raw === "string" ? JSON.parse(raw) : (raw ?? {})) as Record<
      string,
      unknown
    >;
    asked.push({ what, params });
    if (what in answers) return Promise.resolve(answers[what]);
    const id = String(params.id ?? "");
    const from = fixture.nodes.find((node) => node.id === id);
    switch (what) {
      case "atlas_facets":
        return Promise.resolve(facetsAnswer(fixture));
      case "atlas_map": {
        // A SEARCH LEAVES THE ENTRIES WHOSE NAME HOLDS IT, as the engine's does.
        const q = String(params.q ?? "").toLowerCase();
        return Promise.resolve(
          mapAnswer(
            q === ""
              ? fixture
              : {
                  ...fixture,
                  nodes: fixture.nodes.filter((node) => node.name.toLowerCase().includes(q)),
                },
          ),
        );
      }
      case "atlas_entries":
        return Promise.resolve(
          entriesAnswer(
            fixture,
            Number(params.limit ?? 50),
            Number(params.cursor ?? 0),
            String(params.sort ?? ""),
          ),
        );
      case "atlas_activity":
        return Promise.resolve(activityAnswer(fixture));
      case "atlas_log":
        return Promise.resolve(logAnswer(fixture, id));
      case "atlas_entry": {
        const answer = entry(id) ?? (from ? entryAnswer(from) : undefined);
        return answer ? Promise.resolve(answer) : new Promise(() => {});
      }
      case "atlas_trace":
        return from
          ? Promise.resolve(
              traceAnswer(
                fixture,
                from,
                params.direction === "down" ? "down" : "up",
                Number(params.depth ?? 6),
              ),
            )
          : new Promise(() => {});
      // NO TASK NAMES ANY ENTRY unless a suite brings the work that does.
      case "atlas_work":
        return Promise.resolve(workAnswer({}));
      case "atlas_entry_work":
        return from ? Promise.resolve(entryWorkAnswer()) : new Promise(() => {});
      case "atlas_named":
        return Promise.resolve(
          namedAnswer(
            fixture,
            String(params.ids ?? "")
              .split(",")
              .filter(Boolean),
          ),
        );
      default:
        return new Promise(() => {});
    }
  };
  const view = render(
    <ToastProvider>
      <Announcer label="Announcements" />
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <SessionReading>
            <ViewerProvider>
              <Router>{children}</Router>
            </ViewerProvider>
          </SessionReading>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { view, asked, store };
}
