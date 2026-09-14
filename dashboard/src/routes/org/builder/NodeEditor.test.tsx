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

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { CompanyDocument, ConfigRole } from "~/protocol/index.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { locate } from "./model/draft.ts";
import type { BuilderState } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { builderReducer } from "./model/reducer.ts";
import { NodeEditor } from "./NodeEditor.tsx";
import { renderInBuilder, type HarnessOptions } from "./testBuilder.tsx";
import { checkWithProblems, keyedState, problemAt } from "./testState.ts";

afterEach(cleanup);

function edit(state: BuilderState, key: NodeKey, options: HarnessOptions = {}) {
  const onClose = vi.fn();
  const view = renderInBuilder(state, <NodeEditor nodeKey={key} onClose={onClose} />, options);
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
  // The editor is one component in the Builder's dialog host: opening it on
  // another node must build that node's form, not keep the last one's.
  test("opening the editor on another node builds that node's form", () => {
    function Switcher() {
      const [key, setKey] = useState<NodeKey>("seat:dev");
      return (
        <>
          <button onClick={() => setKey("seat:sre")}>Open SRE</button>
          <NodeEditor nodeKey={key} onClose={() => {}} />
        </>
      );
    }
    renderInBuilder(keyedState(fixtureCompany()), <Switcher />);
    type("Goal", "Ship");
    fireEvent.click(screen.getByRole("button", { name: "Open SRE" }));
    expect((field("Goal") as HTMLTextAreaElement).value).toBe("Keep it up");
  });

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
    expect((field("Slack channel ID") as HTMLInputElement).value).toBe("C1");
    expect((field("Mattermost channel") as HTMLInputElement).value).toBe("eng");
    expect(screen.queryByLabelText(labelled("Bot username"))).toBeNull();
    expect(
      screen.getByText(
        "The bot is provisioned under this name. Changing it would make the provisioner find or create a second bot.",
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

  test("a tier on a seat with no GitHub block enrols it, and says so", () => {
    const doc = connected();
    edit(keyedState(doc), "seat:sre");
    expect(screen.queryByText(/This enrols the seat in GitHub/)).toBeNull();
    fireEvent.change(field("Access tier"), { target: { value: "review" } });
    expect(
      screen.getByText(/This enrols the seat in GitHub. Create its app from Integrations./),
    ).toBeDefined();
  });

  test("a Slack channel needs the seat's own app, and the Datadog fallback is named on its seat", () => {
    edit(keyedState(connected()), "seat:sre");
    expect(screen.queryByLabelText(labelled("Slack channel ID"))).toBeNull();
    expect(screen.getByText(/has no Slack app of its own/)).toBeDefined();
    expect(screen.getByText("This seat is the Datadog fallback.")).toBeDefined();
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

describe("the charter", () => {
  test("renaming the company needs its consequences confirmed before Apply", () => {
    const view = edit(keyedState(fixtureCompany()), COMPANY_KEY);
    type("Company name", "Acme Labs");
    const applyButton = screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement;
    expect(applyButton.disabled).toBe(true);
    fireEvent.click(
      screen.getByRole("checkbox", { name: "I understand what renaming the company does" }),
    );
    expect(applyButton.disabled).toBe(false);
    apply();
    expect(view.state().draft.company.name).toBe("Acme Labs");
    expect(view.state().log.ops).toHaveLength(1);
  });
});
