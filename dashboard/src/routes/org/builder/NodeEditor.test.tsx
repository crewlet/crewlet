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
const apply = () => fireEvent.click(screen.getByRole("button", { name: "Apply" }));
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
    expect(screen.queryByRole("dialog", { name: "Discard your changes?" })).toBeNull();
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
    expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
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
    [
      "a choice",
      () =>
        fireEvent.change(field("Access level"), {
          target: { value: "maintainer" },
        }),
    ],
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
    const prompt = screen.getByRole("dialog", { name: "Discard your changes?" });
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
    history.replaceState(null, "", "#/org?lens=builder");
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
    const link = () =>
      within(screen.getByText("GitHub is not connected.", { exact: false })).getByRole("link");
    const prompt = () => screen.findByRole("dialog", { name: "Discard your changes?" });
    // Untouched: the link simply goes.
    fireEvent.click(link());
    await waitFor(() => expect(location.hash).toBe("#/integrations"));
    expect(screen.queryByRole("dialog", { name: "Discard your changes?" })).toBeNull();
    act(() => {
      location.hash = "#/org?lens=builder";
    });
    await waitFor(() => expect(location.hash).toBe("#/org?lens=builder"));

    type("Goal", "Ship");
    fireEvent.click(link());
    const asked = await prompt();
    expect(asked.textContent).toContain("you are leaving the builder");
    // Held, and undone: the page is where it was, and so is the form.
    await waitFor(() => expect(location.hash).toBe("#/org?lens=builder"));
    fireEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Ship");

    act(() => history.back());
    fireEvent.click(within(await prompt()).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(location.hash).toBe("#/org?lens=builder"));
    expect(view.onClose).not.toHaveBeenCalled();

    act(() => history.back());
    fireEvent.click(within(await prompt()).getByRole("button", { name: "Discard changes" }));
    expect(view.onClose).toHaveBeenCalledTimes(1);
    // The move the reader asked for is made: back past the entry they were on.
    await waitFor(() => expect(location.hash).toBe("#/integrations"));
    cleanup();
    history.replaceState(null, "", "#/");
  });

  test("a schedule toggle is a change too", () => {
    const view = edit(keyedState(fixtureCompany()), "unit:Engineering");
    fireEvent.click(screen.getByRole("checkbox", { name: "Enabled: standup" }));
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByRole("dialog", { name: "Discard your changes?" })).toBeDefined();
    expect(view.onClose).not.toHaveBeenCalled();
  });

  test("Keep editing and one Escape close only the question, and the edit is still there", () => {
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
    type("Goal", "Ship");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByRole("dialog", { name: "Discard your changes?" })).toBeNull();
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Ship");

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Discard your changes?" })).toBeNull();
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

  test("a model chain says the order it is tried in, and how to change it", () => {
    const doc = connected();
    doc.units![0]!.roles![1]!.llm = ["smart", "fast"];
    edit(keyedState(doc), "seat:dev");
    expect(
      screen.getByText(
        "Tried in this order: smart, then fast. To change the order, remove a provider and choose it again.",
      ),
    ).toBeDefined();
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
      expect(screen.getByText(reason)).toBeDefined();
      expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(
        true,
      );
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
    ).toBe("#/config");
  });

  // Read only, the banner is the reason Apply is unavailable; a caption asking
  // to correct a field nobody can type in would be a second, wrong one.
  test("a read-only editor asks nobody to correct a field", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1]!.token_budget = -5;
    edit(keyedState(doc), "seat:dev", { readOnly: true });
    expect(screen.getByText(/cannot be changed right now/)).toBeDefined();
    expect(screen.queryByText("Correct the token budget first.")).toBeNull();
    cleanup();
    edit(keyedState(doc), "seat:dev");
    expect(screen.getByText("Correct the token budget first.")).toBeDefined();
  });

  test("a malformed token budget blocks Apply with the reason", () => {
    edit(keyedState(fixtureCompany()), "seat:dev");
    type("Token budget", "lots");
    expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(
      screen.getByText("Give a whole number of tokens, or leave it empty for unlimited."),
    ).toBeDefined();
  });

  test("changing the kind is its own step: the editor closes and opens it, unless the form has changes", () => {
    const view = edit(keyedState(fixtureCompany()), "seat:dev");
    fireEvent.click(screen.getByRole("button", { name: "Change to a human seat" }));
    expect(view.onClose).toHaveBeenCalledTimes(1);
    expect(view.spies.openChangeKind).toHaveBeenCalledWith("seat:dev");
    cleanup();
    edit(keyedState(fixtureCompany()), "seat:dev");
    type("Goal", "Ship");
    expect(
      (screen.getByRole("button", { name: "Change to a human seat" }) as HTMLButtonElement)
        .disabled,
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
      expect(within(fact(label)).getByRole("link").getAttribute("href")).toBe("#/config");
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
      expect(within(note).getByRole("link").getAttribute("href")).toBe("#/integrations");
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

    fireEvent.change(field("Access tier"), { target: { value: "full_access" } });
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
    fireEvent.change(field("Access tier"), { target: { value: "review" } });
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
    expect(alerts[0]!.closest(".field")).toBeNull();
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
    expect(unitAlerts[0]!.closest(".field")).toBeNull();
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
      "#/schedules",
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
    expect(lead.closest(".field")?.querySelector(".field-error")).toBeNull();
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
    const caution = screen.getByText(/names no seat/).closest(".banner") as HTMLElement;
    expect(caution.classList.contains("caution")).toBe(true);
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
    const goal = field("Goal").closest(".field") as HTMLElement;
    expect(within(goal).getByRole("alert").textContent).toContain("too vague to act on");
    const top = screen
      .getAllByRole("alert")
      .find((el) => el.textContent?.includes("names no template"));
    expect(top?.closest(".field")).toBeNull();
  });
});

describe("a unit", () => {
  test("the lead shows the lead it would inherit, and a lead change applies with the rest", () => {
    const state = keyedState(fixtureCompany());
    const view = edit(state, "unit:Platform");
    const lead = field("Lead") as HTMLSelectElement;
    expect(lead.options[0]!.textContent).toBe("No lead (inherits VP Engineering from Engineering)");
    fireEvent.change(lead, { target: { value: "SRE" } });
    type("Purpose", "Keep it running");
    apply();
    expect(view.state().log.ops).toHaveLength(1);
    const found = locate(view.state().draft, "unit:Platform");
    expect(found?.kind === "unit" && found.node.data).toMatchObject({
      lead: "SRE",
      purpose: "Keep it running",
    });
  });

  test("a custom unit type is typed in its own box and applied", () => {
    const view = edit(keyedState(fixtureCompany()), "unit:Platform");
    fireEvent.change(field("Type"), { target: { value: "__custom__" } });
    type("Custom type", "tribe");
    apply();
    const found = locate(view.state().draft, "unit:Platform");
    expect(found?.kind === "unit" && found.node.data.type).toBe("tribe");
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
      "#/secrets",
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
    const applyButton = screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement;
    expect(applyButton.disabled).toBe(true);
    // The same sentence the review asks the operator to accept before a save,
    // so the two cannot come to describe one consequence two ways.
    expect(screen.getByText(ACKNOWLEDGEMENT_TEXT.company_rename)).toBeDefined();
    fireEvent.click(
      screen.getByRole("checkbox", { name: "I understand what renaming the company does" }),
    );
    expect(applyButton.disabled).toBe(false);
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
