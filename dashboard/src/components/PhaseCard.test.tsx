/**
 * A round is ONE block, in the DOM as well as on screen.
 *
 * `rounds()` joins a phase's narration and its tool calls on the number they
 * share, and lib/phases.test.ts proves that join. What is asserted here is the
 * half a reader actually sees: that the join survives into the markup, so the
 * prose a round said and every call it asked for are inside one `.round`
 * element and nothing else is. The complaint that prompted these was "we are
 * not grouping the model's words with its tool call" — the data was grouped
 * and the rendering did not say so.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { PhaseCard } from "./PhaseCard.tsx";
import type { PhaseRecord } from "~/lib/phases.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function phase(over: Partial<PhaseRecord> = {}): PhaseRecord {
  return {
    key: "turn-1|execute|1",
    turnId: "turn-1",
    workKey: "wk-1",
    phase: "execute",
    iteration: 1,
    launch: false,
    role: "Support Engineer",
    agentId: "",
    model: "scripted",
    providerKey: "",
    live: false,
    failed: false,
    error: "",
    errorKind: "",
    systemPrompt: "",
    userPrompt: "",
    systemSections: null,
    userSections: null,
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
    codingAgent: "",
    sandboxId: "",
    launchId: "",
    cliTurns: 0,
    transcript: "",
    transcriptElidedLines: 0,
    transcriptElidedBytes: 0,
    deliveredRefs: [],
    deliveredRefsElided: 0,
    trigger: null,
    at: "2026-09-02T10:00:00Z",
    startedAt: "2026-09-02T10:00:00Z",
    durationMs: 0,
    eventId: "ev-1",
    stage: "",
    timedRounds: [],
    invocations: [],
    hostRound: 0,
    cacheReadTokens: 0,
    maxRounds: 0,
    roundStartedAt: "",
    runStartedAt: "",
    runningCalls: [],
    bodyWithheld: false,
    steers: [],
    node: "",
    clockStart: "",
    ...over,
  };
}

const TWO_ROUNDS = phase({
  roundsUsed: 2,
  narration: [
    { round: 1, content: "Reading the file.", declined: false },
    { round: 2, content: "Posted it.", declined: false },
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
      startedAt: "",
      callId: "",
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
      startedAt: "",
      callId: "",
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

  // AN ELLIPSIS MARKS WHERE THE ZERO REALLY IS AN ABSENCE: a phase still
  // running has not reported its usage yet, which is not the same as having
  // spent nothing.
  test("a live phase's zero is still an absence", () => {
    const { container } = render(
      <PhaseCard record={phase({ live: true, totalTokens: 0, roundsUsed: 0 })} />,
    );
    expect(container.querySelector('[title="total tokens"]')).toBeNull();
    expect(
      container.querySelector('[title="this phase has not reported its usage yet"]')?.textContent,
    ).toBe("…");
    expect(
      container.querySelector('[title="this phase has not finished a round yet"]')?.textContent,
    ).toBe("…");
  });
});

describe("a round is one block", () => {
  test("a round's speech and calls all sit inside that round", () => {
    const { container } = render(<PhaseCard record={TWO_ROUNDS} defaultOpen />);
    const blocks = [...container.querySelectorAll(".round")];
    expect(blocks).toHaveLength(2);

    expect(blocks[0]!.textContent).toContain("Reading the file.");
    expect(blocks[0]!.textContent).toContain("read_file");

    // …and none of round 2 leaked into it. This is the failure the complaint
    // describes from the other side: a call rendered away from the words that
    // asked for it is a call rendered NEXT TO words that did not.
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
    // together unannounced, so the words and their call were read out as two
    // unrelated rows.
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
          narration: [{ round: 2, content: "Done.", declined: false }],
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
              startedAt: "",
              callId: "",
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
              startedAt: "",
              callId: "",
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
    narration: [{ round: 1, content: "Trying.", declined: false }],
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
        startedAt: "",
        callId: "",
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
        startedAt: "",
        callId: "",
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
    expect(screen.getByText("failed, no retry")).not.toBeNull();
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
// never matched the `=== 0` guard the ellipsis above is for, which rendered "0r".
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
    ).toBe("…");
  });
});

// THE BODY READS IN THE ORDER THE PHASE HAPPENED: what it was given, then what
// it did, then what it delegated.
//
// The transcript used to come first and both of its inputs sat underneath it,
// so the question every round raises — what was this told, what was it allowed
// to call — was answered past the end of the answer, and a phase with forty
// rounds put a whole scroll between the two. Both inputs are closed folds, so
// the order costs a reader who only wants the transcript two header rows.
describe("the phase body reads in the order the phase happened", () => {
  const FULL = phase({
    ...TWO_ROUNDS,
    systemPrompt: "## Your turn\nDecide and act.",
    userPrompt: "## Task\npost the summary",
    toolsAvailable: ["submit_work"],
    toolCatalogue: ["mattermost"],
  });

  test("the prompt and the tool surface come before the rounds, and the workers after", () => {
    const { container } = render(
      <PhaseCard
        record={FULL}
        defaultOpen
        nested={[phase({ key: "turn-1|execute|1|w", worker: "researcher" })]}
      />,
    );
    // Document order, which is the reading order: every element of the card in
    // the order the markup puts them.
    const all = [...container.querySelectorAll("*")];
    const at = (el: Element | null) => (el === null ? -1 : all.indexOf(el));

    const prompt = at(screen.getByRole("button", { name: /^Prompt/ }));
    const tools = at(screen.getByRole("button", { name: /^Tool surface/ }));
    const ledger = at(container.querySelector(".round-ledger"));
    const delegated = at(screen.getByRole("button", { name: /^Delegated to/ }));

    // THE CONTROL: "everything is in order" is satisfied by a card that drew
    // none of them, which is the other way to lose a section.
    expect([prompt, tools, ledger, delegated].every((i) => i >= 0)).toBe(true);

    // The request first, and its two halves in the order they were sent: the
    // prompt, then the schema array that went WITH it.
    expect(prompt).toBeLessThan(tools);
    expect(tools).toBeLessThan(ledger);
    // Then what the rounds spawned.
    expect(ledger).toBeLessThan(delegated);
  });

  // THE FOLD SAYS HOW BIG THE REQUEST WAS, in the bytes the Context tab counts.
  // It said the phase's name, which the tag at the head of the card already
  // says — and opened, it reads by the builder's map where the record has one.
  test("the Prompt fold counts the request's size and reads it by the builder's map", () => {
    const system = "Lead.\n\n## Quoted\nfrom a thread";
    render(
      <PhaseCard
        record={phase({
          ...FULL,
          systemPrompt: system,
          systemSections: [{ key: "whole", title: "The whole turn", bytes: system.length }],
        })}
        defaultOpen
      />,
    );
    const fold = screen.getByRole("button", { name: /^Prompt/ });
    expect(fold.textContent).toContain(`${system.length} B system`);
    expect(fold.textContent).toContain("24 B user");
    expect(fold.textContent).not.toContain("execute phase");
    fireEvent.click(fold);
    const toc = screen.getByRole("listbox", { name: /Sections of/ });
    expect(within(toc).getAllByRole("option")[0]!.textContent).toMatch(/^The whole turn/);
  });

  // AND THE FAILURE STAYS AT THE TOP, above the inputs. It is a banner rather
  // than a section — the reason the reader opened the card at all — so it is
  // the one thing that does not wait its turn in the chronology.
  test("an error is still the first thing in the body", () => {
    const { container } = render(
      <PhaseCard
        record={phase({ ...FULL, failed: true, error: "no model answered" })}
        defaultOpen
      />,
    );
    const all = [...container.querySelectorAll("*")];
    const error = all.findIndex((el) => el.textContent === "no model answered");
    expect(error).toBeGreaterThanOrEqual(0);
    expect(error).toBeLessThan(all.indexOf(screen.getByRole("button", { name: /^Prompt/ })));
  });
});

/**
 * A CALL'S ARGUMENTS ARE A JSON DOCUMENT, so they are shown as one.
 *
 * The engine writes them with a plain `json.Marshal` — the whole call on one
 * line, however long it was — and every other JSON block in this product is
 * written at two spaces. What is asserted here is both halves of the rule the
 * transcript now keeps: that the block is indented, and that indenting it did
 * not re-encode it.
 */
