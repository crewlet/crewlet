// @vitest-environment node

/**
 * The assistant's stream, read by the client exactly as the engine writes it.
 *
 * EVERY FRAME HERE IS THE ENGINE'S OWN ENCODING, read from the golden its Go
 * test regenerates (`internal/assistant/wire/testdata/frames.sse`, `make
 * assistant-fixtures`). A fixture typed here would agree with this parser
 * whatever the engine sends.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";

import { repoRoot } from "~/test/engineFiles.ts";
import {
  ASSISTANT_PAGE_CONTEXT_MAX_BYTES,
  type AssistantEventKind,
  type AssistantPageContext,
} from "../contract/assistant.ts";
import {
  assistantUnavailable,
  createAssistantStreamParser,
  isProposalDigest,
  narrowCard,
  pageContextFits,
  questionFits,
  type AssistantNarrowFrame,
  type AssistantStreamFault,
} from "./assistantWire.ts";

const GOLDEN = readFileSync(
  join(repoRoot(), "internal/assistant/wire/testdata/frames.sse"),
  "utf8",
);

/** Every frame of a stream, fed to the parser in chunks of `size` characters. */
function parse(text: string, size: number) {
  const frames: AssistantNarrowFrame[] = [];
  const faults: AssistantStreamFault[] = [];
  const parser = createAssistantStreamParser({
    frame: (f) => frames.push(f),
    fault: (f) => faults.push(f),
  });
  for (let i = 0; i < text.length; i += size) parser.push(text.slice(i, i + size));
  return { frames, faults };
}

const KINDS: Record<AssistantEventKind, true> = {
  conversation: true,
  page_context: true,
  status: true,
  delta: true,
  step: true,
  card: true,
  action: true,
  done: true,
  error: true,
  snapshot: true,
};

describe("the engine's golden stream", () => {
  test("reads every frame once, however the bytes are cut", () => {
    const events = GOLDEN.split("\n").filter((line) => line.startsWith("id: ")).length;
    expect(events).toBeGreaterThan(10);
    for (const size of [1, 7, 64, 4096, GOLDEN.length]) {
      const { frames, faults } = parse(GOLDEN, size);
      expect(faults).toEqual([]);
      expect(frames).toHaveLength(events);
    }
  });

  test("carries every event kind this build reads, and every card kind it draws", () => {
    const { frames } = parse(GOLDEN, GOLDEN.length);
    const seen = new Set(frames.map((f) => f.kind));
    expect([...seen].sort()).toEqual(Object.keys(KINDS).sort());
    const cards = frames.flatMap((f) => (f.kind === "card" ? [narrowCard(f.card.card)] : []));
    expect(cards).not.toContain(null);
    expect([...new Set(cards.map((c) => c?.type))].sort()).toEqual([
      "brief",
      "entity",
      "error",
      "file",
      "notice",
      "open_in_app",
      "oversize",
      "proposal",
      "table",
    ]);
  });

  test("a deep run is re-sent under one step id, its step names it, and its files are a card", () => {
    const { frames } = parse(GOLDEN, 97);
    const steps = frames.flatMap((f) => (f.kind === "step" ? [f.step] : []));
    const runs = steps.filter((s) => s.deep !== undefined);
    expect(runs.map((s) => s.deep?.state)).toEqual(["running", "finished"]);
    expect(new Set(runs.map((s) => s.step_id)).size).toBe(1);
    expect(steps.some((s) => s.parent === runs[0]!.index)).toBe(true);
    const files = frames.flatMap((f) =>
      f.kind === "card" && f.card.card.type === "file" ? (f.card.card.file?.files ?? []) : [],
    );
    expect(files.map((f) => f.media_type)).toContain("image/svg+xml");
    for (const file of files) expect(file.download.startsWith("/assistant/")).toBe(true);
  });

  test("narrows each frame to the payload its kind names", () => {
    for (const frame of parse(GOLDEN, GOLDEN.length).frames) {
      expect(frame[frame.kind as keyof typeof frame]).toBeDefined();
      if (frame.kind === "card" && frame.card.card.type === "proposal") {
        expect(isProposalDigest(frame.card.card.proposal?.digest ?? "")).toBe(true);
      }
    }
  });
});

describe("a stream the parser must not trust", () => {
  const first = GOLDEN.slice(0, GOLDEN.indexOf("\n\n") + 2);

  test("stops at a frame of another protocol version", () => {
    const { frames, faults } = parse(first.replace('"v":1', '"v":2') + GOLDEN, 4096);
    expect(frames).toEqual([]);
    expect(faults).toEqual([{ code: "protocol_mismatch", version: 2 }]);
  });

  test("skips a kind this build does not know, within the version", () => {
    const unknown = first
      .replace("event: conversation", "event: reaction")
      .replace('"kind":"conversation"', '"kind":"reaction"');
    const { frames, faults } = parse(unknown + GOLDEN, 4096);
    expect(faults).toEqual([]);
    expect(frames.length).toBe(parse(GOLDEN, 4096).frames.length);
  });

  test("refuses an event whose id or name disagrees with its frame", () => {
    for (const forged of [
      first.replace(/\.1\n/, ".2\n"),
      first.replace("event: conversation", "event: status"),
    ]) {
      expect(parse(forged, 4096).faults).toHaveLength(1);
    }
  });
});

describe("the bounds a request is held to before it is sent", () => {
  const context: AssistantPageContext = {
    screen: "worklet-tasks",
    title: "Worklet › Tasks",
    params: {},
    query: {},
    resources: [],
    selection: null,
    source_revision: "url:3f9c1a0e",
    accepts: ["work.filter.apply"],
  };

  test("a question is counted in code points, as the engine counts runes", () => {
    expect(questionFits("😀".repeat(4000))).toBe(true);
    expect(questionFits("😀".repeat(4001))).toBe(false);
  });

  test("a page context may not offer the reserved action or grow past its bytes", () => {
    expect(pageContextFits(context)).toBe(true);
    expect(pageContextFits({ ...context, accepts: ["context.invalidate"] })).toBe(false);
    expect(
      pageContextFits({
        ...context,
        query: { q: "x".repeat(ASSISTANT_PAGE_CONTEXT_MAX_BYTES + 1) },
      }),
    ).toBe(false);
  });

  test("a viewer answer with no assistant member reads as disabled", () => {
    expect(assistantUnavailable(undefined)).toBe("disabled");
    expect(assistantUnavailable({ available: true })).toBeNull();
    expect(assistantUnavailable({ available: false, reason: "not_a_person" })).toBe("not_a_person");
  });
});
