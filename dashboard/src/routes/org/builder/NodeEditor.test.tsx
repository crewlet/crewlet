/**
 * The node editor.
 *
 * What these protect: Apply records the form as ONE operation and a refusal
 * keeps the form open with its reason; closing a changed form asks first,
 * whichever field changed, and one Escape closes only the question; a node's
 * address is edited as the chart's rename; a sealed address is never shown or
 * sent as though it were one, and an untouched one is sent back as read; a
 * field for a tool is drawn only where the company connected the tool, with
 * the consequences a GitHub tier and a Mattermost username carry said beside
 * them; a reader not shown the runtime half is told so rather than handed
 * empty fields; the check's findings sit beside the fields they name; and no
 * credential, masked or referenced, reaches the page.
 */

import { act, cleanup, fireEvent, screen, within } from "~/test/inCase.ts";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { ChartRead, ChartSeat, CompanyDocument } from "~/protocol/index.ts";
import { REDACTED } from "~/lib/format.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { locate, type SeatData } from "./model/draft.ts";
import type { BuilderState } from "./model/reducer.ts";
import {
  chartOf,
  fixtureChart,
  fixtureSettings,
  type DerivedOverrides,
  strippedChart,
} from "./model/testkit.ts";
import type { PlacedProblem } from "./model/problems.ts";
import { ACKNOWLEDGEMENT_TEXT } from "./dialogParts.tsx";
import type { EditorSectionName } from "./BuilderContext.tsx";
import { NodeEditor } from "./NodeEditor.tsx";
import { ANNOUNCER_LABEL } from "~/app/announcer.tsx";
import { renderInBuilder, waitInCase, type HarnessOptions } from "./viewTestkit.tsx";
import { checkedEdit, checkWith, findingOn, record } from "./testState.ts";
import { Callout } from "@crewlethq/ui";
import { drawnClasses, isDrawnAs } from "~/testing.tsx";

afterEach(cleanup);

function edit(
  state: BuilderState,
  key: NodeKey,
  options: HarnessOptions = {},
  section?: EditorSectionName,
) {
  const onClose = vi.fn();
  const view = renderInBuilder(
    state,
    <NodeEditor nodeKey={key} section={section} onClose={onClose} />,
    options,
  );
  return { ...view, onClose };
}

/** A label's accessible name, whether or not it says "(optional)" after the text. */
const labelled = (label: string) =>
  new RegExp(`^${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}( \\(optional\\))?$`);
const field = (label: string) => screen.getByLabelText(labelled(label));
const type = (label: string, value: string) =>
  fireEvent.change(field(label), { target: { value } });
/** Pick an option, which is how every choice on this form is made. */
const choose = (label: string, option: string | RegExp) => {
  fireEvent.click(field(label));
  fireEvent.mouseDown(screen.getByRole("option", { name: option }));
};
/**
 * The alert a control's own description points at, or null.
 *
 * The contract rather than the markup: a field's refusal is the one a screen
 * reader reads after its name, so what is asserted is that the control points
 * at it, not which element happens to hold it.
 */
function errorOf(control: HTMLElement): HTMLElement | null {
  for (const id of (control.getAttribute("aria-describedby") ?? "").split(" ")) {
    const node = id ? document.getElementById(id) : null;
    if (node?.getAttribute("role") === "alert") return node;
  }
  return null;
}
const apply = () => fireEvent.click(screen.getByRole("button", { name: "Apply" }));

/**
 * Whether Apply refuses a press, which is `aria-disabled` rather than the
 * native attribute: a natively disabled button takes no focus and no hover,
 * so the reason it cannot be pressed would be unreachable by exactly the
 * reader who needs it. The design system's Button soft-disables and links the
 * reason with `aria-describedby`.
 */
const applyRefuses = (): boolean =>
  screen.getByRole("button", { name: "Apply" }).getAttribute("aria-disabled") === "true";

/**
 * The reason DRAWN in the sheet's foot, as opposed to the copy Apply points
 * at with `aria-describedby`, which is out of view so that a reader who tabs
 * straight to the control hears it there rather than only at the other end of
 * the band. A query by text alone finds both, so the drawn one is the one the
 * button does not name.
 */
const shownReason = (text: string): HTMLElement => {
  const announced = new Set(
    (screen.getByRole("button", { name: "Apply" }).getAttribute("aria-describedby") ?? "").split(
      " ",
    ),
  );
  return screen.getAllByText(text).find((el) => !announced.has(el.id))!;
};
const seatData = (state: BuilderState, key: NodeKey): SeatData => {
  const found = locate(state.draft, key);
  if (found?.kind !== "seat") throw new Error(`no seat ${key}`);
  return found.node.data;
};

/** The fixture chart with Dev's seat as `dev` states it, in Engineering. */
function withDev(dev: Omit<ChartSeat, "handle" | "unit">, chart = fixtureChart()): ChartRead {
  chart.seats = chart.seats.map((s) =>
    s.handle === "dev" ? { handle: "dev", unit: "engineering", ...dev } : s,
  );
  return chart;
}

/** A state on the fixture chart, checked clean. */
const editing = (chart: ChartRead = fixtureChart(), settings?: CompanyDocument) =>
  checkedEdit(chart, {}, settings);

/** The fixture company with every tool a seat's fields depend on connected. */
function connected(): BuilderState {
  const settings = fixtureSettings();
  settings.providers = { llm: { fast: {}, smart: {} }, llm_order: ["smart", "fast"] };
  settings.integrations = {
    ...settings.integrations,
    github: { enabled: true, webhook_secret: REDACTED },
    slack: {},
    mattermost: { enabled: true, url: "https://chat.example.com", team: "acme" },
  };
  const chart = withDev({
    name: "Dev",
    goal: "Build",
    runtime: {
      mcp_env: { tracker: { TOKEN: REDACTED, URL: "${TRACKER_URL}" } },
      sandbox: { enabled: true, run_in: "e2b", env: { KEY: REDACTED } },
      github: {
        tier: "review",
        app_slug: "acme-dev",
        private_key: "${DEV_GITHUB_KEY}",
        webhook_secret: REDACTED,
      },
      slack: {
        bot_token: "Bearer sk-live-${SUFFIX}",
        signing_secret: "${DEV_SLACK_SECRET}",
        channel: "C1",
      },
      mattermost: { bot_token: "${DEV_MM_TOKEN}", channel: "eng", username: "dev-bot" },
    },
  });
  return editing(chart, settings);
}

