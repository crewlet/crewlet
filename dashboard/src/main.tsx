/**
 * Boot.
 *
 * The socket is created BEFORE React mounts and lives for the life of the tab:
 * it is the application's one connection, it re-hydrates the whole projection
 * on every reconnect, and a transport owned by a component tree is a transport
 * that reconnects whenever somebody refactors a provider.
 */

// THE DESIGN SYSTEM, ABOVE EVERY MODULE IMPORT. An import statement is
// evaluated in source order, so a baseline imported below the components'
// sheets is emitted below all of them. Order decides a tie and
// there is one: `:focus-visible` in the baseline and `.crewlet-btn` in the
// component are both a single class, so a baseline that came last gave every
// focused control the baseline's radius and squared off every button the
// moment a reader tabbed to it.
//
// Cascade order within the set: the kit's variables, the themes that repaint
// them, density, the faces, the document baseline, then the components.
import "@crewlethq/tokens/css";
import "@crewlethq/tokens/css/themes";
import "@crewlethq/tokens/css/density";
import "@crewlethq/tokens/css/fonts";
import "@crewlethq/tokens/css/base";
// THE COMPONENTS' SHEETS, WHOLE AND HERE — not one per component as each
// module is reached, which a code-split build turns into a sheet per lazy
// chunk appended after ours. `vite.config.ts`'s `designSystemSheet` says why
// and is what answers this import.
import "virtual:crewlet-design-system.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App.tsx";
import { Router } from "./app/router.tsx";
import { ClientContext } from "./lib/store-hooks.ts";
import { bootTheme } from "./lib/prefs.ts";
import { forgetRetiredKeys } from "./lib/storage.ts";
import { LiveSocket, Store } from "./protocol/index.ts";

// OURS, AFTER ALL OF THEIRS. There is no alias layer between the two: every
// stylesheet here reads the design system's tokens under their own names, so
// the only thing ours adds is layout and the screens' own rules.
import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/components.css";
import "./styles/frame.css";
import "./styles/screens.css";

// Before the first paint, so a reader whose machine is set to light never sees
// a dark flash on the way to their own preference.
bootTheme();

// A key no module reads any more leaves the reader's profile here, once per
// boot — see `lib/storage.ts`.
forgetRetiredKeys(() => localStorage);

const store = new Store();
const socket = new LiveSocket(store);
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
