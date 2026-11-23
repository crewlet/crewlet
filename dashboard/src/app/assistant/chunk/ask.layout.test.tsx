/**
 * ASK CREWLET, LAID OUT: an answer's words read at a measure while its table
 * takes the thread's width, the page's conversation reads in one centred
 * column that the question, the answer and the composer share, and the
 * page's list keeps its head and its titles readable at a tablet's width.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
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

/** The width `length` resolves to inside `el`, at its type: `var(--size-measure)` is in `ch`. */
function widthOf(el: Element, length: string): number {
  const probe = document.createElement("div");
  probe.style.cssText = `width: ${length}; height: 0;`;
  el.append(probe);
  const width = probe.getBoundingClientRect().width;
  probe.remove();
  return width;
}

// A MEASURE ON WORDS, NOT ON A TABLE, and the kit's reading measure, as its
// Prose and its reading pane read. Mutation: let the app's `.prose` rule size
// the kit's Prose again, or bound the whole answer, and either the words run
// the app's 78ch or the table is cut to a column of prose; put the narrow
// measure back and the words stop at two thirds of it.
test("an answer's words read at the reading measure while its table takes the thread's width", async () => {
  await page.viewport(1440, 900);
  host.style.width = "1100px";
  flushSync(() => root.render(<ReplyProse text={`${WORDS}\n\n${TABLE}`} />));
  await document.fonts.ready;
  const words = host.querySelector("p")!;
  const table = host.querySelector("table")!;
  const measure = widthOf(words.parentElement!, "var(--size-measure)");
  expect(words.getBoundingClientRect().width).toBeLessThanOrEqual(measure + 1);
  expect(words.getBoundingClientRect().width).toBeGreaterThan(measure * 0.95);
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
  // About 25 characters of a title, where the 16rem column left about 15.
  expect(title.getBoundingClientRect().width).toBeGreaterThan(170);
});

const OPEN = "5b0c7a52-41a4-4e0e-9d0e-2b8a1c3e9f10";
const QUESTION =
  "Do I need to post any responses or anything to the advisor about the runs that ended on their round cap, or will the finding close on its own once the new cap is in place? The run was https://crewlet.example.com/live/turns/0f1e2d3c4b5a69788796a5b4c3d2e1f0aabbccddeeff00112233445566778899";
const ANSWER = [
  `**No, you do not need to do anything.** ${WORDS}`,
  "Some notes:",
  `- **Nothing to reply to.** ${WORDS}`,
  `- **It can come back.** ${WORDS}`,
  TABLE,
  '```\ncrewlet seats set agent-cto --max-rounds 300 --reason "the review needs more than the old cap of 200 rounds"\n```',
  WORDS,
].join("\n\n");

/** A conversation with every part a turn draws: the page it carried, its steps, a read it never named, a list, a table and code. */
function longTranscript() {
  const when = at(60_000);
  return {
    conversation: { ...ROWS[0]!, id: OPEN, title: "Runs that ended on their round cap" },
    messages: [
      {
        id: "q1",
        turn_id: "t1",
        role: "user",
        text: QUESTION,
        created_at: when,
        page: {
          title: "Advisor › Agent runs ended on their round cap before they finished.",
          screen: "advisor-finding",
        },
      },
      { id: "a1", turn_id: "t1", role: "assistant", text: ANSWER, created_at: when },
    ],
    steps: [
      {
        step_id: "s1",
        turn_id: "t1",
        index: 0,
        tool: "read_api",
        label: "Read the finding",
        state: "done",
        started_at: when,
        duration_ms: 400,
      },
      {
        step_id: "s2",
        turn_id: "t1",
        index: 1,
        tool: "read_api",
        label: "Read the seat's runs",
        state: "done",
        started_at: when,
        duration_ms: 600,
      },
    ],
    cards: [
      {
        card_id: "c1",
        message_id: "a1",
        card: {
          type: "entity",
          entity: {
            kind: "seat",
            id: "qa",
            title: "QA",
            subtitle: "qa",
            facts: [{ label: "State", value: "Idle" }],
            open: { label: "QA", screen: "seat", href: "#/company/org/seats/qa" },
          },
        },
      },
    ],
    open_turn: null,
    audit_reads: [],
    turns: [{ turn_id: "t1", state: "completed" }],
  };
}

interface Edges {
  left: number;
  right: number;
}

function edges(el: Element): Edges {
  const r = el.getBoundingClientRect();
  return { left: r.left, right: r.right };
}

