/**
 * Boot.
 *
 * The socket is created BEFORE React mounts and lives for the life of the tab:
 * it is the application's one connection, it re-hydrates the whole projection
 * on every reconnect, and a transport owned by a component tree is a transport
 * that reconnects whenever somebody refactors a provider.
 */

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App.tsx";
import { Router } from "./app/router.tsx";
import { ClientContext } from "./lib/store-hooks.ts";
import { LiveSocket, Store, apiToken } from "./protocol/index.ts";
import { applyStoredPreferences } from "@crewlethq/ui";

// The design system, in cascade order: the variables, then the themes that
// repaint them, then density, the faces and the document baseline. themes.css
// and tokens.css share the :root selector and neither adds specificity, so
// the later import is the one that paints.
import "@crewlethq/tokens/css";
import "@crewlethq/tokens/css/themes";
import "@crewlethq/tokens/css/density";
import "@crewlethq/tokens/css/fonts";
import "@crewlethq/tokens/css/base";

import "./styles/base.css";
import "./styles/components.css";
import "./styles/shell.css";
import "./styles/screens.css";

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
