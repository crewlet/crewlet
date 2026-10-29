/**
 * The brand setup as each person meets it: a fresh setup opens on Identity
 * and its first Continue makes the draft kit; a step holds Continue until its
 * part is done; a draft resumes where it was left; the pair that blocks an
 * adoption holds Colours until a suggestion fixes it; an administrator adopts
 * from Review once every warning is acknowledged, and an editor saves for
 * review and never adopts; Enter continues from a one-line field; a phone
 * reads the step's place and folds the preview.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { BrandSetup } from "./BrandSetup.tsx";
import {
  applyPageAction,
  pageActionPreview,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "~/app/assistant/context.ts";
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
import { DRAFT_KIT, brandKitRow } from "~/test/brand.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const { createObjectURL, revokeObjectURL } = URL;
const { matchMedia } = globalThis;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Object.assign(URL, { createObjectURL: () => "blob:test/logo", revokeObjectURL: () => {} });
  location.hash = "#/company/brand/setup";
});

afterEach(() => {
  cleanup();
  resetPageContextForTest();
  vi.unstubAllGlobals();
  Object.assign(URL, { createObjectURL, revokeObjectURL });
  Object.defineProperty(globalThis, "matchMedia", {
    configurable: true,
    writable: true,
    value: matchMedia,
  });
  location.hash = "#/";
});

const KIT = DRAFT_KIT;

/** The draft a setup left, with the name, the primary logo and the primary colour. */
function draft(over: Partial<BrandDocument> = {}): BrandDocument {
  return {
    v: 1,
    name: "FairSplit",
    logos: { primary: "logos/primary.svg" },
    colors: { primary: "#0f7b5f" },
    type: { heading: { kit: "geist" }, body: { kit: "geist" } },
    voice: {},
    ...over,
  };
}

/** A draft with every step done: Review is where it resumes. */
function whole(over: Partial<BrandDocument> = {}): BrandDocument {
  return draft({
    type: { heading: { stack: "system-serif" }, body: { kit: "geist" } },
    voice: { summary: "Plain, warm and exact." },
    ...over,
  });
}

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const intent = () =>
  json(
    {
      upload: "u-1",
      expires_at: "",
      transfers: [{ path: "brand.json", method: "PUT", url: "", direct: false }],
    },
    201,
  );

/**
 * An engine serving the draft's `brand.json` and logo at version 2, making a
 * new kit or a version of the draft's on a publish, and taking an adoption.
 * Every request is kept, by method and path.
 */
function engine(doc: BrandDocument | null) {
  const sent: { method: string; url: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input instanceof Request ? input.url : input);
      const method = init.method ?? "GET";
      sent.push({ method, url, body: typeof init.body === "string" ? init.body : "" });
      const at = `${method} ${url}`;
      if (doc && /^GET .*\/artifacts\/01DRAFT\/versions\/2\/files\/brand\.json$/.test(at)) {
        return json(doc);
      }
      if (/^GET .*\/files\/logos\/primary\.svg$/.test(at)) {
        return new Response("<svg xmlns='http://www.w3.org/2000/svg'/>", {
          headers: { "Content-Type": "image/svg+xml" },
        });
      }
      if (/^POST .*\/uploads$/.test(at)) return intent();
      if (/^POST .*\/artifacts$/.test(at)) return json({ artifact: "k-new", number: 1 }, 201);
      if (/^POST .*\/artifacts\/01DRAFT\/versions$/.test(at)) {
        return json({ artifact: KIT, number: 3 }, 201);
      }
      if (/^POST .*\/brand\/adoptions$/.test(at)) return json({}, 200);
      return new Response(null, { status: 204 });
    }),
  );
  return sent;
}

/**
 * Mount the setup for a reader holding `grants`, with no brand adopted and
 * `kits` the brand kits they see (the draft, at version 2, when there is one).
 */
