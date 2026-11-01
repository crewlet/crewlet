/**
 * New project: who can own one, the key it takes, who may make one, and the
 * hand-off to Edit org, which is where the project is made.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
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
    { name: "Software Engineer", handle: "swe", unit: "engineering" },
    { name: "Ada Founder", handle: "ada", kind: "human" },
  ],
  units: [
    {
      name: "Engineering",
      id: "engineering",
      project: "ENG",
      children: [{ name: "Design", id: "design", type: "team" }],
    },
    { name: "User research", id: "research", type: "squad" },
  ],
} as CompanyDocument;

describe("who can own a project", () => {
  // BY IDENTITY: Edit org finds a team by its key and a seat by its handle,
  // and two teams may share a name. A PERSON OWNS NO PROJECT: the engine
  // refuses a human seat one, so Ada's seat is not offered.
  test("teams without one, then agent seats above every team without one, each by its identity", () => {
    expect(ownersOf(doc).map((o) => [o.id, o.name, o.kind, o.query])).toEqual([
      ["unit:design", "Design", "team", { unit: "design" }],
      ["unit:research", "User research", "team", { unit: "research" }],
      ["seat:pm", "Product Manager", "seat", { seat: "pm" }],
    ]);
    expect(ownersOf(null)).toEqual([]);
  });

  test("two teams sharing a name are two owners", () => {
    const twins = {
      name: "X",
      units: [
        { name: "Ops", id: "ops-east" },
        { name: "Ops", id: "ops-west" },
      ],
    } as CompanyDocument;
    expect(ownersOf(twins).map((o) => o.query)).toEqual([
      { unit: "ops-east" },
      { unit: "ops-west" },
    ]);
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
    const owner: ProjectOwner = { id: "unit:design", name: "Design", kind: "team", query: {} };
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
  afterEach(() => {
    cleanup();
    location.hash = "#/";
  });

  /** The reader: an administrator holding Admin, unless a case says otherwise. */
  const ADMIN = {
    login: "ada.admin",
    grants: ["admin"],
    handle: "pm",
    owner: "pm",
    name: "Ada",
    kind: "human",
    acts: [],
    config_writer: true,
    config_managed_by: [],
  };

  function mount(taken: string[] = [], viewer: Record<string, unknown> = ADMIN) {
    const store = new Store();
    store.setConnected(true);
    const socket = new LiveSocket(store);
    const asked: string[] = [];
    (
      socket as unknown as { query: (what: string, p?: { key?: string }) => Promise<unknown> }
    ).query = (what, params) => {
      asked.push(what);
      if (what === "viewer") return Promise.resolve(viewer);
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
        <ViewerProvider>
          <Router>
            <NewProjectDialog onClose={() => {}} />
          </Router>
        </ViewerProvider>
      </ClientContext.Provider>,
    );
    return asked;
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
    await waitFor(() => expect(location.hash).toBe("#/company/org/edit?unit=design&project=DES"));
  });

  // A PROJECT KEY IS THE COMPANY GRANT'S: a lead's write that sets one is
  // refused, so a reader without Admin (a lead on their own team
  // included) is told the gate's sentence, and nothing is read or offered.
  test("a reader without Admin is held with the gate's sentence, and the document is not read", async () => {
    const asked = mount([], { ...ADMIN, login: "lee.lead", grants: ["worklet"] });
    const dialog = await screen.findByRole("dialog", { name: "New project" });
    await waitFor(() => expect(dialog.textContent).toContain(CONFIG_WRITE_REASONS.no_grant));
    expect(screen.queryByRole("textbox", { name: "Project key" })).toBeNull();
    expect(
      screen.getByRole("button", { name: "Continue in Edit org" }).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(asked).not.toContain("config");
  });

  test("a key a project already has holds Continue with the reason", async () => {
    mount(["OLD"]);
    await screen.findByRole("dialog", { name: "New project" });
    // THE FIELD ONCE IT IS DRAWN, which is a commit after the dialog's.
    fireEvent.change(await screen.findByRole("textbox", { name: "Project key" }), {
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