describe("a tool call's arguments", () => {
  function withArgs(args: string): PhaseRecord {
    return phase({
      roundsUsed: 1,
      narration: [{ round: 1, content: "Working.", declined: false }],
      tools: [
        {
          name: "read_file",
          round: 1,
          args,
          result: "contents",
          failed: false,
          durationMs: 0,
          origin: "builtin",
          server: "",
          startedAt: "",
          callId: "",
        },
      ],
    });
  }

  function openedArgs(args: string): string {
    render(<PhaseCard record={withArgs(args)} defaultOpen />);
    // The row mounts its blocks only while open — a closed tool row must not
    // put its result into the round's text — so the click is the test's own
    // reader opening it.
    fireEvent.click(screen.getByRole("button", { name: /^read_file/ }));
    return screen.getByRole("region", { name: "read_file · arguments" }).textContent ?? "";
  }

  test("are indented, one key to a line", () => {
    expect(openedArgs('{"path":"a.go","limit":10}')).toContain(
      '{\n  "path": "a.go",\n  "limit": 10\n}',
    );
  });

  test("keep an id too wide for a double exactly as the engine wrote it", () => {
    // Decoded and re-encoded by the browser this reads 9007199254740992 — an
    // id off by one, on the screen whose job is to say which object a tool
    // was called on. lib/jsontext.ts is why it does not.
    expect(openedArgs('{"id":9007199254740993}')).toContain('"id": 9007199254740993');
  });

  test("show arguments that are not a JSON document at all, untouched", () => {
    expect(openedArgs("not json")).toContain("not json");
  });
});

