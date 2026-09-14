/**
 * The Builder lens decides its posture from what the engine answers, checks
 * every draft with a dry run of exactly the write a save would send, and
 * keeps the operator's work through a change of token.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { clearToken, storeToken, type OrgProjection } from "~/protocol/index.ts";
import { company, Engine, json, mountBuilder, refusal } from "./testkit.tsx";

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  location.hash = "#/";
});

const named: OrgProjection = { name: "Acme", roles: [], units: [] };

/** The Builder's one polite live region. */
const liveRegion = () => document.querySelector(".org-builder-live")!;

describe("the posture table", () => {
  test("a served configuration opens edit mode", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    expect(await screen.findByText("CEO")).toBeDefined();
    // The first check runs at once, on the draft with no operations, as the
    // write a save would send: a PATCH conditional on the revision it read.
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const check = engine.checks()[0]!;
    expect(check.method).toBe("PATCH");
    expect(check.headers["If-Match"]).toBe('"r1"');
    expect(check.headers["Content-Type"]).toBe("application/merge-patch+json");
    expect(check.body).toEqual({});
    expect(await screen.findByText("No problems")).toBeDefined();
  });

  test("no active revision and no company in the org opens create mode", async () => {
    const engine = new Engine(null);
    mountBuilder({ engine });
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const check = engine.checks()[0]!;
    // Create-only, fleet-wide: a company that appeared meanwhile is a 412.
    expect(check.method).toBe("PUT");
    expect(check.headers["If-None-Match"]).toBe("*");
  });

  test("no active revision while the org names a company is a node that has not caught up", async () => {
    const engine = new Engine(null);
    mountBuilder({ engine, org: named });
    expect(
      await screen.findByText("This node has not caught up with the fleet's configuration yet."),
    ).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    // Never create mode: nothing is checked, let alone written.
    await new Promise((r) => setTimeout(r, 50));
    expect(engine.checks()).toHaveLength(0);
  });

  test("create mode waits for the org snapshot before deciding", async () => {
    const engine = new Engine(null);
    const { store } = mountBuilder({ engine, connected: false });
    await new Promise((r) => setTimeout(r, 50));
    expect(engine.checks()).toHaveLength(0);
    act(() => store.applyOrg(named));
    expect(
      await screen.findByText("This node has not caught up with the fleet's configuration yet."),
    ).toBeDefined();
  });

  test("a refusal with no stored token asks for one", async () => {
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 401);
    mountBuilder({ engine });
    expect(
      await screen.findByText("Editing the organization needs an operator token."),
    ).toBeDefined();
    expect(screen.getByRole("button", { name: "Set token" })).toBeDefined();
  });

  test("a refusal of a stored token says the token was refused", async () => {
    localStorage.setItem("crewlet_api_token", "stale");
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 401);
    mountBuilder({ engine });
    expect(await screen.findByText("The engine refused this browser's token.")).toBeDefined();
  });

  test("a plain 404 is a process that does not serve the configuration", async () => {
    const engine = new Engine(company());
    engine.script = () => new Response("404 page not found", { status: 404 });
    mountBuilder({ engine });
    expect(
      await screen.findByText(
        "This process does not serve the configuration. Open the dashboard on a node running the engine.",
      ),
    ).toBeDefined();
  });

  test("a body that is not JSON is a process that does not serve the configuration", async () => {
    const engine = new Engine(company());
    engine.script = () => new Response("<html></html>", { status: 200 });
    mountBuilder({ engine });
    expect(
      await screen.findByText(
        "This process does not serve the configuration. Open the dashboard on a node running the engine.",
      ),
    ).toBeDefined();
  });

  test("an engine that never answers is unreachable", async () => {
    const engine = new Engine(company());
    engine.script = () => Promise.reject(new TypeError("Failed to fetch"));
    mountBuilder({ engine });
    expect(await screen.findByText("The engine could not be reached")).toBeDefined();
  });

  test("a first dry run refused for want of a coordination store makes the lens read-only", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true" ? json({ error: "no_control_plane" }, 503) : null;
    mountBuilder({ engine });
    expect(await screen.findByText("Read-only here")).toBeDefined();
    expect(
      screen.getByText(
        "This process cannot write the configuration because it has no coordination store.",
      ),
    ).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    // An edit is refused before it reaches the log, and says why.
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(liveRegion().textContent).toContain("Editing is paused"));
    expect(engine.checks()).toHaveLength(1);
  });
});

