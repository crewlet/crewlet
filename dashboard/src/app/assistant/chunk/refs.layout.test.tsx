/**
 * A change card's records, laid out: a receipt's words and its record read as
 * one line, and a record's mark sits centred on its words.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { InertWebSocket, LayoutClient, lines } from "~/test/layout.tsx";
import { CardBlock, type CardHandlers } from "./blocks.tsx";

const ORG = {
  name: "Nimbus",
  timezone: "UTC",
  roles: [{ name: "Agent CTO", handle: "agent-cto" }],
  units: [],
};

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
  openHere: () => {},
  ask: () => {},
  undo: () => {},
};

let host: HTMLDivElement;
let root: Root;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});

afterEach(() => {
  root.unmount();
  host.remove();
  vi.unstubAllGlobals();
});

const task = { kind: "task" as const, id: "ENG-156", name: "ENG-156" };
const seat = { kind: "seat" as const, id: "agent-cto", name: "Agent CTO" };
const open = (href: string) => ({ label: "", screen: "", href });

const card: AssistantCardEvent = {
  card_id: "c",
  message_id: "m",
  card: {
    type: "proposal",
    proposal: {
      proposal_id: "p",
      scope: "server",
      tool: "create_task",
      title: "Create task",
      changes: [
        {
          kind: "added",
          field: "assignee",
          label: "Assignee",
          to: [{ ref: { ...seat, open: open("#/company/org/seats/agent-cto") } }],
        },
      ],
      note: [
        { ref: { ...seat, open: open("#/company/org/seats/agent-cto") } },
        { text: " is assigned the new task once this is approved." },
      ],
      digest: `sha256:${"a".repeat(64)}`,
      expires_at: "2099-01-01T00:00:00Z",
      state: "applied",
      approve_label: "Approve and create",
      receipt: {
        summary: [{ text: "Created " }, { ref: { ...task, open: open("#/worklet/ENG-156") } }],
      },
    },
  },
};

// A RECEIPT IS ONE LINE: its words and its record never stack in the strip's
// column. Mutation: draw a phrase's runs as siblings again and the key drops a line.
test("a receipt's words and the record it names read as one line", async () => {
  await page.viewport(1440, 900);
  host.style.width = "420px";
  flushSync(() =>
    root.render(
      <LayoutClient answers={{}} org={ORG}>
        <CardBlock event={card} local={undefined} on={on} opened={false} />
      </LayoutClient>,
    ),
  );
  const ref = host.querySelector<HTMLElement>("a[href='#/worklet/ENG-156']")!;
  const receipt = ref.parentElement!.parentElement!;
  expect(lines(receipt).map((row) => row.map((w) => w.map((g) => g.c).join("")))).toEqual([
    ["Created", "ENG-156"],
  ]);
});

// A MARK SITS ON ITS WORDS: a face and a glyph each centred on the words they
// lead, within a pixel, so a row of records reads as one line of text.
test("a record's mark is centred on its words", async () => {
  await page.viewport(1440, 900);
  host.style.width = "420px";
  flushSync(() =>
    root.render(
      <LayoutClient answers={{}} org={ORG}>
        <CardBlock event={card} local={undefined} on={on} opened={false} />
      </LayoutClient>,
    ),
  );
  const refs = [...host.querySelectorAll<HTMLElement>(".crewlet-inline-ref")];
  expect(refs.length).toBeGreaterThanOrEqual(3);
  for (const ref of refs) {
    const mark = ref.querySelector(".crewlet-inline-ref__mark")!.getBoundingClientRect();
    const words = ref.querySelector(".crewlet-inline-ref__text")!.getBoundingClientRect();
    const off = mark.top + mark.height / 2 - (words.top + words.height / 2);
    expect(Math.abs(off), ref.textContent ?? "").toBeLessThanOrEqual(1.5);
  }
});