function mount(
  grants: string[],
  kits: ArtifactListItem[] = [],
  brand: BrandAnswer["brand"] = null,
) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(withDerived({ name: "FairSplit", roles: [] }));
  const socket = new LiveSocket(store);
  const answers: Record<string, unknown> = {
    viewer: { login: "ana", owner: "ana", handle: "ana", kind: "human", grants, acts: [] },
    brand: { brand },
    artifacts: { contents: kits, common_prefixes: [], is_truncated: false },
    artifact: { id: KIT, kind: "brand", role: kits[0]?.role ?? "owner", head_number: 2 },
    artifact_version: {
      manifest: {
        files: [
          { path: "brand.json", object: { hash: "b".repeat(64), size: 10 } },
          { path: "logos/primary.svg", object: { hash: "a".repeat(64), size: 10 } },
        ],
      },
    },
  };
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what in answers ? Promise.resolve(answers[what]) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <BrandSetup />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const stepHeading = (name: string) => screen.findByRole("heading", { name, level: 3 });
const writes = (sent: { method: string; url: string; body: string }[], re: RegExp) =>
  sent.filter((s) => s.method === "POST" && re.test(s.url));

test("a fresh setup opens on Identity with the company's name, and its first Continue makes the draft kit", async () => {
  const sent = engine(null);
  mount(["admin"]);
  expect(await screen.findByRole("heading", { name: "Set up your brand" })).toBeTruthy();
  expect(await stepHeading("Identity")).toBeTruthy();
  expect(screen.getByText("This is how agents name the company in what they make.")).toBeTruthy();
  expect(screen.getByDisplayValue("FairSplit")).toBeTruthy();
  // NO BACK ON THE FIRST STEP, and Identity is never skipped.
  expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Skip for now" })).toBeNull();
  // Continue is the wizard's primary action, the slot that keeps one width on every step.
  expect(
    screen.getByRole("button", { name: "Continue" }).closest(".crewlet-setup-wizard__primary"),
  ).not.toBeNull();
  fireEvent.change(screen.getByDisplayValue("FairSplit"), { target: { value: "FairSplit Labs" } });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(await stepHeading("Logos")).toBeTruthy();
  const [create] = writes(sent, /\/artifacts$/);
  expect(JSON.parse(create!.body)).toMatchObject({
    kind: "brand",
    title: "FairSplit Labs brand",
    shares: [{ audience: { kind: "company" }, role: "viewer" }],
  });
  // A DRAFT, NEVER AN ADOPTION.
  expect(writes(sent, /\/brand\/adoptions$/)).toHaveLength(0);
  const index = sent.find((s) => /files\/brand\.json$/.test(s.url) && s.method === "PUT");
  expect(JSON.parse(index!.body)).toMatchObject({ name: "FairSplit Labs", logos: {}, colors: {} });
});

test("Logos holds Continue until the primary logo is added, and says why", async () => {
  const sent = engine(null);
  mount(["admin"]);
  fireEvent.click(await screen.findByRole("button", { name: "Continue" }));
  expect(await stepHeading("Logos")).toBeTruthy();
  const published = sent.length;
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect((await screen.findByRole("alert")).textContent).toBe(
    "Add the primary logo, for light grounds.",
  );
  expect(screen.getByRole("heading", { name: "Logos", level: 3 })).toBeTruthy();
  expect(sent.length).toBe(published);
});

test("a draft resumes at the first step not done, and the optional steps may be skipped", async () => {
  engine(draft());
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Typography")).toBeTruthy();
  // EVERY STEP UP TO IT IS A WAY BACK in the rail; Typography is optional.
  const rail = screen.getByRole("navigation", { name: "Brand setup steps" });
  expect(within(rail).getByRole("button", { name: /Identity/ })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));
  expect(await stepHeading("Voice")).toBeTruthy();
  expect(
    screen.getByText(/To shape every message they write, add a standing policy in Principles\./),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));
  expect(await stepHeading("Review")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Back" }));
  expect(await stepHeading("Voice")).toBeTruthy();
});

