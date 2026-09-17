/**
 * Boot.
 *
 * The socket is created BEFORE React mounts and lives for the life of the tab:
 * it is the application's one connection, it re-hydrates the whole projection
 * on every reconnect, and a transport owned by a component tree is a transport
 * that reconnects whenever somebody refactors a provider.
 */

// THE DESIGN SYSTEM, ABOVE EVERY MODULE IMPORT. An import statement is
// evaluated in source order, and the module graph under `./app/App.tsx`
// carries a side-effect stylesheet per uilet component — so a baseline
// imported below it is emitted below all of them. Order decides a tie and
// there is one: `:focus-visible` in the baseline and `.crewlet-btn` in the
// component are both a single class, so a baseline that came last gave every
// focused control the baseline's radius and squared off every button the
// moment a reader tabbed to it.
//
// Cascade order within the set: the variables, the themes that repaint them,
// density, the faces, then the document baseline.
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
import { bootTheme } from "./lib/prefs.ts";
import { LiveSocket, Store, apiToken } from "./protocol/index.ts";

// OUR NAMES, RESOLVED TO THEIRS. Between uilet's variables and our
// stylesheets: the targets have to exist before an alias can resolve, and
// tokens.css below still owns the layout constants uilet has no opinion
// about — the rail's width, a row's height, the peek's column.
import "./styles/uilet.css";
import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/components.css";
import "./styles/shell.css";
// THE FRAME AFTER THE SHELL, so the rail, the workspace sidebar and the page
// bar win over the single-sidebar layout the shell still carries rules for.
import "./styles/frame.css";
import "./styles/screens.css";

// Before the first paint, so a reader whose machine is set to light never sees
// a dark flash on the way to their own preference.
bootTheme();

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
