/**
 * New project: who can own one, the key it takes, and the hand-off to Edit
 * org, which is where the project is made.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, clearToken, storeToken } from "~/protocol/index.ts";
import type { CompanyDocument } from "~/protocol/index.ts";
import {
  NewProjectDialog,
  cleanKey,
  keyVerdict,
  ownersOf,
  suggestedKey,
  type ProjectOwner,
} from "./NewProject.tsx";

const doc: CompanyDocument = {
  name: "FairSplit",
  roles: [
    { name: "Product Manager", handle: "pm" },
    { name: "Founder", handle: "miles", project: "FAIR" },
    { name: "Software Engineer", unit: "Engineering" },
  ],
  units: [
    {
      name: "Engineering",
      project: "ENG",
      children: [{ name: "Design", type: "team" }],
    },
    { name: "User research", type: "squad" },
  ],
} as CompanyDocument;

describe("who can own a project", () => {
  test("teams without one, then seats above every team without one", () => {
    expect(ownersOf(doc).map((o) => [o.name, o.kind, o.query])).toEqual([
      ["Design", "team", { unit: "Design" }],
      ["User research", "team", { unit: "User research" }],
      ["Product Manager", "seat", { seat: "pm" }],
    ]);
    expect(ownersOf(null)).toEqual([]);
  });
});

describe("the key", () => {
  test("is held to capitals, digits and underscores, twelve at most", () => {
    expect(cleanKey("de-s 1_x")).toBe("DES1_X");
    expect(cleanKey("abcdefghijklmnop")).toBe("ABCDEFGHIJKL");
  });

  test("is suggested from the owner's name", () => {
    expect(suggestedKey("Design")).toBe("DES");
    expect(suggestedKey("User research")).toBe("USE");
    expect(suggestedKey("")).toBe("PRJ");
  });

  test("says what is wrong with it, or what its tasks will be called", () => {
    const owner: ProjectOwner = { id: "unit:Design", name: "Design", kind: "team", query: {} };
    const free = { inDocument: false, inTracker: false };
    expect(keyVerdict("", owner, free)).toEqual({
      ok: false,
      message: "Choose a key, such as DES.",
    });
    expect(keyVerdict("1DES", owner, free)).toEqual({
      ok: false,
      message: "Use capital letters, digits and underscores, starting with a letter.",
    });
    expect(keyVerdict("ENG", owner, { inDocument: true, inTracker: false }).message).toBe(
      "ENG is already a project.",
    );
    expect(keyVerdict("OLD", owner, { inDocument: false, inTracker: true }).message).toBe(
      "OLD is already a project.",
    );
    expect(keyVerdict("DES", owner, { inDocument: false, inTracker: null })).toEqual({
      ok: false,
      message: "Checking DES…",
    });
    expect(keyVerdict("DES", owner, free)).toEqual({
      ok: true,
      message: "Tasks will be numbered DES-1, DES-2 and on.",
    });
  });
});

describe("the dialog", () => {
  beforeEach(() => storeToken("operator-token"));
  afterEach(() => {
    cleanup();
    clearToken();
    location.hash = "#/";
  });

  function mount(taken: string[] = []) {
    const store = new Store();
    const socket = new LiveSocket(store);
    (
      socket as unknown as { query: (what: string, p?: { key?: string }) => Promise<unknown> }
    ).query = (what, params) => {
      if (what === "config") return Promise.resolve(doc);
      if (what === "work_project") {
        return taken.includes(params?.key ?? "")
          ? Promise.resolve({ key: params?.key, name: "Taken" })
          : Promise.reject(new Error("not_found"));
      }
      return new Promise(() => {});
    };
    render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <NewProjectDialog onClose={() => {}} />
        </Router>
      </ClientContext.Provider>,
    );
  }

  test("an owner and a free key continue in Edit org, holding the key for that owner", async () => {
    mount();
    await screen.findByRole("dialog", { name: "New project" });
    await screen.findByText("Choose a key, such as DES.", {
      selector: ".crewlet-form-field__helper",
    });
    const field = screen.getByRole("textbox", { name: "Project key" });
    fireEvent.change(field, { target: { value: "des" } });
    await screen.findByText("Tasks will be numbered DES-1, DES-2 and on.", {
      selector: ".crewlet-form-field__helper",
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Continue in Edit org" }));
    });
    await waitFor(() => expect(location.hash).toBe("#/agents/edit?unit=Design&project=DES"));
  });

  test("a key a project already has holds Continue with the reason", async () => {
    mount(["OLD"]);
    await screen.findByRole("dialog", { name: "New project" });
    fireEvent.change(screen.getByRole("textbox", { name: "Project key" }), {
      target: { value: "old" },
    });
    await screen.findByText("OLD is already a project.", {
      selector: ".crewlet-form-field__error",
    });
    expect(
      screen.getByRole("button", { name: "Continue in Edit org" }).getAttribute("aria-disabled"),
    ).toBe("true");
    // AND ONE THE DOCUMENT ALREADY NAMES, before the tracker has made it.
    fireEvent.change(screen.getByRole("textbox", { name: "Project key" }), {
      target: { value: "eng" },
    });
    await screen.findByText("ENG is already a project.", {
      selector: ".crewlet-form-field__error",
    });
  });
});
