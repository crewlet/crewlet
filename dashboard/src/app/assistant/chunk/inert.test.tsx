/**
 * TestTheAssistantRendersNoLinkAndNoImage: the drawer draws no live link and
 * no picture out of untrusted text, wherever that text arrives.
 *
 * ZERO TOLERANCE, OVER THE WHOLE THREAD. `prose.test.tsx` holds the renderer
 * to one sentence per vector; this suite holds the DRAWER to the rule. An
 * adversarial corpus (inline, reference and autolinks, pictures, raw HTML,
 * `javascript:`, `data:` and `vbscript:` addresses in any case, SVG, frames
 * and event handler attributes) is written into every member of the wire a
 * model or a record controls: the reply's deltas, an entity card's title,
 * subtitle, facts and text, a list's items, a table's title, columns and
 * cells, a notice and a refusal. The stream goes through the client's own
 * parser and reducer and the whole `Thread` is rendered, so the assertion is
 * about what a person's browser would hold, not about one function.
 *
 * What the DOM may hold afterwards: no element that fetches or runs anything
 * (`img`, `picture`, `source`, `video`, `audio`, `iframe`, `frame`, `object`,
 * `embed`, `script`, `style`, `link`, `meta`, `base`, `form`, SVG `image` and
 * `use`), no `src`, `srcset`, `srcdoc`, `poster`, `background`, `formaction` or
 * `xlink:href` attribute, no `on*` handler attribute, and no anchor at all
 * except the engine's own Open in app card, whose address is a `#/` route the
 * engine built. Each corpus line's readable words are still on the page: the
 * rule makes text inert, it never drops it.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";

import type { AssistantCard, AssistantEventKind } from "~/contract/assistant.ts";
import {
  createAssistantStreamParser,
  type AssistantNarrowFrame,
} from "~/protocol/assistantWire.ts";
import { Thread } from "./Thread.tsx";
import { EMPTY, reduce, type ConversationView, type TurnEvent } from "./turn.ts";

afterEach(cleanup);

const TURN = "0b5e5f0e-6a43-4c41-9d0f-3f1d7a2c9e10";
const CONV = "6c1b8f7a-2d4e-4b9a-8c3f-5e7d1a0b2c4d";
const MESSAGE = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d";
const AT = "2026-10-24T09:41:00Z";

/**
 * Every way a string can try to become a link, a fetch or a script, each with
 * the words a reader must still be able to read once it is inert.
 */
const CORPUS: { text: string; readable: string }[] = [
  {
    text: "See [the pricing page](https://docs.example.com/pricing).",
    readable: "the pricing page",
  },
  { text: "[click me](javascript:alert(1))", readable: "click me" },
  { text: "[Shout](JaVaScRiPt:alert(1))", readable: "Shout" },
  { text: "[spaced]( javascript:alert(1) )", readable: "spaced" },
  { text: "[old browsers](vbscript:msgbox(1))", readable: "old browsers" },
  {
    text: "[inline page](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)",
    readable: "inline page",
  },
  { text: "[this task](#/worklet/ENG-1)", readable: "this task" },
  { text: "[relative](/settings/iam)", readable: "relative" },
  { text: "[ref link][r1]\n\n[r1]: https://evil.example/r", readable: "ref link" },
  { text: "Open <https://evil.example/login?next=1> now.", readable: "evil.example/login" },
  { text: "Mail <attacker@evil.example> today.", readable: "attacker@evil.example" },
  {
    text: "Visit https://evil.example/bare or www.evil.example soon.",
    readable: "evil.example/bare",
  },
  { text: "![a chart](https://tracker.example/pixel.png)", readable: "a chart" },
  { text: "![](data:image/png;base64,AAAA)", readable: "" },
  { text: '![tracked](https://tracker.example/p.gif "title")', readable: "tracked" },
  {
    text: "[![nested](https://tracker.example/n.png)](https://evil.example/n)",
    readable: "nested",
  },
  { text: '<img src=x onerror="alert(1)">', readable: "<img" },
  { text: '<a href="https://evil.example">raw anchor</a>', readable: "raw anchor" },
  { text: "<iframe src=https://evil.example></iframe>", readable: "<iframe" },
  { text: "<script>alert(1)</script>", readable: "<script>" },
  {
    text: '<svg onload="alert(1)"><image href="https://tracker.example/s.png"/></svg>',
    readable: "<svg",
  },
  { text: '<form action="https://evil.example"><button>Submit</button></form>', readable: "<form" },
  {
    text: '<div style="background:url(https://tracker.example/bg.png)">styled</div>',
    readable: "styled",
  },
  { text: "&lt;img src=x onerror=alert(1)&gt;", readable: "img src=x" },
  { text: "`[code](javascript:alert(1))`", readable: "javascript:alert(1)" },
];

const ALL = CORPUS.map((c) => c.text).join("\n\n");

