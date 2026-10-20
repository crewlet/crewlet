/**
 * Boot a prototype page: the dashboard's own boot (`src/main.tsx`), with the
 * demo client where the network would be and the variant bar beside the app.
 *
 * EVERYTHING ELSE IS THE SHIPPED BOOT: the stylesheets in the same order, the
 * theme applied before the first paint, the retired storage keys swept, the
 * session kept alive, and `<App/>` under `ClientContext` and the hash
 * `Router`, in `StrictMode`. Nothing dials here: the frame reads
 * `GET /auth/session` and dials for the session it finds (`app/Shell.tsx`),
 * which the demo engine answers as the browser's principal. Only the client
 * is different, so a variant renders exactly as the shipped dashboard renders. `check/boot.test.ts` holds the stylesheet list below to
 * `src/main.tsx`'s, in order.
 */

// THE DESIGN SYSTEM, ABOVE EVERY MODULE IMPORT, in `src/main.tsx`'s order:
// the kit's variables, the palettes and the named themes, density, the faces,
// the document baseline, then the components' one sheet.
import "@crewlethq/tokens/css";
import "@crewlethq/tokens/css/themes";
import "@crewlethq/tokens/css/themes/all";
import "@crewlethq/tokens/css/density";
import "@crewlethq/tokens/css/fonts";
import "@crewlethq/tokens/css/base";
import "virtual:crewlet-design-system.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { bootTheme } from "~/lib/prefs.ts";
import { forgetRetiredKeys } from "~/lib/storage.ts";
import { SessionKeepAlive } from "~/protocol/index.ts";
import { fairsplit } from "../fixtures/fairsplit.ts";
import type { Topic } from "./defineTopic.ts";
import { makeDemoClient, type DemoClient } from "./demoClient.ts";
import { LOG } from "./engine/refusal.ts";
import { installRestShim } from "./restShim.ts";
import {
  freshReader,
  loadSession,
  rememberVariant,
  saveSession,
  scenarioActions,
} from "./session.ts";
import { VariantBar } from "./VariantBar.tsx";

// THE APP'S OWN SHEETS, AFTER ALL OF THE KIT'S, in `src/main.tsx`'s order; the
// variant bar's placement after them; a variant's own sheets after every one
// of these (the build's entry imports them after this module).
import "~/styles/tokens.css";
import "~/styles/base.css";
import "~/styles/components.css";
import "~/styles/frame.css";
import "~/styles/screens.css";
import "./VariantBar.css";

declare global {
  interface Window {
    /** The running prototype, for a reader poking at it from the console. */
    crewletPrototype?: { topic: Topic; variant: string; client: DemoClient };
  }
}

/** Web Storage that is in memory, for a frame that refuses the browser's. */
function memoryStorage(): Storage {
  const items = new Map<string, string>();
  return {
    get length() {
      return items.size;
    },
    key: (index: number) => [...items.keys()][index] ?? null,
    getItem: (key: string) => items.get(key) ?? null,
    setItem: (key: string, value: string) => void items.set(key, String(value)),
    removeItem: (key: string) => void items.delete(key),
    clear: () => items.clear(),
  };
}

/**
 * Storage that works: the browser's own where the frame allows it, and an
 * in-memory stand-in where it does not, so nothing in the dashboard reads a
 * refusal as a reader with no preferences who cannot keep any.
 */
function ensureStorage(): void {
  for (const area of ["localStorage", "sessionStorage"] as const) {
    try {
      const store = window[area];
      store.setItem("crewlet-prototype:probe", "1");
      store.removeItem("crewlet-prototype:probe");
    } catch {
      Object.defineProperty(window, area, { configurable: true, value: memoryStorage() });
    }
  }
}

/** The element the variant bar renders into, beside the app's root. */
const BAR_ROOT = "crewlet-prototype-bar";

/**
 * Boot `topic`'s variant `variantId` into the page's `#root`: the scenario
 * this tab last chose (the topic's first when it chose none), the company as
 * this tab's writes left it, and the variant bar.
 */
export function bootPrototype(topic: Topic, variantId: string): DemoClient {
  const variant = topic.variants.find((v) => v.id === variantId);
  if (!variant) throw new Error(`The topic "${topic.id}" has no variant "${variantId}".`);

  ensureStorage();
  rememberVariant(topic.id, variant.id);
  // Before the first paint, as the shipped boot does.
  bootTheme();
  forgetRetiredKeys(() => localStorage);
  // A page opened with no address opens where the topic starts.
  if (location.hash === "" || location.hash === "#" || location.hash === "#/") {
    history.replaceState(history.state, "", topic.start);
  }

  const saved = loadSession(topic);
  const scenario = topic.scenarios.find((s) => s.id === saved?.scenario) ?? topic.scenarios[0]!;
  if (!saved?.company) freshReader();
  const company = saved?.company ?? scenario.apply(fairsplit());
  const client = makeDemoClient(company);
  installRestShim(client.engine);
  client.engine.onChange((changed) => saveSession(topic, scenario.id, changed));
  // The session idles out with no request, and the socket is not one.
  new SessionKeepAlive(client.socket).start();
  window.crewletPrototype = { topic, variant: variant.id, client };

  const host = document.getElementById("root");
  if (!host) throw new Error("no #root in the shell");
  createRoot(host).render(
    <StrictMode>
      <ClientContext.Provider value={{ store: client.store, socket: client.socket }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>
    </StrictMode>,
  );

  let bar = document.getElementById(BAR_ROOT);
  if (!bar) {
    bar = document.createElement("div");
    bar.id = BAR_ROOT;
    document.body.append(bar);
  }
  createRoot(bar).render(
    <StrictMode>
      <VariantBar
        topic={topic}
        variant={variant}
        scenario={scenario.id}
        {...scenarioActions(topic, scenario.id)}
      />
    </StrictMode>,
  );
  console.info(LOG, `${topic.title}, variant "${variant.label}", scenario "${scenario.label}".`);
  return client;
}
