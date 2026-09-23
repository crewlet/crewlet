/**
 * The node editor.
 *
 * What these protect: Apply records the form as ONE operation and a refusal
 * keeps the form open with its reason; closing a changed form asks first,
 * whichever field changed, and one Escape closes only the question; a field
 * for a tool is drawn only where the company connected the tool, with the
 * consequences a GitHub tier and a Mattermost username carry said beside
 * them; the engine's problems sit beside the fields they name; and no
 * credential, masked or referenced, reaches the page.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { CompanyDocument, ConfigRole } from "~/protocol/index.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { locate } from "./model/draft.ts";
import type { BuilderState } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { builderReducer } from "./model/reducer.ts";
import { ACKNOWLEDGEMENT_TEXT } from "./dialogParts.tsx";
import type { EditorSectionName } from "./BuilderContext.tsx";
import { NodeEditor } from "./NodeEditor.tsx";
import { renderInBuilder, type HarnessOptions } from "./viewTestkit.tsx";
import {
  checkWithProblems,
  checkWithWarnings,
  keyedState,
  problemAt,
  recheck,
  warningAt,
} from "./testState.ts";
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
const seatData = (state: BuilderState, key: NodeKey): ConfigRole => {
  const found = locate(state.draft, key);
  if (found?.kind !== "seat") throw new Error(`no seat ${key}`);
  return found.node.data;
};

/** The fixture company with every tool a seat's fields depend on connected. */
function connected(): CompanyDocument {
  const doc = fixtureCompany();
  doc.providers = { llm: { fast: {}, smart: {} }, llm_order: ["smart", "fast"] };
  doc.integrations = {
    ...doc.integrations,
    github: { enabled: true, webhook_secret: "__redacted__" },
    slack: {},
    mattermost: { enabled: true, url: "https://chat.example.com", team: "acme" },
    jira: { enabled: true },
    confluence: { enabled: true },
  };
  doc.units![0]!.roles![1] = {
    name: "Dev",
    goal: "Build",
    mcp_env: { tracker: { TOKEN: "__redacted__", URL: "${TRACKER_URL}" } },
    sandbox: { enabled: true, run_in: "e2b", env: { KEY: "__redacted__" } },
    integrations: {
      github: {
        tier: "review",
        app_slug: "acme-dev",
        private_key: "${DEV_GITHUB_KEY}",
        webhook_secret: "__redacted__",
      },
      slack: {
        bot_token: "Bearer sk-live-${SUFFIX}",
        signing_secret: "${DEV_SLACK_SECRET}",
        channel: "C1",
      },
      mattermost: { bot_token: "${DEV_MM_TOKEN}", channel: "eng", username: "dev-bot" },
    },
  };
  return doc;
}

