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
import { bootTheme } from "./lib/theme.ts";
import { LiveSocket, Store, apiToken } from "./protocol/index.ts";

import "./styles/tokens.css";
import "./styles/fonts.css";
import "./styles/base.css";
import "./styles/components.css";
import "./styles/shell.css";
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
