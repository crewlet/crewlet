/**
 * The company's task types: each change sends the company's own declarations
 * whole with the one change applied, never the effective set, and every
 * control is drawn for every reader and held for anybody but an admin.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { mountSettings } from "~/test/settingsScreen.tsx";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import type { WorkTypeDef } from "~/protocol/index.ts";
import { TaskTypesDialog, typesAfter } from "./TaskTypes.tsx";

const TASK: WorkTypeDef = { slug: "task", name: "Task", plural: "Tasks", builtin: true };
const BUG: WorkTypeDef = { slug: "bug", name: "Defect", builtin: true };
const INCIDENT: WorkTypeDef = { slug: "incident", name: "Incident" };
const RETIRED: WorkTypeDef = { slug: "retired", name: "Retired", archived: true };

/** The catalogue as the engine answers it: the effective set, and the company's own. */
const CATALOGUE = {
  types: [TASK, BUG, INCIDENT, RETIRED],
  declared_types: [
    { slug: "bug", name: "Defect" },
    { slug: "incident", name: "Incident" },
    { slug: "retired", name: "Retired", archived: true },
  ],
  fields: [],
  policy_version: 1,
  types_version: 3,
  fields_version: 1,
  complete: true,
};

let puts: { path: string; body: unknown }[];

beforeEach(() => {
  puts = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      puts.push({
        path: new URL(url, "http://engine.test").pathname,
        body: JSON.parse(String(init.body)),
      });
      return new Response(JSON.stringify({ types: { outcome: "applied" } }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

async function mount(viewer: Record<string, unknown> = ADMIN_VIEWER) {
  mountSettings(<TaskTypesDialog onClose={() => {}} />, { work_catalogue: CATALOGUE }, { viewer });
  await act(afterMicrotasks);
}

test("a change sends the company's declarations whole, never the effective set", () => {
  const declared = CATALOGUE.declared_types;
  const effective = CATALOGUE.types;
  // A BUILTIN NEVER REWORDED is declared from its own words, without the mark.
  expect(typesAfter(declared, effective, { kind: "rename", slug: "task", name: "Ticket" })).toEqual(
    [...declared, { slug: "task", name: "Ticket", plural: "Tasks" }],
  );
  // A DECLARED ONE changes in place.
  expect(typesAfter(declared, effective, { kind: "archive", slug: "incident" })).toEqual([
    declared[0],
    { slug: "incident", name: "Incident", archived: true },
    declared[2],
  ]);
  expect(typesAfter(declared, effective, { kind: "restore", slug: "retired" })).toEqual([
    declared[0],
    declared[1],
    { slug: "retired", name: "Retired" },
  ]);
  expect(
    typesAfter(declared, effective, {
      kind: "add",
      slug: "spike_test",
      name: " Spike test ",
      description: "",
    }),
  ).toEqual([...declared, { slug: "spike_test", name: "Spike test" }]);
});

test("an admin renames a builtin, and the write carries the declarations with it", async () => {
  await mount();
  fireEvent.click(await screen.findByRole("button", { name: "Rename the type Task" }));
  const field = screen.getByLabelText("Name");
  fireEvent.change(field, { target: { value: "Ticket" } });
  fireEvent.click(screen.getByRole("button", { name: "Rename" }));
  await waitFor(() => expect(puts).toHaveLength(1));
  expect(puts[0]).toEqual({
    path: "/worklet/catalogue",
    body: {
      types: [...CATALOGUE.declared_types, { slug: "task", name: "Ticket", plural: "Tasks" }],
    },
  });
});

test("archiving asks first, and an archived type is restored from its own list", async () => {
  await mount();
  fireEvent.click(await screen.findByRole("button", { name: "Archive the type Incident" }));
  expect(puts).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "Archive type" }));
  await waitFor(() => expect(puts).toHaveLength(1));
  const archived = within(screen.getByRole("list", { name: "Archived task types" }));
  fireEvent.click(archived.getByRole("button", { name: "Restore the type Retired" }));
  await waitFor(() => expect(puts).toHaveLength(2));
  expect((puts[1]!.body as { types: WorkTypeDef[] }).types.at(-1)).toEqual({
    slug: "retired",
    name: "Retired",
  });
});

test("a name another type already files tasks under is held before anything is sent", async () => {
  await mount();
  fireEvent.click(await screen.findByRole("button", { name: "Add type" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Incident" } });
  const add = within(screen.getByRole("form", { name: "Add a type" })).getByRole("button", {
    name: "Add type",
  });
  expect(add.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(add);
  expect(puts).toHaveLength(0);
});

test("a reader who is not an admin sees every control, held with why", async () => {
  await mount({ ...ADMIN_VIEWER, grants: ["worklet"], admin: false });
  const rename = await screen.findByRole("button", { name: "Rename the type Task" });
  expect(rename.getAttribute("aria-disabled")).toBe("true");
  const reason = (rename.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(reason).toMatch(/Changing the company’s task types needs Admin access/);
  fireEvent.click(rename);
  expect(screen.queryByLabelText("Name")).toBeNull();
});
