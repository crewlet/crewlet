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

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { PhaseCard } from "./PhaseCard.tsx";
import type { PhaseRecord } from "~/lib/phases.ts";
import { Router } from "~/app/router.tsx";

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
    backend: "",
    codingAgent: "",
    sandboxId: "",
    launchId: "",
    transcript: "",
    deliveredRefs: [],
    trigger: null,
    at: "2026-09-02T10:00:00Z",
    startedAt: "2026-09-02T10:00:00Z",
    durationMs: 0,
    eventId: "ev-1",
    stage: "",
    timedRounds: [],
    hostRound: 0,
    cacheReadTokens: 0,
    maxRounds: 0,
    roundStartedAt: "",
    runningCall: null,
    steers: [],
    node: "",
    clockStart: "",
    ...over,
  };
}

const TWO_ROUNDS = phase({
  roundsUsed: 2,
  narration: [
    { round: 1, reasoning: "the file first", content: "Reading the file.", declined: false },
    { round: 2, reasoning: "that is enough", content: "Posted it.", declined: false },
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
          narration: [{ round: 2, reasoning: "", content: "Done.", declined: false }],
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
    narration: [{ round: 1, reasoning: "", content: "Trying.", declined: false }],
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
      narration: [{ round: 1, reasoning: "", content: "Working.", declined: false }],
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
    return screen.getByRole("region", { name: "read_file — arguments" }).textContent ?? "";
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

// A CODING RUN IS NOT A MODEL CALL, and its card says what it is. It has no
// rounds because the engine drove none, so the fallback that labels a joined
// response "recorded before rounds were kept apart" would misname the report
// the run wrote back — and the run's activity log, the whole account of what
// an agent with no telemetry did, has to be reachable from the card.
describe("a coding run's card", () => {
  const RUN = phase({
    key: "turn-1|sandbox|1|job-1",
    phase: "sandbox",
    backend: "sandbox",
    codingAgent: "claude-code",
    launchId: "job-1",
    response: "Fixed the flake and opened the pull request.",
    transcript: "[tool] bash: git clone\n[tool] bash: go test ./...",
  });

  test("shows the report and the activity, never the legacy transcript", () => {
    render(<PhaseCard record={RUN} defaultOpen />);
    expect(screen.getByText("Report")).toBeDefined();
    expect(screen.getByText(/Fixed the flake/)).toBeDefined();
    expect(screen.queryByText(/recorded before rounds were kept apart/)).toBeNull();
    const activity = screen.getByRole("button", { name: /^Activity/ });
    fireEvent.click(activity);
    expect(screen.getByText(/go test \.\/\.\.\./)).toBeDefined();
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
  });
  const SUBMISSION = '```json\n{"outcome": "delivered"}\n```';

  test("before a later round, says the engine asked again", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 2,
          narration: [
            { round: 1, reasoning: "", content: SUBMISSION, declined: true },
            { round: 2, reasoning: "", content: "", declined: false },
          ],
          tools: [call("submit_work", 2)],
        })}
        defaultOpen
      />,
    );
    const [first, second] = [...container.querySelectorAll(".round")];
    expect(first!.querySelector(".round-note")?.textContent).toMatch(/asked it again/);
    // Neutral: the loop working is not a caution.
    expect(first!.querySelector(".round-note.caution")).toBeNull();
    // THE CONTROL: a round that called its tool carries no note.
    expect(second!.querySelector(".round-note")).toBeNull();
    expect(screen.getByText("1 answered in prose")).toBeDefined();
  });

  test("as the last round of a settled phase, says the phase ended without its submission", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          roundsUsed: 2,
          decision: "incomplete",
          rescueFired: true,
          narration: [
            { round: 1, reasoning: "", content: "Commenting.", declined: false },
            { round: 2, reasoning: "", content: SUBMISSION, declined: true },
          ],
          tools: [call("comment_on_work_item", 1)],
        })}
        defaultOpen
      />,
    );
    const note = container.querySelectorAll(".round")[1]!.querySelector(".round-note.caution");
    expect(note?.textContent).toMatch(/last round/);
    expect(note?.textContent).toMatch(/without its submission/);
    expect(note?.textContent).toMatch(/rescued/);
  });

  test("as the newest round of a running phase, claims neither outcome", () => {
    const { container } = render(
      <PhaseCard
        record={phase({
          live: true,
          roundsUsed: 1,
          narration: [{ round: 1, reasoning: "", content: SUBMISSION, declined: true }],
        })}
        defaultOpen
      />,
    );
    const note = container.querySelector(".round-note")?.textContent ?? "";
    expect(note).toMatch(/finishes only by calling a tool/);
    expect(note).not.toMatch(/asked it again|last round/);
  });

  test("the rescue chip says the phase ended without its submission, not that it was re-asked", () => {
    render(<PhaseCard record={phase({ rescueFired: true, decision: "incomplete" })} />);
    const title = screen.getByText("rescued").closest("[title]")?.getAttribute("title") ?? "";
    expect(title).toMatch(/ended without its submission/);
    expect(title).not.toMatch(/re-asked/);
  });
});

/**
 * A MODEL'S WORDS ARE THE MARKDOWN IT WROTE — its speech, its thinking, a
 * coding run's report — read by a model's habits rather than a document's.
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
          narration: [{ round: 1, reasoning: "", content: SAID, declined: false }],
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
          narration: [{ round: 1, reasoning: "## Weighing it", content: SAID, declined: false }],
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
          key: "turn-1|sandbox|1|job-1",
          phase: "sandbox",
          backend: "sandbox",
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
    expect(observed).toContain(scroller.querySelector(".round-ledger"));
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