describe("applying", () => {
  test("Apply records every changed field as one operation and closes the editor", () => {
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
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
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
    apply();
    expect(view.state().log.ops).toHaveLength(0);
    expect(view.onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
    expect(view.onClose).toHaveBeenCalledTimes(2);
  });

  test("a refused edit stays open with the reducer's reason, and nothing is recorded", () => {
    // A seat added in this draft has no handle until the next check, and the
    // company holds GitLab access levels keyed by handle, so renaming it now
    // is refused rather than leaving a level behind.
    const state = builderReducer(keyedState(fixtureCompany()), {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "QA" },
      },
    });
    const view = edit(state, "new:qa");
    type("Name", "Quality");
    apply();
    expect(view.onClose).not.toHaveBeenCalled();
    expect(screen.getByText(/has not reported this seat's handle yet/)).toBeDefined();
    expect(view.state().log.ops).toHaveLength(1);
  });

  test("a read-only builder applies nothing", () => {
    const view = edit(keyedState(fixtureCompany()), "seat:dev", { readOnly: true });
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

    edit(keyedState(fixtureCompany()), COMPANY_KEY, readOnly);
    expect(field("Company name")).toBeDefined();
    expect(enabled()).toEqual([]);
    cleanup();

    edit(keyedState(fixtureCompany()), "unit:Engineering", readOnly);
    expect(field("Type")).toBeDefined();
    expect(enabled()).toEqual([]);
    cleanup();

    edit(keyedState(connected()), "seat:dev", readOnly);
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
    edit(keyedState(fixtureCompany()), "seat:dev");
    const cancel = screen.getByRole("button", { name: "Cancel" });
    const apply = screen.getByRole("button", { name: "Apply" });
    const firstField = field("Name");
    for (const control of [cancel, apply]) {
      expect(
        control.compareDocumentPosition(firstField) & Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
    }
    // Cancel IS the way out. A close control beside the title would be a
    // second, unnamed spelling of it, which is what the console does not have
    // and what this panel drew at the same time as Cancel.
    expect(screen.queryByRole("button", { name: "Close" })).toBeNull();
  });

  /*
   * THE NODE'S OWN MARK, not a pencil on all four. The head used to draw an
   * edit glyph for the company, a unit, an agent seat and a human seat alike,
   * so the panel said that it edits rather than what it is editing.
   */
  test("each kind of node is marked as its own kind", () => {
    const human = fixtureCompany();
    human.units![0]!.roles![1] = { name: "Dev", kind: "human", contact: { github_login: "dev" } };
    const cases: [CompanyDocument, NodeKey][] = [
      [fixtureCompany(), COMPANY_KEY],
      [fixtureCompany(), "unit:Engineering"],
      [fixtureCompany(), "seat:dev"],
      [human, "seat:dev"],
    ];
    const drawn: string[] = [];
    for (const [doc, key] of cases) {
      edit(keyedState(doc), key);
      drawn.push(mark());
      cleanup();
    }
    expect(new Set(drawn).size).toBe(4);
  });
});

/*
 * THE HUE, STATED RATHER THAN OFFERED. The console's agent editor ends with a
 * picker over six colour schemes and stores the answer on the node; a Crewlet
 * company document has no colour field, so this dashboard derives the hue from
 * the seat's own key. The derivation is right; the silence was not, and an
 * operator who saw the console's picker found neither the control nor a reason
 * it was gone.
 */
describe("the colour", () => {
  test("an agent seat says which hue it is drawn in, and why it cannot be set", () => {
    edit(keyedState(fixtureCompany()), "seat:dev");
    expect(screen.getByText("Colour")).toBeDefined();
    expect(screen.getByText(/hue follows the seat's own identity/)).toBeDefined();
    // Stated, never written: there is no control to change it.
    expect(screen.queryByRole("radio", { name: /purple|cyan|green|amber|rose|blue/i })).toBeNull();
  });

  test("a human seat and a unit have no hue to say", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1] = { name: "Dev", kind: "human", contact: { github_login: "dev" } };
    edit(keyedState(doc), "seat:dev");
    expect(screen.queryByText("Colour")).toBeNull();
    cleanup();
    edit(keyedState(fixtureCompany()), "unit:Engineering");
    expect(screen.queryByText("Colour")).toBeNull();
  });
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
    const view = edit(keyedState(connected()), "seat:dev");
    change();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    const prompt = screen.getByRole("alertdialog", { name: "Discard your changes?" });
    expect(view.onClose).not.toHaveBeenCalled();
    fireEvent.click(within(prompt).getByRole("button", { name: "Discard changes" }));
    expect(view.onClose).toHaveBeenCalledTimes(1);
    expect(view.state().log.ops).toHaveLength(0);
  });

  // A MOVE TO ANOTHER ENTRY takes the form with it, whatever makes it: one of
  // the form's own links, Back or Forward, or a push from code. So a changed
  // form holds every move and asks first; keeping the changes undoes the
  // move, and discarding them makes it.
  test("a link, Back and a push each ask before they leave a changed form", async () => {
    history.replaceState(null, "", "#/company?lens=builder");
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
    const link = () =>
      within(screen.getByText("GitHub is not connected.", { exact: false })).getByRole("link");
    const prompt = () => screen.findByRole("alertdialog", { name: "Discard your changes?" });
    // Untouched: the link simply goes.
    fireEvent.click(link());
    await waitFor(() => expect(location.hash).toBe("#/admin/integrations"));
    expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
    act(() => {
      location.hash = "#/company?lens=builder";
    });
    await waitFor(() => expect(location.hash).toBe("#/company?lens=builder"));

    type("Goal", "Ship");
    fireEvent.click(link());
    const asked = await prompt();
    expect(asked.textContent).toContain("you are leaving the builder");
    // Held, and undone: the page is where it was, and so is the form.
    await waitFor(() => expect(location.hash).toBe("#/company?lens=builder"));
    fireEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Ship");

    act(() => history.back());
    fireEvent.click(within(await prompt()).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(location.hash).toBe("#/company?lens=builder"));
    expect(view.onClose).not.toHaveBeenCalled();

    act(() => history.back());
    fireEvent.click(within(await prompt()).getByRole("button", { name: "Discard changes" }));
    expect(view.onClose).toHaveBeenCalledTimes(1);
    // The move the reader asked for is made: back past the entry they were on.
    await waitFor(() => expect(location.hash).toBe("#/admin/integrations"));
    cleanup();
    history.replaceState(null, "", "#/");
  });

  /*
   * AND A MOVE THAT IS NOT A DEPARTURE IS NOT ASKED ABOUT. Choosing the table
   * view, or the reporting chart, is a `section` — the router PUSHES for one,
   * so it really does reach the guard — and it keeps the lens, the draft and
   * this form exactly as they are. A guard that asked here would be a
   * departure prompt over a reader who chose a view, which is the shape a
   * predicate naming the wrong screen produces for EVERY move.
   */
  test("a move within the lens is not a departure, and asks nothing", async () => {
    history.replaceState(null, "", "#/company?lens=builder&view=visualization");
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
    type("Goal", "Ship");
    for (const hash of [
      "#/company?lens=builder&view=table",
      "#/company?lens=builder&view=visualization&chart=reporting",
      "#/company?lens=builder&view=visualization&chart=reporting&seat=ceo",
    ]) {
      act(() => {
        location.hash = hash;
      });
      await waitFor(() => expect(location.hash).toBe(hash));
      expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" }), hash).toBeNull();
    }
    // The form is still open, still changed, and was never closed.
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Ship");
    expect(view.onClose).not.toHaveBeenCalled();
    cleanup();
    history.replaceState(null, "", "#/");
  });

  test("a schedule toggle is a change too", () => {
    const view = edit(keyedState(fixtureCompany()), "unit:Engineering");
    fireEvent.click(screen.getByRole("checkbox", { name: "Enabled: standup" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("alertdialog", { name: "Discard your changes?" })).toBeDefined();
    expect(view.onClose).not.toHaveBeenCalled();
  });

  test("Keep editing and one Escape close only the question, and the edit is still there", () => {
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
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
  test("an existing seat's handle is read-only with the reason; a new seat's is editable", () => {
    edit(keyedState(fixtureCompany()), "seat:dev");
    expect(screen.queryByLabelText(labelled("Handle"))).toBeNull();
    expect(
      screen.getByText(/keeps its handle: it is the identity its memory and mailbox attach to/),
    ).toBeDefined();
    cleanup();
    const state = builderReducer(keyedState(fixtureCompany()), {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "QA" },
      },
    });
    edit(state, "new:qa");
    expect(field("Handle")).toBeDefined();
  });

  // The engine derives an undeclared handle from the seat's own name, so a
  // check vouches for it exactly while the seat keeps the name that check saw,
  // whatever else changed since; the reducer reads the same rule, so an access
  // level offered here is one it records.
  test("a new seat's derived handle is the one a check reported for the name it has now", () => {
    const added = builderReducer(keyedState(fixtureCompany()), {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "QA" },
      },
    });
    const checked = recheck(added, { seats: { "units[1].roles[0]": { handle: "qa" } } });
    edit(checked, "new:qa");
    expect(
      screen.getByText("Empty uses the handle the engine derives from the name: qa."),
    ).toBeDefined();
    expect((field("Access level") as HTMLSelectElement).disabled).toBe(false);
    expect(
      screen.getByText(
        "Kept by handle: choosing a handle for this seat carries its level with it.",
      ),
    ).toBeDefined();
    type("Name", "Quality");
    expect(
      screen.getByText(
        "Empty uses the handle the engine derives from the name, shown here after the next check.",
      ),
    ).toBeDefined();
    cleanup();

    // Another node edited since: the check is of an older draft, and still
    // saw this seat's name.
    const later = builderReducer(checked, {
      type: "record",
      intent: {
        type: "updateUnit",
        target: "unit:Sales",
        set: [{ path: ["purpose"], value: "Sell" }],
      },
    });
    edit(later, "new:qa");
    expect((field("Access level") as HTMLSelectElement).disabled).toBe(false);
    cleanup();

    // Renamed in the draft: no check has seen the name it derives from now.
    const renamed = builderReducer(later, {
      type: "record",
      intent: { type: "renameSeat", target: "new:qa", name: "Quality" },
    });
    edit(renamed, "new:qa");
    expect((field("Access level") as HTMLSelectElement).disabled).toBe(true);
    expect(
      screen.getByText(
        "Access levels are kept by handle, so this is available once the check reports this seat's handle.",
      ),
    ).toBeDefined();
  });

  // THE ORDER IS THE CHAIN, read first to last, so it is MOVED rather than
  // retyped. The help line used to end "to change the order, remove a provider
  // and choose it again", which was a workaround for a control that could not
  // reorder its own chips.
  test("a model chain says the order it is tried in, and is reordered in place", () => {
    const doc = connected();
    doc.units![0]!.roles![1]!.llm = ["smart", "fast"];
    const view = edit(keyedState(doc), "seat:dev");
    expect(screen.getByText("Tried in this order: smart, then fast.")).toBeDefined();

    fireEvent.click(screen.getByRole("button", { name: "Move fast earlier" }));
    expect(screen.getByText("Tried in this order: fast, then smart.")).toBeDefined();

    apply();
    expect(view.state().log.ops).toHaveLength(1);
    expect(seatData(view.state(), "seat:dev").llm).toEqual(["fast", "smart"]);
  });

  test("a GitLab block without provisioning has no access level to set, and says so", () => {
    const doc = fixtureCompany();
    doc.integrations = { ...doc.integrations, gitlab: { enabled: true } };
    edit(keyedState(doc), "seat:dev");
    expect(screen.queryByLabelText(labelled("Access level"))).toBeNull();
    expect(
      screen.getByText(/GitLab provisioning is not set up, so there is no access level to set./),
    ).toBeDefined();
  });

  test("a seat, a unit and the charter each need a name before Apply", () => {
    const cases: [NodeKey, string, string][] = [
      ["seat:dev", "Name", "A seat needs a name."],
      ["unit:Engineering", "Name", "A unit needs a name."],
      [COMPANY_KEY, "Company name", "The company needs a name."],
    ];
    for (const [key, label, reason] of cases) {
      edit(keyedState(fixtureCompany()), key);
      type(label, "  ");
      expect(shownReason(reason)).toBeDefined();
      expect(applyRefuses()).toBe(true);
      cleanup();
    }
  });

  test("a node that has left the draft opens as an editor that says so", () => {
    edit(keyedState(fixtureCompany()), "seat:gone");
    expect(screen.getByText("This node is no longer in the draft")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
  });

  // A manages entry that names both a seat and a unit names the seat, so a
  // unit sharing a seat's name, or the edited seat's own, is not offered as a
  // second meaning of it.
  test("a unit that shares a seat's name is not offered as a second meaning of it", () => {
    const doc = fixtureCompany();
    doc.roles!.push({ name: "Sales" });
    edit(keyedState(doc), "seat:dev");
    fireEvent.click(screen.getByRole("combobox", { name: /^Manages/ }));
    expect(screen.getAllByRole("option", { name: /^Sales/ })).toHaveLength(1);
    expect(screen.getByRole("option", { name: /^Engineering/ })).toBeDefined();
    cleanup();

    edit(keyedState(doc), "seat:sales");
    fireEvent.click(screen.getByRole("combobox", { name: /^Manages/ }));
    expect(screen.queryByRole("option", { name: /^Sales/ })).toBeNull();
  });

  test("automatic reports are their own read-only group beside the manages list", () => {
    edit(
      keyedState(fixtureCompany(), {
        seats: { "units[0].roles[0]": { auto_reports: ["dev"], reports: ["dev"] } },
      }),
      "seat:vp-engineering",
    );
    expect(screen.getByText("Managed automatically as lead of Engineering")).toBeDefined();
    expect(screen.getByRole("combobox", { name: /^Manages/ })).toBeDefined();
  });

  test("the model is a chain over the company's providers; a per-phase mapping is shown and not edited", () => {
    edit(keyedState(connected()), "seat:dev");
    expect(
      screen.getByText(/Runs on smart, the provider a seat that names none runs on/),
    ).toBeDefined();
    cleanup();
    const doc = connected();
    doc.units![0]!.roles![1]!.llm = { default: "fast", review: ["smart", "fast"] };
    edit(keyedState(doc), "seat:dev");
    expect(screen.queryByRole("combobox", { name: /^Model/ })).toBeNull();
    expect(screen.getByText("review: smart, then fast")).toBeDefined();
    expect(
      screen.getByRole("link", { name: "Edit in the configuration document" }).getAttribute("href"),
    ).toBe("#/admin/config");
  });

  // Read only, the banner is the reason Apply is unavailable; a caption asking
  // to correct a field nobody can type in would be a second, wrong one.
  test("a read-only editor asks nobody to correct a field", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1]!.token_budget = { week: -5 };
    edit(keyedState(doc), "seat:dev", { readOnly: true });
    expect(screen.getByText(/cannot be changed right now/)).toBeDefined();
    // The foot says the posture, not the field: a form nobody can write is
    // not a form with a mistake in it.
    expect(
      shownReason("These fields are for reading, so there is nothing to apply."),
    ).toBeDefined();
    cleanup();
    edit(keyedState(doc), "seat:dev");
    expect(shownReason("Correct the token budget first.")).toBeDefined();
  });

  test("a malformed token ceiling blocks Apply with the reason, under its own window", () => {
    edit(keyedState(fixtureCompany()), "seat:dev");
    type("Weekly token ceiling", "lots");
    expect(applyRefuses()).toBe(true);
    expect(
      screen.getByText("Give a whole number of tokens, or leave it empty for no weekly ceiling."),
    ).toBeDefined();
  });

  // A 0 WAS "UNLIMITED" AND IS REFUSED NOW, by the engine and so by the form
  // before a save: the only way to leave a window uncapped is an empty box.
  test("a ceiling of 0 is refused in the engine's words", () => {
    edit(keyedState(fixtureCompany()), "seat:dev");
    type("Daily token ceiling", "0");
    expect(applyRefuses()).toBe(true);
    expect(
      screen.getByText("A ceiling of 0 is refused: leave it empty for no daily ceiling."),
    ).toBeDefined();
  });

  // ONE BOX PER WINDOW, and a saved window is shown in its own box: the
  // form reads the mapping the engine writes, not one number.
  test("each window the seat caps is shown in its own box", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1]!.token_budget = { day: 1500, month: 40000 };
    edit(keyedState(doc), "seat:dev");
    const box = (label: string) => field(label) as HTMLInputElement;
    expect(box("Daily token ceiling").value).toBe("1500");
    expect(box("Weekly token ceiling").value).toBe("");
    expect(box("Monthly token ceiling").value).toBe("40000");
  });

  test("changing the kind is its own step: the editor closes and opens it, unless the form has changes", () => {
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
    fireEvent.click(screen.getByRole("button", { name: "Change to human seat" }));
    expect(view.onClose).toHaveBeenCalledTimes(1);
    expect(view.spies.openChangeKind).toHaveBeenCalledWith("seat:dev");
    cleanup();
    edit(keyedState(fixtureCompany()), "seat:dev");
    type("Goal", "Ship");
    expect(
      (screen.getByRole("button", { name: "Change to human seat" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  // The field coverage's read-only row: each setting the builder shows and
  // does not edit, with the reason and where it is edited instead.
  test("the settings the builder does not edit are shown, each with where it is edited", () => {
    const doc = fixtureCompany();
    Object.assign(doc.units![0]!.roles![1]!, {
      llm_review: ["smart", "fast"],
      llm_judge: "fast",
      sandbox: { enabled: true, run_in: "e2b", env: { KEY: "__redacted__" } },
      workers: ["researcher", "writer"],
      learning_enabled: false,
    });
    edit(keyedState(doc), "seat:dev");
    const section = screen
      .getByRole("heading", { name: "Configured in the document" })
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
      expect(within(fact(label)).getByRole("link").getAttribute("href")).toBe("#/admin/config");
    }
    expect(section.innerHTML).not.toContain("__redacted__");
  });

  test("a seat's placement is shown as the node and labels it names", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1]!.placement = { labels: { region: "eu" } };
    edit(keyedState(doc), "seat:dev");
    const fact = screen.getByText("Placement").closest(".builder-fact") as HTMLElement;
    expect(fact.textContent).toContain("Nodes labelled region=eu");
    expect(fact.textContent).not.toContain("[object Object]");
  });

  test("a human seat shows contact and availability, and no agent-only field", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1] = { name: "Dev", kind: "human", contact: { github_login: "dev" } };
    edit(keyedState(doc), "seat:dev");
    expect((field("GitHub login") as HTMLInputElement).value).toBe("dev");
    expect(field("Availability")).toBeDefined();
    for (const agentOnly of ["Model", "Token budget", "Behavioral guidelines"]) {
      expect(screen.queryByLabelText(labelled(agentOnly))).toBeNull();
    }
    expect(screen.queryByRole("heading", { name: "Integrations" })).toBeNull();
  });
});