// A LAUNCH IS NOT A MODEL CALL, and the card says what it is. It has no rounds
// because the engine drove none, so its response is the report the run wrote
// back — and the run's activity log, the whole account of what an agent with
// no telemetry did, has to be reachable from the card. It is the executor
// round's work, so the round's card draws it INSIDE: one card per round, its
// launches in it.
describe("an executor round's launches", () => {
  const LAUNCH = phase({
    key: "turn-1|execute|1|job-1",
    launch: true,
    codingAgent: "claude-code",
    launchId: "job-1",
    model: "claude-sonnet-5",
    response: "Fixed the flake and opened the pull request.",
    transcript: "[tool] bash: git clone\n[tool] bash: go test ./...",
    inputTokens: 9_000,
    outputTokens: 700,
    totalTokens: 9_700,
    cliTurns: 31,
    at: "2026-09-02T10:05:00Z",
  });

  test("the round's card holds its launch's report and activity", () => {
    render(
      <PhaseCard record={phase({ codingAgent: "claude-code" })} launches={[LAUNCH]} defaultOpen />,
    );
    expect(screen.getByText("Report")).toBeDefined();
    expect(screen.getByText(/Fixed the flake/)).toBeDefined();
    expect(screen.getByText("31 turns")).toBeDefined();
    const activity = screen.getByRole("button", { name: /^Activity/ });
    fireEvent.click(activity);
    expect(screen.getByText(/go test \.\/\.\.\./)).toBeDefined();
  });

  // THE ROUND'S FIGURE IS WHAT THE ROUND SPENT. The executor's record states
  // its round with no tokens — the engine made no model call for it — so a
  // card that printed the record's own figure read 0 over a run that spent
  // thousands, beside a turn total counting them.
  //
  // Mutation: print the record's own `totalTokens` in the header, and the
  // round reads 0.
  test("the round's card counts its launches' tokens as the round's", () => {
    const second = phase({
      ...LAUNCH,
      key: "turn-1|execute|1|job-2",
      launchId: "job-2",
      totalTokens: 200,
    });
    render(<PhaseCard record={phase()} launches={[LAUNCH, second]} />);
    expect(screen.getByTitle("total tokens, its run's launches included").textContent).toBe(
      "9,900",
    );
  });

  // A LAUNCH WHOSE ROUND HAS NO RECORD IN HAND is drawn on its own, and says
  // the same things: its report and its activity, under its round's phase.
  test("a launch drawn on its own shows its report and activity", () => {
    render(<PhaseCard record={LAUNCH} defaultOpen />);
    expect(screen.getByText("launch job-1")).toBeDefined();
    expect(screen.getByText(/Fixed the flake/)).toBeDefined();
    expect(screen.getByRole("button", { name: /^Activity/ })).toBeDefined();
  });

  // A LAUNCH THAT DID NOT FINISH SAYS HOW: one that failed by its error, and
  // one that stopped to ask, was stopped or was cut off by its notes — inside
  // the round, beside the launch it is about.
  test("a launch that did not finish says why inside its round", () => {
    const asked = phase({ ...LAUNCH, notes: "stopped to ask: which branch?" });
    const broke = phase({
      ...LAUNCH,
      key: "turn-1|execute|1|job-2",
      launchId: "job-2",
      failed: true,
      error: "the coding agent exited with status 1",
      errorKind: "launch_failed",
    });
    render(<PhaseCard record={phase()} launches={[asked, broke]} defaultOpen />);
    expect(screen.getByText("stopped to ask: which branch?")).toBeDefined();
    expect(screen.getByText("the coding agent exited with status 1")).toBeDefined();
  });

  // THE BADGE IS THE RUN'S OWN FACT: the card names the CLI wherever the
  // record names a run — its CLI, or the box it ran in — and badges nothing
  // that ran on the engine host. No field says where a phase ran.
  //
  // Mutation: badge only a launch, and the executor that collected a run
  // loses its badge; drop the box half, and a record naming only its box
  // does.
  test("badges the run a record names, and nothing that ran on the host", () => {
    render(<PhaseCard record={phase({ codingAgent: "claude-code", sandboxId: "box-7" })} />);
    expect(screen.getByText("claude-code")).toBeDefined();
    cleanup();
    render(<PhaseCard record={phase({ sandboxId: "box-7" })} />);
    expect(screen.getByText("sandbox")).toBeDefined();
    cleanup();
    render(<PhaseCard record={phase({ key: "turn-1|review|1", phase: "review" })} />);
    expect(screen.queryByText("sandbox")).toBeNull();
    expect(screen.queryByText("claude-code")).toBeNull();
  });

  // THE COUNT IS NOT THE KEPT PART PRESENTED AS THE WHOLE. A long run's
  // record keeps its transcript's start and end, so the lines on the card
  // are not the lines the run wrote — and the fold said "3 lines" of a run
  // that wrote thousands.
  //
  // Mutation: count `record.transcript` alone, as the card did, and this
  // reads "3 lines".
  test("counts what the record left out of a long run's activity", () => {
    render(
      <PhaseCard
        record={phase()}
        launches={[
          phase({
            ...LAUNCH,
            transcript:
              "[tool] Bash: go test ./...\n(1200 line(s), 380 KiB, left out here)\nAll green.",
            transcriptElidedLines: 1200,
            transcriptElidedBytes: 389_120,
          }),
        ]}
        defaultOpen
      />,
    );
    expect(screen.getByRole("button", { name: /^Activity.*2 of 1,202 lines/ })).toBeDefined();
  });
});