async function drawPage(width: number): Promise<HTMLElement> {
  await page.viewport(width, 900);
  host.style.cssText = `width: ${width}px; height: 900px; display: flex; flex-direction: column;`;
  vi.mocked(useAskReading).mockReturnValue({
    unavailable: null,
    model: null,
    attach: null,
    admin: false,
    name: "Jane Founder",
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      const path = new URL(url, location.href).pathname;
      const body =
        path === `/assistant/conversations/${OPEN}` ? longTranscript() : { conversations: ROWS };
      return Response.json(body, { headers: { [ENGINE_HEADER]: ENGINE_HEADER_VALUE } });
    }),
  );
  window.location.hash = `#/ask/${OPEN}`;
  flushSync(() =>
    root.render(
      <LayoutClient answers={{}} org={ORG}>
        <AskPage conversation={OPEN} />
      </LayoutClient>,
    ),
  );
  await eventually(() => expect(host.querySelector(".crewlet-message__bubble")).not.toBeNull());
  await eventually(() => expect(host.querySelector(".ask-answer table")).not.toBeNull());
  await document.fonts.ready;
  return host;
}

// ONE COLUMN ON A WIDE PAGE. Mutation: drop `column` from the page's thread,
// and the bubble stands at the pane's far edge while the answer hugs the
// other; drop the foot's centring, and the composer spans the pane.
for (const width of [1440, 2000]) {
  test(`at ${width} wide the conversation reads in one centred column the composer shares`, async () => {
    const at = await drawPage(width);
    const pane = edges(at.querySelector(".ask-page__open")!);
    const items = at.querySelector<HTMLElement>(".crewlet-chat-thread__items")!;
    const style = getComputedStyle(items);
    const box = items.getBoundingClientRect();
    const column = {
      left: box.left + parseFloat(style.paddingLeft),
      right: box.right - parseFloat(style.paddingRight),
    };
    // Centred on the pane, at the wide measure.
    expect(column.left - pane.left).toBeCloseTo(pane.right - column.right, 0);
    expect(column.right - column.left).toBeCloseTo(widthOf(items, "var(--size-measure-wide)"), 0);

    // The question ends on the column's end edge; the steps, the answer, the
    // line of reads and the turn's foot start on its start edge.
    expect(edges(at.querySelector(".crewlet-message__bubble")!).right).toBeCloseTo(column.right, 0);
    const answer = at.querySelector(".ask-answer")!;
    expect(edges(answer).left).toBeCloseTo(column.left, 0);
    // The steps row's chevron, which the kit hangs its trigger's padding out to.
    const steps = [...at.querySelectorAll("button")].find((b) => /Done/.test(b.textContent ?? ""))!;
    expect(edges(steps.querySelector("svg")!).left).toBeCloseTo(column.left, 0);
    const reads = at.querySelector(".crewlet-record-chip-list")!;
    expect(edges(reads).left).toBeGreaterThanOrEqual(column.left - 1);
    expect(edges(reads).right).toBeLessThanOrEqual(column.right + 1);
    const copy = [...at.querySelectorAll("button")].find((b) => b.textContent === "Copy")!;
    expect(edges(copy).left).toBeGreaterThan(column.left);
    expect(edges(copy).right).toBeLessThan(column.right);

    // Words at the reading measure; the table and the code at the column's width, no further.
    const measure = widthOf(answer, "var(--size-measure)");
    for (const p of answer.querySelectorAll<HTMLElement>(":scope > p, :scope > ul")) {
      expect(p.getBoundingClientRect().width).toBeLessThanOrEqual(measure + 1);
    }
    const longest = Math.max(
      ...[...answer.querySelectorAll<HTMLElement>(":scope > p")].map(
        (p) => p.getBoundingClientRect().width,
      ),
    );
    expect(longest).toBeGreaterThan(measure * 0.9);
    for (const wide of answer.querySelectorAll(":scope > .md-table, :scope > pre, table")) {
      expect(edges(wide).right).toBeLessThanOrEqual(column.right + 1);
    }

    // The composer shares both edges.
    const composer = edges(at.querySelector(".ask-page__foot .crewlet-composer")!);
    expect(composer.left).toBeCloseTo(column.left, 0);
    expect(composer.right).toBeCloseTo(column.right, 0);

    // Nothing scrolls sideways.
    const scroller = at.querySelector<HTMLElement>(".crewlet-chat-thread__scroller")!;
    expect(scroller.scrollWidth).toBeLessThanOrEqual(scroller.clientWidth);
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(
      document.documentElement.clientWidth,
    );
  });
}

// AT A PHONE'S WIDTH THE COLUMN IS THE PANE, on the thread's own inset, and
// the long question wraps rather than push the page sideways.
test("at 390 wide the conversation keeps its inset and nothing scrolls sideways", async () => {
  const at = await drawPage(390);
  const pane = edges(at.querySelector(".ask-page__open")!);
  const bubble = edges(at.querySelector(".crewlet-message__bubble")!);
  const composer = edges(at.querySelector(".ask-page__foot .crewlet-composer")!);
  expect(composer.left - pane.left).toBeCloseTo(16, 0);
  expect(pane.right - composer.right).toBeCloseTo(16, 0);
  expect(bubble.right).toBeLessThanOrEqual(pane.right - 16 + 1);
  const scroller = at.querySelector<HTMLElement>(".crewlet-chat-thread__scroller")!;
  expect(scroller.scrollWidth).toBeLessThanOrEqual(scroller.clientWidth);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(
    document.documentElement.clientWidth,
  );
});

