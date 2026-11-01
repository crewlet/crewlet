/**
 * Company › Brand as each reader meets it: a reader reads the published brand
 * and is handed no control; an administrator edits one card at a time, a
 * colour that would be refused holds Save with the engine's reason, and a
 * newer version is offered for publishing; with no brand published, an
 * administrator is offered the setup and everyone else is told what that
 * means; a draft nobody published is drawn as a draft, says it is not live
 * and who last changed it, and offers the one next step that fits it: one
 * that lacks a primary logo or colour is finished, never published; and its
 * owner may discard it, which moves the kit to the trash.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { BrandPage } from "./BrandPage.tsx";
import { brandDraftLabel } from "~/lib/brand/assistantDraft.ts";
import { pageSnapshot, resetPageContextForTest, setFramePage } from "~/app/assistant/context.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import {
  LiveSocket,
  Store,
  type ArtifactListItem,
  type BrandAnswer,
  type BrandDocument,
} from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { brandKitRow } from "~/test/brand.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 404 })),
  );
  location.hash = "#/company/brand";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const ADOPTED: NonNullable<BrandAnswer["brand"]> = {
  kit: "01KIT",
  kit_version: 7,
  document: {
    v: 1,
    name: "FairSplit",
    tagline: "Split the trip, keep the friends",
    logos: { primary: "logos/primary.svg" },
    colors: { primary: "#0f7b5f", text: "#111418", light_ground: "#ffffff" },
    type: { heading: { kit: "geist" }, body: { stack: "system-sans" } },
    voice: {
      summary: "Plain, warm and exact.",
      write_like: ["Say what happened, then what to do."],
      avoid: ["Exclamation marks in errors."],
    },
  },
  css_sha: "c".repeat(64),
  files: [
    { slot: "css", content_type: "text/css", object: { hash: "c".repeat(64), size: 10 } },
    {
      slot: "logo.primary",
      content_type: "image/svg+xml",
      object: { hash: "a".repeat(64), size: 10 },
    },
  ],
  adopted_by: { name: "maya", kind: "human" },
  adopted_at: "2026-10-24T09:00:00Z",
};

/** A draft with nothing chosen but its name: what the first Continue publishes. */
const PARTIAL: BrandDocument = {
  v: 1,
  name: "FairSplit",
  logos: {},
  colors: {},
  type: { heading: { kit: "geist" }, body: { kit: "geist" } },
  voice: {},
};

/** A draft with what publishing needs: the primary logo and colour. */
const READY: BrandDocument = {
  ...PARTIAL,
  logos: { primary: "logos/primary.svg" },
  colors: { primary: "#0f7b5f" },
};

/**
 * Serve each kit version's `brand.json` from `versions` (`kit:n`), take a
 * kit to the trash (`DELETE /artifacts/{id}`), and 404 anything else. Every
 * write is recorded, as its method and path.
 */
function serveVersions(versions: Record<string, BrandDocument>) {
  const writes: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input instanceof Request ? input.url : input);
      const method = (input instanceof Request ? input.method : init?.method) ?? "GET";
      if (method !== "GET") {
        writes.push(`${method} ${new URL(url, location.href).pathname}`);
        return new Response("{}", { status: 200 });
      }
      const m = /\/artifacts\/([^/]+)\/versions\/(\d+)\/files\/brand\.json/.exec(url);
      const doc = m ? versions[`${decodeURIComponent(m[1]!)}:${m[2]}`] : undefined;
      return doc
        ? new Response(JSON.stringify(doc), { status: 200 })
        : new Response("{}", { status: 404 });
    }),
  );
  return writes;
}

/** Who published every version the suite reads, and when: the draft's last edit. */
const EDITED = { author: { name: "ana", kind: "human" }, created_at: "2026-10-25T10:00:00Z" };

/**
 * Mount the page for a reader holding `grants`, over the company's `brand`
 * answer and the brand kits the reader may see (`kits`).
 */
