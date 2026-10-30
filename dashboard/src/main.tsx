/**
 * Boot.
 *
 * The socket is created BEFORE React mounts and lives for the life of the tab:
 * it is the application's one connection, it re-hydrates the whole projection
 * on every reconnect, and a transport owned by a component tree is a transport
 * that reconnects whenever somebody refactors a provider. It is DIALLED by the
 * frame (`app/Shell.tsx`) — or by a sign-in — and not here: a page outside the
 * frame needs no session, and dialling from boot sent each of them a refused
 * handshake and two refused reads.
 */

// THE DESIGN SYSTEM, ABOVE EVERY MODULE IMPORT. An import statement is
// evaluated in source order, so a baseline imported below the components'
// sheets is emitted below all of them. Order decides a tie and
// there is one: `:focus-visible` in the baseline and `.crewlet-btn` in the
// component are both a single class, so a baseline that came last gave every
// focused control the baseline's radius and squared off every button the
// moment a reader tabbed to it.
//
// Cascade order within the set: the kit's variables, the default palettes
// and the named themes that repaint them, density, the faces, the document
// baseline, then the components.
import "@crewlethq/tokens/css";
import "@crewlethq/tokens/css/themes";
// EVERY NAMED THEME, in one sheet: a theme is switched by the root's
// `data-palette` with no request, so a preview is instant and nothing new
// reaches the content security policy. See `lib/prefs.ts`.
import "@crewlethq/tokens/css/themes/all";
import "@crewlethq/tokens/css/density";
import "@crewlethq/tokens/css/fonts";
import "@crewlethq/tokens/css/base";
// THE BRAND SCOPE'S MAPPING (`BrandScope`): inside a doc's document and a
// board, the company's adopted brand restyles the tokens, and nowhere else.
// Here, with the rest of the design system, because the kit's own import of
// it from its component is answered empty (`vite.config.ts`'s
// `designSystemSheet`), as every component sheet is.
import "@crewlethq/tokens/css/brand-scope";
// THE COMPONENTS' SHEETS, WHOLE AND HERE — not one per component as each
// module is reached, which a code-split build turns into a sheet per lazy
// chunk appended after ours. `vite.config.ts`'s `designSystemSheet` says why
// and is what answers this import.
import "virtual:crewlet-design-system.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { trackFocusModality } from "@crewlethq/ui";
import { App } from "./app/App.tsx";
import { Router } from "./app/router.tsx";
import { ClientContext } from "./lib/store-hooks.ts";
import { bootTheme } from "./lib/prefs.ts";
import { currentReader } from "./lib/reader.ts";
import { sessionMoved, takeSession } from "./lib/session.ts";
import { forgetRetiredKeys } from "./lib/storage.ts";
import { LiveSocket, SessionKeepAlive, setTabReader, Store } from "./protocol/index.ts";

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

// HOW THE READER LAST MOVED, kept for the life of the tab: a field rings at
// 2px for the keyboard and at its own hairline where a pointer put the caret.
// Every field starts this while it is mounted; started here as well, the first
// key a reader presses (Enter on the link that opens a page whose title is
// focused on load) is seen before any field has mounted.
trackFocusModality();

// A key no module reads any more leaves the reader's profile here, once per
// boot — see `lib/storage.ts`.
forgetRetiredKeys(() => localStorage);

// EVERY REQUEST NAMES WHO THIS TAB IS READ BY, and one the engine says was
// made for somebody else hands the tab to whoever holds the browser now —
// installed before anything can send one (`protocol/signin.ts`).
setTabReader({ current: currentReader, moved: () => void sessionMoved() });

const store = new Store();
// A RE-DIAL AFTER A SESSION ENDED IS FOR THIS TAB'S OWN READER ONLY: anybody
// else is handed the tab, with nothing of its last reader kept.
const socket = new LiveSocket(store, { takeSession });
// The session idles out after twelve hours with no request, and the socket is
// not one: a tab that only listens keeps it alive here (`protocol/keepalive.ts`).
new SessionKeepAlive(socket).start();

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
