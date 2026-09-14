/**
 * A newer revision found by a check is offered as an update of the draft,
 * never replayed over silently: what applies is replayed onto the revision
 * the node serves, and a value somebody else changed waits for a choice.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { company, Engine, json, mountBuilder } from "./testkit.tsx";

beforeEach(() => {
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  location.hash = "#/";
});

/** The company as a colleague saved it: `change` applied to the fixture. */
function upstream(change: (doc: CompanyDocument) => void): CompanyDocument {
  const doc = company();
  change(doc);
  return doc;
}

/** Opens the builder, lets a colleague save `next` as r2, and edits the CEO. */
async function editAfterUpstreamSave(next: CompanyDocument) {
  const engine = new Engine(company());
  mountBuilder({ engine });
  await screen.findByText("No problems");
  engine.document = next;
  engine.revision = "r2";
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  expect(await screen.findByText("The configuration changed")).toBeDefined();
  return engine;
}

test("an edit that still applies is replayed onto the newer revision", async () => {
  const engine = await editAfterUpstreamSave(
    upstream((doc) => {
      doc.roles![1]!.goal = "Design things";
    }),
  );
  // The banner links to what changed between the draft's base and now.
  expect(screen.getByRole("link", { name: "Show what changed" }).getAttribute("href")).toBe(
    "#/config?lens=diff&revision=r1",
  );
  fireEvent.click(screen.getByRole("button", { name: "Update my draft" }));
  const dialog = await screen.findByRole("dialog", { name: "Update my draft and review" });
  expect(within(dialog).getByText("All 1 change still apply.")).toBeDefined();
  fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));

  await waitFor(() => {
    const last = engine.checks().at(-1)!;
    expect(last.headers["If-Match"]).toBe('"r2"');
  });
  const patch = engine.checks().at(-1)!.body as { roles: { goal: string }[] };
  expect(patch.roles[0]!.goal).toBe("Lead and more");
  // The colleague's change is part of the base now, not overwritten.
  expect(patch.roles[1]!.goal).toBe("Design things");
  expect(await screen.findByText("No problems")).toBeDefined();
});

test("a value somebody else changed waits for a choice, and keeping theirs drops mine", async () => {
  const engine = await editAfterUpstreamSave(
    upstream((doc) => {
      doc.roles![0]!.goal = "Lead well";
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Update my draft" }));
  const dialog = await screen.findByRole("dialog", { name: "Update my draft and review" });
  const row = within(dialog).getByRole("row", { name: /goal/ });
  expect(within(row).getByText("Lead")).toBeDefined();
  expect(within(row).getByText("Lead well")).toBeDefined();
  expect(within(row).getByText("Lead and more")).toBeDefined();
  const confirm = within(dialog).getByRole("button", { name: "Update my draft" });
  expect((confirm as HTMLButtonElement).disabled).toBe(true);

  fireEvent.click(within(dialog).getByRole("radio", { name: "Keep theirs" }));
  expect((confirm as HTMLButtonElement).disabled).toBe(false);
  fireEvent.click(confirm);

  await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"'));
  expect(engine.checks().at(-1)!.body).toEqual({});
});

test("a node still serving the draft's base is not updated onto", async () => {
  const engine = await editAfterUpstreamSave(
    upstream((doc) => {
      doc.roles![1]!.goal = "Design things";
    }),
  );
  // This node answers reads with the old revision while the conflict named r2.
  engine.script = (r) =>
    r.method === "GET" && r.path === "/config" ? json(company(), 200, { ETag: '"r1"' }) : null;
  fireEvent.click(screen.getByRole("button", { name: "Update my draft" }));
  expect(
    await screen.findByText(
      "This node has not caught up with the newer revision yet. Try again in a moment.",
    ),
  ).toBeDefined();
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("a draft of a configuration that is no longer active offers to discard and reload", async () => {
  const engine = new Engine(company());
  mountBuilder({ engine });
  await screen.findByText("No problems");
  engine.script = (r) =>
    r.query.get("dry_run") === "true" ? json({ error: "no_active_revision" }, 412) : null;
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  expect(
    await screen.findByText(
      "The configuration this draft edits is no longer active on this engine, so the draft cannot be saved.",
    ),
  ).toBeDefined();
  engine.script = () => null;
  const reads = engine.sent("GET").length;
  fireEvent.click(screen.getByRole("button", { name: "Discard and reload" }));
  await waitFor(() => expect(engine.sent("GET").length).toBe(reads + 1));
  expect(await screen.findByText("No problems")).toBeDefined();
  expect(engine.checks().at(-1)!.body).toEqual({});
});