/** The cards an answer may carry, each member a model or a record controls filled with the corpus. */
function cards(): AssistantCard[] {
  const line = (i: number) => CORPUS[i % CORPUS.length]!.text;
  return [
    {
      type: "entity",
      entity: {
        kind: "task",
        id: "ENG-7",
        title: line(0),
        subtitle: line(9),
        facts: CORPUS.map((c, i) => ({ label: `Field ${i}`, value: c.text })),
        text: ALL,
        open: { label: "Open ENG-7", screen: "item", href: "#/worklet/ENG-7" },
      },
    },
    {
      type: "entity",
      entity: {
        kind: "task",
        id: "list",
        title: line(12),
        items: CORPUS.map((c, i) => ({
          kind: "task",
          id: `ENG-${i}`,
          label: c.text,
          fact: line(i + 1),
        })),
      },
    },
    {
      type: "table",
      table: {
        title: line(16),
        columns: [
          { key: "name", label: line(1), type: "text" },
          { key: "note", label: line(13), type: "text" },
        ],
        rows: CORPUS.map((c, i) => [c.text, line(i + 3)]),
        row_count: CORPUS.length,
        truncated: false,
        as_of: AT,
      },
    },
    { type: "notice", notice: { tone: "warning", text: ALL } },
    {
      type: "error",
      error: { refusal: { code: "unauthorized", message: ALL, hint: line(17) }, phrase: line(2) },
    },
    {
      type: "open_in_app",
      open_in_app: {
        link: { label: line(18), screen: "item", href: "#/worklet/ENG-7" },
        permitted: true,
      },
    },
  ] as unknown as AssistantCard[];
}

/** One turn's stream as the engine writes it: each frame one SSE event. */
function stream(): string {
  let seq = 0;
  const event = <K extends AssistantEventKind>(kind: K, payload: unknown) => {
    seq++;
    const frame = {
      v: 1,
      conversation_id: CONV,
      turn_id: TURN,
      seq,
      at: AT,
      kind,
      [kind]: payload,
    };
    return `id: ${TURN}.${seq}\nevent: ${kind}\ndata: ${JSON.stringify(frame)}\n\n`;
  };
  let body = event("status", { state: "thinking" });
  for (const c of CORPUS) body += event("delta", { message_id: MESSAGE, text: `${c.text}\n\n` });
  cards().forEach((card, i) => {
    body += event("card", {
      card_id: `c0c0c0c0-0000-4000-8000-00000000000${i}`,
      message_id: MESSAGE,
      card,
    });
  });
  body += event("done", { state: "completed", duration_ms: 1200, steps: 0, message_id: MESSAGE });
  return body;
}

/** The conversation the stream leaves, through the client's own parser and reducer. */
function view(): ConversationView {
  const frames: AssistantNarrowFrame[] = [];
  const faults: string[] = [];
  createAssistantStreamParser({
    frame: (f) => frames.push(f),
    fault: (why) => faults.push(JSON.stringify(why)),
  }).push(stream());
  // THE CORPUS MUST REACH THE DRAWER: a frame the parser refused would
  // make this suite pass by drawing nothing.
  expect(faults).toEqual([]);
  expect(frames.filter((f) => f.kind === "card")).toHaveLength(cards().length);
  const start: TurnEvent[] = [
    {
      type: "send",
      key: "k1",
      text: "What is on ENG-7?",
      page: null,
      attachments: [],
      mode: "ask",
      at: AT,
    },
    { type: "accepted", key: "k1", turnId: TURN, afterSeq: 0 },
  ];
  return [...start, ...frames.map((frame): TurnEvent => ({ type: "frame", frame }))].reduce(
    reduce,
    EMPTY,
  );
}

/** Elements that fetch, frame, submit or run something. */
const LIVE_ELEMENTS =
  "img, picture, source, video, audio, track, iframe, frame, object, embed, script, style, " +
  "link, meta, base, form, image, use";

/** Attributes that make a browser fetch or run something. */
const LIVE_ATTRIBUTES = [
  "src",
  "srcset",
  "srcdoc",
  "poster",
  "background",
  "formaction",
  "xlink:href",
];

function renderThread() {
  const nothing = () => {};
  return render(
    <Thread
      view={view()}
      now={Date.parse(AT) + 5_000}
      on={{
        approve: nothing,
        decline: nothing,
        prepareAgain: nothing,
        openHere: nothing,
        ask: nothing,
        undo: nothing,
      }}
      opened={new Set()}
      onRetry={nothing}
      onReconnect={nothing}
    />,
  );
}

describe("TestTheAssistantRendersNoLinkAndNoImage", () => {
  test("nothing in the thread fetches, frames, submits or runs", () => {
    const { container } = renderThread();
    expect([...container.querySelectorAll(LIVE_ELEMENTS)].map((el) => el.outerHTML)).toEqual([]);
    const live: string[] = [];
    for (const el of container.querySelectorAll("*")) {
      for (const attr of el.attributes) {
        const name = attr.name.toLowerCase();
        if (name.startsWith("on") || LIVE_ATTRIBUTES.includes(name))
          live.push(`${el.tagName} ${name}`);
        if (/^\s*(javascript|vbscript|data):/i.test(attr.value))
          live.push(`${el.tagName} ${name}=${attr.value}`);
      }
    }
    expect(live).toEqual([]);
  });

  test("the only links are the engine's own, to this dashboard", () => {
    const { container } = renderThread();
    const hrefs = [...container.querySelectorAll("a, area, [href]")].map((el) =>
      el.getAttribute("href"),
    );
    // The entity card's Open and the Open in app card: each a `#/` route the
    // engine built from its own table, never an address the model wrote.
    expect(hrefs.length).toBeGreaterThan(0);
    for (const href of hrefs) expect(href).toMatch(/^#\/worklet\/ENG-7$/);
  });

  test("every corpus line is still there to read", () => {
    const { container } = renderThread();
    const text = container.textContent ?? "";
    for (const { readable } of CORPUS) if (readable) expect(text).toContain(readable);
  });
});
