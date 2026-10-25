/**
 * The brand setup as each person meets it: a fresh setup opens on Identity
 * and its first Continue makes the draft kit; a step holds Continue until its
 * part is done; a draft resumes where it was left; the pair that blocks an
 * adoption holds Colours until a suggestion fixes it; an administrator adopts
 * from Review once every warning is acknowledged, and an editor saves for
 * review and never adopts; Enter continues from a one-line field; a phone
 * reads the step's place and folds the preview.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { BrandSetup } from "./BrandSetup.tsx";
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
  expect(screen.getByText("Blocks adoption")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  await waitFor(() =>
    expect(
      screen.getAllByRole("alert").some((a) => /must reach 4\.5:1/.test(a.textContent ?? "")),
    ).toBe(true),
  );
  expect(screen.getByRole("heading", { name: "Colours", level: 3 })).toBeTruthy();
  fireEvent.click(screen.getAllByRole("button", { name: /^#[0-9a-f]{6}/ })[0]!);
  await waitFor(() => expect(screen.queryByText("Blocks adoption")).toBeNull());
});

test("an administrator adopts from Review once every contrast warning is acknowledged", async () => {
  const sent = engine(whole({ colors: { primary: "#00baba" } }));
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Review")).toBeTruthy();
  const adopt = screen.getByRole("button", { name: "Adopt as the company brand" });
  fireEvent.click(adopt);
  expect((await screen.findByRole("alert")).textContent).toMatch(/^Confirm the .*contrast warning/);
  expect(writes(sent, /\/brand\/adoptions$/)).toHaveLength(0);
  const warnings = screen.getByRole("region", { name: "Contrast warnings" });
  for (const box of within(warnings).getAllByRole("checkbox")) fireEvent.click(box);
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
  expect(screen.queryByRole("button", { name: "Adopt as the company brand" })).toBeNull();
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
  fireEvent.click(screen.getByRole("button", { name: "Adopt as the company brand" }));
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

test("Review shows each part's values and status beside the preview, then what to confirm before adopting", async () => {
  engine(
    whole({
      colors: { primary: "#00baba" },
      voice: { summary: "Plain, warm and exact.", avoid: ["Jargon."] },
    }),
  );
  mount(["admin"], [brandKitRow()]);
  expect(await stepHeading("Review")).toBeTruthy();
  const identity = screen.getByRole("region", { name: "Identity" });
  expect(within(identity).getByText("FairSplit")).toBeTruthy();
  expect(within(identity).getByText("Complete")).toBeTruthy();
  const colours = screen.getByRole("region", { name: "Colours" });
  expect(within(colours).getByText("Uses defaults")).toBeTruthy();
  expect(within(colours).getByText("#00baba")).toBeTruthy();
  expect(within(colours).getByText("5 more use Crewlet's defaults.")).toBeTruthy();
  const voice = screen.getByRole("region", { name: "Voice" });
  expect(within(voice).getByText("Plain, warm and exact.")).toBeTruthy();
  expect(within(voice).getByText("Write like this: 0 lines. Avoid: 1 line.")).toBeTruthy();
  expect(screen.getByRole("region", { name: "Preview of your draft" })).toBeTruthy();
  const before = screen.getByRole("region", { name: "Before you adopt" });
  expect(within(before).getByRole("heading", { name: "When you adopt" })).toBeTruthy();
  expect(within(before).getByText("Docs and dashboards switch to it right away.")).toBeTruthy();
  expect(within(before).getAllByRole("checkbox").length).toBeGreaterThan(0);
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
