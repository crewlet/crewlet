/**
 * A PHONE'S BAR KEEPS ITS PRIMARY ACTION WHOLE. On Home the working-now pill
 * (faces and a count) sat beside the screen's own actions, which shrink and
 * scroll on a phone while the pill does not: at 390px "New task" was cut to
 * "New tas" behind the scroller's fade. The pill folds away on a phone, where
 * the greeting and the figures already say who is working. Laid out in a real
 * browser, at a phone's width.
 */
import "~/test/layout.tsx";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { AppShell, Button, LayerHost, ToastProvider } from "@crewlethq/ui";
import { PageHeader } from "./PageHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { servicesRun } from "~/lib/services.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { eventually } from "~/test/settle.ts";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

const AGENTS = ["cto", "pm", "swe", "qa", "sre", "devrel"];

function mount(width: number) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(
    withDerived({
      name: "Acme",
      roles: AGENTS.map((handle) => ({ name: handle.toUpperCase(), handle, kind: "agent" })),
      units: [],
    }) as never,
  );
  store.applySeats(
    AGENTS.map((handle) => ({
      id: handle,
      agent_id: handle,
      role: handle.toUpperCase(),
      handle,
      activity: "working",
    })) as never,
  );
  const socket = new LiveSocket(store);
  socket.query = (() => Promise.resolve({})) as typeof socket.query;
  host = document.createElement("div");
  host.style.width = `${width}px`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <ToastProvider>
        <LayerHost>
          <ClientContext.Provider value={{ store, socket }}>
            <FrameReadings>
              <Router>
                <AppShell
                  topbar={
                    <PageHeader
                      crumbs={[{ label: "Home" }]}
                      row={undefined}
                      counts={{}}
                      title="Home"
                      workspace="home"
                      working
                      runs={servicesRun(null)}
                      grants={[]}
                    />
                  }
                >
                  <PageActions>
                    <Button variant="primary">New task</Button>
                  </PageActions>
                </AppShell>
              </Router>
            </FrameReadings>
          </ClientContext.Provider>
        </LayerHost>
      </ToastProvider>,
    ),
  );
}

test("on a phone the primary action stays whole, and the working faces fold away", async () => {
  mount(390);
  const button = await eventually(() => {
    const found = [...host!.querySelectorAll("button")].find((b) => b.textContent === "New task");
    if (!found) throw new Error("the action has not landed in the bar");
    return found;
  });
  const run = button.closest(".page-actions") as HTMLElement;
  // NOTHING OF THE RUN IS PAST ITS EDGE: the action is drawn whole.
  expect(run.scrollWidth).toBeLessThanOrEqual(run.clientWidth);
  expect(button.getBoundingClientRect().right).toBeLessThanOrEqual(
    run.getBoundingClientRect().right + 0.5,
  );
  const pill = host!.querySelector<HTMLElement>(".working-now");
  expect(pill === null || getComputedStyle(pill).display === "none").toBe(true);
});

test("on a wide bar the working faces are drawn", async () => {
  mount(1200);
  const pill = await eventually(() => {
    const found = host!.querySelector<HTMLElement>(".working-now");
    if (!found) throw new Error("the pill has not been drawn");
    return found;
  });
  expect(getComputedStyle(pill).display).not.toBe("none");
});