describe("checking the draft", () => {
  test("an edit is checked as the merge patch a save would send", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const patch = engine.checks()[1]!.body as { roles: { name: string; goal: string }[] };
    expect(Object.keys(patch)).toEqual(["roles"]);
    expect(patch.roles[0]).toMatchObject({ name: "CEO", goal: "Lead and more" });
  });

  test("problems the engine reports are placed on the seat they name", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true" && r.method === "PATCH" && "roles" in (r.body as object)
        ? refusal([
            {
              path: "roles[1].goal",
              segments: ["roles", 1, "goal"],
              kind: "invalid",
              message: "roles[1].goal: too long",
            },
          ])
        : null;
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    expect(await screen.findByText("1 problem")).toBeDefined();
    expect(screen.getByTestId("problems Designer").textContent).toBe("1");
    expect(screen.getByTestId("problems CEO").textContent).toBe("0");
  });

  test("the live region says what an edit did, and undo from the keyboard reverts it", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    const region = liveRegion;
    await waitFor(() => expect(region().textContent).toBe("Edited CEO: goal."));
    // Ctrl or Command with Z, from anywhere in the page that is not a text field.
    fireEvent.keyDown(document.body, { key: "z", code: "KeyZ", ctrlKey: true });
    await waitFor(() => expect(region().textContent).toBe("Edited CEO: goal."));
    await waitFor(() => expect(engine.checks().at(-1)!.body).toEqual({}));
  });

  test("undo is left to a text field that has focus", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const field = document.createElement("input");
    document.querySelector(".org-builder")!.appendChild(field);
    fireEvent.keyDown(field, { key: "z", code: "KeyZ", metaKey: true });
    await new Promise((r) => setTimeout(r, 400));
    expect(engine.checks()).toHaveLength(2);
    field.remove();
  });
});

// NO PROVIDER, NO AGENT. The dashboard writes none, so the lens says where
// one comes from rather than leaving a company nobody can run.
test("a company with no model provider is told so, and one with a provider is not", async () => {
  const without = company();
  delete without.providers;
  const engine = new Engine(without);
  mountBuilder({ engine });
  expect(await screen.findByText(/No model provider is configured/)).toBeDefined();
  cleanup();

  const engineWith = new Engine(company());
  mountBuilder({ engine: engineWith });
  await screen.findByText("No problems");
  expect(screen.queryByText(/No model provider is configured/)).toBeNull();
});

describe("a token change mid-edit", () => {
  test("keeps the draft, reads the configuration again and checks under the new token", async () => {
    storeToken("first");
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const reads = engine.sent("GET").length;

    act(() => {
      storeToken("second");
    });
    await waitFor(() => expect(engine.sent("GET").length).toBe(reads + 1));
    await waitFor(() => expect(engine.checks()).toHaveLength(3));
    const recheck = engine.checks()[2]!;
    expect(recheck.headers.Authorization).toBe("Bearer second");
    // The edit is still in the draft that was checked.
    expect(JSON.stringify(recheck.body)).toContain("Lead and more");
  });

  test("a dry run refused for the new token pauses editing while the configuration still reads", async () => {
    storeToken("first");
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));

    // Reads are served, writes and dry runs are refused: the token lacks the
    // right to write, which only the check can find out.
    engine.script = (r) =>
      r.query.get("dry_run") === "true" && r.headers.Authorization === "Bearer reader"
        ? json({ error: "forbidden" }, 403)
        : null;
    act(() => {
      storeToken("reader");
    });
    expect(await screen.findByText("The engine refused the token")).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
  });

  test("a token the engine refuses pauses editing without discarding the draft", async () => {
    storeToken("first");
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));

    engine.script = (r) => (r.headers.Authorization === "Bearer first" ? null : json({}, 401));
    act(() => {
      clearToken();
    });
    expect(
      await screen.findByText(
        "Editing the organization needs an operator token. Your draft is kept on this page.",
      ),
    ).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    // The seats are still drawn from the draft, not replaced by a refusal.
    expect(within(screen.getByRole("list", { name: "Seats" })).getByText("CEO")).toBeDefined();
  });
});
