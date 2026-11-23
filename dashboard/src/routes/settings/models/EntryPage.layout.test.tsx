/**
 * A model's page, laid out: the cards that share a row end at one height, at
 * the widths a 1440px and a 2560px window give the page.
 */
import "~/styles/models.css";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { InertWebSocket, LayoutClient, unevenCardRows } from "~/test/layout.tsx";
import { EntryPage } from "./EntryPage.tsx";
import { MODELS_NOW, MODELS_ORG, at, modelEntry, modelsData, signIn } from "./fixtures.ts";

vi.mock("./api.ts", () => ({ checkHostLogin: vi.fn() }));

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
  pinZoneForTest("UTC");
  vi.stubGlobal("WebSocket", InertWebSocket);
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  pinZoneForTest("");
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

// A sign-in with every line it can carry beside an Answers card with one.
const MODEL = modelEntry({
  key: "sonnet",
  sign_in: signIn({ expires_at: at(60 * 24 * 365) }),
  usage: { days: 7, calls: 0, total_tokens: 0 },
});

async function mount(width: number) {
  await page.viewport(Math.max(1440, width + 40), 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  const data = modelsData({ entries: [MODEL], admin: true });
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={MODELS_ORG as never}>
        <EntryPage model={data.models[0]!} data={data} onFlow={vi.fn()} />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  for (let i = 0; i < 5; i++) await new Promise((r) => requestAnimationFrame(r));
  return host;
}

const card = (at: HTMLElement, title: string) =>
  [...at.querySelectorAll(".crewlet-card")].find(
    (c) => c.querySelector(".crewlet-card__title")?.textContent === title,
  )!;

// The page's own width in a 1440px and in a 2560px window, beside the rail and the settings column.
for (const width of [910, 2030]) {
  test(`at ${width}px the cards beside each other end at one height`, async () => {
    const at = await mount(width);
    const signInCard = card(at, "Sign-in").getBoundingClientRect();
    const answers = card(at, "Answers").getBoundingClientRect();
    // Side by side, so the equal feet are the grid's doing.
    expect(Math.abs(signInCard.top - answers.top)).toBeLessThan(1);
    expect(unevenCardRows(at)).toEqual([]);
  });
}

test("narrow, the cards stand one under another at their own heights", async () => {
  const at = await mount(360);
  const signInCard = card(at, "Sign-in").getBoundingClientRect();
  const answers = card(at, "Answers").getBoundingClientRect();
  expect(answers.top).toBeGreaterThan(signInCard.bottom);
  expect(answers.height).toBeLessThan(signInCard.height);
});