// "event →" IS A WAY OUT, and on the event's own page it is a way back to the
// same page. The guard spelled the event's address itself and kept the one
// from before the log moved under Live, so it never matched and the card drew
// a link to the page it was on.
describe("the link to the phase's own event", () => {
  const links = () =>
    screen.queryAllByRole("link", { name: /event/ }).map((a) => a.getAttribute("href"));

  test("is not drawn on that event's own page", () => {
    location.hash = "#/live/events/ev-1";
    render(
      <Router>
        <PhaseCard record={phase()} defaultOpen />
      </Router>,
    );
    expect(links()).toEqual([]);
  });

  test("is drawn everywhere else, at the event log's address", () => {
    location.hash = "#/live/turns/turn-1";
    render(
      <Router>
        <PhaseCard record={phase()} defaultOpen />
      </Router>,
    );
    expect(links()).toEqual(["#/live/events/ev-1"]);
  });
});

// A PARKED TURN'S CALL IS SILENT ON PURPOSE. The executor suspended into a
// detached coding run, and its round does not move until the run comes back —
// which can be hours, or days while a question waits on a person. The card
// used to read only its own `at`, so every legitimately silent run was drawn
// stalled.
describe("a live call's staleness", () => {
  const OLD = { live: true, at: "2020-01-01T00:00:00Z", startedAt: "2020-01-01T00:00:00Z" };

  test("a call that stopped moving on a running turn is called stalled", () => {
    render(<PhaseCard record={phase({ ...OLD, stage: "phase" })} />);
    expect(screen.getByText("no update in 10m")).toBeDefined();
  });

  test("a call on a turn parked on its coding run is not", () => {
    render(<PhaseCard record={phase({ ...OLD, stage: "parked" })} />);
    expect(screen.queryByText(/no update in/)).toBeNull();
    const tag = screen.getByText("parked on its coding run").closest(".crewlet-tag");
    // Drawn as the work in progress it is — never the stalled danger, nor the
    // amber that is kept for a seat that needs a person.
    expect(tag?.className).toContain("crewlet-tag--info");
  });
});

/**
 * A ROUND THAT ANSWERED IN PROSE SAYS WHAT BECAME OF IT.
 *
 * The screenshot that prompted it: an executor's second round was a fenced
 * ```json block — `{"summary":…,"outcome":"delivered",…}` — the `submit_work`
 * payload written as TEXT, followed by "never said what it did" and "rescued"
 * on the header and nothing connecting the two. Prose is not a call, and the
 * phase finishes only by one.
 */
describe("a round that answered in prose", () => {
  const call = (name: string, round: number) => ({
    name,
    round,
    args: "{}",
    result: "ok",
    failed: false,
    durationMs: 0,
    origin: "builtin",
    server: "",
    startedAt: "",
    callId: "",
  });
  const SUBMISSION = '```json\n{"outcome": "delivered"}\n```';

  test("before a later round, says the engine asked again", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 2,
          narration: [
            { round: 1, content: SUBMISSION, declined: true },
            { round: 2, content: "", declined: false },
          ],
          tools: [call("submit_work", 2)],
        })}
        defaultOpen
      />,
    );
    const [first, second] = [...container.querySelectorAll(".round")];
    expect(first!.querySelector(".round-note")?.textContent).toBe(
      "It answered in prose and called no tool. It didn’t submit, so it was asked again.",
    );
    // Neutral: the loop working is not a caution.
    expect(first!.querySelector(".round-note.caution")).toBeNull();
    // THE CONTROL: a round that called its tool carries no note.
    expect(second!.querySelector(".round-note")).toBeNull();
    expect(screen.getByText("1 answered in prose")).toBeDefined();
    // The node on the rail says it too, where a reader scanning for the round
    // that went wrong looks — the note alone, at caption size, was easy to miss.
    expect(first!.classList.contains("declined")).toBe(true);
    expect(second!.classList.contains("declined")).toBe(false);
  });

  test("as the last round of a settled phase, says the engine filled in the outcome", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 2,
          decision: "incomplete",
          rescueFired: true,
          narration: [
            { round: 1, content: "Commenting.", declined: false },
            { round: 2, content: SUBMISSION, declined: true },
          ],
          tools: [call("comment_on_work_item", 1)],
        })}
        defaultOpen
      />,
    );
    const note = container.querySelectorAll(".round")[1]!.querySelector(".round-note.caution");
    expect(note?.textContent?.trim()).toBe(
      "It answered in prose and called no tool. Last round, so the engine filled in the outcome.",
    );
  });

  test("as the newest round of a running phase, claims neither outcome", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          live: true,
          roundsUsed: 1,
          narration: [{ round: 1, content: SUBMISSION, declined: true }],
        })}
        defaultOpen
      />,
    );
    const note = container.querySelector(".round-note")?.textContent ?? "";
    expect(note).toBe("It answered in prose and called no tool. It didn’t submit.");
    expect(note).not.toMatch(/asked again|Last round/);
  });

  test("the rescue chip says the engine filled in the outcome, not that it was re-asked", () => {
    render(<PhaseCard record={phase({ rescueFired: true, decision: "incomplete" })} />);
    const title = screen.getByText("rescued").closest("[title]")?.getAttribute("title") ?? "";
    expect(title).toBe("No submission, so the engine filled in the outcome");
    expect(title).not.toMatch(/re-asked|asked again/);
  });
});