/** Dev as a human seat holding a GitHub login. */
const humanDev = () =>
  withDev({ name: "Dev", kind: "human", runtime: { contact: { github_login: "dev" } } });

describe("applying", () => {
  test("Apply records every changed field as one operation and closes the editor", () => {
    const view = edit(editing(), "seat:dev");
    type("Name", "Developer");
    type("Goal", "Ship");
    apply();
    const state = view.state();
    expect(state.log.ops).toHaveLength(1);
    expect(state.log.ops[0]).toMatchObject({ type: "edit", target: "seat:dev" });
    expect(seatData(state, "seat:dev")).toMatchObject({ name: "Developer", goal: "Ship" });
    expect(view.onClose).toHaveBeenCalledTimes(1);
  });

  test("an untouched form records nothing and closes without asking", () => {
    const view = edit(editing(), "seat:dev");
    apply();
    expect(view.state().log.ops).toHaveLength(0);
    expect(view.onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
    expect(view.onClose).toHaveBeenCalledTimes(2);
  });

  test("a refused edit stays open with the reducer's reason, and nothing is recorded", () => {
    // A handle another seat holds: the chart names one seat by it, so the
    // rename is refused rather than recorded.
    const view = edit(editing(), "seat:dev");
    type("Handle", "ceo");
    apply();
    expect(view.onClose).not.toHaveBeenCalled();
    expect(
      screen.getByText("The handle ceo already names a seat in this draft (CEO)."),
    ).toBeDefined();
    expect(view.state().log.ops).toHaveLength(0);
  });

  test("a read-only builder applies nothing", () => {
    const view = edit(editing(), "seat:dev", { readOnly: true });
    expect(applyRefuses()).toBe(true);
    expect(screen.getByText(/cannot be changed right now/)).toBeDefined();
    view.unmount();
  });

  // THE BANNER SAYS THESE FIELDS ARE FOR READING, so every one of them is: a
  // box that still takes typing makes the form dirty, asks whether to discard
  // work that was never going anywhere, and reads as a save the builder lost.
  test("every field of a read-only editor is for reading, the charter's and a unit's type included", () => {
    const readOnly = { readOnly: true };
    const enabled = () =>
      ["textbox", "combobox", "checkbox"]
        .flatMap((role) => screen.queryAllByRole(role))
        .filter((el) => !(el as HTMLInputElement).disabled)
        .map((el) => el.getAttribute("aria-label") ?? el.id);

    edit(editing(), COMPANY_KEY, readOnly);
    expect(field("Company name")).toBeDefined();
    expect(enabled()).toEqual([]);
    cleanup();

    edit(editing(), "unit:engineering", readOnly);
    expect(field("Type")).toBeDefined();
    expect(enabled()).toEqual([]);
    cleanup();

    edit(connected(), "seat:dev", readOnly);
    expect(enabled()).toEqual([]);
  });
});

/*
 * THE PANEL'S OWN HEAD, which is what the console commits its edit panel from
 * and what this sheet did not have: a 49px band with a close control alone,
 * and the commit pair in a foot past 970px of scroll on a seat with a dozen
 * fields.
 */
describe("the head", () => {
  /** The panel's mark: the first drawing in it, which is the head's glyph. */
  const mark = (): string => document.querySelector("svg")!.outerHTML;

  test("Cancel and Apply are in the head, before every field, and there is no second way out", () => {
    edit(editing(), "seat:dev");
    const cancel = screen.getByRole("button", { name: "Cancel" });
    const applyButton = screen.getByRole("button", { name: "Apply" });
    const firstField = field("Name");
    for (const control of [cancel, applyButton]) {
      expect(
        control.compareDocumentPosition(firstField) & Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
    }
    // Cancel IS the way out. A close control beside the title would be a
    // second, unnamed spelling of it.
    expect(screen.queryByRole("button", { name: "Close" })).toBeNull();
  });

  /*
   * THE NODE'S OWN MARK, not a pencil on all four. The head used to draw an
   * edit glyph for the company, a unit, an agent seat and a human seat alike,
   * so the panel said that it edits rather than what it is editing.
   */
  test("each kind of node is marked as its own kind", () => {
    const cases: [ChartRead, NodeKey][] = [
      [fixtureChart(), COMPANY_KEY],
      [fixtureChart(), "unit:engineering"],
      [fixtureChart(), "seat:dev"],
      [humanDev(), "seat:dev"],
    ];
    const drawn: string[] = [];
    for (const [chart, key] of cases) {
      edit(editing(chart), key);
      drawn.push(mark());
      cleanup();
    }
    expect(new Set(drawn).size).toBe(4);
  });
});

/*
 * NO NODE HAS A COLOUR TO STATE. Colour on this dashboard says what a seat is
 * DOING, never who it is, so the chart draws every node on its own neutral
 * surface and there is no hue for an editor to explain. It used to end an
 * agent seat's form with a read-only "Colour" fact naming a hue hashed from
 * the seat's key — a legend for a decoration the live chart never drew.
 */
test("no node's editor states or offers a colour", () => {
  for (const [state, key] of [
    [editing(), "seat:dev"],
    [editing(humanDev()), "seat:dev"],
    [editing(), "unit:engineering"],
  ] as const) {
    edit(state, key);
    expect(screen.queryByText("Colour"), key).toBeNull();
    expect(screen.queryByText(/\bhue\b/i), key).toBeNull();
    expect(screen.queryByRole("radio", { name: /purple|cyan|green|amber|rose|blue/i })).toBeNull();
    cleanup();
  }
});

describe("the unsaved-changes prompt", () => {
  const changes: [string, () => void][] = [
    ["a text field", () => type("Goal", "Ship")],
    [
      "a list",
      () => {
        type("New responsibility", "Review pull requests");
        fireEvent.keyDown(field("New responsibility"), { key: "Enter" });
      },
    ],
    [
      "the manages picker",
      () => {
        fireEvent.click(screen.getByRole("combobox", { name: /^Manages/ }));
        fireEvent.mouseDown(screen.getByRole("option", { name: /SRE/ }));
      },
    ],
    ["a choice", () => choose("Access level", "Maintainer")],
    [
      "a model chain",
      () => {
        fireEvent.click(screen.getByRole("combobox", { name: /^Model/ }));
        fireEvent.mouseDown(screen.getByRole("option", { name: /smart/ }));
      },
    ],
  ];

  test.each(changes)("asks before discarding a change to %s", (_, change) => {
    const view = edit(connected(), "seat:dev");
    change();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    const prompt = screen.getByRole("alertdialog", { name: "Discard your changes?" });
    expect(view.onClose).not.toHaveBeenCalled();
    fireEvent.click(within(prompt).getByRole("button", { name: "Discard changes" }));
    expect(view.onClose).toHaveBeenCalledTimes(1);
    expect(view.state().log.ops).toHaveLength(0);
  });

  /**
   * Makes a move with `go` — a link pressed, a hash written, Back — and waits
   * until the browser has landed on `landsOn`, then renders what the router
   * made of it.
   *
   * ON THE BROWSER'S OWN EVENTS, never a poll of `location.hash`: the hash is
   * written at once, before the router has heard of the move, so a poll for
   * it passed before anything the case meant to wait for — and an assertion
   * that nothing asked was read before there was anything to ask. jsdom
   * dispatches `hashchange` and `popstate` as tasks of their own, and a held
   * move is undone with a second traversal, so the landing is the first of
   * those events to find the browser on `landsOn`. Waited for outside `act`
   * and ended with the case ([waitInCase]), because the guard that undoes a
   * held move runs in the router's listener and its question renders from
   * it; then `act` renders that question.
   */
  async function move(go: () => void, landsOn: string): Promise<void> {
    await waitInCase<void>(
      "move",
      (done) => {
        const moved = () => {
          if (location.hash === landsOn) done();
        };
        window.addEventListener("hashchange", moved);
        window.addEventListener("popstate", moved);
        return () => {
          window.removeEventListener("hashchange", moved);
          window.removeEventListener("popstate", moved);
        };
      },
      go,
    );
    await act(async () => {});
  }

  const discardPrompt = () => screen.getByRole("alertdialog", { name: "Discard your changes?" });

  // A MOVE TO ANOTHER ENTRY takes the form with it, whatever makes it: one of
  // the form's own links, Back or Forward, or a push from code. So a changed
  // form holds every move and asks first; keeping the changes undoes the
  // move, and discarding them makes it.
  test("a link, Back and a push each ask before they leave a changed form", async () => {
    const onTheBuilder = "#/agents/edit";
    const elsewhere = "#/settings/integrations";
    history.replaceState(null, "", onTheBuilder);
    const view = edit(editing(), "seat:dev");
    const link = () =>
      within(screen.getByText("GitHub is not connected.", { exact: false })).getByRole("link");
    // Untouched: the link simply goes.
    await move(() => fireEvent.click(link()), elsewhere);
    expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
    await move(() => {
      location.hash = onTheBuilder;
    }, onTheBuilder);

    type("Goal", "Ship");
    // Held, and undone: the page is back where it was, and so is the form.
    await move(() => fireEvent.click(link()), onTheBuilder);
    const asked = discardPrompt();
    expect(asked.textContent).toContain("you are leaving the builder");
    fireEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Ship");

    await move(() => history.back(), onTheBuilder);
    fireEvent.click(within(discardPrompt()).getByRole("button", { name: "Keep editing" }));
    expect(view.onClose).not.toHaveBeenCalled();

    await move(() => history.back(), onTheBuilder);
    // The move the reader asked for is made: back past the entry they were on.
    await move(
      () =>
        fireEvent.click(within(discardPrompt()).getByRole("button", { name: "Discard changes" })),
      elsewhere,
    );
    expect(view.onClose).toHaveBeenCalledTimes(1);
    cleanup();
    history.replaceState(null, "", "#/");
  });

  /*
   * AND A MOVE THAT IS NOT A DEPARTURE IS NOT ASKED ABOUT. Choosing the table
   * view, or the reporting chart, is a `section` — the router PUSHES for one,
   * so it really does reach the guard — and it keeps the builder, the draft and
   * this form exactly as they are. A guard that asked here would be a
   * departure prompt over a reader who chose a view, which is the shape a
   * predicate naming the wrong screen produces for EVERY move.
   */
  test("a move within the builder is not a departure, and asks nothing", async () => {
    history.replaceState(null, "", "#/agents/edit?view=visualization");
    const view = edit(editing(), "seat:dev");
    type("Goal", "Ship");
    for (const hash of [
      "#/agents/edit?view=table",
      "#/agents/edit?view=visualization&chart=reporting",
      "#/agents/edit?view=visualization&chart=reporting&seat=ceo",
    ]) {
      await move(() => {
        location.hash = hash;
      }, hash);
      expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" }), hash).toBeNull();
    }
    // The form is still open, still changed, and was never closed.
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Ship");
    expect(view.onClose).not.toHaveBeenCalled();
    cleanup();
    history.replaceState(null, "", "#/");
  });

  test("a schedule toggle is a change too", () => {
    const view = edit(editing(), "unit:engineering");
    fireEvent.click(screen.getByRole("checkbox", { name: "Enabled: standup" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("alertdialog", { name: "Discard your changes?" })).toBeDefined();
    expect(view.onClose).not.toHaveBeenCalled();
  });

  test("Keep editing and one Escape close only the question, and the edit is still there", () => {
    const view = edit(editing(), "seat:dev");
    type("Goal", "Ship");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Ship");

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
    expect(screen.getByRole("dialog", { name: "Edit Dev" })).toBeDefined();
    expect(view.onClose).not.toHaveBeenCalled();
  });
});

describe("seat fields", () => {
  // A NEW HANDLE ON A SAVED SEAT IS THE CHART'S RENAME: the seat keeps its
  // identity, and the form says so rather than refusing the edit.
  test("a saved seat's handle is its address, and a new one is recorded as its rename", () => {
    const view = edit(editing(), "seat:dev");
    expect(
      screen.getByText(/A new handle keeps the seat, its memory and its mailbox/),
    ).toBeDefined();
    type("Handle", "developer");
    apply();
    expect(seatData(view.state(), "seat:dev").handle).toBe("developer");
    cleanup();

    const added = record(editing(), {
      type: "addSeat",
      key: "new:qa",
      placement: { parent: "unit:sales" },
      data: { handle: "qa", name: "QA" },
    });
    edit(added, "new:qa");
    // A seat this draft creates has no identity yet to keep.
    expect(screen.queryByText(/A new handle keeps the seat/)).toBeNull();
    expect((field("Handle") as HTMLInputElement).value).toBe("qa");
  });

  // A MODEL CHAIN IS MOVED rather than retyped: the order is the fallback.
  test("a model chain says the order it is tried in, and is reordered in place", () => {
    const state = connected();
    const chain = record(state, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["runtime", "llm"], value: ["smart", "fast"] }],
    });
    const view = edit(chain, "seat:dev");
    expect(screen.getByText("Tried in this order: smart, then fast.")).toBeDefined();

    fireEvent.click(screen.getByRole("button", { name: "Move fast earlier" }));
    expect(screen.getByText("Tried in this order: fast, then smart.")).toBeDefined();
    // AND A READER WHO CANNOT SEE THE LIST IS TOLD, in the application's own
    // region: nothing mounted one, so this move was said to nobody.
    expect(screen.getByRole("status", { name: ANNOUNCER_LABEL }).textContent).toBe(
      "Moved fast to position 1 of 2",
    );

    apply();
    expect(view.state().log.ops).toHaveLength(2);
    expect(seatData(view.state(), "seat:dev").runtime?.llm).toEqual(["fast", "smart"]);
  });

  test("a GitLab block without provisioning has no access level to set, and says so", () => {
    const settings = fixtureSettings();
    settings.integrations = { ...settings.integrations, gitlab: { enabled: true } };
    edit(editing(fixtureChart(), settings), "seat:dev");
    expect(screen.queryByLabelText(labelled("Access level"))).toBeNull();
    expect(
      screen.getByText(/GitLab provisioning is not set up, so there is no access level to set./),
    ).toBeDefined();
  });

  test("a seat, a unit and the charter each need a name before Apply", () => {
    const cases: [NodeKey, string, string][] = [
      ["seat:dev", "Name", "A seat needs a name."],
      ["unit:engineering", "Name", "A unit needs a name."],
      [COMPANY_KEY, "Company name", "The company needs a name."],
    ];
    for (const [key, label, reason] of cases) {
      edit(editing(), key);
      type(label, "  ");
      expect(shownReason(reason)).toBeDefined();
      expect(applyRefuses()).toBe(true);
      cleanup();
    }
  });

  test("a node that has left the draft opens as an editor that says so", () => {
    edit(editing(), "seat:gone");
    expect(screen.getByText("This node is no longer in the draft")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
  });

  // A MANAGES ENTRY NAMES AN ADDRESS, and one that is both a seat's handle
  // and a unit's key reaches the seat, so the unit is not offered as a second
  // meaning of it.
  test("a unit whose key is a seat's handle is not offered as a second meaning of it", () => {
    const chart = fixtureChart();
    chart.seats.push({ handle: "sales", name: "Sales Desk" });
    edit(editing(chart), "seat:dev");
    fireEvent.click(screen.getByRole("combobox", { name: /^Manages/ }));
    expect(screen.getAllByRole("option", { name: /^Sales/ })).toHaveLength(1);
    expect(screen.getByRole("option", { name: /^Sales Desk/ })).toBeDefined();
    // The control: a unit whose key no seat holds is offered.
    expect(screen.getByRole("option", { name: /^Engineering/ })).toBeDefined();
  });

  test("automatic reports are their own read-only group beside the manages list", () => {
    const overrides: DerivedOverrides = {
      seats: { "vp-engineering": { auto_reports: ["dev"], reports: ["dev"] } },
    };
    edit(checkedEdit(fixtureChart(), overrides), "seat:vp-engineering");
    expect(screen.getByText("Managed automatically as lead of Engineering")).toBeDefined();
    expect(screen.getByRole("combobox", { name: /^Manages/ })).toBeDefined();
  });

  test("an unpinned seat says which provider it runs on", () => {
    edit(connected(), "seat:dev");
    expect(
      screen.getByText(/Runs on smart, the provider a seat that names none runs on/),
    ).toBeDefined();
  });

  // Read only, the banner is the reason Apply is unavailable; a caption asking
  // to correct a field nobody can type in would be a second, wrong one.
  test("a read-only editor asks nobody to correct a field", () => {
    const chart = withDev({ name: "Dev", runtime: { token_budget: { week: -5 } } });
    edit(editing(chart), "seat:dev", { readOnly: true });
    expect(screen.getByText(/cannot be changed right now/)).toBeDefined();
    // The foot says the posture, not the field: a form nobody can write is
    // not a form with a mistake in it.
    expect(
      shownReason("These fields are for reading, so there is nothing to apply."),
    ).toBeDefined();
    cleanup();
    edit(editing(chart), "seat:dev");
    expect(shownReason("Correct the token budget first.")).toBeDefined();
  });

  test("a malformed token ceiling blocks Apply with the reason, under its own window", () => {
    edit(editing(), "seat:dev");
    type("Weekly token ceiling", "lots");
    expect(applyRefuses()).toBe(true);
    expect(
      screen.getByText(
        "Give a whole number of tokens (40000000, or 40M), or leave it empty for no weekly ceiling.",
      ),
    ).toBeDefined();
  });

  // A 0 WAS "UNLIMITED" AND IS REFUSED NOW, by the engine and so by the form
  // before a save: the only way to leave a window uncapped is an empty box.
  test("a ceiling of 0 is refused in the engine's words", () => {
    edit(editing(), "seat:dev");
    type("Daily token ceiling", "0");
    expect(applyRefuses()).toBe(true);
    expect(
      screen.getByText("A ceiling of 0 is refused: leave it empty for no daily ceiling."),
    ).toBeDefined();
  });

  // ONE BOX PER WINDOW, and a saved window is shown in its own box: the
  // form reads the mapping the engine writes, not one number.
  test("each window the seat caps is shown in its own box", () => {
    edit(
      editing(withDev({ name: "Dev", runtime: { token_budget: { day: 1500, month: 40000 } } })),
      "seat:dev",
    );
    const box = (label: string) => field(label) as HTMLInputElement;
    expect(box("Daily token ceiling").value).toBe("1500");
    expect(box("Weekly token ceiling").value).toBe("");
    expect(box("Monthly token ceiling").value).toBe("40000");
  });

  // THE THREE WINDOWS ARE WRITTEN AS THE ENGINE READS THEM: one key per
  // calendar window on the seat's runtime `token_budget`, each a number of
  // tokens — never a single lifetime ceiling, which the periodic budgets
  // replaced.
  test("the budget editor writes the day, week and month the reader typed", () => {
    const view = edit(editing(), "seat:dev");
    type("Daily token ceiling", "2000");
    type("Weekly token ceiling", "10000");
    type("Monthly token ceiling", "40000");
    apply();
    expect(view.state().log.ops).toHaveLength(1);
    expect(seatData(view.state(), "seat:dev").runtime?.token_budget).toEqual({
      day: 2000,
      week: 10000,
      month: 40000,
    });
  });

  test("changing the kind is its own step: the editor closes and opens it, unless the form has changes", () => {
    const view = edit(editing(), "seat:dev");
    fireEvent.click(screen.getByRole("button", { name: "Change to human seat" }));
    expect(view.onClose).toHaveBeenCalledTimes(1);
    expect(view.spies.openChangeKind).toHaveBeenCalledWith("seat:dev");
    cleanup();
    edit(editing(), "seat:dev");
    type("Goal", "Ship");
    expect(
      (screen.getByRole("button", { name: "Change to human seat" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  // The runtime half's settings the builder shows and does not write, each
  // with the reason and where it is written instead.
  test("the runtime settings the builder does not edit are shown, each saying why", () => {
    const chart = withDev({
      name: "Dev",
      runtime: {
        llm_review: ["smart", "fast"],
        llm_judge: "fast",
        sandbox: { enabled: true, run_in: "e2b", env: { KEY: REDACTED } },
        workers: ["researcher", "writer"],
        learning_enabled: false,
      },
    });
    edit(editing(chart), "seat:dev");
    const section = screen
      .getByRole("heading", { name: "Shown, not edited here" })
      .closest("section") as HTMLElement;
    const fact = (label: string) =>
      within(section).getByText(label).closest(".builder-fact") as HTMLElement;
    expect(fact("Models per phase").textContent).toContain("review: smart, then fast");
    expect(fact("Models per phase").textContent).toContain("judge: fast");
    expect(fact("Sandbox").textContent).toContain("Enabled, runs in e2b");
    expect(fact("Workers").textContent).toContain("researcher, writer");
    expect(fact("Learning").textContent).toContain("Off");
    expect(fact("Datadog fallback").textContent).toContain("Not the Datadog fallback.");
    expect(
      within(section).getByText("5 settings the builder shows and does not edit."),
    ).toBeDefined();
    for (const label of ["Models per phase", "Sandbox", "Workers", "Learning"]) {
      expect(fact(label).textContent).toContain("crewlet config import");
    }
    expect(section.innerHTML).not.toContain(REDACTED);
  });

  test("a seat's placement is shown as the node and labels it names", () => {
    const chart = withDev({ name: "Dev", runtime: { placement: { labels: { region: "eu" } } } });
    edit(editing(chart), "seat:dev");
    const fact = screen.getByText("Placement").closest(".builder-fact") as HTMLElement;
    expect(fact.textContent).toContain("Nodes labelled region=eu");
    expect(fact.textContent).not.toContain("[object Object]");
  });

  test("a human seat shows contact and availability, and no agent-only field", () => {
    edit(editing(humanDev()), "seat:dev");
    expect((field("GitHub login") as HTMLInputElement).value).toBe("dev");
    expect(field("Availability")).toBeDefined();
    for (const agentOnly of ["Model", "Daily token ceiling", "Behavioral guidelines"]) {
      expect(screen.queryByLabelText(labelled(agentOnly))).toBeNull();
    }
    expect(screen.queryByRole("heading", { name: "Integrations" })).toBeNull();
  });

  // THE RUNTIME HALF IS SERVED ONLY TO A READER WHO MAY READ IT, and one who
  // may not is told so: empty fields would read as a seat with no model and
  // no contact, and an Apply of them would clear what the reader never saw.
  test("a reader not shown the runtime half is told so, and edits the rest without touching it", () => {
    const view = edit(editing(strippedChart(fixtureChart())), "seat:dev");
    expect(screen.getByText(/runtime half was not shown to you/)).toBeDefined();
    expect(screen.queryByRole("combobox", { name: /^Model/ })).toBeNull();
    expect(screen.queryByLabelText(labelled("Daily token ceiling"))).toBeNull();
    type("Goal", "Ship");
    apply();
    expect(seatData(view.state(), "seat:dev")).not.toHaveProperty("runtime");
    cleanup();
    // The control: shown the half, the fields are there.
    edit(editing(), "seat:dev");
    expect(screen.queryByText(/runtime half was not shown to you/)).toBeNull();
    expect(field("Daily token ceiling")).toBeDefined();
  });
});

// A SEAT'S ADDRESS IS SEALED LIKE A CREDENTIAL: the chart serves the reference
// it was sealed under, or the mask where a row holds something else, and
// restores either from the row when a write sends it back unchanged.
describe("a sealed email address", () => {
  const sealed = (email: string) => editing(withDev({ name: "Dev", goal: "Build", email }));

  test("a masked address is said to be set, never put in a box as text", () => {
    const view = edit(sealed(REDACTED), "seat:dev");
    expect(screen.getByText("An address is set (hidden).")).toBeDefined();
    expect(screen.queryByLabelText(labelled("Email"))).toBeNull();
    expect(view.container.ownerDocument.body.innerHTML).not.toContain(REDACTED);
  });

  test("a sealed reference is named as the reference it is, and not offered for editing", () => {
    edit(sealed("${CHART_SEAT_DEV_EMAIL}"), "seat:dev");
    const fact = screen.getByText(/An address is set, sealed as/);
    expect(fact.textContent).toContain("${CHART_SEAT_DEV_EMAIL}");
    expect(screen.queryByLabelText(labelled("Email"))).toBeNull();
  });

  // THE ROUND TRIP: an edit of another field sends the address back exactly
  // as it was read, which the chart restores from the row.
  test("an edit of another field leaves the sealed address exactly as read", () => {
    for (const email of [REDACTED, "${CHART_SEAT_DEV_EMAIL}"]) {
      const view = edit(sealed(email), "seat:dev");
      type("Goal", "Ship");
      apply();
      expect(view.state().log.ops).toHaveLength(1);
      expect(seatData(view.state(), "seat:dev")).toMatchObject({ goal: "Ship", email });
      cleanup();
    }
  });

  test("Replace types a new address, and Keep puts the sealed one back", () => {
    const view = edit(sealed(REDACTED), "seat:dev");
    fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    expect((field("Email") as HTMLInputElement).value).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Keep the sealed address" }));
    expect(screen.getByText("An address is set (hidden).")).toBeDefined();
    // Kept, so nothing changed and nothing is recorded.
    apply();
    expect(view.state().log.ops).toHaveLength(0);
    cleanup();

    const replaced = edit(sealed(REDACTED), "seat:dev");
    fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    type("Email", "dev@example.com");
    apply();
    expect(seatData(replaced.state(), "seat:dev").email).toBe("dev@example.com");
  });

  // The control: an address no read masked (one this draft typed, or a row
  // written before the chart sealed addresses) is an ordinary field.
  test("an address in the clear is an ordinary field", () => {
    edit(sealed("dev@example.com"), "seat:dev");
    expect((field("Email") as HTMLInputElement).value).toBe("dev@example.com");
    expect(screen.queryByText(/An address is set/)).toBeNull();
  });
});

describe("integrations", () => {
  test("a tool the company has not connected says so and links to Integrations", () => {
    edit(editing(), "seat:dev");
    for (const tool of ["GitHub", "Mattermost"]) {
      const note = screen.getByText(`${tool} is not connected.`, { exact: false });
      expect(within(note).getByRole("link").getAttribute("href")).toBe("#/settings/integrations");
    }
    expect(screen.queryByLabelText(labelled("Access tier"))).toBeNull();
    // GitLab provisioning is connected in the fixture.
    expect(field("Access level")).toBeDefined();
  });

  test("with the tools connected, each field is there and says what it changes", () => {
    edit(connected(), "seat:dev");
    // A tool's section is a part of Integrations, and is heard as one.
    expect(screen.getByRole("heading", { name: "Integrations", level: 2 })).toBeDefined();
    expect(screen.getByRole("heading", { name: "GitHub", level: 3 })).toBeDefined();
    expect(screen.getByText("acme-dev")).toBeDefined();
    expect((field("Mattermost channel") as HTMLInputElement).value).toBe("eng");
    expect(screen.queryByLabelText(labelled("Bot username"))).toBeNull();
    expect(
      screen.getByText(
        "The engine provisions this bot, because its token names a secret store entry. Changing the username would make the provisioner find or create a second bot.",
      ),
    ).toBeDefined();
    expect(screen.getByText("Not the Datadog fallback.")).toBeDefined();
    // Already enrolled: its block exists, so a tier says nothing about enrolling.
    expect(screen.queryByText(/This enrols the seat in GitHub/)).toBeNull();

    choose("Access tier", "Full access");
    expect(
      screen.getByText(
        "The app's permissions were fixed when it was created. Raise them at GitHub as well.",
      ),
    ).toBeDefined();
  });

  // A LITERAL TOKEN IS A BOT SOMEBODY MANAGES. The provisioner only mints into
  // the secret store entry a whole ${VAR} names; every other seat it notes and
  // leaves alone. A literal reaches this screen as its mask, so a screen that
  // read "has a token" as "is provisioned" locked the username of a bot the
  // engine never touches, with copy saying the opposite.
  test("a bot whose token is a literal is managed by hand, and its username stays editable", () => {
    const state = connected();
    const settings = state.draft.company;
    settings.integrations = {
      ...(settings.integrations as Record<string, unknown>),
      mattermost: {
        enabled: true,
        url: "https://chat.example.com",
        team: "acme",
        provisioning: { username_prefix: "Agent-" },
      },
    };
    const chart = withDev({
      name: "Dev",
      runtime: { mattermost: { bot_token: REDACTED, channel: "eng" } },
    });
    edit(editing(chart, settings), "seat:dev");
    const username = field("Bot username") as HTMLInputElement;
    expect(username.disabled).toBe(false);
    expect(username.value).toBe("");
    expect(
      screen.getByText(
        "The engine provisions a bot only where its token names a secret store entry, so this one is managed by hand. Empty uses agent-dev.",
      ),
    ).toBeDefined();
  });

  // THE PROVISIONER NAMES A BOT AFTER THE HANDLE THE SEAT WAS CREATED UNDER,
  // which no rename moves: a renamed seat's default is its identity's.
  test("a renamed seat's default bot name is the handle it was created under", () => {
    const settings = fixtureSettings();
    settings.integrations = {
      ...settings.integrations,
      mattermost: { enabled: true, url: "https://chat.example.com", team: "acme" },
    };
    const chart = chartOf({
      seats: [
        {
          handle: "developer",
          name: "Developer",
          origin_handle: "dev",
          former_handles: ["dev"],
          runtime: { mattermost: { bot_token: REDACTED, channel: "eng" } },
        },
      ],
    });
    edit(editing(chart, settings), "seat:dev");
    expect(screen.getByText(/so this one is managed by hand\. Empty uses dev\./)).toBeDefined();
    // Typing a new handle moves nothing: the identity is the chart's.
    type("Handle", "engineer");
    expect(screen.getByText(/Empty uses dev\./)).toBeDefined();
  });

  test("a tier on a seat with no GitHub block enrols it, and says so", () => {
    edit(connected(), "seat:sre");
    expect(screen.queryByText(/This enrols the seat in GitHub/)).toBeNull();
    choose("Access tier", "Review");
    expect(
      screen.getByText(/This enrols the seat in GitHub. Create its app from Integrations./),
    ).toBeDefined();
    cleanup();

    // A repository alone writes the block as well, so it enrols the seat too.
    edit(connected(), "seat:sre");
    type("New repository", "acme/api");
    fireEvent.keyDown(field("New repository"), { key: "Enter" });
    expect(screen.getByText(/This enrols the seat in GitHub/)).toBeDefined();
  });

  test("a Mattermost channel needs the seat's own bot, and the Datadog fallback is named on its seat", () => {
    edit(connected(), "seat:sre");
    expect(screen.queryByLabelText(labelled("Mattermost channel"))).toBeNull();
    expect(
      screen.getByText(/This seat has no Mattermost bot of its own, so it has no channel to set./),
    ).toBeDefined();
    expect(screen.getByText("This seat is the Datadog fallback.")).toBeDefined();
    // Where it is chosen, which is also where the builder asks for a new one.
    expect(
      screen.getByText(
        /chosen from Integrations, or here when the fallback seat is deleted or changed to a human seat/,
      ),
    ).toBeDefined();
    cleanup();

    // Switched off, Datadog wakes nobody whatever its route_to names, which
    // is how the engine and the chart read it.
    const off = fixtureSettings();
    off.integrations = { ...off.integrations, datadog: { enabled: false, route_to: "sre" } };
    edit(editing(fixtureChart(), off), "seat:sre");
    expect(
      screen.getByText("Datadog is switched off, so no alert wakes a fallback seat."),
    ).toBeDefined();
    expect(screen.queryByText("This seat is the Datadog fallback.")).toBeNull();
  });

  test("no credential reaches the page: not a mask, not a reference, not the literal half of a partial one", () => {
    const view = edit(connected(), "seat:dev");
    const html = view.container.ownerDocument.body.innerHTML;
    for (const secret of [
      REDACTED,
      "sk-live",
      "DEV_GITHUB_KEY",
      "DEV_SLACK_SECRET",
      "DEV_MM_TOKEN",
      "TRACKER_URL",
    ]) {
      expect(html, secret).not.toContain(secret);
    }
    // What IS shown of them is their names.
    expect(screen.getByText("tracker")).toBeDefined();
    expect(screen.getByText(/TOKEN, URL/)).toBeDefined();
  });
});

describe("findings", () => {
  /**
   * Whether any control on screen points at this alert as its description.
   *
   * That is what "attached to a field" MEANS: the design system's field gives
   * its error an id and names it in the control's `aria-describedby`, so a
   * reader on the control hears it.
   */
  function describesAControl(alert: HTMLElement): boolean {
    return alert.id !== "" && document.querySelector(`[aria-describedby~="${alert.id}"]`) !== null;
  }

  // A finding about a field this form does not draw must not be attached to
  // one: it would be reported nowhere a reader can see it.
  test("a problem about a field the form does not draw is listed at the top", () => {
    // The fixture company has not connected GitHub, so the tier is not drawn.
    const state = checkWith(editing(), [
      findingOn("seat:dev", ["runtime", "github", "tier"], "names no tier"),
    ]);
    edit(state, "seat:dev");
    const alerts = screen.getAllByRole("alert");
    expect(alerts).toHaveLength(1);
    expect(alerts[0]!.textContent).toContain("names no tier");
    expect(describesAControl(alerts[0]!)).toBe(false);
  });

  test("a schedule problem is shown in the schedules panel, where the toggle that fixes it is", () => {
    const state = checkWith(editing(), [
      findingOn(
        "unit:engineering",
        ["runtime", "schedules"],
        "schedule has no runner: the effective lead is a human seat",
      ),
    ]);
    edit(state, "unit:engineering");
    const panel = screen.getByRole("heading", { name: "Schedules" }).closest("section")!;
    expect(within(panel as HTMLElement).getByRole("alert").textContent).toContain(
      "schedule has no runner",
    );
  });

  // A problem that reaches the top is usually about something the builder does
  // not author, so the link it carries is the only thing on screen saying
  // where it is fixed. A human seat draws no schedules panel at all.
  test("a problem the form cannot place keeps the link that says where it is fixed", () => {
    const finding: PlacedProblem = {
      ...findingOn("seat:dev", ["runtime", "schedules"], "a human seat must not carry schedules"),
      link: "schedules",
    };
    edit(checkWith(editing(humanDev()), [finding]), "seat:dev");
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("must not carry schedules");
    expect(within(alert).getByRole("link", { name: "Open Schedules" }).getAttribute("href")).toBe(
      "#/agents/schedules",
    );
  });

  // The engine takes a draft with warnings; a field's error slot says it is
  // refused (critical, invalid, an alert). So a warning names its field at the
  // top, in the caution tone, and a refusal beside it stays a refusal.
  test("a warning is a caution at the top, never a field's error", () => {
    const warned = checkWith(editing(), [
      findingOn("unit:platform", ["lead"], "the lead names no seat", "warning"),
    ]);
    edit(warned, "unit:platform");
    const lead = field("Lead");
    expect(lead.getAttribute("aria-invalid")).toBeNull();
    expect(errorOf(lead)).toBeNull();
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
    // In the caution tone, not the critical one.
    const said = screen.getByText(/names no seat/);
    const caution = said.closest(`.${drawnClasses(Callout, { children: "" })[0]}`)!;
    expect(
      isDrawnAs(caution, Callout, { variant: "warning", children: "" }, { children: "" }),
    ).toBe(true);
  });

  // THE ENGINE REFUSES A CEILING AT ITS WINDOW'S KEY, so the refusal sits
  // under that window's box and no other: a daily ceiling of 0 blamed on the
  // monthly box is an operator correcting the wrong number.
  test("a refused ceiling sits beside its own window's box", () => {
    const state = checkWith(editing(), [
      findingOn("seat:dev", ["runtime", "token_budget", "week"], "must be at least 1 token"),
    ]);
    edit(state, "seat:dev");
    expect(errorOf(field("Weekly token ceiling"))?.textContent).toContain("must be at least 1");
    expect(errorOf(field("Daily token ceiling"))).toBeNull();
    expect(errorOf(field("Monthly token ceiling"))).toBeNull();
  });

  test("a problem sits beside the field it names, and the rest are listed at the top", () => {
    const state = checkWith(editing(), [
      findingOn("seat:dev", ["goal"], "the goal is too long for the chart"),
      findingOn("seat:dev", ["runtime", "workers"], "names no template"),
    ]);
    edit(state, "seat:dev");
    expect(errorOf(field("Goal"))?.textContent).toContain("too long for the chart");
    // AND THE REST ARE LISTED AT THE TOP: an alert no field points at.
    const top = screen
      .getAllByRole("alert")
      .find((alert) => alert.textContent?.includes("names no template"));
    expect(top).toBeDefined();
    expect(describesAControl(top!)).toBe(false);
    // And the one that DOES sit beside a field answers the other way, which is
    // what stops the two cases above passing over an answer of "never".
    const beside = errorOf(field("Goal"));
    expect(beside).toBeDefined();
    expect(describesAControl(beside!)).toBe(true);
  });
});

describe("a unit", () => {
  test("the lead shows the lead it would inherit, and a lead change applies with the rest", () => {
    const view = edit(editing(), "unit:platform");
    expect(field("Lead").textContent).toBe("No lead (inherits VP Engineering from Engineering)");
    choose("Lead", "SRE");
    type("Purpose", "Keep it running");
    apply();
    expect(view.state().log.ops).toHaveLength(1);
    const found = locate(view.state().draft, "unit:platform");
    expect(found?.kind === "unit" && found.node.data).toMatchObject({
      lead: "sre",
      purpose: "Keep it running",
    });
  });

  // A NEW KEY ON A SAVED UNIT IS ITS RENAME, as a seat's handle is; a new NAME
  // is prose, and says what reads the name.
  test("a saved unit's key is its address, and a new name says what reads it", () => {
    const view = edit(editing(), "unit:engineering");
    expect(screen.getByText(/A new key keeps the unit/)).toBeDefined();
    type("Name", "Product Engineering");
    expect(
      screen.getByText(
        "Onboarding pages are looked up under a unit's name, so the seats in it read the pages under the new name.",
      ),
    ).toBeDefined();
    type("Key", "product-engineering");
    apply();
    const found = locate(view.state().draft, "unit:engineering");
    expect(found?.kind === "unit" && found.node.data).toMatchObject({
      key: "product-engineering",
      name: "Product Engineering",
    });
  });

  /*
   * ONE CONTROL FOR ONE STRING. The type used to be a select whose "Custom
   * type" option revealed a SECOND labelled field below it. It is one field
   * that offers the engine's nine names and takes whatever is typed.
   */
  test("a unit type the engine does not name is typed in the same one box", () => {
    const view = edit(editing(), "unit:platform");
    expect(screen.queryByLabelText(labelled("Custom type"))).toBeNull();
    type("Type", "tribe");
    apply();
    const found = locate(view.state().draft, "unit:platform");
    expect(found?.kind === "unit" && found.node.data.type).toBe("tribe");
  });

  test("the nine names the engine knows are offered in that same box", () => {
    const view = edit(editing(), "unit:platform");
    choose("Type", "Department");
    apply();
    const found = locate(view.state().draft, "unit:platform");
    expect(found?.kind === "unit" && found.node.data.type).toBe("department");
  });

  test("a unit's schedule says when it runs and who runs it", () => {
    const chart = fixtureChart();
    chart.units
      .find((u) => u.key === "engineering")!
      .runtime!.schedules!.push({ name: "triage", cron: "0 * * * *", task: "Triage the queue" });
    edit(editing(chart), "unit:engineering");
    const standup = screen.getByRole("checkbox", { name: "Enabled: standup" });
    const triage = screen.getByRole("checkbox", { name: "Enabled: triage" });
    const described = (box: HTMLElement) =>
      document.getElementById(box.getAttribute("aria-describedby") ?? "")?.textContent;
    expect(described(standup)).toBe("0 9 * * 1-5, run by the unit lead. Run standup");
    expect(described(triage)).toBe("0 * * * *, run by each agent member. Triage the queue");
  });

  test("an empty channel says what the unit inherits from the unit above it", () => {
    const chart = fixtureChart();
    chart.units.find((u) => u.key === "engineering")!.channel = "eng";
    edit(editing(chart), "unit:platform");
    const channel = field("Channel") as HTMLInputElement;
    expect(channel.placeholder).toBe("eng");
    expect(screen.getByText("Empty inherits eng from Engineering.")).toBeDefined();
  });

  test("a unit schedule is toggled as part of the edit", () => {
    const view = edit(editing(), "unit:engineering");
    fireEvent.click(screen.getByRole("checkbox", { name: "Enabled: standup" }));
    apply();
    const found = locate(view.state().draft, "unit:engineering");
    expect(found?.kind === "unit" && found.node.data.runtime?.schedules?.[0]?.enabled).toBe(false);
    expect(screen.getByText("Free-text references, not a read scope.")).toBeDefined();
  });

  test("a reader not shown the runtime half is told a unit's schedules and credentials are withheld", () => {
    edit(editing(strippedChart(fixtureChart())), "unit:engineering");
    expect(screen.getByText(/runtime half was not shown to you/)).toBeDefined();
    expect(screen.queryByRole("checkbox", { name: "Enabled: standup" })).toBeNull();
  });
});

// "Edit reports" is about one field of a long form, and "Choose another
// seat" in a unit's lead chip about another: each opens the editor on that
// field rather than on the name at the top.
describe("opening at a part of the form", () => {
  test("a seat opened at its reports starts on Manages, and one opened plainly on its name", () => {
    edit(editing(), "seat:dev", {}, "reports");
    expect(document.activeElement).toBe(screen.getByRole("combobox", { name: /^Manages/ }));
    cleanup();
    edit(editing(), "seat:dev");
    expect(document.activeElement).toBe(field("Name"));
  });

  test("a unit opened at its leadership starts on its lead", () => {
    edit(editing(), "unit:sales", {}, "leadership");
    expect(document.activeElement).toBe(field("Lead"));
  });
});

describe("the charter", () => {
  test("renaming the company needs its consequences confirmed before Apply", () => {
    const view = edit(editing(), COMPANY_KEY);
    type("Company name", "Acme Labs");
    expect(applyRefuses()).toBe(true);
    // The same sentence the review asks the operator to accept before a save,
    // so the two cannot come to describe one consequence two ways.
    expect(screen.getByText(ACKNOWLEDGEMENT_TEXT.company_rename)).toBeDefined();
    fireEvent.click(
      screen.getByRole("checkbox", { name: "I understand what renaming the company does" }),
    );
    expect(applyRefuses()).toBe(false);
    apply();
    expect(view.state().draft.company.name).toBe("Acme Labs");
    expect(view.state().log.ops).toHaveLength(1);
  });

  // The acknowledgement is about what THIS Apply does: a name the draft
  // already carries, or one the settings stored with a space around it, is
  // not a rename of the mission edit that follows.
  test("a charter edit that renames nothing asks nothing, and renames nothing", () => {
    const renamed = record(editing(), {
      type: "updateCompany",
      set: [{ path: ["name"], value: "Acme Labs" }],
    });
    const view = edit(renamed, COMPANY_KEY);
    type("Mission", "Make more things.");
    expect(screen.queryByRole("checkbox", { name: /renaming the company/ })).toBeNull();
    apply();
    expect(view.state().log.ops).toHaveLength(2);
    cleanup();

    const padded = { ...fixtureSettings(), name: " Acme" };
    const second = edit(editing(fixtureChart(), padded), COMPANY_KEY);
    type("Mission", "Make more things.");
    expect(screen.queryByRole("checkbox", { name: /renaming the company/ })).toBeNull();
    apply();
    expect(second.state().draft.company.name).toBe(" Acme");
    expect(second.state().draft.company.mission).toBe("Make more things.");
    cleanup();

    // Taking the space out IS a rename: the engine derives agent ids from the
    // name exactly as stored.
    edit(editing(fixtureChart(), padded), COMPANY_KEY);
    type("Company name", "Acme");
    expect(
      screen.getByRole("checkbox", { name: "I understand what renaming the company does" }),
    ).toBeDefined();
  });
});