const DAYS = [
  { title: "Brief: Inbox", ago: 60_000 },
  { title: "What value should I copy into the settings", ago: 120_000 },
  { title: "Web restyle confirmed live", ago: 3 * 86_400_000 },
  { title: "Why agents are idle with no work", ago: 3.2 * 86_400_000 },
].map(({ title, ago }, i) => ({
  ...ROWS[0]!,
  id: `7d2e9c74-63c6-4b2a-9f2a-4d0b3e5f1c3${i}`,
  title,
  created_at: at(ago),
  updated_at: at(ago),
  last_activity_at: at(ago),
}));

/** A box's middle, in px from the viewport's top. */
const middle = (box: DOMRect) => box.top + box.height / 2;

// A ROW'S MENU STANDS ON ITS MIDDLE, AND A DAY IS A THIN FILLED BAND.
// Mutation: a kit that pins the tray to the row's top corner puts it 8px
// over the middle; one that sets the day as a rule draws no fill and sits low.
test("each conversation's menu stands on its row's middle, and each day is a thin band between the rows", async () => {
  await page.viewport(1440, 900);
  host.style.cssText = "width: 1440px; height: 900px; display: flex; flex-direction: column;";
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
      Response.json({ conversations: DAYS }, { headers: { [ENGINE_HEADER]: ENGINE_HEADER_VALUE } }),
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
  await eventually(() => expect(host.querySelectorAll("[data-conversation]")).toHaveLength(4));
  await document.fonts.ready;
  for (const row of host.querySelectorAll<HTMLElement>("[data-conversation]")) {
    row.querySelector<HTMLElement>(".crewlet-inbox-row__control")!.focus();
    const tray = row.querySelector(".crewlet-message-actions")!;
    await eventually(() =>
      expect(getComputedStyle(tray.parentElement!).visibility).toBe("visible"),
    );
    const rowMiddle = row.getBoundingClientRect().top + row.clientHeight / 2;
    const name = row.querySelector(".crewlet-inbox-row__title")!.getClientRects()[0]!;
    expect(Math.abs(middle(tray.getBoundingClientRect()) - rowMiddle)).toBeLessThanOrEqual(1);
    expect(Math.abs(middle(name) - rowMiddle)).toBeLessThanOrEqual(1);
  }
  const labels = [...host.querySelectorAll(".crewlet-inbox-list__label")];
  // Today and three days back: two days at least, however near midnight it runs.
  expect(labels.length).toBeGreaterThanOrEqual(2);
  const row = host.querySelector("[data-conversation]")!.getBoundingClientRect();
  for (const label of labels) {
    expect(getComputedStyle(label, "::after").content).toBe("none");
    expect(getComputedStyle(label).backgroundColor).not.toBe("rgba(0, 0, 0, 0)");
    const band = label.getBoundingClientRect();
    const words = label.querySelector(".crewlet-inbox-list__day")!.getBoundingClientRect();
    expect(band.height).toBeLessThan(row.height / 2);
    expect(Math.abs(words.top - band.top - (band.bottom - words.bottom))).toBeLessThanOrEqual(0.5);
  }
});

const LONG_QUESTION = Array.from(
  { length: 12 },
  (_, i) =>
    `Question ${i + 1}: do I add the developer key and the app manager key, or change the role too?`,
).join("\n");

// THE FIELD'S OWN INSET, WHICHEVER SHEET LOADS LAST, AND A BAR ALONG THE WORDS.
// Mutation: a kit whose field inset ties with the textarea's sets the line 3px
// under Send's middle; one without the track's inset runs the bar edge to edge.
test("the composer's first line sits beside Send, and past its cap its bar keeps the words' inset", async () => {
  const at = await drawPage(1440);
  const field = at.querySelector<HTMLTextAreaElement>("#assistant-composer")!;
  const send = at.querySelector(".ask-page__foot .crewlet-composer__trailing button")!;
  const style = getComputedStyle(field);
  const line =
    field.getBoundingClientRect().top +
    parseFloat(style.paddingTop) +
    parseFloat(style.lineHeight) / 2;
  expect(Math.abs(line - middle(send.getBoundingClientRect()))).toBeLessThanOrEqual(1);
  await userEvent.fill(field, LONG_QUESTION);
  await eventually(() => expect(field.scrollHeight).toBeGreaterThan(field.clientHeight));
  const track = getComputedStyle(field, "::-webkit-scrollbar-track");
  expect(parseFloat(style.paddingTop)).toBeGreaterThan(0);
  expect(track.marginTop).toBe(style.paddingTop);
  expect(track.marginBottom).toBe(style.paddingBottom);
  expect(style.scrollbarGutter).toBe("stable");
});
