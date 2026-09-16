/**
 * Boot.
 *
 * The socket is created BEFORE React mounts and lives for the life of the tab:
 * it is the application's one connection, it re-hydrates the whole projection
 * on every reconnect, and a transport owned by a component tree is a transport
 * that reconnects whenever somebody refactors a provider.
 */

// The design system, in cascade order: the variables, then the themes that
// repaint them, then density, the faces and the document baseline. themes.css
// and tokens.css share the :root selector and neither adds specificity, so
// the later import is the one that paints.
//
// FIRST IN THE FILE, ABOVE EVERY MODULE IMPORT. An import statement is
// evaluated in source order, and the module graph under `./app/App.tsx`
// carries a side-effect stylesheet per uilet component, so a baseline
// imported below it is emitted below all of them. Order decides a tie, and
// there is one: `:focus-visible` in the baseline and `.crewlet-btn` in the
// component both count as a single class, so a baseline that came last gave
// every focused control the baseline's own radius and squared off every
// button the moment a reader tabbed to it.
import "@crewlethq/tokens/css";
import "@crewlethq/tokens/css/themes";
import "@crewlethq/tokens/css/density";
import "@crewlethq/tokens/css/fonts";
import "@crewlethq/tokens/css/base";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App.tsx";
import { Router } from "./app/router.tsx";
import { ClientContext } from "./lib/store-hooks.ts";
import { LiveSocket, Store, apiToken } from "./protocol/index.ts";
import { applyStoredPreferences } from "@crewlethq/ui";

// The engine's own sheets last, so a screen's own layout outranks anything it
// inherits. There is no component sheet: every primitive this dashboard draws
// is the design system's, and what is left is the utilities every screen leans
// on plus the layout each FAMILY of screens needs on top of them.
//
// ONE FILE PER FAMILY, and the file name is who owns it: a rule a screen needs
// goes in its family's sheet and nowhere else, which is what stops the single
// sheet these replace from growing a rule for every screen again. They are
// imported HERE rather than from the screens themselves for the reason the
// block above gives: an import inside the module graph lands above this file's
// own imports, so a screen's layout would be emitted before the baseline it
// sits on and lose every cascade tie to it.
import "./styles/base.css";
import "./styles/live.css";
import "./styles/records.css";
import "./styles/org.css";
import "./styles/configure.css";
import "./styles/integrations.css";

// Before the first paint, so a reader whose machine is set to light never sees
// a dark flash on the way to their own preference. A Content-Security-Policy
// with no inline script is why this is a call from the module bundle rather
// than the usual snippet in the document head.
applyStoredPreferences({ themeKey: "crewlet_theme", densityKey: "crewlet_density" });

const store = new Store();
const socket = new LiveSocket(store);
socket.setToken(apiToken());
socket.start();

const host = document.getElementById("root");
if (!host) throw new Error("no #root in the shell");

createRoot(host).render(
  <StrictMode>
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>
  </StrictMode>,
);