describe("integrations", () => {
  test("a tool the company has not connected says so and links to Integrations", () => {
    edit(keyedState(fixtureCompany()), "seat:dev");
    for (const tool of ["GitHub", "Slack", "Mattermost", "Jira", "Confluence"]) {
      const note = screen.getByText(`${tool} is not connected.`, { exact: false });
      expect(within(note).getByRole("link").getAttribute("href")).toBe("#/admin/integrations");
    }
    expect(screen.queryByLabelText(labelled("Access tier"))).toBeNull();
    expect(screen.queryByLabelText(labelled("Jira project"))).toBeNull();
    // GitLab provisioning is connected in the fixture.
    expect(field("Access level")).toBeDefined();
  });

  test("with the tools connected, each field is there and says what it changes", () => {
    edit(keyedState(connected()), "seat:dev");
    // A tool's section is a part of Integrations, and is heard as one.
    expect(screen.getByRole("heading", { name: "Integrations", level: 2 })).toBeDefined();
    expect(screen.getByRole("heading", { name: "GitHub", level: 3 })).toBeDefined();
    expect((field("Slack channel ID") as HTMLInputElement).value).toBe("C1");
    expect((field("Mattermost channel") as HTMLInputElement).value).toBe("eng");
    expect(screen.queryByLabelText(labelled("Bot username"))).toBeNull();
    expect(
      screen.getByText(
        "The engine provisions this bot, because its token names a secret store entry. Changing the username would make the provisioner find or create a second bot.",
      ),
    ).toBeDefined();
    expect(
      screen.getByText("Where unrouted work for this seat goes. Not a permission."),
    ).toBeDefined();
    expect(field("Jira project")).toBeDefined();
    expect(field("Confluence space")).toBeDefined();
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
    const doc = connected();
    doc.integrations!.mattermost = {
      enabled: true,
      url: "https://chat.example.com",
      team: "acme",
      provisioning: { username_prefix: "Agent-" },
    };
    doc.units![0]!.roles![1]!.integrations!.mattermost = {
      bot_token: "__redacted__",
      channel: "eng",
    };
    edit(keyedState(doc), "seat:dev");
    const username = field("Bot username") as HTMLInputElement;
    expect(username.disabled).toBe(false);
    expect(username.value).toBe("");
    expect(
      screen.getByText(
        "The engine provisions a bot only where its token names a secret store entry, so this one is managed by hand. Empty uses agent-dev.",
      ),
    ).toBeDefined();
  });

  test("a tier on a seat with no GitHub block enrols it, and says so", () => {
    const doc = connected();
    edit(keyedState(doc), "seat:sre");
    expect(screen.queryByText(/This enrols the seat in GitHub/)).toBeNull();
    choose("Access tier", "Review");
    expect(
      screen.getByText(/This enrols the seat in GitHub. Create its app from Integrations./),
    ).toBeDefined();
    cleanup();

    // A repository alone writes the block as well, so it enrols the seat too.
    edit(keyedState(doc), "seat:sre");
    type("New repository", "acme/api");
    fireEvent.keyDown(field("New repository"), { key: "Enter" });
    expect(screen.getByText(/This enrols the seat in GitHub/)).toBeDefined();
  });

  test("a Slack channel needs the seat's own app, and the Datadog fallback is named on its seat", () => {
    edit(keyedState(connected()), "seat:sre");
    expect(screen.queryByLabelText(labelled("Slack channel ID"))).toBeNull();
    expect(screen.getByText(/has no Slack app of its own/)).toBeDefined();
    // The same for a Mattermost channel, which a bot of the seat's own carries.
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
    const off = connected();
    off.integrations = { ...off.integrations, datadog: { enabled: false, route_to: "sre" } };
    edit(keyedState(off), "seat:sre");
    expect(
      screen.getByText("Datadog is switched off, so no alert wakes a fallback seat."),
    ).toBeDefined();
    expect(screen.queryByText("This seat is the Datadog fallback.")).toBeNull();
  });

  test("no credential reaches the page: not a mask, not a reference, not the literal half of a partial one", () => {
    const view = edit(keyedState(connected()), "seat:dev");
    const html = view.container.ownerDocument.body.innerHTML;
    for (const secret of [
      "__redacted__",
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

describe("problems", () => {
  /**
   * Whether any control on screen points at this alert as its description.
   *
   * That is what "attached to a field" MEANS: the design system's field gives
   * its error an id and names it in the control's `aria-describedby`, so a
   * reader on the control hears it. The question used to be asked as
   * `closest(".field")`, the class the engine's own field drew, which has no
   * owner in this tree any more: the answer became null for every alert and
   * the two cases below passed while asserting nothing at all.
   */
  function describesAControl(alert: HTMLElement): boolean {
    return alert.id !== "" && document.querySelector(`[aria-describedby~="${alert.id}"]`) !== null;
  }

  // A problem about a field this form does not draw must not be attached to
  // one: it would be reported nowhere a reader can see it.
  test("a problem about a field the form does not draw is listed at the top", () => {
    // The fixture company has not connected Jira, so the seat's Jira field is
    // not drawn at all.
    const state = checkWithProblems(keyedState(fixtureCompany()), [
      problemAt(
        ["units", 0, "roles", 1, "integrations", "jira", "project"],
        "units[0].roles[1].integrations.jira.project: names no project",
      ),
    ]);
    edit(state, "seat:dev");
    const alerts = screen.getAllByRole("alert");
    expect(alerts).toHaveLength(1);
    expect(alerts[0]!.textContent).toContain("names no project");
    expect(describesAControl(alerts[0]!)).toBe(false);
    cleanup();

    // The same for a unit, whose Confluence field is not drawn either.
    const unit = checkWithProblems(keyedState(fixtureCompany()), [
      problemAt(
        ["units", 0, "integrations", "confluence", "space"],
        "units[0].integrations.confluence.space: names no space",
      ),
    ]);
    edit(unit, "unit:Engineering");
    const unitAlerts = screen.getAllByRole("alert");
    expect(unitAlerts).toHaveLength(1);
    expect(describesAControl(unitAlerts[0]!)).toBe(false);
  });

  test("a schedule problem is shown in the schedules panel, where the toggle that fixes it is", () => {
    const state = checkWithProblems(keyedState(fixtureCompany()), [
      problemAt(
        ["units", 0, "schedules", 0],
        "units[0].schedules[0]: schedule has no runner: the effective lead is a human seat",
      ),
    ]);
    edit(state, "unit:Engineering");
    const panel = screen.getByRole("heading", { name: "Schedules" }).closest("section")!;
    expect(within(panel as HTMLElement).getByRole("alert").textContent).toContain(
      "schedule has no runner",
    );
  });

  // A problem that reaches the top is usually about something the builder does
  // not author, so the link it carries is the only thing on screen saying
  // where it is fixed. A human seat draws no schedules panel at all.
  test("a problem the form cannot place keeps the link that says where it is fixed", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1] = {
      name: "Dev",
      kind: "human",
      contact: { github_login: "dev" },
      schedules: [{ name: "digest", cron: "0 8 * * *", task: "Digest" }],
    };
    const state = checkWithProblems(keyedState(doc), [
      problemAt(
        ["units", 0, "roles", 1, "schedules"],
        "units[0].roles[1]: a human seat must not carry schedules",
      ),
    ]);
    edit(state, "seat:dev");
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("must not carry schedules");
    expect(within(alert).getByRole("link", { name: "Open Schedules" }).getAttribute("href")).toBe(
      "#/activity/schedules",
    );
  });

  // The engine takes a draft with warnings; a field's error slot says it is
  // refused (critical, invalid, an alert). So a warning names its path at the
  // top, in the caution tone, and a refusal beside it stays a refusal.
  test("a warning is a caution at the top, never a field's error", () => {
    const warned = checkWithWarnings(keyedState(fixtureCompany()), [
      warningAt(["units", 0, "children", 0, "lead"], "units[0].children[0].lead: names no seat"),
    ]);
    edit(warned, "unit:Platform");
    const lead = field("Lead");
    expect(lead.getAttribute("aria-invalid")).toBeNull();
    expect(errorOf(lead)).toBeNull();
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
    // In the caution tone, not the critical one: a warning is a thing to look
    // at, and drawing it in the refusal colour makes every save look refused.
    // The tone is asked of the design system rather than named by its class.
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
    const state = checkWithProblems(keyedState(fixtureCompany()), [
      problemAt(
        ["units", 0, "roles", 1, "token_budget", "week"],
        "units[0].roles[1].token_budget.week: must be at least 1 token",
      ),
    ]);
    edit(state, "seat:dev");
    expect(errorOf(field("Weekly token ceiling"))?.textContent).toContain("must be at least 1");
    expect(errorOf(field("Daily token ceiling"))).toBeNull();
    expect(errorOf(field("Monthly token ceiling"))).toBeNull();
  });

  test("a problem sits beside the field it names, and the rest are listed at the top", () => {
    const state = checkWithProblems(keyedState(fixtureCompany()), [
      problemAt(["units", 0, "roles", 1, "goal"], "units[0].roles[1].goal: too vague to act on"),
      problemAt(
        ["units", 0, "roles", 1, "workers"],
        "units[0].roles[1].workers: names no template",
      ),
    ]);
    edit(state, "seat:dev");
    expect(errorOf(field("Goal"))?.textContent).toContain("too vague to act on");
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
    const state = keyedState(fixtureCompany());
    const view = edit(state, "unit:Platform");
    expect(field("Lead").textContent).toBe("No lead (inherits VP Engineering from Engineering)");
    choose("Lead", "SRE");
    type("Purpose", "Keep it running");
    apply();
    expect(view.state().log.ops).toHaveLength(1);
    const found = locate(view.state().draft, "unit:Platform");
    expect(found?.kind === "unit" && found.node.data).toMatchObject({
      lead: "SRE",
      purpose: "Keep it running",
    });
  });

  /*
   * ONE CONTROL FOR ONE STRING. The type used to be a select whose "Custom
   * type" option revealed a SECOND labelled field below it, so a custom type
   * cost two stacked controls. It is one field that offers the engine's nine
   * names and takes whatever is typed, which is exactly what the engine
   * accepts.
   */
  test("a unit type the engine does not name is typed in the same one box", () => {
    const view = edit(keyedState(fixtureCompany()), "unit:Platform");
    expect(screen.queryByLabelText(labelled("Custom type"))).toBeNull();
    type("Type", "tribe");
    apply();
    const found = locate(view.state().draft, "unit:Platform");
    expect(found?.kind === "unit" && found.node.data.type).toBe("tribe");
  });

  test("the nine names the engine knows are offered in that same box", () => {
    const view = edit(keyedState(fixtureCompany()), "unit:Platform");
    choose("Type", "Department");
    apply();
    const found = locate(view.state().draft, "unit:Platform");
    expect(found?.kind === "unit" && found.node.data.type).toBe("department");
  });

  test("renaming a unit names its masked literal credentials and links to Secrets", () => {
    const doc = fixtureCompany();
    doc.units![0]!.mcp_env = { tracker: { TOKEN: "__redacted__" } };
    const view = edit(keyedState(doc), "unit:Engineering");
    expect(screen.queryByText(/stored as literals/)).toBeNull();
    type("Name", "Product Engineering");
    expect(
      screen.getByText(
        "These credentials are stored as literals. Move each one to the secret store and reference it as ${NAME} before renaming, or the engine will refuse the save.",
      ),
    ).toBeDefined();
    expect(screen.getByText("units[0].mcp_env.tracker.TOKEN")).toBeDefined();
    expect(screen.getByRole("link", { name: "Open Secrets" }).getAttribute("href")).toBe(
      "#/admin/credentials",
    );
    expect(view.container.ownerDocument.body.innerHTML).not.toContain("__redacted__");
  });

  test("a unit's schedule says when it runs and who runs it", () => {
    const doc = fixtureCompany();
    doc.units![0]!.schedules!.push({ name: "triage", cron: "0 * * * *", task: "Triage the queue" });
    edit(keyedState(doc), "unit:Engineering");
    const standup = screen.getByRole("checkbox", { name: "Enabled: standup" });
    const triage = screen.getByRole("checkbox", { name: "Enabled: triage" });
    const described = (box: HTMLElement) =>
      document.getElementById(box.getAttribute("aria-describedby") ?? "")?.textContent;
    expect(described(standup)).toBe("0 9 * * 1-5, run by the unit lead. Run standup");
    expect(described(triage)).toBe("0 * * * *, run by each agent member. Triage the queue");
  });

  test("an empty channel says what the unit inherits, as the check reported it", () => {
    const doc = fixtureCompany();
    doc.units![0]!.channel = "eng";
    edit(keyedState(doc, { units: { "units[0]": { channel: "eng" } } }), "unit:Platform");
    const channel = field("Channel") as HTMLInputElement;
    expect(channel.placeholder).toBe("eng");
    expect(screen.getByText("Empty inherits eng from Engineering.")).toBeDefined();
  });

  test("a unit schedule is toggled as part of the edit", () => {
    const view = edit(keyedState(fixtureCompany()), "unit:Engineering");
    fireEvent.click(screen.getByRole("checkbox", { name: "Enabled: standup" }));
    apply();
    const found = locate(view.state().draft, "unit:Engineering");
    expect(found?.kind === "unit" && found.node.data.schedules?.[0]?.enabled).toBe(false);
    expect(screen.getByText(/Knowledge/)).toBeDefined();
    expect(screen.getByText("Free-text references, not a read scope.")).toBeDefined();
  });
});

// "Edit reports" is about one field of a long form, and "Choose another
// seat" in a unit's lead chip about another: each opens the editor on that
// field rather than on the name at the top.
describe("opening at a part of the form", () => {
  test("a seat opened at its reports starts on Manages, and one opened plainly on its name", () => {
    edit(keyedState(fixtureCompany()), "seat:dev", {}, "reports");
    expect(document.activeElement).toBe(screen.getByRole("combobox", { name: /^Manages/ }));
    cleanup();
    edit(keyedState(fixtureCompany()), "seat:dev");
    expect(document.activeElement).toBe(field("Name"));
  });

  test("a unit opened at its leadership starts on its lead", () => {
    edit(keyedState(fixtureCompany()), "unit:Sales", {}, "leadership");
    expect(document.activeElement).toBe(field("Lead"));
  });
});

describe("the charter", () => {
  test("renaming the company needs its consequences confirmed before Apply", () => {
    const view = edit(keyedState(fixtureCompany()), COMPANY_KEY);
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
  // already carries, or one the document stored with a space around it, is
  // not a rename of the mission edit that follows.
  test("a charter edit that renames nothing asks nothing, and renames nothing", () => {
    const renamed = builderReducer(keyedState(fixtureCompany()), {
      type: "record",
      intent: { type: "updateCompany", set: [{ path: ["name"], value: "Acme Labs" }] },
    });
    const view = edit(renamed, COMPANY_KEY);
    type("Mission", "Make more things.");
    expect(screen.queryByRole("checkbox", { name: /renaming the company/ })).toBeNull();
    apply();
    expect(view.state().log.ops).toHaveLength(2);
    cleanup();

    const padded = { ...fixtureCompany(), name: " Acme" };
    const second = edit(keyedState(padded), COMPANY_KEY);
    type("Mission", "Make more things.");
    expect(screen.queryByRole("checkbox", { name: /renaming the company/ })).toBeNull();
    apply();
    expect(second.state().draft.company.name).toBe(" Acme");
    expect(second.state().draft.company.mission).toBe("Make more things.");
    cleanup();

    // Taking the space out IS a rename: the engine derives agent ids from the
    // name exactly as stored.
    edit(keyedState(padded), COMPANY_KEY);
    type("Company name", "Acme");
    expect(
      screen.getByRole("checkbox", { name: "I understand what renaming the company does" }),
    ).toBeDefined();
  });
});