/**
 * A MODEL'S WORDS ARE THE MARKDOWN IT WROTE — its speech, a coding run's
 * report — read by a model's habits rather than a document's.
 */
describe("a model's words", () => {
  const SAID = [
    "## Summary",
    "Name: the fix",
    "Status: posted",
    "",
    "```json",
    '{"outcome": "delivered"}',
    "```",
  ].join("\n");

  test("a round's speech renders its fence as code and its lines as lines", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 1,
          narration: [{ round: 1, content: SAID, declined: false }],
        })}
        defaultOpen
      />,
    );
    const round = container.querySelector(".round")!;
    // The fence is a block of code holding the exact bytes, not three
    // backticks and a language tag in a paragraph.
    expect(round.querySelector("pre.md-code")?.textContent).toBe('{"outcome": "delivered"}');
    expect(round.textContent).not.toContain("```");
    // Each single newline the model wrote is a line of its own.
    expect(round.querySelectorAll("br").length).toBeGreaterThanOrEqual(1);
  });

  test("its headings are styled lines, never headings of the turn page", () => {
    // A transcript item is not a section of the page — the rule a tool row's
    // disclosure keeps by rendering no heading either.
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 1,
          narration: [{ round: 1, content: SAID, declined: false }],
        })}
        defaultOpen
      />,
    );
    const ledger = container.querySelector(".round-ledger")!;
    expect(ledger.querySelectorAll("h1, h2, h3, h4, h5, h6")).toHaveLength(0);
    expect(ledger.querySelector(".md-heading")?.textContent).toBe("Summary");
  });

  test("a coding run's report renders the same way", () => {
    render(
      <PhaseCard
        record={phase({
          key: "turn-1|execute|1|job-1",
          launch: true,
          launchId: "job-1",
          response: "Opened **the pull request**.",
        })}
        defaultOpen
      />,
    );
    expect(screen.getByText("the pull request").closest("strong")).not.toBeNull();
  });
});

/**
 * A RUNNING PHASE'S TRANSCRIPT FOLLOWS ITS NEWEST ROUND — and keeps following
 * through everything that changes the box rather than what is in it.
 *
 * jsdom lays nothing out and has no ResizeObserver, so the observer is a stub
 * this suite fires by hand and the box's geometry is defined on the element.
 * What is asserted is the wiring the layout depends on: WHICH elements are
 * observed, and where the box is left after an observation.
 */
describe("a running phase's transcript", () => {
  class Watching {
    static live: Watching[] = [];
    observed: Element[] = [];
    constructor(readonly report: ResizeObserverCallback) {
      Watching.live.push(this);
    }
    observe(el: Element) {
      this.observed.push(el);
    }
    unobserve() {}
    disconnect() {
      Watching.live = Watching.live.filter((w) => w !== this);
    }
  }
  const real = globalThis.ResizeObserver;
  beforeEach(() => {
    Watching.live = [];
    globalThis.ResizeObserver = Watching as unknown as typeof ResizeObserver;
  });
  afterEach(() => {
    globalThis.ResizeObserver = real;
  });

  /** Whatever is observing right now reports a change of size. */
  const resize = () => {
    for (const w of [...Watching.live]) w.report([], w as unknown as ResizeObserver);
  };

  /** A box holding 1000px of transcript in a 200px view, so its end is 800. */
  function geometry(box: HTMLElement): HTMLElement {
    let top = 0;
    Object.defineProperty(box, "scrollHeight", { configurable: true, get: () => 1000 });
    Object.defineProperty(box, "clientHeight", { configurable: true, get: () => 200 });
    Object.defineProperty(box, "scrollTop", {
      configurable: true,
      get: () => top,
      set: (v: number) => {
        top = Math.min(v, 800);
      },
    });
    return box;
  }

  const LIVE = phase({ ...TWO_ROUNDS, live: true });
  const box = (container: HTMLElement) => container.querySelector<HTMLElement>(".tail-scroll");

  test("is bounded only while it runs; a finished one flows", () => {
    const live = render(<PhaseCard record={LIVE} defaultOpen />);
    expect(box(live.container)?.classList.contains("tailing")).toBe(true);
    cleanup();
    const settled = render(<PhaseCard record={TWO_ROUNDS} defaultOpen />);
    // THE CONTROL: the box is there, with the same frame, and is not bounded.
    expect(box(settled.container)).not.toBeNull();
    expect(box(settled.container)?.classList.contains("tailing")).toBe(false);
  });

  test("observes the box itself, so a window that changes its height re-sticks the tail", () => {
    // The box's bound is the scroller's view, so a resized window resizes the
    // BOX while its content stays put — and a tail measured only on the
    // content came unstuck by the difference.
    const { container } = render(<PhaseCard record={LIVE} defaultOpen />);
    const scroller = box(container)!;
    const observed = Watching.live.flatMap((w) => w.observed);
    expect(observed).toContain(scroller);
    // The ledger the box holds — a live phase's runs, each heading its rounds.
    expect(observed).toContain(scroller.firstElementChild);
    geometry(scroller);
    resize();
    expect(scroller.scrollTop).toBe(800);
  });

  test("a reader who scrolled up is left where they are", () => {
    const { container } = render(<PhaseCard record={LIVE} defaultOpen />);
    const scroller = geometry(box(container)!);
    resize();
    scroller.scrollTop = 100;
    fireEvent.scroll(scroller);
    resize();
    expect(scroller.scrollTop).toBe(100);
  });

  test("and is following again once they close the card and open it", () => {
    // The flag outlived the element: the reopened card's NEW box inherited
    // "not following" from the one the reader had scrolled, and never stuck.
    const { container } = render(<PhaseCard record={LIVE} defaultOpen />);
    const first = geometry(box(container)!);
    resize();
    first.scrollTop = 100;
    fireEvent.scroll(first);
    const head = container.querySelector(".phase-head")!;
    fireEvent.click(head);
    expect(box(container)).toBeNull();
    fireEvent.click(head);
    const second = geometry(box(container)!);
    expect(second).not.toBe(first);
    resize();
    expect(second.scrollTop).toBe(800);
  });

  test("follows from its first round when it was opened before there was one", () => {
    // The rounds section exists only once a round does, so the box is mounted
    // AFTER the phase went live — and an effect keyed on liveness alone had
    // already run against nothing, and never attached to it.
    const opening = phase({ live: true, roundsUsed: 0 });
    const { container, rerender } = render(<PhaseCard record={opening} defaultOpen />);
    expect(box(container)).toBeNull();
    rerender(<PhaseCard record={LIVE} defaultOpen />);
    const scroller = box(container)!;
    expect(Watching.live.flatMap((w) => w.observed)).toContain(scroller);
    geometry(scroller);
    resize();
    expect(scroller.scrollTop).toBe(800);
  });
});

