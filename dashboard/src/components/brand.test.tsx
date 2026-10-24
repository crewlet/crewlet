/**
 * The brand where artifacts are made and read: the Make row's chip names the
 * adopted brand (or offers an administrator the setup), the note under Ask an
 * agent names the brand the agent follows, and the Details fact says whether
 * the version on show is in the current brand, an older one, or Crewlet's
 * look, by how its kind takes the brand.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";
import { BrandScope } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type ArtifactKind, type BrandAnswer } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { BRAND_KIT_FACES, BRAND_SYSTEM_STACKS } from "~/contract/artifacts.ts";
import { faceDrawing } from "~/lib/brand/brand.ts";
import { fmtDate } from "~/lib/format.ts";
import type { BrandFace } from "~/protocol/types.ts";
import { AdoptedBrandScope, BrandAgentNote, BrandFact, BrandMakeChip } from "./brand.tsx";

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
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const CSS = "c".repeat(64);

const ADOPTED: NonNullable<BrandAnswer["brand"]> = {
  kit: "01KIT",
  kit_version: 7,
  document: {
    v: 1,
    name: "FairSplit",
    logos: { primary: "logos/primary.svg" },
    colors: { primary: "#0f7b5f" },
    type: { heading: { kit: "geist" }, body: { kit: "geist" } },
    voice: {},
  },
  css_sha: CSS,
  files: [],
  adopted_by: { name: "maya", kind: "human" },
  adopted_at: "2026-10-24T09:00:00Z",
};

/** Mount `ui` for a reader holding `grants`, over the brand answer and the answers to other reads. */
function mount(
  ui: ReactNode,
  grants: string[],
  brand: BrandAnswer["brand"],
  answers: Record<string, unknown> = {},
) {
  const store = new Store();
  store.applyHealth(healthFrame());
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
        : what in answers
          ? Promise.resolve(answers[what])
          : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <dl>{ui}</dl>
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

test("the Make row's chip names the adopted brand and leads to where it is set", async () => {
  mount(<BrandMakeChip />, [], ADOPTED);
  const chip = await screen.findByRole("link", { name: /FairSplit brand/ });
  expect(chip.getAttribute("href")).toBe("#/company/brand");
});

test("with no brand, an administrator is offered the setup and anyone else nothing", async () => {
  mount(<BrandMakeChip />, ["admin"], null);
  expect(await screen.findByRole("link", { name: /Set up your brand/ })).toBeTruthy();
  cleanup();
  mount(
    <>
      <BrandMakeChip />
      <BrandAgentNote />
      <span>drawn</span>
    </>,
    [],
    null,
  );
  await screen.findByText("drawn");
  await new Promise((r) => setTimeout(r, 0));
  expect(screen.queryByRole("link")).toBeNull();
});

test("the note under Ask an agent names the brand the agent follows", async () => {
  mount(<BrandAgentNote />, [], ADOPTED);
  expect(
    await screen.findByRole("link", { name: /The agent follows the FairSplit brand/ }),
  ).toBeTruthy();
});

/** When the artifact a Brand fact is about was made. */
const MADE = "2026-10-20T09:00:00Z";

/** The Brand fact for an artifact of `kind` whose shown version records `brand`. */
function fact(
  kind: ArtifactKind,
  brand: BrandAnswer["brand"],
  versionBrand?: string,
  theme?: string,
) {
  // THE DECK IS READ OVER HTTP as the fact mounts, so its answer is in place first.
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(JSON.stringify({ artifact: "01ART", version: 3, deck: { theme } }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    ),
  );
  mount(<BrandFact id="01ART" kind={kind} version={3} made={MADE} />, [], brand, {
    artifact_version: {
      artifact: "01ART",
      number: 3,
      capabilities: [],
      ...(versionBrand ? { brand: versionBrand } : {}),
      manifest: { entry: "index.html", files: [] },
    },
  });
}

test("a doc or a dashboard is in the current brand, live", async () => {
  fact("doc", ADOPTED);
  expect(await screen.findByText("FairSplit brand, current")).toBeTruthy();
});

test("a page keeps the brand it was published with until it is next published", async () => {
  fact("html", ADOPTED, CSS);
  expect(await screen.findByText("FairSplit brand, current")).toBeTruthy();
  cleanup();
  fact("html", ADOPTED, "d".repeat(64));
  expect(await screen.findByText("An older FairSplit brand")).toBeTruthy();
  expect(screen.getByText("changes at the next publish")).toBeTruthy();
  cleanup();
  fact("design", ADOPTED);
  expect(await screen.findByText("Crewlet's look")).toBeTruthy();
});

// A DECK COPIES THE BRAND WHEN IT IS MADE and never follows it, so it is never
// "current" nor "older": it says which brand it was made with, and when.
test("a deck says the brand it was made with, and the day", async () => {
  fact("slides", ADOPTED, undefined, "brand");
  expect(await screen.findByText(`Made with the FairSplit brand of ${fmtDate(MADE)}`)).toBeTruthy();
  cleanup();
  fact("slides", ADOPTED, undefined, "midnight");
  expect(await screen.findByText("Crewlet's look")).toBeTruthy();
});

test("a sheet and a brand kit carry no Brand fact", async () => {
  fact("sheet", ADOPTED);
  await new Promise((r) => setTimeout(r, 0));
  expect(screen.queryByText("Brand")).toBeNull();
});

/** The brand inputs the scope drawn in `container` set, by name. */
function scopeInput(container: HTMLElement, name: string): string {
  const scope = container.querySelector<HTMLElement>(".crewlet-brand-scope");
  return scope?.style.getPropertyValue(name) ?? "";
}

// EVERY FACE A BRAND MAY NAME REACHES THE REGION IT DRAWS: a kit face, a
// system stack and an uploaded family (under the name this page registered
// its bytes by), each as the family list `faceDrawing` writes and the kit's
// scope admits. A stack the scope's gate refused set no input, and the doc
// drew in the dashboard's own face whatever the brand said. Mutation: a gate
// that refuses a name starting with "-" (`-apple-system`) or caps the list
// below the longest stack fails every row.
test.each<[string, BrandFace, string | undefined]>([
  ...BRAND_KIT_FACES.map((kit): [string, BrandFace, undefined] => [
    `kit ${kit}`,
    { kit },
    undefined,
  ]),
  ...BRAND_SYSTEM_STACKS.map((stack): [string, BrandFace, undefined] => [
    `system ${stack}`,
    { stack },
    undefined,
  ]),
  [
    "an uploaded family",
    {
      family: "Acme Sans",
      files: [{ path: "fonts/acme.woff2", weight: 400, style: "normal" }],
      licensed: true,
    },
    "crewlet-brand-heading-1a2b",
  ],
])("%s sets the region's heading and body faces", (_, face, registered) => {
  const { stack } = faceDrawing(face, registered);
  expect(stack).not.toBe("");
  const { container } = render(
    <BrandScope colors={{}} headingFamily={stack} bodyFamily={stack}>
      <p>Body</p>
    </BrandScope>,
  );
  expect(scopeInput(container, "--brand-font-heading")).toContain(stack);
  expect(scopeInput(container, "--brand-font-body")).toContain(stack);
  if (registered) expect(scopeInput(container, "--brand-font-heading")).toMatch(`"${registered}"`);
});

test("the adopted brand's faces restyle a doc's body", async () => {
  mount(
    <AdoptedBrandScope>
      <p>Body</p>
    </AdoptedBrandScope>,
    [],
    {
      ...ADOPTED,
      document: {
        ...ADOPTED.document,
        type: { heading: { kit: "geist" }, body: { stack: "system-serif" } },
      },
    },
  );
  await waitFor(() =>
    expect(scopeInput(document.body, "--brand-font-heading")).toContain(
      faceDrawing({ kit: "geist" }).stack,
    ),
  );
  expect(scopeInput(document.body, "--brand-font-body")).toContain(
    faceDrawing({ stack: "system-serif" }).stack,
  );
});
