/**
 * The setup dialog renders a form from data, and never renders a value.
 *
 * The invariant worth breaking a build over is the second one: no route the
 * engine serves returns a credential, so any value on this page would have to
 * have come from somewhere it should not have.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { HELD, SetupDialog, fieldsFor, fillTemplate, vendorLink } from "./SetupDialog.tsx";
import type { SetupRequirement, SetupToolState } from "~/protocol/index.ts";

function req(over: Partial<SetupRequirement>): SetupRequirement {
  return {
    field: "f",
    label: "F",
    kind: "text",
    config_path: "integrations.datadog.f",
    required: true,
    present: false,
    resolved: false,
    ...over,
  };
}

const tool: SetupToolState = {
  key: "datadog",
  configured: false,
  enabled: false,
  satisfied: false,
  inbound_path: "/webhooks/datadog",
  public_url: "https://engine.example.com/webhooks/datadog",
  requirements: [
    req({
      field: "enabled",
      label: "Accept Datadog deliveries",
      kind: "toggle",
      config_path: "integrations.datadog.enabled",
    }),
    req({
      field: "webhook_token",
      label: "Shared token",
      kind: "secret",
      mintable: true,
      secret_name: "DATADOG_WEBHOOK_TOKEN",
      config_path: "integrations.datadog.webhook_token",
      blocks: "credential_missing",
    }),
    req({
      field: "route_to",
      label: "Fallback seat",
      kind: "handle",
      config_path: "integrations.datadog.route_to",
      blocks: "credential_missing",
      choices: [{ value: "sre-lead", label: "sre-lead" }],
    }),
    req({
      field: "handle_tag",
      label: "Owner tag key",
      required: false,
      config_path: "integrations.datadog.handle_tag",
    }),
  ],
};

function stubFetch(handler: (path: string, init?: RequestInit) => Response) {
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "http://engine.test").pathname;
    return Promise.resolve(handler(path, init));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return spy;
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.restoreAllMocks();
});

// EVERY WORD COMES FROM THE ENGINE. Nothing about any third-party app is in the
// component, which is what makes adding one a Go change and no screen work.
test("the form is rendered from the requirement list", () => {
  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText("Fallback seat")).toBeDefined();
  expect(screen.getByText("Owner tag key")).toBeDefined();
  expect(screen.getByText(/Accept Datadog deliveries/)).toBeDefined();
});

// A MINTABLE SECRET HAS NO INPUT. Asking a person to invent a shared token is
// asking them to invent a password.
test("a mintable secret is not on the form at all", () => {
  const { container } = render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  // NO ROW, not a row without an input. It was a label over a sentence
  // saying the engine would handle it, which is the engine narrating its own
  // plumbing in the middle of a form somebody is filling in.
  expect(screen.queryByText("Shared token")).toBeNull();
  expect(screen.queryByText(/generates this/)).toBeNull();
  expect(container.querySelectorAll('input[type="password"]').length).toBe(0);
});

// A HELD CREDENTIAL SHOWS THAT IT IS HELD, and never what it is.
//
// The engine does not send a credential back, so an empty box under a
// required label read as an unanswered question on a form that is already
// complete. The dots say "there is one", which is the only thing this
// process actually knows.
test("a stored credential is prefilled with dots, never a value", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "GitLab",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({
                field: "admin_token",
                label: "Group Owner token",
                kind: "secret",
                secret_name: "GITLAB_ADMIN_TOKEN",
                required: true,
                present: true,
              }),
            ],
          },
        },
      ]}
      title="GitLab"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  const input = screen.getByLabelText("Group Owner token") as HTMLInputElement;
  expect(input.value).toBe(HELD);
  expect(input.type).toBe("password");
  // Still required, because the app says so and that does not change once a
  // company has answered it.
  expect(screen.queryByText("(optional)")).toBeNull();
  // And the form says nothing about where it is kept.
  expect(screen.queryByText(/Stored as/)).toBeNull();
  expect(screen.queryByText(/GITLAB_ADMIN_TOKEN/)).toBeNull();
});

// AND LEAVING THE DOTS ALONE WRITES NOTHING. Submitting the placeholder
// would rewrite a working key with sixteen bullet characters.
test("untouched dots are not submitted", async () => {
  const spy = stubFetch(
    () => new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 }),
  );
  render(
    <SetupDialog
      sections={[
        {
          name: "GitLab",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({ field: "admin_token", label: "Owner token", kind: "secret", present: true }),
              req({
                field: "url",
                label: "Instance",
                kind: "url",
                present: true,
                value: "https://g",
              }),
            ],
          },
        },
      ]}
      title="GitLab"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  fireEvent.change(screen.getByLabelText("Instance"), { target: { value: "https://gitlab.com" } });
  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());

  const body = JSON.parse(String(spy.mock.calls[0]?.[1]?.body)) as {
    values: Record<string, string>;
  };
  expect(body.values.url).toBe("https://gitlab.com");
  expect("admin_token" in body.values).toBe(false);
});

// A SHARED VALUE IS ASKED ONCE AND WRITTEN TO EVERY SURFACE.
//
// Atlassian is two config blocks and one product family: the account email
// and the API token are the same Atlassian account, and asking for each of
// them twice under two headings in one dialog is one question with two
// inputs, which eventually holds two answers.
test("a shared field appears once and is submitted to both surfaces", async () => {
  const spy = stubFetch(
    () => new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 }),
  );
  const atlassian = (key: string, own: string) => ({
    ...tool,
    key,
    configured: true,
    requirements: [
      req({ field: "url", label: own, kind: "url", connect: true }),
      req({ field: "email", label: "Account email", kind: "text", connect: true, shared: true }),
    ],
  });
  render(
    <SetupDialog
      sections={[
        { name: "Jira", tool: atlassian("jira", "Jira site") },
        { name: "Confluence", tool: atlassian("confluence", "Confluence site") },
      ]}
      title="Atlassian"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );

  // ONCE on screen, though both surfaces declare it.
  expect(screen.getAllByText("Account email").length).toBe(1);
  // And each surface still asks for its own address, which is not shared.
  expect(screen.getByText("Jira site")).toBeTruthy();
  expect(screen.getByText("Confluence site")).toBeTruthy();

  fireEvent.change(screen.getByLabelText(/Account email/), {
    target: { value: "ops@example.com" },
  });
  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  await vi.waitFor(() => expect(spy.mock.calls.length).toBe(2));

  // WRITTEN TO BOTH. Submitting only what was on screen would leave the
  // second block without the value the first one collected.
  for (const call of spy.mock.calls) {
    const body = JSON.parse(String(call[1]?.body)) as { values: Record<string, string> };
    expect(body.values.email).toBe("ops@example.com");
  }
});

// TWO SURFACES, ONE FIELD NAME, TWO DIFFERENT VALUES.
//
// Jira and Confluence both declare `url` and they are different addresses.
// Keyed on the name alone the second section's value overwrote the first's,
// so the Jira site input showed the Confluence address — and saving would
// have written it into Jira's block.
test("a field name shared by two surfaces holds two values", () => {
  const surface = (key: string, label: string, value: string) => ({
    ...tool,
    key,
    configured: true,
    requirements: [req({ field: "url", label, kind: "url", connect: true, present: true, value })],
  });
  render(
    <SetupDialog
      sections={[
        { name: "Jira", tool: surface("jira", "Jira site", "https://acme.atlassian.net") },
        {
          name: "Confluence",
          tool: surface("confluence", "Confluence site", "https://acme.atlassian.net/wiki"),
        },
      ]}
      title="Atlassian"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect((screen.getByLabelText("Jira site") as HTMLInputElement).value).toBe(
    "https://acme.atlassian.net",
  );
  expect((screen.getByLabelText("Confluence site") as HTMLInputElement).value).toBe(
    "https://acme.atlassian.net/wiki",
  );
});

// A HIDDEN FIELD IS WRITTEN WITHOUT BEING ASKED FOR.
//
// Connecting an integration and leaving it switched off is not a thing
// anybody means, so the toggle was a control whose only sensible answer was
// the one it already had. Dropping it from the form must not drop it from
// the submission: the block needs the field, and a company that has just
// connected an app wants its route open.
test("a hidden field is submitted from its default and never rendered", async () => {
  const spy = stubFetch(
    () => new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 }),
  );
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [
              req({ field: "site", label: "Datadog region", kind: "text", connect: true }),
              req({
                field: "enabled",
                label: "Accept Datadog deliveries",
                kind: "toggle",
                hidden: true,
                default: "true",
              }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.queryByText("Accept Datadog deliveries")).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());
  const body = JSON.parse(String(spy.mock.calls[0]?.[1]?.body)) as {
    values: Record<string, string>;
  };
  expect(body.values.enabled).toBe("true");
});

// A LINK THAT NEEDS AN ANSWER IS NOT A LINK UNTIL IT HAS ONE.
//
// Atlassian's API keys live at a per-organization address, so until somebody
// has typed the organization id there is no page to open — and a link to the
// console's front door sends them somewhere they then have to navigate out
// of, which is worse than no link at all.
test("a templated vendor link waits for the field it needs", () => {
  const url = "https://admin.atlassian.com/o/{org_id}/api-keys";
  const same = (f: string) => f;
  expect(vendorLink(url, {}, same)).toBe("");
  expect(vendorLink(url, { org_id: "   " }, same)).toBe("");
  expect(vendorLink(url, { org_id: "f124-abc" }, same)).toBe(
    "https://admin.atlassian.com/o/f124-abc/api-keys",
  );
  // AND AN ANSWER IS ESCAPED, because it lands in a path.
  expect(vendorLink(url, { org_id: "a/b" }, same)).toBe(
    "https://admin.atlassian.com/o/a%2Fb/api-keys",
  );
  // A plain link is untouched.
  expect(vendorLink("https://example.com/keys", {}, same)).toBe("https://example.com/keys");
});

// A FIX NARROWS TO THE FIELDS THAT CLEAR THE FINDING, which is what the
// blocks field on a requirement is for.
test("a finding narrows the form to what clears it", () => {
  const shown = fieldsFor(tool.requirements, "credential_missing");
  expect(shown.map((r) => r.field)).toEqual(["webhook_token", "route_to"]);
  // And a finding nothing clears falls back to the whole list rather than an
  // empty dialog.
  expect(fieldsFor(tool.requirements, "grant_short").length).toBe(tool.requirements.length);
});

// THE SUBMISSION SENDS A MINT REQUEST, NOT A VALUE, and sends nothing for a
// field the operator did not touch.
test("submitting asks for the mint and sends only what was filled in", async () => {
  const spy = stubFetch(
    () => new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 }),
  );
  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());

  const init = spy.mock.calls[0]?.[1];
  const body = JSON.parse(String(init?.body)) as {
    values: Record<string, string>;
    generate: string[];
  };
  expect(body.generate).toEqual(["webhook_token"]);
  // The toggle defaults on, because connecting something and leaving it off
  // is not what the button says.
  expect(body.values.enabled).toBe("true");
  // Nothing was typed into these, so nothing is sent: a field sent back
  // unchanged is a field rewritten for no reason.
  expect(body.values.route_to).toBeUndefined();
  expect(body.values.handle_tag).toBeUndefined();
});

// A LITERAL REFUSAL LANDS ON THE FIELD, not in a banner nobody connects to an
// input.
test("a literal_in_config refusal is shown against its field", async () => {
  stubFetch(
    () =>
      new Response(
        JSON.stringify({
          error: "literal_in_config",
          path: "integrations.datadog.webhook_token",
        }),
        { status: 409 },
      ),
  );
  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  // IN THE BANNER, because this is the one field with no row to sit under:
  // a generated credential is exactly what the engine refuses this way, and
  // the operator never sees the slot it will not overwrite. Beside a field
  // that is not rendered, the refusal was invisible.
  expect(await screen.findByText(/holds a value at/)).toBeDefined();
  expect(screen.getByText(/integrations.datadog.webhook_token/)).toBeDefined();
});

// THE FORM SAYS WHAT CONNECTING DOES BEFORE IT ASKS FOR ANYTHING.
//
// A form that opens with a credential field asks for a secret before saying
// what it is for. The sentence comes from the engine, with the app whose
// requirements it introduces, because this screen knows nothing about any
// app — the same rule every label and help line here already follows.
test("the form opens with the engine's own summary", () => {
  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool: { ...tool, summary: "Datadog sends firing monitors." } }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText("Datadog sends firing monitors.")).toBeTruthy();
});

// AND AN EXTERNAL LINK NAMES THE APP. "Open the third-party app" makes a
// reader guess which one a form with three sections is sending them to.
test("an external link names the app it opens", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [
              req({
                field: "webhook_token",
                label: "Shared token",
                kind: "secret",
                vendor_url: "https://app.datadoghq.com/integrations/webhooks",
              }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByRole("link", { name: "Open Datadog" })).toBeTruthy();
});

// ONE FORM, CONNECT FIELDS FIRST.
//
// The connect form and the settings form are the same form, because two
// forms behind one dialog made an app's settings a screen its operator had
// never seen. What the split was protecting is the ORDER: asking which seat
// an alert wakes while somebody is pasting an API key asks the second
// question before the first is answered, and ordering answers that without
// putting the routing fields somewhere a person cannot reach them.
test("the connect form leads with the fields that establish the connection", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: false,
            requirements: [
              // DECLARED OUT OF ORDER on purpose: the form's grouping is its
              // own, not a property of how an app happened to list them.
              req({ field: "route_to", label: "Fallback seat", kind: "handle" }),
              req({ field: "site", label: "Datadog region", kind: "choice", connect: true }),
              req({ field: "handle_tag", label: "Owner tag key", kind: "text" }),
              req({ field: "api_key", label: "API key", kind: "secret", connect: true }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );

  // Everything is here, which is the whole change: none of it is reachable
  // anywhere else.
  for (const label of ["Datadog region", "API key", "Fallback seat", "Owner tag key"]) {
    expect(screen.getByText(label)).toBeTruthy();
  }

  // And the connect fields lead, in the order the app declared them.
  const labels = [...document.querySelectorAll("label")].map((l) => l.textContent);
  expect(labels).toEqual(["Datadog region", "API key", "Fallback seat", "Owner tag key"]);
});

// THE SAME FORM EITHER WAY. A connected app's settings must not be a screen
// its operator has never seen: same fields, same order, same labels, with
// only the title and the button naming which of the two things is happening.
test("connecting and managing render one identical form", () => {
  // EVERY STATE A FIELD CAN BE IN, on the configured side: a stored
  // credential, a stored mintable credential, a stored setting, a stored
  // choice. Each one of those was at some point what made the settings form
  // a different screen.
  const requirements = (present: boolean) => [
    req({
      field: "site",
      label: "Datadog region",
      kind: "choice",
      connect: true,
      required: false,
      present,
      value: present ? "datadoghq.com" : undefined,
      choices: [{ value: "datadoghq.com", label: "datadoghq.com" }],
    }),
    req({
      field: "api_key",
      label: "API key",
      kind: "secret",
      connect: true,
      secret_name: "DATADOG_API_KEY",
      help: "Create one on your",
      link_text: "API keys page",
      vendor_url: "https://example.com/keys",
      present,
    }),
    req({
      field: "webhook_token",
      label: "Shared token",
      kind: "secret",
      mintable: true,
      secret_name: "DATADOG_WEBHOOK_TOKEN",
      present,
    }),
    req({ field: "enabled", label: "Accept deliveries", kind: "toggle", present }),
    req({
      field: "route_to",
      label: "Fallback seat",
      kind: "handle",
      present,
      value: present ? "sre-lead" : undefined,
      choices: [{ value: "sre-lead", label: "SRE Lead (sre-lead)" }],
    }),
  ];

  // THE WHOLE SUBTREE, character for character, not a list of labels and not
  // a list of controls. Both of those passed while the two forms visibly
  // differed: the labels matched while one had inputs and the other had
  // static text, and then the controls matched while the descriptions
  // differed. The only assertion that cannot be satisfied by a form that
  // looks different is the form itself.
  const formOf = (configured: boolean): string => {
    const view = render(
      <SetupDialog
        sections={[
          {
            name: "Datadog",
            tool: { ...tool, configured, requirements: requirements(configured) },
          },
        ]}
        title="Datadog"
        onClose={() => {}}
        onDone={() => {}}
      />,
    );
    const form = view.baseElement.querySelector(".int-form");
    // Two things are allowed to differ, and only two. React mints an id per
    // rendered field, so a second render of the same form has different
    // ones. And a credential this company already holds is prefilled with
    // dots — the field, its label, its help and its required mark are all
    // the same, and what it CONTAINS is the state the form is showing.
    const html = (form?.innerHTML ?? "").replace(/\b(id|for|aria-describedby|value)="[^"]*"/g, "");
    view.unmount();
    return html;
  };
  const connecting = formOf(false);
  const settings = formOf(true);
  expect(connecting.length).toBeGreaterThan(0);
  if (connecting !== settings) {
    const a = connecting.split("><");
    const b = settings.split("><");
    const diff = a
      .map((x, i) => (x === b[i] ? "" : `\n  connect : ${x}\n  settings: ${b[i]}`))
      .filter(Boolean);
    throw new Error("the two forms differ:" + diff.join(""));
  }
});

// THE FORM ASKS WHAT CONNECTS THE APP, AND FOLDS THE REST AWAY.
//
// The console asks three things to connect Datadog. This engine also receives
// Datadog's deliveries and routes its alerts, so it has four more fields the
// console has no equivalent for, and all seven in one column made connecting
// an app read as filling in a configuration file.
test("only the connect fields are open, the rest are folded away", () => {
  const { baseElement } = render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [
              req({ field: "site", label: "Datadog region", kind: "choice", connect: true }),
              req({ field: "api_key", label: "API key", kind: "secret", connect: true }),
              req({ field: "enabled", label: "Accept deliveries", kind: "toggle" }),
              req({ field: "route_to", label: "Fallback seat", kind: "handle" }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );

  const more = baseElement.querySelector("details.int-form-more") as HTMLDetailsElement;
  expect(more).toBeTruthy();
  expect(more.open).toBe(false);

  // The connect fields are in the form itself, not behind the disclosure.
  for (const label of ["Datadog region", "API key"]) {
    const field = screen.getByText(label).closest(".field");
    expect(more.contains(field)).toBe(false);
  }
  // And everything that configures what happens over the connection is.
  for (const label of ["Accept deliveries", "Fallback seat"]) {
    const field = screen.getByText(label).closest(".field");
    expect(more.contains(field)).toBe(true);
  }
});

// AN APP WITH NOTHING BUT CONNECT FIELDS HAS NO DISCLOSURE. A fold over
// nothing is a control that opens an empty box.
test("a form with one group folds nothing away", () => {
  const { baseElement } = render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [
              req({ field: "site", label: "Datadog region", kind: "choice", connect: true }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(baseElement.querySelector("details.int-form-more")).toBeNull();
  expect(screen.getByText("Datadog region")).toBeTruthy();
});

// A SETTINGS FORM OPENS ON WHAT IS ALREADY SET.
//
// It opened on nothing: a region and a fallback seat the document held both
// read "Choose one", so saving the form blanked whatever the operator had not
// retyped. A blank form over a live configuration is a data loss waiting for
// somebody to change one field.
test("a stored setting is what the form opens on", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({
                field: "route_to",
                label: "Fallback seat",
                kind: "handle",
                present: true,
                value: "sre-lead",
                // A handle field is a picker, and the engine fills it with
                // the company's roster.
                choices: [{ value: "sre-lead", label: "SRE Lead (sre-lead)" }],
              }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect((screen.getByLabelText(/Fallback seat/) as HTMLInputElement).value).toBe("sre-lead");
});

// AND A CREDENTIAL LEFT ALONE IS NOT SENT.
//
// The input opens empty because the engine never sends a credential back, so
// an empty one has to mean "keep it" rather than "set it to nothing". This is
// what the Replace button used to buy, and it has to survive the button.
test("an untouched credential is not written", async () => {
  const spy = stubFetch(
    () => new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 }),
  );
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({ field: "api_key", label: "API key", kind: "secret", present: true }),
              req({
                field: "handle_tag",
                label: "Owner tag key",
                kind: "text",
                present: true,
                value: "crewlet",
              }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  fireEvent.change(screen.getByLabelText(/Owner tag key/), { target: { value: "owner" } });
  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());

  const body = JSON.parse(String(spy.mock.calls[0]?.[1]?.body)) as {
    values: Record<string, string>;
  };
  expect(body.values.handle_tag).toBe("owner");
  expect("api_key" in body.values).toBe(false);
});

// A PAUSED APP OPENS PAUSED, and an unconfigured one opens on.
//
// The toggle was seeded "true" unconditionally, so opening the settings of a
// deliberately paused integration and saving anything else switched it back
// on. The two cases are different questions: nothing configured is a default,
// and something configured is a state to show.
test("the toggle opens on what the app is set to", () => {
  const enabled = req({ field: "enabled", label: "Accept deliveries", kind: "toggle" });
  const open = (configured: boolean, value: string) => {
    const view = render(
      <SetupDialog
        sections={[
          {
            name: "Datadog",
            tool: { ...tool, configured, requirements: [{ ...enabled, value, present: false }] },
          },
        ]}
        title="Datadog"
        onClose={() => {}}
        onDone={() => {}}
      />,
    );
    const got = (screen.getByLabelText(/Accept deliveries/) as HTMLSelectElement).value;
    view.unmount();
    return got;
  };
  // Nothing configured: on, whatever an unset block reports.
  expect(open(false, "false")).toBe("true");
  // Configured and paused: paused.
  expect(open(true, "false")).toBe("false");
  expect(open(true, "true")).toBe("true");
});

// NOTHING TO JUDGE IS NOT "UNCONFIGURED".
//
// every() is true of an empty list, so a tool whose setup read was refused
// arrived with no sections and called itself unconnected: the dialog opened
// titled "Connect X", with a Connect button, over an integration the card
// beside it was reporting Connected.
test("a dialog with no sections does not claim to be connecting", () => {
  render(<SetupDialog sections={[]} title="Datadog" onClose={() => {}} onDone={() => {}} />);
  expect(screen.queryByRole("button", { name: "Connect" })).toBeNull();
  expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
});

// AND A CONFIGURED APP SHOWS EVERY FIELD, which it now shares with an
// unconfigured one.
test("a configured app shows every field", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({ field: "site", label: "Datadog region", kind: "choice", connect: true }),
              req({ field: "route_to", label: "Fallback seat", kind: "handle" }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText("Datadog region")).toBeTruthy();
  expect(screen.getByText("Fallback seat")).toBeTruthy();
});

// AN APP THAT DECLARES NONE shows all of them. Most apps' every field is part
// of connecting, and an empty form is worse than a long one.
test("an app with no connect fields declared shows the whole form", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Jira",
          tool: {
            ...tool,
            configured: false,
            requirements: [req({ field: "url", label: "Jira site", kind: "url" })],
          },
        },
      ]}
      title="Jira"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText("Jira site")).toBeTruthy();
});

// THE BUTTON SAYS WHAT PRESSING IT DOES. A form of connection fields under a
// button reading "Save" is the wrong promise; so is "Connect" over a form of
// settings for an app that is already connected.
test("the button reads Connect before there is a connection and Save after", () => {
  const { unmount } = render(
    <SetupDialog
      sections={[{ name: "Datadog", tool: { ...tool, configured: false } }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByRole("button", { name: "Connect" })).toBeTruthy();
  unmount();

  render(
    <SetupDialog
      sections={[{ name: "Datadog", tool: { ...tool, configured: true } }]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
});

// A DEFAULT IS OFFERED WHERE THERE IS NOTHING, and only there.
//
// Datadog's region is US1 for most organizations, so "Choose one" makes
// everybody answer a question with an obvious answer. It is seeded into the
// FORM rather than assumed on the far side, so what is submitted is what was
// on screen.
test("a field with a default opens holding it", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: false,
            requirements: [
              req({
                field: "site",
                label: "Datadog region",
                kind: "choice",
                connect: true,
                default: "datadoghq.com",
                choices: [
                  { value: "datadoghq.com", label: "datadoghq.com" },
                  { value: "datadoghq.eu", label: "datadoghq.eu" },
                ],
              }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("datadoghq.com");
});

// AND NOT OVER AN ANSWER THIS COMPANY ALREADY GAVE. Offering the common value
// on top of a working setting quietly proposes changing it.
test("a field this company has answered keeps its answer", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({
                field: "site",
                label: "Datadog region",
                kind: "choice",
                connect: true,
                default: "datadoghq.com",
                present: true,
                choices: [
                  { value: "datadoghq.com", label: "datadoghq.com" },
                  { value: "datadoghq.eu", label: "datadoghq.eu" },
                ],
              }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  // Empty: the form sends only what was touched, so an untouched field
  // carrying a stored value must not arrive pre-filled with the default.
  expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("");
});

// The description quotes the value, so it has to follow the value. A tag key
// changed to `owner` left the example reading `crewlet:…`, describing the
// setting the operator had just replaced.
test("fillTemplate fills a placeholder from what the form holds", () => {
  const said = fillTemplate(
    'A monitor tagged "{handle_tag}:<seat handle>" wakes that seat.',
    () => "owner",
  );
  expect(said).toBe('A monitor tagged "owner:<seat handle>" wakes that seat.');
});

// A blank optional field is not an absent setting: the engine reads the
// default, so that is what the sentence has to quote.
test("fillTemplate falls back to the default a blank field is read as", () => {
  expect(fillTemplate("tag {handle_tag}", () => "crewlet")).toBe("tag crewlet");
});

// Visibly unresolved rather than silently wrong: a template naming a field
// with neither a value nor a default is an authoring mistake.
test("fillTemplate leaves a placeholder nothing answers alone", () => {
  expect(fillTemplate("tag {nothing}", () => "")).toBe("tag {nothing}");
});

test("fillTemplate passes text with no placeholder through unchanged", () => {
  expect(fillTemplate("plain words", () => "x")).toBe("plain words");
});

// A `{field}` reference follows the FIELD IT NAMES, across surfaces. Jira's
// site address cites `{org_id}`, which Atlassian asks once for the whole tool
// and keeps under the bare name. Resolved against the referring section, the
// lookup was `jira:org_id`, which nothing answers, so the link was dropped
// however much had been typed.
test("a link cites a shared field from another section", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Organization",
          tool: {
            ...tool,
            key: "atlassian",
            requirements: [
              req({
                field: "org_id",
                label: "Organization id",
                kind: "id",
                shared: true,
                present: true,
                value: "org-42",
              }),
            ],
          },
        },
        {
          name: "Jira",
          tool: {
            ...tool,
            key: "jira",
            requirements: [
              req({
                field: "url",
                label: "Jira site",
                kind: "url",
                link_text: "App URLs",
                vendor_url: "https://admin.atlassian.com/o/{org_id}/product-urls",
              }),
            ],
          },
        },
      ]}
      title="Atlassian"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  const link = screen.getByText("App URLs") as HTMLAnchorElement;
  expect(link.href).toBe("https://admin.atlassian.com/o/org-42/product-urls");
});

// AND STAYS UNLINKED UNTIL THERE IS ONE, which is the rule the organization
// id already set: a per-organization address with a hole in it opens the
// console's front door, somewhere a person then has to navigate out of.
//
// THE WORDS STAY. The name of the page is the useful half and is true
// whether or not this form can open it yet; dropping it left the sentence
// it ends ("Find the Jira site value under") stopping at nothing.
test("a link citing an unanswered field keeps its words and loses its anchor", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Jira",
          tool: {
            ...tool,
            key: "jira",
            requirements: [
              req({
                field: "url",
                label: "Jira site",
                kind: "url",
                help: "Find the Jira site value under",
                link_text: "App URLs",
                vendor_url: "https://admin.atlassian.com/o/{org_id}/product-urls",
              }),
            ],
          },
        },
      ]}
      title="Atlassian"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.queryByRole("link", { name: "App URLs" })).toBeNull();
  expect(screen.getByText(/Find the Jira site value under App URLs\./)).toBeTruthy();
});

// A value SEVERAL surfaces share renders after them, not inside the first.
// Claimed by the first section, Atlassian's account email and API token sat
// inside Jira and pushed the Confluence site below them, so the two site
// addresses (one question asked twice) were split by two fields belonging to
// neither.
test("a value shared across surfaces renders after them", () => {
  const shared = [
    req({ field: "email", label: "Account email", shared: true, connect: true }),
    req({ field: "token", label: "API token", kind: "secret", shared: true, connect: true }),
  ];
  render(
    <SetupDialog
      sections={[
        {
          name: "Jira",
          tool: {
            ...tool,
            key: "jira",
            requirements: [
              req({ field: "url", label: "Jira site", kind: "url", connect: true }),
              ...shared,
            ],
          },
        },
        {
          name: "Confluence",
          tool: {
            ...tool,
            key: "confluence",
            requirements: [
              req({ field: "url", label: "Confluence site", kind: "url", connect: true }),
              ...shared,
            ],
          },
        },
      ]}
      title="Atlassian"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  const labels = screen.getAllByText(/^(Jira site|Confluence site|Account email|API token)$/);
  expect(labels.map((l) => l.textContent)).toEqual([
    "Jira site",
    "Confluence site",
    "Account email",
    "API token",
  ]);
});