/**
 * A FAILED PHASE SAYS WHAT FAILED, in the engine's own words: its error kind on
 * the head a reader scans down a turn, and its error in the body. The CONTROL
 * is the same phase that did not fail, which says neither.
 */
describe("a failed phase", () => {
  test("names its error kind on its head and its error in its body", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          failed: true,
          errorKind: "rate_limit",
          error: "cli-agent/claude-code: sitting out until 14:05",
        })}
        defaultOpen
      />,
    );
    expect(container.querySelector(".phase-head")?.textContent).toContain("Rate limited");
    expect(container.querySelector(".phase-body")?.textContent).toContain(
      "cli-agent/claude-code: sitting out until 14:05",
    );
    cleanup();
    const { container: whole } = render(<PhaseCard record={phase()} defaultOpen />);
    expect(whole.textContent).not.toContain("rate_limit");
    expect(whole.textContent).not.toContain("failed");
  });
});

// A WITHHELD BODY IS SAID, never drawn as a phase that was told nothing and
// answered nothing: the engine sent this reader its status alone. The CONTROL
// is the same phase sent whole, which says nothing of the kind. Mutation:
// drop the notice and the first assertion fails.
describe("a phase whose body was withheld", () => {
  function withStore(node: React.ReactNode, seed?: (store: Store) => void) {
    const store = new Store();
    seed?.(store);
    render(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>{node}</Router>
      </ClientContext.Provider>,
    );
  }

  test("says it is hidden, and who can see it", () => {
    withStore(<PhaseCard record={phase({ bodyWithheld: true })} defaultOpen />);
    expect(screen.getByText("Hidden. Only this seat’s lead and admins can see it.")).toBeDefined();
    cleanup();
    withStore(<PhaseCard record={phase()} defaultOpen />);
    expect(screen.queryByText(/^Hidden\./)).toBeNull();
  });

  // THE LEAD IS NAMED, with a link, so the reader knows whom to ask. Found by
  // the phase's agent id, never its role name. Mutation: drop the lookup and
  // the general sentence is drawn instead.
  test("names the seat's lead, linked, where the chart has one", () => {
    withStore(
      <PhaseCard record={phase({ bodyWithheld: true, agentId: "a-dev" })} defaultOpen />,
      (store) => {
        // THE REPORTING LINE IS THE SUBJECT, so its derived block is written
        // out (`test/org.ts`): Jane leads Dev.
        const seat = (handle: string, name: string, manager: string) => ({
          handle,
          name,
          kind: "agent",
          placed_by_ref: false,
          manager,
          managers: manager ? [manager] : null,
          reports: null,
          auto_reports: null,
          onboarding_chain: null,
        });
        store.applyOrg({
          roles: [
            { name: "Jane Founder", handle: "jane" },
            { name: "Dev", handle: "dev" },
          ],
          derived: {
            seats: [seat("jane", "Jane Founder", ""), seat("dev", "Dev", "jane")],
            units: [],
          },
        } as never);
        store.applySeats([{ agent_id: "a-dev", role: "Dev", handle: "dev" }] as never);
      },
    );
    const lead = screen.getByRole("link", { name: "Jane Founder" });
    expect(lead.getAttribute("href")).toContain("jane");
    expect(lead.closest(".crewlet-callout")?.textContent).toBe(
      "Only Jane Founder (this seat’s lead) and admins can see what it says.",
    );
  });
});

