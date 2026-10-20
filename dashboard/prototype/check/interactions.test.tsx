/**
 * A person's writes, made through the SHIPPED controls, land in the demo
 * engine and come back to the screen that made them.
 *
 * `engine.test.ts` holds what the engine does with a request; this holds that
 * the request the dashboard actually sends for a press is one the engine
 * models: the seat header's Resume, an option pressed on a decision in the
 * inbox, a connect form submitted from the integrations room, and a seat added
 * in the org builder and saved.
 */

import { fireEvent, screen, within } from "@testing-library/react";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { beforeAll, describe, expect, test, vi } from "vitest";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { fairsplit } from "../fixtures/fairsplit.ts";
import { makeDemoClient, type DemoClient } from "../harness/demoClient.ts";
import { installRestShim } from "../harness/restShim.ts";

let client: DemoClient;
const said: string[] = [];

async function settle(): Promise<void> {
  let last = "";
  let steady = 0;
  for (let waited = 0; waited < 15_000 && steady < 4; waited += 120) {
    await new Promise((r) => setTimeout(r, 120));
    const now = document.body.innerHTML;
    steady = now === last ? steady + 1 : 0;
    last = now;
  }
}

async function go(route: string): Promise<void> {
  location.hash = route;
  await settle();
}

describe("writes made through the shipped controls", () => {
  beforeAll(async () => {
    vi.spyOn(console, "info").mockImplementation(() => {});
    vi.spyOn(console, "warn").mockImplementation((...args) => void said.push(args.join(" ")));
    vi.spyOn(console, "error").mockImplementation((...args) => void said.push(args.join(" ")));
    client = makeDemoClient(fairsplit());
    installRestShim(client.engine);
    client.socket.start();
    document.body.innerHTML = '<div id="root"></div>';
    history.replaceState(null, "", "#/home");
    createRoot(document.getElementById("root")!).render(
      <StrictMode>
        <ClientContext.Provider value={{ store: client.store, socket: client.socket }}>
          <Router>
            <App />
          </Router>
        </ClientContext.Provider>
      </StrictMode>,
    );
    await settle();
  });

  test("Resume on a seat's header resumes it", async () => {
    await go("#/agents/seats/pm");
    expect(document.body.textContent).toMatch(/Paused by Miles/);
    fireEvent.click(screen.getByRole("button", { name: /^Resume$/ }));
    await settle();
    expect(client.engine.company.seats.pm?.overlay).toMatchObject({
      activity: "idle",
      paused: null,
    });
    expect(client.store.state.agents.find((a) => a.handle === "pm")?.activity).toBe("idle");
    expect(document.body.textContent).not.toMatch(/Paused by Miles/);
  });

  test("an option pressed on a decision answers it", async () => {
    await go("#/inbox");
    expect(client.engine.answer("decisions", {}).total).toBe(2);
    fireEvent.click(screen.getByRole("button", { name: /One JSON file per group/ }));
    await settle();
    expect(client.engine.answer("decisions", {}).total).toBe(1);
    const brief = client.engine.company.work.items.find((i) => i.task.key === "FAIR-1");
    expect(brief?.comments.at(-1)).toMatchObject({ author: "miles", choice: "json" });
  });

  test("a connect form submitted from the room connects the tool", async () => {
    await go("#/settings/integrations");
    const card = screen.getByRole("heading", { name: "Mattermost" }).closest("article")!;
    fireEvent.click(within(card as HTMLElement).getByRole("button", { name: /Connect/ }));
    await settle();
    const dialog = screen.getByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText(/Mattermost instance/), {
      target: { value: "https://chat.fairsplit.example" },
    });
    fireEvent.change(within(dialog).getByLabelText(/^Team/), { target: { value: "fairsplit" } });
    fireEvent.change(within(dialog).getByLabelText(/Administrator token/), {
      target: { value: "demo-admin-token" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /^Connect$/ }));
    await settle();
    const room = client.engine.answer("integrations", {});
    expect(room.tools?.find((t) => t.key === "mattermost")?.state).toBe("connected");
    expect(document.body.textContent).toMatch(/Connected\s*1/);
  });

  test("a seat added in the org builder and saved is a new revision the chart draws", async () => {
    await go("#/agents/edit");
    const before = client.engine.company.config.epoch;
    fireEvent.click(screen.getByRole("button", { name: /^Add$/ }));
    await settle();
    fireEvent.click(screen.getByRole("menuitem", { name: /Add agent seat/ }));
    await settle();
    const form = document.querySelector("form")!;
    fireEvent.change(form.querySelector("input")!, { target: { value: "Crewlet-QA" } });
    const submit = [...form.querySelectorAll("button")].find((b) => b.type === "submit")!;
    fireEvent.click(submit);
    await settle();
    fireEvent.click(screen.getByRole("button", { name: /Review and save/ }));
    await settle();
    const review = screen.getByRole("dialog");
    expect(review.textContent).toMatch(/Adds the seat Crewlet-QA/);
    fireEvent.click(within(review).getByRole("button", { name: /^Save$/ }));
    await settle();
    expect(client.engine.company.config.epoch).toBe(before + 1);
    expect(client.engine.agentRows().map((a) => a.handle)).toContain("crewlet-qa");
    expect(client.store.state.org?.derived?.seats?.some((s) => s.handle === "crewlet-qa")).toBe(
      true,
    );
    expect(document.body.textContent).toMatch(/Saved revision .*Applied/);
  });

  test("nothing was refused or warned about along the way", () => {
    expect(said).toEqual([]);
  });
});
