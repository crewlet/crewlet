/**
 * Settings › Models & Fleets after its own writes, on the screen the frame mounts.
 *
 * The invariant: after a change the page draws the engine's answer, never the
 * one from before the change. A config write answers once its revision is
 * active, before this node applies it, so the read the write fires can still
 * hold the old list: the page reads again when this node applies the
 * revision. A refused change leaves the list as the engine has it and says why.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ModelEntriesAnswer } from "~/protocol/models.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { engineAnswer, mountSettings, stubEngine } from "~/test/settingsScreen.tsx";
import { DEFAULT_FLEET_EMPTY, modelEntry } from "./fixtures.ts";
import { Models } from "./Models.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const LLM = {
  default: { harness: "claude-code", model: "opus", auth: { mode: "subscription" } },
  spare: { harness: "claude-code", model: "haiku", auth: { mode: "subscription" } },
};
const DOC = { name: "Acme", providers: { llm: LLM } };

const answer = (...keys: string[]): ModelEntriesAnswer =>
  ({
    reading: "admin",
    node: "node-0",
    nodes: [{ id: "node-0", reported: true }],
    alarms: [],
    executor_harnesses: ["claude-code", "opencode"],
    entries: keys.map((key) => modelEntry({ key, model: key === "spare" ? "haiku" : "opus" })),
  }) as unknown as ModelEntriesAnswer;

/** The engine-made default fleet alone, before any member joins. */
const FLEETS = { reading: "admin", fleets: [DEFAULT_FLEET_EMPTY], assignments: [] };

/** The model names the list draws, in its order. */
const listed = () =>
  within(screen.getByRole("table"))
    .getAllByRole("row")
    .map((r) => /^(default|spare)/.exec(r.textContent ?? "")?.[1] ?? "")
    .filter(Boolean);

/** Mounts the list with `spare`'s Remove open, the engine answering the dialog's reads with `remove`. */
async function removeSpare(remove: () => Response) {
  let served = answer("default", "spare");
  stubEngine((call) => {
    if (call.method === "GET" && call.path.endsWith("/references"))
      return engineAnswer({ references: [] }, 200);
    if (call.method === "GET") return engineAnswer(LLM.spare, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true"))
      return engineAnswer(
        { valid: true, base_revision_id: "r1", warnings: [], derived: null },
        200,
      );
    return remove();
  });
  const mounted = mountSettings(
    <Models />,
    {
      llm_fleets: FLEETS,
      llm_entries: () => served,
      config: DOC,
      llm_models: { aliases: [], models: [] },
    },
    { hash: "#/settings/models?flow=remove&entry=spare" },
  );
  await act(afterMicrotasks);
  await act(afterMicrotasks);
  expect(listed()).toEqual(["default", "spare"]);
  const dialog = screen.getByRole("dialog");
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: /^Remove/ }));
  });
  await act(afterMicrotasks);
  return {
    ...mounted,
    serve: (next: ModelEntriesAnswer) => {
      served = next;
    },
  };
}

test("a model removed leaves the list once this node applies the removal, with no reload", async () => {
  const { store, serve } = await removeSpare(() =>
    engineAnswer({ revision_id: "r2", epoch: 5, warnings: [], derived: null }, 200),
  );
  // THE READ THE REMOVAL FIRED came before this node applied it.
  expect(listed()).toEqual(["default", "spare"]);
  serve(answer("default"));
  await act(async () => {
    store.applyHealth(healthFrame({ applied_epoch: 5 }));
  });
  await act(afterMicrotasks);
  expect(listed()).toEqual(["default"]);
});

test("a removal the engine refuses leaves the model listed and says why", async () => {
  await removeSpare(() =>
    engineAnswer({ error: "conflict", detail: "the configuration changed since it was read" }, 412),
  );
  expect(screen.getByText("Not removed")).toBeTruthy();
  expect(listed()).toEqual(["default", "spare"]);
});

// AN ADD OR AN EDIT, BY THIS TAB'S WIZARD OR ANYONE ELSE, is a revision: the
// list is drawn as the engine answers once this node applies it.
const edited = (): ModelEntriesAnswer => {
  const a = answer("default", "spare");
  a.entries[1] = modelEntry({ key: "spare", model: "claude-sonnet-5-5" });
  return a;
};
const rows = () =>
  within(screen.getByRole("table"))
    .getAllByRole("row")
    .map((r) =>
      /^(default|spare|fresh)(\S*)/
        .exec(r.textContent ?? "")
        ?.slice(1)
        .join(" "),
    )
    .filter(Boolean);

test.each([
  [
    "added",
    () => answer("default", "spare", "fresh"),
    ["default opus", "fresh opus", "spare haiku"],
  ],
  ["edited", edited, ["default opus", "spare claude-sonnet-5-5"]],
] as const)(
  "a model %s is drawn as the engine answers once this node applies it",
  async (_, next, want) => {
    let served = answer("default", "spare");
    const { store } = mountSettings(
      <Models />,
      { llm_fleets: FLEETS, llm_entries: () => served, config: DOC },
      { hash: "#/settings/models" },
    );
    await act(afterMicrotasks);
    expect(rows()).toEqual(["default opus", "spare haiku"]);
    served = next();
    await act(async () => {
      store.applyHealth(healthFrame({ applied_epoch: 5 }));
    });
    await act(afterMicrotasks);
    expect(rows()).toEqual([...want]);
  },
);

// A HELPER'S MODEL CHOSEN HERE REACHES THE ASK BUTTON with no reload: the
// button draws the viewer's answer, read from the revision this node runs.
test("the viewer, which says whether Ask Crewlet has a model, is asked again once this node applies a revision", async () => {
  const { store, asked } = mountSettings(
    <Models />,
    { llm_fleets: FLEETS, llm_entries: () => answer("default", "spare"), config: DOC },
    { hash: "#/settings/models" },
  );
  await act(afterMicrotasks);
  const viewerReads = () => asked.filter((what) => what === "viewer").length;
  const before = viewerReads();
  expect(before).toBeGreaterThan(0);
  await act(async () => {
    store.applyHealth(healthFrame({ applied_epoch: 5 }));
  });
  await act(afterMicrotasks);
  expect(viewerReads()).toBe(before + 1);
});