/**
 * A CALL THAT LANDED A VERSION LINKS THE ARTIFACT IT WROTE, under the call and
 * outside its fold, so a reader reaches the artifact without opening the
 * result and copying an id out of it. A call that failed links nothing.
 */
describe("a publishing call", () => {
  const ID = "32f4d7b8-57d4-5ab7-a82f-413394e1825b";
  function publishing(failed: boolean): PhaseRecord {
    return phase({
      roundsUsed: 1,
      narration: [{ round: 1, content: "Publishing.", declined: false }],
      tools: [
        {
          name: "create_artifact",
          round: 1,
          args: JSON.stringify({ kind: "sheet", title: "Vendor review" }),
          result: failed
            ? "create_artifact needs a `title`: two to four words."
            : JSON.stringify({ outcome: "created", artifact: ID, version: 1, write: "applied" }),
          failed,
          durationMs: 0,
          origin: "builtin",
          server: "",
          startedAt: "",
          callId: "",
        },
      ],
    });
  }

  test("links the artifact it wrote", () => {
    render(<PhaseCard record={publishing(false)} defaultOpen />);
    const link = screen.getByRole("link", { name: "Open “Vendor review”, version 1" });
    expect(link.getAttribute("href")).toBe(`#/artifacts/${ID}`);
  });

  test("links nothing when it failed", () => {
    render(<PhaseCard record={publishing(true)} defaultOpen />);
    expect(screen.queryByRole("link", { name: /^Open/ })).toBeNull();
  });
});

/**
 * EVERY CALL IN FLIGHT IS IN ITS ROUND, named running, after the calls of that
 * round that have answered. A CLI runs a response's calls side by side, so a
 * round can have several out at once; a card that drew none of them showed a
 * model's words and nothing after them for as long as the calls took, which
 * reads as a seat that stalled. And a call that answers is the SAME row, open
 * if the reader opened it — keyed by the call's own id, which its running entry
 * and its answered row share.
 *
 * Mutation: drop the running calls from the ledger, give a running call a
 * component of its own, or key the rows by position, and this fails.
 */
describe("the calls a running phase is making", () => {
  const answered = (name: string, callId: string, result: string) => ({
    name,
    round: 1,
    args: "{}",
    result,
    failed: false,
    durationMs: 0,
    origin: "builtin",
    server: "",
    startedAt: "",
    callId,
  });
  const running = (name: string, id: string, args: string) => ({
    id,
    round: 1,
    name,
    arguments: args,
    startedAt: "2026-09-02T10:00:00Z",
  });
  const live = (over: Partial<PhaseRecord>) =>
    phase({
      live: true,
      roundsUsed: 1,
      narration: [{ round: 1, content: "Reading all three.", declined: false }],
      ...over,
    });

  test("are drawn in their round, each named running, after the calls that answered", () => {
    const { container } = render(
      <PhaseCard
        record={live({
          tools: [answered("read_file", "c1", "contents a")],
          runningCalls: [
            running("read_file", "c2", '{"path":"b.go"}'),
            running("run_tests", "c3", '{"pkg":"./..."}'),
          ],
        })}
        defaultOpen
      />,
    );
    const round = container.querySelector(".round") as HTMLElement;
    const rows = within(round).getAllByRole("button");
    expect(rows.map((b) => b.textContent)).toEqual([
      expect.not.stringMatching(/running/),
      expect.stringMatching(/^read_file.*running/),
      expect.stringMatching(/^run_tests.*running/),
    ]);
    expect(round.querySelectorAll(".tool-row.running")).toHaveLength(2);
  });

  test("a call that answers keeps its row, open where the reader opened it", () => {
    const before = live({
      tools: [answered("read_file", "c1", "contents a")],
      runningCalls: [running("read_file", "c2", '{"path":"b.go"}')],
    });
    const { container, rerender } = render(<PhaseCard record={before} defaultOpen />);
    const row = () => container.querySelectorAll(".tool-row")[1] as HTMLElement;
    const held = row();
    fireEvent.click(within(held).getByRole("button"));
    expect(within(held).getByText(/Still running/)).toBeDefined();
    rerender(
      <PhaseCard
        record={live({
          tools: [
            answered("read_file", "c1", "contents a"),
            answered("read_file", "c2", "the text of b"),
          ],
          runningCalls: [],
        })}
        defaultOpen
      />,
    );
    expect(row()).toBe(held);
    expect(held.classList.contains("running")).toBe(false);
    expect(within(held).getByText(/the text of b/)).toBeDefined();
  });
});

