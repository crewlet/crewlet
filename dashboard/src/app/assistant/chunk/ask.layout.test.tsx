/**
 * ASK CREWLET, LAID OUT: an answer's words read at a measure while its table
 * takes the thread's width, and the page's list keeps its head and its titles
 * readable at a tablet's width.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { Prose } from "@crewlethq/ui";
import { ENGINE_HEADER, ENGINE_HEADER_VALUE } from "~/contract/engine.ts";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { useAskReading } from "../place.ts";
import { AskPage } from "./AskPage.tsx";
import { ReplyProse } from "./blocks.tsx";
import { resetSessionForTest } from "./session.ts";

vi.mock("../place.ts", () => ({ useAskReading: vi.fn() }));

const ORG = { name: "Nimbus", timezone: "UTC", roles: [], units: [] };

let host: HTMLDivElement;
let root: Root;

beforeEach(() => {
  resetSessionForTest();
  sessionStorage.clear();
  vi.stubGlobal("WebSocket", InertWebSocket);
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});

afterEach(() => {
  root.unmount();
  host.remove();
  vi.unstubAllGlobals();
  window.location.hash = "";
});

const WORDS =
  "The release train is on time, but two seats carry most of the load and one of them is paused until the architecture review, so the items it holds wait. ".repeat(
    3,
  );
const TABLE = `| Seat | Open | Held | Waiting on | Last moved |\n|---|---|---|---|---|\n| agent-cto | 2 | 2 | the architecture review of the storage engine | yesterday afternoon |\n| agent-swe | 3 | 0 | nothing | an hour ago |`;

// A MEASURE ON WORDS, NOT ON A TABLE. Mutation: let the app's `.prose` rule
// size the kit's Prose again, or bound the whole answer, and either the words
// run 110 characters a line or the table is cut to a column of prose.
test("an answer's words read at the narrow measure while its table takes the thread's width", async () => {
  await page.viewport(1440, 900);
  host.style.width = "1100px";
  flushSync(() => root.render(<ReplyProse text={`${WORDS}\n\n${TABLE}`} />));
  const words = host.querySelector("p")!;
  const table = host.querySelector("table")!;
  const measure = parseFloat(getComputedStyle(words).getPropertyValue("max-width") || "0");
  expect(measure).toBeGreaterThan(0);
  expect(words.getBoundingClientRect().width).toBeLessThanOrEqual(measure + 1);
  // About seventy characters a line, never a hundred.
  expect(words.getBoundingClientRect().width).toBeLessThan(480);
  expect(table.getBoundingClientRect().width).toBeGreaterThan(words.getBoundingClientRect().width);
});

test("the kit's Prose keeps the measure its caller chose under the app's `.prose` rule", async () => {
  host.style.width = "1100px";
  flushSync(() =>
    root.render(
      <>
        <Prose as="div" measure="narrow" className="prose md" data-case="narrow">
          {WORDS}
        </Prose>
        <Prose as="div" measure="none" className="prose md" data-case="none">
          {WORDS}
        </Prose>
      </>,
    ),
  );
  const narrow = host.querySelector('[data-case="narrow"]')!.getBoundingClientRect().width;
  const none = host.querySelector('[data-case="none"]')!.getBoundingClientRect().width;
  expect(narrow).toBeLessThan(480);
  expect(none).toBeGreaterThan(1000);
});

const at = (ago: number) => new Date(Date.now() - ago).toISOString();
const ROWS = [
  "Follow-ups on the release train for Monday",
  "Summarise every open item in every project",
  "Held work and paused seats this week",
].map((title, i) => ({
  id: `6c1d8b63-52b5-4a1f-8e1f-3c9a2d4f0b2${i}`,
  title,
  title_set_by: "model",
  created_at: at(60_000 * (i + 1)),
  updated_at: at(60_000 * (i + 1)),
  last_activity_at: at(60_000 * (i + 1)),
}));

// AT A TABLET'S WIDTH THE LIST'S HEAD FITS, AND A TITLE SAYS WHAT IT IS.
// Mutation: put the 16rem column back, and New conversation runs under the
// divider and titles stop at fifteen characters.
test("at 768px the list's head fits its column and its titles keep their words", async () => {
  await page.viewport(768, 1024);
  host.style.cssText = "width: 768px; height: 900px; display: flex; flex-direction: column;";
  vi.mocked(useAskReading).mockReturnValue({
    unavailable: null,
    model: null,
    attach: null,
    admin: false,
    name: "Jane Founder",
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({ conversations: ROWS }, { headers: { [ENGINE_HEADER]: ENGINE_HEADER_VALUE } }),
    ),
  );
  window.location.hash = "#/ask";
  flushSync(() =>
    root.render(
      <LayoutClient answers={{}} org={ORG}>
        <AskPage />
      </LayoutClient>,
    ),
  );
  await eventually(() => expect(host.querySelector("[data-conversation]")).not.toBeNull());
  const list = host.querySelector(".ask-page__list")!.getBoundingClientRect();
  const fresh = [...host.querySelectorAll(".ask-page__list-head button")].find(
    (el) => el.textContent === "New conversation",
  )!;
  expect(fresh.getBoundingClientRect().right).toBeLessThanOrEqual(list.right);
  expect(fresh.scrollWidth).toBeLessThanOrEqual(fresh.clientWidth + 1);
  const title = host.querySelector("[data-conversation] .crewlet-inbox-row__title")!;
  expect(title.getBoundingClientRect().width).toBeGreaterThan(180);
});
