/**
 * A round is ONE block, in the DOM as well as on screen.
 *
 * `rounds()` joins a phase's narration and its tool calls on the number they
 * share, and lib/phases.test.ts proves that join. What is asserted here is the
 * half a reader actually sees: that the join survives into the markup, so a
 * round's thinking, the prose it said and every call it asked for are inside
 * one `.round` element and nothing else is. The complaint that prompted these
 * was "we are not grouping the thinking with its tool call" — the data was
 * grouped and the rendering did not say so.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { PhaseCard } from "./PhaseCard.tsx";
import type { PhaseRecord } from "~/lib/phases.ts";

afterEach(cleanup);

function phase(over: Partial<PhaseRecord> = {}): PhaseRecord {
  return {
    key: "turn-1|execute|1",
    turnId: "turn-1",
    phase: "execute",
    iteration: 1,
    role: "Support Engineer",
    model: "scripted",
    providerKey: "",
    live: false,
    failed: false,
    error: "",
    errorKind: "",
    systemPrompt: "",
    userPrompt: "",
    response: "",
    tools: [],
    narration: [],
    partial: null,
    inputTokens: 0,
    outputTokens: 0,
    totalTokens: 0,
    roundNum: 0,
    roundsUsed: 0,
    exhaustedRounds: false,
    emptyAnswerRounds: 0,
    rescueFired: false,
    decision: "",
    notes: "",
    conversationKey: "",
    toolsAvailable: [],
    toolCatalogue: [],
    worker: "",
    taskId: "",
    hostPhase: "",
    hostIteration: 0,
    backend: "",
    codingAgent: "",
    trigger: null,
    at: "2026-09-02T10:00:00Z",
    startedAt: "2026-09-02T10:00:00Z",
    eventId: "ev-1",
    ...over,
  };
}

const TWO_ROUNDS = phase({
  roundsUsed: 2,
  narration: [
    { round: 1, reasoning: "the file first", content: "Reading the file." },
    { round: 2, reasoning: "that is enough", content: "Posted it." },
  ],
  tools: [
    { name: "read_file", round: 1, args: "{}", result: "contents", failed: false },
    { name: "submit_work", round: 2, args: "{}", result: "ok", failed: false },
  ],
});

describe("a round is one block", () => {
  test("a round's thinking, speech and calls all sit inside that round", () => {
    const { container } = render(<PhaseCard record={TWO_ROUNDS} defaultOpen />);
    const blocks = [...container.querySelectorAll(".round")];
    expect(blocks).toHaveLength(2);

    // The thinking's character count is what its collapsed head shows, so it
    // identifies WHICH round's reasoning without expanding anything.
    expect(blocks[0]!.textContent).toContain(`${"the file first".length} chars`);
    expect(blocks[0]!.textContent).toContain("Reading the file.");
    expect(blocks[0]!.textContent).toContain("read_file");

    // …and none of round 2 leaked into it. This is the failure the complaint
    // describes from the other side: a call rendered away from the thinking
    // that asked for it is a call rendered NEXT TO thinking that did not.
    expect(blocks[0]!.textContent).not.toContain("submit_work");
    expect(blocks[0]!.textContent).not.toContain("Posted it.");
    expect(blocks[1]!.textContent).toContain("submit_work");
    expect(blocks[1]!.textContent).toContain("Posted it.");
  });

  test("no tool call is rendered outside the round that asked for it", () => {
    // The surface this replaces distributed tool badges across
    // inter-paragraph slots, so a call belonged to no round at all and every
    // earlier badge moved each time a new one landed.
    const { container } = render(<PhaseCard record={TWO_ROUNDS} defaultOpen />);
    const rows = [...container.querySelectorAll(".tool-row")];
    // Counted, not just walked: "every row is inside a round" is satisfied by
    // rendering no rows at all, which is the other way to lose a call.
    expect(rows).toHaveLength(2);
    for (const row of rows) {
      expect(row.closest(".round")).not.toBeNull();
    }
  });

  test("a round says which round it is, to a reader who cannot see the rail", () => {
    // The numeral is drawn as a node in the rail and hidden from assistive
    // tech with it — leaving the one thing that ties the blocks below
    // together unannounced, so the thinking and its call were read out as two
    // unrelated collapsed rows.
    render(<PhaseCard record={TWO_ROUNDS} defaultOpen />);
    // Read, not drawn: the numeral a reader sees is the rail's own node, and
    // the sentence beside it is what a screen reader is given instead.
    expect(screen.getByText("Round 1")).toBeDefined();
    expect(screen.getByText("Round 2")).toBeDefined();
  });

  test("a round that called a tool and said nothing is still that round's block", () => {
    // narrations() drops an entry blank in both fields, so this round reaches
    // the ledger through its tool call alone. It must not be dropped and must
    // not merge into its neighbour.
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 2,
          narration: [{ round: 2, reasoning: "", content: "Done." }],
          tools: [
            { name: "read_file", round: 1, args: "{}", result: "contents", failed: false },
            { name: "submit_work", round: 2, args: "{}", result: "ok", failed: false },
          ],
        })}
        defaultOpen
      />,
    );
    const blocks = [...container.querySelectorAll(".round")];
    expect(blocks).toHaveLength(2);
    expect(blocks[0]!.textContent).toContain("read_file");
    expect(blocks[0]!.textContent).not.toContain("Done.");
  });
});

// A FAILED CALL SAYS SO IN WORDS. It was marked with a red glyph carrying no
// name, so the row a reader most needs to find was announced exactly like the
// one above it, and the hue was the only signal anybody got.
test("a failed tool call is named as failed, not coloured as failed", () => {
  render(
    <PhaseCard
      record={phase({
        roundsUsed: 1,
        narration: [{ round: 1, reasoning: "", content: "Trying." }],
        tools: [
          { name: "read_file", round: 1, args: "{}", result: "no such file", failed: true },
          { name: "submit_work", round: 1, args: "{}", result: "ok", failed: false },
        ],
      })}
      defaultOpen
    />,
  );
  expect(screen.getByRole("button", { name: /read_file failed/ })).toBeDefined();
  expect(screen.getByRole("button", { name: "submit_work" })).toBeDefined();
});

/**
 * A HEADER FACT THE ENGINE DID NOT REPORT IS A MARKED ABSENCE.
 *
 * The three facts in this header that can be missing, the model, the round
 * count and the token total, each drew a bare em dash: a mark a screen reader
 * reads as "dash" or skips, and the design system's own absent mark is not
 * one. A reader on the cell with the least to say heard nothing at all, and
 * nothing told them whether the phase used no tokens or whether nobody
 * counted them.
 */
test("a header fact the engine did not report says so, rather than drawing a dash", () => {
  render(<PhaseCard record={phase({ model: "", totalTokens: 0, roundNum: 0 })} />);
  for (const said of ["Model not recorded", "No tool round recorded", "Tokens not reported"]) {
    expect(screen.getByText(said)).toBeDefined();
  }
  // And the punctuation is the design system's, not this file's idea of one.
  expect(document.body.textContent).not.toContain("—");
});