/**
 * A PHASE'S CLI RUNS HEAD THE ROUNDS EACH MADE: which entry served it, the
 * rounds it made, how it ended — filed by the round span the run names. A run
 * whose CLI showed no requests says its one round is the whole run; a run that
 * made none says so; and on a live phase the rounds past every ended run are
 * the run in flight's.
 *
 * Mutation: file every round under the first run, drop a run that made no
 * request, or stop heading a live phase's rounds, and this fails.
 */
describe("a phase's CLI runs", () => {
  const narrated = (n: number) =>
    Array.from({ length: n }, (_, i) => ({
      round: i + 1,
      content: `Round ${i + 1} said this.`,
      declined: false,
    }));
  const run = (over: Partial<PhaseRecord["invocations"][number]>) => ({
    run: 1,
    startedAt: "2026-09-02T10:00:00Z",
    durationMs: 4_000,
    providerKey: "claude-max",
    model: "scripted",
    outcome: "end",
    firstRound: 1,
    lastRound: 1,
    requestsObserved: true,
    inputTokens: 1_000,
    outputTokens: 100,
    cacheReadTokens: 0,
    toolCalls: 0,
    ...over,
  });
  const roundsIn = (el: Element) =>
    [...el.querySelectorAll(".round .round-node")].map((n) => n.textContent);

  test("each run heads the rounds it names, and says how it ended", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 4,
          narration: narrated(4),
          invocations: [
            run({ run: 1, firstRound: 1, lastRound: 2, outcome: "stopped" }),
            run({ run: 2, firstRound: 0, lastRound: 0, outcome: "failed", providerKey: "codex" }),
            run({ run: 3, firstRound: 3, lastRound: 3, requestsObserved: false }),
            run({ run: 4, firstRound: 4, lastRound: 4, outcome: "cap" }),
          ],
        })}
        defaultOpen
      />,
    );
    const runs = [...container.querySelectorAll(".run")];
    expect(runs.map(roundsIn)).toEqual([["1", "2"], [], ["3"], ["4"]]);
    const heads = runs.map((r) => r.querySelector(".run-head")?.textContent ?? "");
    expect(heads[0]).toMatch(/CLI run 1.*claude-max.*rounds 1–2.*the engine ended it/);
    expect(heads[1]).toMatch(/CLI run 2.*codex.*no request.*failed/);
    expect(heads[3]).toMatch(/CLI run 4.*round 4.*turn cap/);
    expect(runs[2]!.textContent).toContain("the whole run is one round");
    // THE CONTROL: a run whose CLI showed its requests says nothing of it.
    expect(runs[0]!.textContent).not.toContain("the whole run is one round");
  });

  test("a live phase's rounds past every ended run are the run in flight's", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          live: true,
          roundsUsed: 3,
          narration: narrated(3),
          invocations: [run({ run: 1, firstRound: 1, lastRound: 2 })],
        })}
        defaultOpen
      />,
    );
    const runs = [...container.querySelectorAll(".run")];
    expect(runs.map(roundsIn)).toEqual([["1", "2"], ["3"]]);
    expect(runs[1]!.querySelector(".run-head")?.textContent).toMatch(/CLI run 2.*running/);
  });

  test("a live phase whose first run has not ended is headed as that run", () => {
    const { container } = render(
      <PhaseCard
        record={phase({ live: true, roundsUsed: 1, narration: narrated(1) })}
        defaultOpen
      />,
    );
    expect(container.querySelector(".run-head")?.textContent).toMatch(/CLI run 1.*running/);
  });

  // THE CONTROL: a settled phase that recorded no runs draws its rounds as
  // ever, with no head claiming a run nothing recorded.
  test("a settled phase with no runs on its record draws no head", () => {
    const { container } = render(<PhaseCard record={TWO_ROUNDS} defaultOpen />);
    expect(container.querySelector(".run-head")).toBeNull();
    expect(container.querySelectorAll(".round")).toHaveLength(2);
  });
});

/**
 * A ROUND THE CLI'S OWN TURN CAP ENDED SAYS SO under its words — the one stop
 * reason that ends a round unfinished (`contract/stops.ts`). The CONTROL is a
 * round that ended because its model was done, which says nothing.
 *
 * Mutation: drop the note, or draw it on every round, and this fails.
 */
describe("a round the CLI's turn cap ended", () => {
  test("says the cap ended its run with the model still working", () => {
    const timed = (round: number, stopReason: string) => ({
      round,
      startedAt: "2026-09-02T10:00:00Z",
      durationMs: 1_000,
      model: "scripted",
      inputTokens: 0,
      outputTokens: 0,
      cacheReadTokens: 0,
      toolCalls: 0,
      stopReason,
    });
    const { container } = render(
      <PhaseCard
        record={phase({
          ...TWO_ROUNDS,
          timedRounds: [timed(1, "end"), timed(2, "cap")],
        })}
        defaultOpen
      />,
    );
    const [first, second] = [...container.querySelectorAll(".round")];
    expect(first!.querySelector(".round-note")).toBeNull();
    expect(second!.querySelector(".round-note.caution")?.textContent).toMatch(
      /CLI’s own turn cap ended its run here, with the model still working/,
    );
  });
});