function mount(grants: string[], brand: BrandAnswer["brand"], kits: ArtifactListItem[] = []) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(withDerived({ name: "FairSplit", roles: [] }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer"
      ? Promise.resolve({
          login: "ana",
          owner: "ana",
          handle: "ana",
          kind: "human",
          grants,
          acts: [],
        })
      : what === "brand"
        ? Promise.resolve({ brand })
        : what === "artifacts"
          ? Promise.resolve({ contents: kits, common_prefixes: [], is_truncated: false })
          : what === "artifact_version"
            ? Promise.resolve({ manifest: { files: [] }, ...EDITED })
            : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <BrandPage />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

test("a reader reads the adopted brand card by card and is handed no control", async () => {
  mount([], ADOPTED);
  expect(await screen.findByRole("heading", { name: "Logos" })).toBeTruthy();
  for (const card of ["Colours", "Typography", "Voice and tone"]) {
    expect(screen.getByRole("heading", { name: card })).toBeTruthy();
  }
  const logosCard = screen.getByRole("region", { name: "Logos" });
  expect(within(logosCard).getAllByText("FairSplit").length).toBeGreaterThan(0);
  expect(screen.getByText("#0f7b5f")).toBeTruthy();
  expect(screen.getByText("Text on light ground")).toBeTruthy();
  expect(screen.getByRole("list", { name: "Write like this" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Edit / })).toBeNull();
  expect(screen.queryByRole("button", { name: /More brand actions/ })).toBeNull();
  // WHO PUBLISHED IT, AS A FACT OF ITS OWN beside the day (one value wrapped to
  // three lines in its track), and which version, linked to the kit.
  expect(screen.getByText("Published by")).toBeTruthy();
  expect(screen.getByText("maya")).toBeTruthy();
});

test("a reader with no brand published is told what that means, and offered nothing", async () => {
  mount([], null);
  expect(await screen.findByText("No brand yet")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Set up your brand" })).toBeNull();
  expect(screen.queryByRole("link", { name: brandDraftLabel(false) })).toBeNull();
});

test("an administrator with no brand published is offered the setup, on a page of its own", async () => {
  mount(["admin"], null);
  const setUp = await screen.findByRole("link", { name: "Set up your brand" });
  expect(setUp.getAttribute("href")).toBe("#/company/brand/setup");
  expect(screen.queryByRole("dialog")).toBeNull();
});

// A DRAFT IN PROGRESS IS DRAWN, NOT DENIED: the notice says it is not live,
// who last changed it and when, and what publishing still needs, and its one
// primary action finishes it; the brand's own cards draw it read-only with
// its preview tagged a draft, where "No brand yet" used to contradict the
// notice over it. Mutation: draw the empty state under a standing draft, or
// offer "Publish brand" for one that is not ready, and this fails.
test("a draft a setup left is drawn as a draft, says what it lacks, and is finished before it is published", async () => {
  serveVersions({ "01DRAFT:2": PARTIAL });
  mount(["admin"], null, [brandKitRow()]);
  const notice = await screen.findByText(/Not ready to publish: add a primary logo and colour\./);
  expect(notice.textContent).toMatch(
    /This draft of the FairSplit brand is not published yet, so artifacts still use Crewlet's look\./,
  );
  expect(notice.textContent).toMatch(/Last edited by ana on /);
  const go = screen.getByRole("link", { name: "Finish draft" });
  expect(go.getAttribute("href")).toBe("#/company/brand/setup");
  expect(screen.queryByRole("button", { name: "Publish brand" })).toBeNull();
  expect(screen.queryByRole("link", { name: "Edit draft" })).toBeNull();
  // THE DRAFT ITSELF, read-only, in place of "No brand yet" and its setup.
  expect(await screen.findByRole("heading", { name: "Logos" })).toBeTruthy();
  const preview = screen.getByRole("region", { name: "Preview" });
  expect(within(preview).getByText("Draft")).toBeTruthy();
  expect(within(preview).getByText(/drawn with this draft/)).toBeTruthy();
  expect(screen.queryByText("No brand yet")).toBeNull();
  expect(screen.queryByRole("link", { name: "Set up your brand" })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Edit / })).toBeNull();
});

// THE ASSISTANT IS TOLD OF A STANDING DRAFT, read-only: the saved kit and
// version the notice offers to continue, its values, no field it may set and
// no draft.set, and the help naming where it is changed. A reader the draft
// is not continued by is told of none. Mutation: declare no draft here while
// no card is in edit, or keep draft.set for a draft nothing on the page can
// change, and this fails.
test("the assistant reads a draft a setup left as saved and changed only in its setup", async () => {
  resetPageContextForTest();
  setFramePage({
    screen: "company-brand",
    title: "Company › Brand",
    path: ["company", "brand"],
    params: {},
    query: {},
  });
  serveVersions({ "01DRAFT:2": PARTIAL });
  mount(["admin"], null, [brandKitRow()]);
  expect(await screen.findByRole("link", { name: brandDraftLabel(false) })).toBeTruthy();
  await waitFor(() => expect(pageSnapshot()?.context.draft).toBeDefined());
  const context = pageSnapshot()!.context;
  expect(context.draft).toMatchObject({ kind: "server", id: "01DRAFT", revision: "2" });
  // THE HELP NAMES THE BUTTON THE PAGE SHOWS, by the label the button reads.
  const go = screen.getByRole("link", { name: brandDraftLabel(false) });
  expect(context.draft!.help).toContain(go.textContent!);
  expect(context.draft!.fields.find((f) => f.path === "name")).toMatchObject({
    value: "FairSplit",
  });
  expect(context.draft!.fields.some((f) => f.editable)).toBe(false);
  expect(context.accepts).not.toContain("draft.set");
  cleanup();
  resetPageContextForTest();
  mount([], null, [brandKitRow({ role: "viewer" })]);
  expect(await screen.findByText("No brand yet")).toBeTruthy();
  expect(pageSnapshot()?.context.draft).toBeUndefined();
});

test("a complete draft is published by an administrator, and an editor edits their own", async () => {
  serveVersions({ "01DRAFT:2": READY });
  mount(["admin"], null, [brandKitRow()]);
  // ONE PRIMARY ACTION, publishing, which reviews the version and confirms it
  // in the person's word; editing stays beside it.
  const publish = await screen.findByRole("button", { name: "Publish brand" });
  expect(screen.getByRole("link", { name: "Edit draft" })).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Finish draft" })).toBeNull();
  expect(screen.queryByText(/not ready to publish/i)).toBeNull();
  fireEvent.click(publish);
  const dialog = await screen.findByRole("dialog", {
    name: "Publish version 2 as the FairSplit brand?",
  });
  expect(within(dialog).getByRole("button", { name: "Publish version 2" })).toBeTruthy();
  expect(within(dialog).getByRole("heading", { name: "When you publish" })).toBeTruthy();
  cleanup();
  // AN EDITOR OF THE KIT, who may not publish: theirs to edit, an
  // administrator's to publish.
  mount([], null, [brandKitRow({ role: "editor" })]);
  expect(await screen.findByRole("link", { name: "Edit draft" })).toBeTruthy();
  expect(screen.getByText(/An administrator can review and publish it\./)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Publish brand" })).toBeNull();
  cleanup();
  // A VIEWER OF IT is not handed the draft at all.
  mount([], null, [brandKitRow({ role: "viewer" })]);
  expect(await screen.findByText("No brand yet")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Edit draft" })).toBeNull();
});

// DISCARDING IS THE TRASH, CONFIRMED: the kit's owner (or an administrator),
// a person, moves the draft kit to the trash through the artifact's own route
// and its own permission (`artifacts.trash`), which the engine decides. An
// editor who does not own it is offered no discard. Mutation: send the
// discard without the confirm, or offer it to an editor, and this fails.
test("the owner of a draft discards it after a confirm, and an editor is offered no discard", async () => {
  const writes = serveVersions({ "01DRAFT:2": PARTIAL });
  mount([], null, [brandKitRow({ role: "owner" })]);
  fireEvent.click(await screen.findByRole("button", { name: "Discard draft" }));
  const dialog = await screen.findByRole("alertdialog", { name: "Discard this draft?" });
  expect(dialog.textContent).toMatch(/moves the FairSplit brand draft to the trash in Artifacts/);
  expect(dialog.textContent).toMatch(/unless its owner restores it/);
  expect(writes).toEqual([]);
  fireEvent.click(within(dialog).getByRole("button", { name: "Discard draft" }));
  await waitFor(() => expect(writes).toEqual(["DELETE /artifacts/01DRAFT"]));
  cleanup();
  serveVersions({ "01DRAFT:2": PARTIAL });
  mount([], null, [brandKitRow({ role: "editor" })]);
  expect(await screen.findByRole("link", { name: "Finish draft" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Discard draft" })).toBeNull();
});

test("an administrator runs the setup again from the bar", async () => {
  mount(["admin"], ADOPTED);
  fireEvent.click(await screen.findByRole("button", { name: /More brand actions/ }));
  expect(await screen.findByRole("menuitem", { name: "Run setup again" })).toBeTruthy();
});

// ON A PHONE THE BAR KEEPS ONE "MORE": the brand's own menu is marked to fold
// (`page-action-folds`), its actions published to the bar's menu, so two
// identical ellipses never stand side by side.
test("an administrator's brand menu folds into the bar's More on a phone", async () => {
  mount(["admin"], ADOPTED);
  const more = await screen.findByRole("button", { name: /More brand actions/ });
  expect(more.closest(".page-action-folds")).not.toBeNull();
  expect(screen.getByRole("link", { name: "History" }).closest(".page-action-folds")).toBeNull();
});

test("an administrator edits one card at a time, and a colour that would be refused holds Save", async () => {
  mount(["admin"], ADOPTED);
  fireEvent.click(await screen.findByRole("button", { name: "Edit Colours" }));
  // ONE CARD AT A TIME: the others say why they wait.
  const logos = screen.getByRole("button", { name: "Edit Logos" });
  expect(logos.getAttribute("aria-disabled") ?? logos.getAttribute("disabled")).not.toBeNull();
  const text = screen.getByRole("textbox", { name: /^Text/ });
  fireEvent.change(text, { target: { value: "9aa3ad" } });
  await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/must reach 4\.5:1/));
  const save = screen.getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled") ?? save.getAttribute("disabled")).not.toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
});

// A NEWER VERSION OF THE PUBLISHED KIT is a draft too, said the same way,
// with no discard: it is a version of the published kit, not a kit of its own.
test("a newer version is offered to an administrator to publish, and named to a reader who sees it", async () => {
  const newer = { ...ADOPTED, newer_version: 9, newer_by: { name: "brand-agent", kind: "agent" } };
  serveVersions({ "01KIT:9": { ...READY, name: "FairSplit" } });
  mount(["admin"], newer);
  expect(
    await screen.findByText(
      /Version 9 of the FairSplit brand is a draft\. Artifacts keep using the published version 7 until this one is published\./,
    ),
  ).toBeTruthy();
  expect(screen.getByRole("button", { name: "Publish brand" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Edit draft" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Discard draft" })).toBeNull();
  cleanup();
  mount([], newer);
  const told = await screen.findByText(/An administrator can review and publish it\./);
  expect(told.textContent).toMatch(/Last edited by brand-agent\./);
  expect(screen.queryByRole("button", { name: "Publish brand" })).toBeNull();
  expect(screen.queryByRole("link", { name: /draft$/ })).toBeNull();
});