test("the pair that blocks an adoption holds Colours until a suggested shade fixes it", async () => {
  engine(draft({ colors: { primary: "#0f7b5f", text: "#9aa3ad" } }));
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Colours")).toBeTruthy();
  expect(screen.getByText("Blocks publishing")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  await waitFor(() =>
    expect(
      screen.getAllByRole("alert").some((a) => /must reach 4\.5:1/.test(a.textContent ?? "")),
    ).toBe(true),
  );
  expect(screen.getByRole("heading", { name: "Colours", level: 3 })).toBeTruthy();
  fireEvent.click(screen.getAllByRole("button", { name: /^#[0-9a-f]{6}/ })[0]!);
  await waitFor(() => expect(screen.queryByText("Blocks publishing")).toBeNull());
});

test("an administrator adopts from Review once every contrast warning is acknowledged", async () => {
  const sent = engine(whole({ colors: { primary: "#00baba" } }));
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Review")).toBeTruthy();
  const adopt = screen.getByRole("button", { name: "Publish as the company brand" });
  // THE FINISH IS THE WIZARD'S PRIMARY ACTION, by name: on a phone that slot takes the footer's second row.
  expect(adopt.closest(".crewlet-setup-wizard__primary")).not.toBeNull();
  expect(
    screen.getByRole("button", { name: "Save as draft" }).closest(".crewlet-setup-wizard__primary"),
  ).toBeNull();
  // NOTHING IS SAID BEFORE A FINISH IS TRIED.
  expect(screen.queryByRole("alert")).toBeNull();
  fireEvent.click(adopt);
  // WHAT HOLDS IT IS SAID AT THE FIELD, and focus moves to the box to tick.
  const warnings = screen.getByRole("group", { name: "Confirm low contrast" });
  const alert = await within(warnings).findByRole("alert");
  expect(alert.textContent).toBe("Confirm the pair to publish.");
  const [box] = within(warnings).getAllByRole("checkbox");
  await waitFor(() => expect(document.activeElement).toBe(box));
  expect(box!.getAttribute("aria-invalid")).toBe("true");
  expect(document.querySelector(".brand-setup-hold")).toBeNull();
  expect(writes(sent, /\/brand\/adoptions$/)).toHaveLength(0);
  for (const each of within(warnings).getAllByRole("checkbox")) fireEvent.click(each);
  expect(within(warnings).queryByRole("alert")).toBeNull();
  fireEvent.click(adopt);
  await waitFor(() => expect(writes(sent, /\/brand\/adoptions$/)).toHaveLength(1));
  // NOTHING CHANGED SINCE THE DRAFT WAS SAVED: the saved version is adopted,
  // with no publish before it.
  expect(JSON.parse(writes(sent, /\/brand\/adoptions$/)[0]!.body)).toEqual({
    kit: KIT,
    version: 2,
  });
  expect(writes(sent, /\/versions$/)).toHaveLength(0);
  await waitFor(() => expect(location.hash).toBe("#/company/brand"));
});

test("an editor of the kit finishes with Save for review, and never adopts", async () => {
  const sent = engine(whole());
  mount([], [brandKitRow({ role: "editor" })]);
  expect(await stepHeading("Review")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Publish as the company brand" })).toBeNull();
  expect(screen.getByRole("region", { name: "Before you save for review" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Save for review" }));
  await waitFor(() => expect(location.hash).toBe("#/company/brand"));
  expect(writes(sent, /\/brand\/adoptions$/)).toHaveLength(0);
});

test("a reader who is neither an administrator nor an editor of the kit is told so", async () => {
  engine(null);
  mount([]);
  expect(await screen.findByText("Only an administrator sets up the brand")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
});

test("Enter in a one-line field continues to the next step", async () => {
  const sent = engine(null);
  mount(["admin"]);
  const name = await screen.findByDisplayValue("FairSplit");
  fireEvent.keyDown(name, { key: "Enter" });
  expect(await stepHeading("Logos")).toBeTruthy();
  expect(writes(sent, /\/artifacts$/)).toHaveLength(1);
});

test("on a phone the step's place is said above it and the preview folds away", async () => {
  Object.defineProperty(globalThis, "matchMedia", {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      matches: /width\s*</.test(query),
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
  engine(null);
  mount(["admin"]);
  expect(await screen.findByText(/Step 1 of 6/)).toBeTruthy();
  expect(screen.getByRole("button", { name: /Preview/ })).toBeTruthy();
});

test("a change made on the way to Review is saved as a version of the draft, and that version is adopted", async () => {
  const sent = engine(whole());
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Review")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Edit Voice" }));
  expect(await stepHeading("Voice")).toBeTruthy();
  fireEvent.change(screen.getByDisplayValue("Plain, warm and exact."), {
    target: { value: "Plain and exact." },
  });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(await stepHeading("Review")).toBeTruthy();
  const [publish] = writes(sent, /\/artifacts\/01DRAFT\/versions$/);
  expect(JSON.parse(publish!.body)).toMatchObject({ mode: "whole", entry: "brand.json" });
  fireEvent.click(screen.getByRole("button", { name: "Publish as the company brand" }));
  await waitFor(() => expect(writes(sent, /\/brand\/adoptions$/)).toHaveLength(1));
  expect(JSON.parse(writes(sent, /\/brand\/adoptions$/)[0]!.body)).toEqual({
    kit: KIT,
    version: 3,
  });
});

test("the rail says the current step is optional, or needs attention, as well as current", async () => {
  engine(draft({ colors: { primary: "#00baba" } }));
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Typography")).toBeTruthy();
  const rail = screen.getByRole("navigation", { name: "Brand setup steps" });
  const current = within(rail).getByRole("button", { current: "step" });
  expect(current.textContent).toMatch(/Typography/);
  expect(current.textContent).toMatch(/Optional/);
  expect(within(rail).getByRole("button", { name: /Colours/ }).textContent).toMatch(
    /Needs attention/,
  );
});

test("Review shows each part's values and status in one summary, beside the preview and what to confirm", async () => {
  engine(
    whole({
      colors: { primary: "#00baba" },
      voice: { summary: "Plain, warm and exact.", avoid: ["Jargon."] },
    }),
  );
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Review")).toBeTruthy();
  const summary = () => screen.getByRole("list", { name: "Brand summary" });
  // READ AFRESH EACH TIME: Review is drawn again after an Edit and a return.
  const part = (name: string) =>
    within(summary())
      .getAllByRole("listitem")
      .find((row) => within(row).queryByRole("heading", { name }))!;
  // ONE ROW A PART, in the order the setup walks them, each with its way back.
  expect(
    within(summary())
      .getAllByRole("heading")
      .map((h) => h.textContent),
  ).toEqual(["Identity", "Logos", "Colours", "Typography", "Voice"]);
  expect(within(part("Identity")).getByText("FairSplit")).toBeTruthy();
  expect(within(part("Identity")).getByText("Complete")).toBeTruthy();
  expect(within(part("Logos")).getByRole("group", { name: "Primary" })).toBeTruthy();
  const colours = part("Colours");
  // A LOW PAIR NOT YET CONFIRMED is what the colours need.
  expect(within(colours).getByText("Needs attention")).toBeTruthy();
  expect(within(colours).getByText("#00baba")).toBeTruthy();
  expect(
    within(colours).getByText(
      "Crewlet's defaults: Primary on dark, Secondary, Text, Light ground and Dark ground.",
    ),
  ).toBeTruthy();
  const voice = part("Voice");
  expect(within(voice).getByText("Plain, warm and exact.")).toBeTruthy();
  expect(within(voice).getByText("Write like this: none. Avoid: 1 line.")).toBeTruthy();
  fireEvent.click(within(part("Typography")).getByRole("button", { name: "Edit Typography" }));
  expect(await stepHeading("Typography")).toBeTruthy();
  fireEvent.click(
    within(screen.getByRole("navigation", { name: "Brand setup steps" })).getByRole("button", {
      name: /Review/,
    }),
  );
  expect(await stepHeading("Review")).toBeTruthy();
  expect(screen.getByRole("region", { name: "Preview of your draft" })).toBeTruthy();
  const before = screen.getByRole("region", { name: "Before you publish" });
  expect(within(before).getByRole("heading", { name: "When you publish" })).toBeTruthy();
  expect(within(before).getByText("Docs and dashboards switch to it right away.")).toBeTruthy();
  const boxes = within(before).getAllByRole("checkbox");
  for (const box of boxes) fireEvent.click(box);
  // CONFIRMED, the colours lean on Crewlet's defaults and nothing more.
  expect(within(part("Colours")).getByText("Uses defaults")).toBeTruthy();
  // EXIT LEADS THE FOOTER, apart from the step's own actions.
  expect(screen.getByRole("button", { name: "Exit" })).toBeTruthy();
});

test("a low pair says what it needs and why in one line, and offers shades and dark grounds instead", async () => {
  engine(draft({ colors: { primary: "#00baba" } }));
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Typography")).toBeTruthy();
  const rail = screen.getByRole("navigation", { name: "Brand setup steps" });
  fireEvent.click(within(rail).getByRole("button", { name: /Colours/ }));
  expect(await stepHeading("Colours")).toBeTruthy();
  expect(screen.getByText("needs 3:1")).toBeTruthy();
  expect(screen.getByText("Hard to see as links and buttons on light pages.")).toBeTruthy();
  const offers = screen.getByRole("group", { name: "Suggestions for Primary on light ground" });
  expect(within(offers).getByRole("button", { name: /meets the minimum/ })).toBeTruthy();
  expect(within(offers).getByRole("button", { name: /best for text links/ })).toBeTruthy();
  fireEvent.click(within(offers).getByRole("button", { name: /^Use on dark grounds/ }));
  await waitFor(() => expect(screen.queryByText("needs 3:1")).toBeNull());
  // THE APPLIED OFFER STAYS DRAWN, marked chosen, while the colours hold it.
  const kept = screen.getByRole("group", { name: "Suggestions for Primary on light ground" });
  expect(
    within(kept)
      .getByRole("button", { name: /^Use on dark grounds/ })
      .getAttribute("aria-pressed"),
  ).toBe("true");
  expect(screen.getAllByDisplayValue(/00baba/).length).toBeGreaterThan(0);
});

test("the open draft is the assistant's to read, and a change it proposes previews and applies through the setup", async () => {
  engine(draft());
  setFramePage({
    screen: "company-brand-setup",
    title: "Brand › Set up",
    path: ["company", "brand", "setup"],
    params: {},
    query: {},
  });
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Typography")).toBeTruthy();
  const open = () => pageSnapshot()!.context;
  const field = (path: string) => open().draft?.fields.find((f) => f.path === path);
  // WHAT IS OPEN: the kit and version it was opened on, the step, the values,
  // and the logos as metadata the assistant reads and never sets.
  expect(open().draft).toMatchObject({ kind: "client", id: KIT, revision: "2", step: "type" });
  expect(open().draft!.help).toMatch(/no images/);
  expect(open().accepts).toContain("draft.set");
  expect(field("colors.primary")).toMatchObject({ value: "#0f7b5f", editable: true });
  expect(field("type.heading")).toMatchObject({ value: "geist", editable: true });
  expect(field("logos.primary")).toEqual({
    path: "logos.primary",
    label: "Primary logo",
    kind: "file",
    value: "logos/primary.svg",
    hint: expect.stringContaining("image/svg+xml"),
    editable: true,
  });
  const payload = { values: { "colors.primary": "#5B21B6", "voice.summary": "Plain and warm." } };
  // THE CARD SHOWS EACH FIELD'S VALUE NOW beside the proposed one, in the
  // setup's own words.
  expect(pageActionPreview("draft.set", payload)).toEqual({
    rows: [
      { field: "Primary colour", next: "#5b21b6", current: "#0f7b5f" },
      { field: "Voice summary", next: "Plain and warm.", current: "(empty)" },
    ],
    blocked: null,
  });
  let result: ReturnType<typeof applyPageAction> | undefined;
  act(() => {
    result = applyPageAction({
      correlation_id: "c1",
      action: "draft.set",
      payload,
      source_revision: open().source_revision,
    });
  });
  expect(result).toMatchObject({ outcome: "applied" });
  await waitFor(() => expect(field("colors.primary")?.value).toBe("#5b21b6"));
  expect(field("voice.summary")?.value).toBe("Plain and warm.");
  // NOTHING IS SAVED: the change is the person's draft until they continue.
  expect(screen.getByRole("heading", { name: "Typography", level: 3 })).toBeTruthy();
  // A LOGO IS A FILE OF THE CONVERSATION, never markup or a name.
  expect(
    applyPageAction({
      correlation_id: "c2",
      action: "draft.set",
      payload: { values: { "logos.primary": "<svg/>" } },
      source_revision: open().source_revision,
    }),
  ).toMatchObject({ outcome: "invalid", reason: expect.stringMatching(/takes one of your files/) });
});
