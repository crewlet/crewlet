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
    sandboxId: "",
    costUSD: 0,
    deliveredRefs: [],
    trigger: null,
    at: "2026-09-02T10:00:00Z",
    startedAt: "2026-09-02T10:00:00Z",
    durationMs: 0,
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
    {
      name: "read_file",
      round: 1,
      args: "{}",
      result: "contents",
      failed: false,
      durationMs: 0,
      origin: "builtin",
      server: "",
    },
    {
      name: "submit_work",
      round: 2,
      args: "{}",
      result: "ok",
      failed: false,
      durationMs: 0,
      origin: "builtin",
      server: "",
    },
  ],
});

describe("a settled phase's zero is a number", () => {
  /**
   * ABSENT AND ZERO ARE DIFFERENT FACTS, and a dash claims the first about the
   * second.
   *
   * A phase run on a subscription CLI backend reports no usage at all, and one
   * the engine stopped before its first call came back has a `total_tokens` of
   * 0 on a record that is settled. `totalTokens ? … : "—"` printed "not
   * recorded" for both, on the card an operator opens precisely to find out
   * which phase was expensive — and the same card already prints the
   * delegation total through `fmtCount` unconditionally, so the file
   * contradicted itself.
   */
  test("a phase that spent nothing says 0, not “not recorded”", () => {
    const { container } = render(<PhaseCard record={phase({ totalTokens: 0, failed: true })} />);
    const tokens = container.querySelector('[title="total tokens"]');
    expect(tokens?.textContent).toBe("0");
  });

  test("and the same for the rounds it never took", () => {
    const { container } = render(<PhaseCard record={phase({ failed: true })} />);
    expect(container.querySelector('[title="tool rounds used"]')?.textContent).toBe("0r");
  });

  // THE DASH SURVIVES WHERE THE ZERO REALLY IS AN ABSENCE: a phase still
  // running has not reported its usage yet, which is not the same as having
  // spent nothing.
  test("a live phase's zero is still an absence", () => {
    const { container } = render(
      <PhaseCard record={phase({ live: true, totalTokens: 0, roundsUsed: 0 })} />,
    );
    expect(container.querySelector('[title="total tokens"]')).toBeNull();
    expect(
      container.querySelector('[title="this phase has not reported its usage yet"]')?.textContent,
    ).toBe("—");
    expect(
      container.querySelector('[title="this phase has not finished a round yet"]')?.textContent,
    ).toBe("—");
  });
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
    const { container } = render(<PhaseCard record={TWO_ROUNDS} defaultOpen />);
    const spoken = [...container.querySelectorAll(".round .sr-only")].map((n) => n.textContent);
    expect(spoken).toEqual(["Round 1", "Round 2"]);
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
            {
              name: "read_file",
              round: 1,
              args: "{}",
              result: "contents",
              failed: false,
              durationMs: 0,
              origin: "builtin",
              server: "",
            },
            {
              name: "submit_work",
              round: 2,
              args: "{}",
              result: "ok",
              failed: false,
              durationMs: 0,
              origin: "builtin",
              server: "",
            },
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

// A FAILED CALL SAYS SO IN WORDS. Its mark was a red glyph carrying no
// accessible name at all, so the row a reader most needs to find was announced
// exactly like the one above it, and the hue was the only signal anybody got.
describe("a failed tool call", () => {
  const withFailure = phase({
    roundsUsed: 1,
    narration: [{ round: 1, reasoning: "", content: "Trying." }],
    tools: [
      {
        name: "read_file",
        round: 1,
        args: "{}",
        result: "no such file",
        failed: true,
        durationMs: 0,
        origin: "builtin",
        server: "",
      },
      {
        name: "submit_work",
        round: 1,
        args: "{}",
        result: "ok",
        failed: false,
        durationMs: 0,
        origin: "builtin",
        server: "",
      },
    ],
  });

  test("is named as failed rather than only coloured as failed", () => {
    render(<PhaseCard record={withFailure} defaultOpen />);
    expect(screen.getByRole("button", { name: /read_file.*failed/i })).toBeDefined();
  });

  // THE CONTROL. Without it the rule could be "every tool row says failed"
  // and still pass, which would be the same defect the other way round.
  test("leaves a call that succeeded unmarked", () => {
    render(<PhaseCard record={withFailure} defaultOpen />);
    expect(screen.getByRole("button", { name: /^submit_work/ }).textContent).not.toContain(
      "failed",
    );
  });
});

// THE DECISION CHIP IS DRAWN IN THE STATE IT NAMES.
//
// It was `=== "self_iterate" ? warning : neutral`, keyed on ONE value, so
// `blocked` — the executor reporting it could not do the work — drew the
// ordinary grey pill, indistinguishable from `delivered` two rows up. The
// fixture leaves `failed`, `exhaustedRounds`, `emptyAnswerRounds` and
// `rescueFired` at their quiet defaults, so the decision chip is the only
// warning or danger pill the card can draw.
describe("the decision chip is drawn in the state it names", () => {
  test("an executor that could not do the work is not the ordinary pill", () => {
    const { container } = render(<PhaseCard record={phase({ decision: "blocked" })} />);
    expect(container.querySelector(".crewlet-tag--warning")).not.toBeNull();
    // COLOUR IS NEVER THE ONLY CARRIER: the sentence is beside it.
    expect(screen.getByText("blocked, and said why")).not.toBeNull();
  });

  // The one the card had no other way to say. A review record never sets the
  // phase's `failed` flag, so the danger tag in the header does not fire and
  // this chip was the whole of what a reader got.
  test("a review that ended the turn is drawn as the failure it is", () => {
    const { container } = render(
      <PhaseCard record={phase({ phase: "review", decision: "failed" })} />,
    );
    expect(container.querySelector(".crewlet-tag--danger")).not.toBeNull();
    expect(screen.getByText("failed — the turn will not retry")).not.toBeNull();
  });

  // AND AN UNEVENTFUL TURN KEEPS THE QUIET PILL, or four status hues are spent
  // on every row and therefore on none: a seat's feed is mostly made of these.
  test("an uneventful turn keeps the quiet pill", () => {
    const { container } = render(<PhaseCard record={phase({ decision: "no_action" })} />);
    expect(container.querySelector(".crewlet-tag--warning")).toBeNull();
    expect(container.querySelector(".crewlet-tag--danger")).toBeNull();
  });
});

// AND THE ROUND COUNT IS THE ENGINE'S OWN, whatever narrated.
//
// `roundNum` held the engine's ZERO-BASED `round_num` on a live record and
// `rounds_used` on a settled one — one name, two quantities — so a live phase
// whose rounds narrated nothing counted one short, and the opening frame's `-1`
// never matched the `=== 0` guard the dash above is for, which rendered "0r".
describe("the tool-round count", () => {
  test("a live phase counts the rounds the engine reported, not the ones that narrated", () => {
    const { container } = render(
      <PhaseCard record={phase({ live: true, roundsUsed: 3, narration: [], tools: [] })} />,
    );
    expect(container.querySelector('[title="tool rounds used"]')?.textContent).toBe("3r");
  });

  test("a live phase whose first round has not come back draws the marked absence", () => {
    const { container } = render(
      <PhaseCard record={phase({ live: true, roundsUsed: 0, narration: [], tools: [] })} />,
    );
    expect(
      container.querySelector('[title="this phase has not finished a round yet"]')?.textContent,
    ).toBe("—");
  });
});
