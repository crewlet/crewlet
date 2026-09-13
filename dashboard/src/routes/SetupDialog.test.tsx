/**
 * The setup dialog renders a form from data, and never renders a value.
 *
 * The invariant worth breaking a build over is the second one: no route the
 * engine serves returns a credential, so any value on this page would have to
 * have come from somewhere it should not have.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { ToastProvider } from "~/ui/Toast";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  HELD,
  SetupDialog,
  fieldsFor,
  fillTemplate,
  gateOpen,
  neededField,
  shownField,
  vendorLink,
} from "./SetupDialog.tsx";
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

/**
 * The dialog's WRITE, which is not always its first request.
 *
 * It reads the company's sealed entry NAMES when it opens, so typing `$` in
 * a box can offer them, and a test indexing calls[0] was asserting against
 * that read. What every one of these is about is what the form SENDS, so
 * they ask for it by method rather than by position.
 */
function sent(spy: ReturnType<typeof stubFetch>): RequestInit | undefined {
  for (const [, init] of spy.mock.calls) {
    const method = String(init?.method ?? "GET").toUpperCase();
    if (method !== "GET") return init;
  }
  return undefined;
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
test("a stored credential shows the dots as a PLACEHOLDER, never a value", () => {
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
  // EMPTY, WITH THE DOTS AS THE PLACEHOLDER. Seeded as the VALUE, the
  // sentinel had to be recognised on the way out by an exact string compare
  // — so any edit that left the box holding something other than exactly
  // those sixteen characters submitted the whole string as the credential,
  // and two ordinary gestures did that: typing at the caret without
  // select-all, and the ${NAME} completion, which keeps everything before
  // the `$`.
  expect(input.value).toBe("");
  expect(input.placeholder).toBe(HELD);
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

  const body = JSON.parse(String(sent(spy)?.body)) as {
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

  // WRITTEN TO BOTH. Submitting only what was on screen would leave the
  // second block without the value the first one collected.
  //
  // COUNTED AS WRITES rather than as requests: the dialog also READS the
  // company's sealed entry names when it opens, so waiting for two requests
  // stopped waiting before the second block was written. See [sent].
  const writes = () =>
    spy.mock.calls.filter(([, init]) => String(init?.method ?? "GET").toUpperCase() !== "GET");
  await vi.waitFor(() => expect(writes().length).toBe(2));
  for (const [, init] of writes()) {
    const body = JSON.parse(String(init?.body)) as { values: Record<string, string> };
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
  // Without the scheme, which a url field wears as an affix rather than
  // holding in the box. What this asserts is that the two boxes hold
  // DIFFERENT values, which is what one field name across two surfaces got
  // wrong.
  expect((screen.getByLabelText("Jira site") as HTMLInputElement).value).toBe("acme.atlassian.net");
  expect((screen.getByLabelText("Confluence site") as HTMLInputElement).value).toBe(
    "acme.atlassian.net/wiki",
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
  const body = JSON.parse(String(sent(spy)?.body)) as {
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

// CONNECT FIELDS COME FIRST, AND NOTHING IS HIDDEN.
//
// This used to assert the opposite half — that a finding narrowed the form to
// the fields clearing it — over an argument nothing ever passed, because the
// Fix control that would have passed it was removed. What the function
// actually promises is an ordering: the person pasting an API key finds it at
// the top, and the person changing a fallback seat can still reach it.
test("connect fields come first and every field survives", () => {
  // DECLARED OUT OF ORDER ON PURPOSE, and not read off the shared fixture.
  // That fixture already lists its connect fields first, so asserting against
  // its own order passes whatever this function does — the first version of
  // this test did exactly that and stayed green when the partition was
  // deleted. A list the partition has to actually move is the only one that
  // can fail.
  const reqs = [
    req({ field: "handle_tag", connect: false }),
    req({ field: "webhook_token", connect: true }),
    req({ field: "route_to", connect: false }),
    req({ field: "site", connect: true }),
  ];

  const shown = fieldsFor(reqs);

  expect(shown.map((r) => r.field)).toEqual(["webhook_token", "site", "handle_tag", "route_to"]);
  // AND THE ORDER WITHIN EACH GROUP SURVIVES, which is the other half: a
  // stable partition, so an app's own declared sequence is not reshuffled.
  expect(shown.length).toBe(reqs.length);
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

  const init = sent(spy);
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
    // ones. And a credential this company already holds shows the dots as
    // its PLACEHOLDER — the field, its label, its help and its required
    // mark are all the same, and what it SHOWS is the state the form is
    // reporting.
    const html = (form?.innerHTML ?? "")
      .replace(/\b(id|for|aria-describedby|value|placeholder)="[^"]*"/g, "")
      // The removals leave the gaps their attributes sat in.
      .replace(/\s+/g, " ");
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

  const body = JSON.parse(String(sent(spy)?.body)) as {
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

// A CREDENTIAL'S REFERENCE IS NOT THE CREDENTIAL. The engine sends `${NAME}`
// back for a field pointing at the sealed store, and showing it is how an
// operator tells "this reads SHARED_TOKEN" from "type here to replace what is
// behind this field".
test("a secret pointing at the store opens naming the entry it reads", () => {
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
                field: "webhook_token",
                label: "Shared token",
                kind: "secret",
                present: true,
                value: "${SHARED_TOKEN}",
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
  const input = screen.getByLabelText("Shared token") as HTMLInputElement;
  expect(input.value).toBe("${SHARED_TOKEN}");
  expect(input.value).not.toBe(HELD);
});

// AND THE DOTS SURVIVE FOR THE ONE CASE THAT HAS NO REFERENCE: a company that
// hand-wrote a literal into its config document. The engine sends no value
// there, correctly, and an empty box under a required label reads as an
// unanswered question on a form that is already complete.
test("a literal credential still shows only that it is held", () => {
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({ field: "webhook_token", label: "Shared token", kind: "secret", present: true }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  const held = screen.getByLabelText("Shared token") as HTMLInputElement;
  expect(held.value).toBe("");
  expect(held.placeholder).toBe(HELD);
});

// TYPING INTO A HELD BOX SUBMITS EXACTLY WHAT WAS TYPED.
//
// The sentinel was the input's value and payloadFor recognised it by an
// exact string compare, so anything that left the box holding something
// else went to the engine whole — dots and all — and was sealed as the
// credential by name. Inserting at the caret without select-all is the
// ordinary way to do that.
test("typing after the dots submits only what was typed", async () => {
  let sent: Record<string, unknown> = {};
  const spy = stubFetch((_url, init) => {
    sent = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
    return new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 });
  });
  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            configured: true,
            requirements: [
              req({ field: "webhook_token", label: "Shared token", kind: "secret", present: true }),
            ],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  const box = screen.getByLabelText("Shared token") as HTMLInputElement;
  // What an insert at the caret produces when the box is genuinely empty.
  fireEvent.change(box, { target: { value: "brand-new-token" } });
  fireEvent.click(screen.getByRole("button", { name: /save|connect/i }));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());

  const values = sent.values as Record<string, string>;
  expect(values.webhook_token).toBe("brand-new-token");
});

// The recommendation is an alternative to what the form asked for, so it is
// shown only where there is a credential to keep somewhere.
test("the secrets recommendation appears only on a form with a credential", () => {
  const { unmount } = render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [req({ field: "route_to", label: "Fallback seat", kind: "handle" })],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.queryByText(/Recommendation:/)).toBeNull();
  unmount();

  render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [req({ field: "webhook_token", label: "Shared token", kind: "secret" })],
          },
        },
      ]}
      title="Datadog"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  // The one line a reader skims, with the reasoning behind the icon's hover.
  expect(screen.getByText(/keep credentials in/)).toBeTruthy();
  expect(screen.getByTitle(/rotating it is one edit in one place/)).toBeTruthy();
  expect((screen.getByText("Secrets") as HTMLAnchorElement).getAttribute("href")).toContain(
    "secrets",
  );
});

// A LINK IS BUILT OUT OF VALUES, and a reference is a name. Once the
// organization id lived in the sealed store, the API keys link was built out
// of the literal text `${ATLASSIAN_ORG_ID}` and opened a console page for an
// organization of that name.
test("a link built from a referenced field uses what the reference reads", () => {
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
                value: "${ATLASSIAN_ORG_ID}",
                resolved_value: "org-42",
              }),
              req({
                field: "api_key",
                label: "API key",
                kind: "secret",
                link_text: "API keys",
                vendor_url: "https://admin.atlassian.com/o/{org_id}/api-keys",
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
  expect((screen.getByText("API keys") as HTMLAnchorElement).href).toBe(
    "https://admin.atlassian.com/o/org-42/api-keys",
  );
});

// AND A REFERENCE NAMING NOTHING DRAWS NO LINK, rather than an address with
// the name of a missing entry in its path.
test("a link built from an unresolved reference is not drawn", () => {
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
                value: "${GONE}",
              }),
              req({
                field: "api_key",
                label: "API key",
                kind: "secret",
                link_text: "API keys",
                vendor_url: "https://admin.atlassian.com/o/{org_id}/api-keys",
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
  expect(screen.queryByRole("link", { name: "API keys" })).toBeNull();
});

/**
 * A per-seat app: each agent has its own app at the vendor, so each has its
 * own credentials and its own delivery route. Slack's shape, and the reason
 * this dialog has more than one section.
 */
const perSeatTool: SetupToolState = {
  key: "slack",
  configured: false,
  enabled: false,
  satisfied: false,
  seats_required: true,
  can_provision: false,
  requirements: [
    req({
      field: "typing_status",
      label: "Working indicator",
      kind: "choice",
      required: false,
      config_path: "integrations.slack.typing_status",
      choices: [{ value: "off", label: "Never" }],
    }),
  ],
  seats: [
    {
      handle: "sre-lead",
      name: "SRE Lead",
      satisfied: false,
      public_url: "https://engine.example.com/webhooks/slack/sre-lead",
      requirements: [
        req({
          field: "bot_token",
          label: "Bot token",
          kind: "secret",
          connect: true,
          seat: "sre-lead",
        }),
        req({
          field: "channel",
          label: "Default channel",
          required: false,
          seat: "sre-lead",
        }),
      ],
    },
    {
      handle: "builder",
      name: "Builder",
      satisfied: true,
      public_url: "https://engine.example.com/webhooks/slack/builder",
      requirements: [
        req({
          field: "bot_token",
          label: "Bot token",
          kind: "secret",
          connect: true,
          seat: "builder",
          present: true,
          resolved: true,
          value: "${BUILDER_SLACK_BOT_TOKEN}",
        }),
      ],
    },
  ],
};

function perSeatSections() {
  return [
    { name: "Slack", tool: perSeatTool },
    { name: "SRE Lead", tool: perSeatTool, seat: "sre-lead" },
    { name: "Builder", tool: perSeatTool, seat: "builder" },
  ];
}

// ONE FOLDED BLOCK PER AGENT.
//
// A per-seat app asks for the same credentials once per agent, so a company
// with ten agents opened a dialog with twenty inputs in one scroll and no way
// to see how many were left. Folded, the dialog opens as the roster it is:
// every agent named, each saying whether it is done.
test("a per-seat app gives every agent its own collapsible block", () => {
  const { container } = render(
    <SetupDialog sections={perSeatSections()} title="Slack" onClose={() => {}} onDone={() => {}} />,
  );
  const blocks = [...container.querySelectorAll("details.int-seat-form")];
  expect(blocks.length).toBe(2);
  expect(screen.getByText("SRE Lead")).toBeDefined();
  expect(screen.getByText("Builder")).toBeDefined();

  // AND EACH SAYS WHETHER IT IS DONE, which is the answer a reader wants
  // before opening anything.
  expect(screen.getByText("Needs setup")).toBeDefined();
  expect(screen.getByText("Configured")).toBeDefined();

  // THE UNFINISHED ONE IS OPEN. A single-agent company opens straight into
  // its fields; a larger one opens on the agent with work left rather than
  // on all of them at once.
  expect((blocks[0] as HTMLDetailsElement).open).toBe(true);
  expect((blocks[1] as HTMLDetailsElement).open).toBe(false);
});

// NOBODY IS COMING TO DO THIS FOR YOU, said once, at the top.
//
// Where the seats are mandatory and this build has no pass that can create
// them, every block below is manual work, and a card with no roster looks
// exactly like an app with nothing to do.
test("an app the engine cannot provision says so before the blocks", () => {
  render(
    <SetupDialog sections={perSeatSections()} title="Slack" onClose={() => {}} onDone={() => {}} />,
  );
  expect(
    screen.getByText(/Slack does not support automatic agent provisioning at the moment/),
  ).toBeDefined();
  // ONE LINE. What follows it is the roster of agents to configure, which
  // says the rest by being there.
  expect(screen.queryByText(/Configure a dedicated seat/)).toBeNull();
});

// THE DELIVERY ADDRESS IS IN THE MANIFEST, not beside it.
//
// A per-seat app's request URL is part of the app definition, so pasting the
// manifest sets it. A banner telling somebody to paste the same address by
// hand is a second instruction for a step the first one already did, and two
// instructions for one step is how one of them goes stale.
test("an agent's block does not ask for its delivery address twice", () => {
  const { container } = render(
    <SetupDialog sections={perSeatSections()} title="Slack" onClose={() => {}} onDone={() => {}} />,
  );
  const blocks = [...container.querySelectorAll("details.int-seat-form")];
  expect(blocks[0]!.textContent).not.toContain("Paste that address");
  expect(blocks[0]!.textContent).not.toContain("/webhooks/slack/sre-lead");
});

// A SEAT'S OPTIONAL FIELD STAYS WITH ITS SEAT.
//
// Gathered into the dialog's shared "More settings" fold, an agent's optional
// fields lost the one thing that said whose they were: a three-agent company
// showed three identical "Default channel" boxes in one list.
test("an agent's optional field is inside that agent's block", () => {
  const { container } = render(
    <SetupDialog sections={perSeatSections()} title="Slack" onClose={() => {}} onDone={() => {}} />,
  );
  const blocks = [...container.querySelectorAll("details.int-seat-form")];
  expect(blocks[0]!.textContent).toContain("Default channel");
  // ONCE, AND ONLY THERE. Gathered at the foot of the dialog it appeared per
  // agent with nothing saying whose each one was.
  expect(screen.getAllByText("Default channel").length).toBe(1);
  expect(blocks[1]!.textContent).not.toContain("Default channel");

  // AND THE COMPANY'S OWN FIELD IS NOT INSIDE ANYBODY'S BLOCK: it is one
  // setting for the whole app, not a question about an agent.
  expect(screen.getByText("Working indicator")).toBeDefined();
  for (const block of blocks) {
    expect(block.textContent).not.toContain("Working indicator");
  }
});

// THE LINK NAMES THE APP, not the agent whose block it is in. A per-seat
// app names its sections after the AGENT, so the link under a bot token
// offered to "Open SRE Lead", which is not a page and not a product.
test("a vendor link inside an agent's block names the app", () => {
  const linked = {
    ...perSeatTool,
    seats: [
      {
        ...perSeatTool.seats![0]!,
        requirements: [
          req({
            field: "bot_token",
            label: "Bot token",
            kind: "secret",
            connect: true,
            seat: "sre-lead",
            vendor_url: "https://api.slack.com/apps",
          }),
        ],
      },
    ],
  };
  render(
    <SetupDialog
      sections={[{ name: "SRE Lead", tool: linked, seat: "sre-lead" }]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText("Open Slack")).toBeDefined();
  expect(screen.queryByText("Open SRE Lead")).toBeNull();
});

// A CLOSED BLOCK IS STILL A BLOCK SOMEBODY CAN OPEN.
test("an agent's block opens when its summary is clicked", () => {
  const { container } = render(
    <SetupDialog sections={perSeatSections()} title="Slack" onClose={() => {}} onDone={() => {}} />,
  );
  const blocks = [...container.querySelectorAll("details.int-seat-form")];
  const closed = blocks[1] as HTMLDetailsElement;
  expect(closed.open).toBe(false);
  closed.open = true;
  fireEvent(closed, new Event("toggle"));
  expect((container.querySelectorAll("details.int-seat-form")[1] as HTMLDetailsElement).open).toBe(
    true,
  );
});

// THE APP AN AGENT IS BUILT FROM, offered rather than described.
//
// Slack issues the credential that creates an app by hand, so an operator
// usually builds each agent's app themselves. Told only where to click, they
// were reproducing seventeen scopes and five event subscriptions from a
// documentation table, and one of them wrong is a bot that installs, reports
// success and sees an empty workspace.
test("an agent's block offers the manifest its app is built from", () => {
  const withManifest = {
    ...perSeatTool,
    seats: [{ ...perSeatTool.seats![0]!, manifest: '{\n  "display_information": {}\n}' }],
  };
  const { container } = render(
    <SetupDialog
      sections={[{ name: "SRE Lead", tool: withManifest, seat: "sre-lead" }]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  const block = container.querySelector("details.int-seat-form");
  // NOT "for SRE Lead": the block this sits in is that agent's and carries
  // their name two lines above, so repeating it says nothing and pushes the
  // words that do off the end of a narrow dialog.
  expect(block?.textContent).toContain("App manifest");
  expect(block?.textContent).not.toContain("App manifest for");
  expect(container.querySelector(".int-manifest-text")?.textContent).toContain(
    "display_information",
  );
  // INSIDE THE AGENT'S OWN BLOCK, because a per-seat app has one manifest
  // per agent and they differ by exactly the request URL that decides whose
  // mentions arrive where.
  expect(block?.querySelector(".int-manifest")).not.toBeNull();
});

// A SEAT WITH NO MANIFEST OFFERS NOTHING, rather than an empty box to paste.
// The request URL is built from the company's public address, so without one
// there is no app definition worth pasting.
test("an agent with no manifest is offered no manifest", () => {
  const { container } = render(
    <SetupDialog
      sections={[{ name: "SRE Lead", tool: perSeatTool, seat: "sre-lead" }]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(container.querySelector(".int-manifest")).toBeNull();
  expect(screen.queryByText(/App manifest/)).toBeNull();
});

// COPYING IT IS ONE CLICK, because it is forty lines nobody selects by hand.
test("the manifest can be copied", async () => {
  const written: string[] = [];
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t: string) => (written.push(t), Promise.resolve()) },
  });
  const manifest = '{\n  "display_information": {"name": "SRE Lead"}\n}';
  const withManifest = {
    ...perSeatTool,
    seats: [{ ...perSeatTool.seats![0]!, manifest }],
  };
  render(
    <SetupDialog
      sections={[{ name: "SRE Lead", tool: withManifest, seat: "sre-lead" }]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  fireEvent.click(screen.getByText("Copy"));
  expect(written).toEqual([manifest]);
});

// A MISSING MANIFEST SAYS WHY.
//
// The field below it tells the operator to paste one, so a block with nothing
// in it is an instruction pointing at what is not there. Both causes, a
// company with no public address and a role name past Slack's app-name cap,
// are one edit away from fixed and neither is guessable.
test("an agent with no manifest is told why not", () => {
  const noted = {
    ...perSeatTool,
    seats: [
      {
        ...perSeatTool.seats![0]!,
        manifest_note: "no manifest yet: set integrations.public_base_url",
      },
    ],
  };
  const { container } = render(
    <SetupDialog
      sections={[{ name: "SRE Lead", tool: noted, seat: "sre-lead" }]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.getByText(/No app manifest for SRE Lead yet/)).toBeDefined();
  expect(screen.getByText(/integrations.public_base_url/)).toBeDefined();
  // AND NO EMPTY BOX to copy nothing out of.
  expect(container.querySelector(".int-manifest")).toBeNull();
});

// A MANIFEST OUTRANKS ITS OWN ABSENCE. Both rendered would be a block saying
// it has nothing while showing it.
test("a seat carrying a manifest shows no missing-manifest note", () => {
  const both = {
    ...perSeatTool,
    seats: [
      {
        ...perSeatTool.seats![0]!,
        manifest: '{\n  "display_information": {}\n}',
        manifest_note: "should not be shown",
      },
    ],
  };
  render(
    <SetupDialog
      sections={[{ name: "SRE Lead", tool: both, seat: "sre-lead" }]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  expect(screen.queryByText(/No app manifest/)).toBeNull();
  expect(screen.getByText(/App manifest/)).toBeDefined();
});

// COPYING THE MANIFEST DOES NOT OPEN IT.
//
// The button sits in the disclosure's summary, where copying is one click
// from a folded block rather than two. A click there toggles the disclosure on
// its way through, so an operator who only wanted the text got the forty
// lines they were copying instead of it.
test("copying the manifest does not toggle its disclosure", () => {
  const written: string[] = [];
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t: string) => (written.push(t), Promise.resolve()) },
  });
  const manifest = '{\n  "display_information": {"name": "SRE Lead"}\n}';
  const withManifest = {
    ...perSeatTool,
    seats: [{ ...perSeatTool.seats![0]!, manifest }],
  };
  const { container } = render(
    <SetupDialog
      sections={[{ name: "SRE Lead", tool: withManifest, seat: "sre-lead" }]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );
  const fold = container.querySelector("details.int-manifest") as HTMLDetailsElement;
  expect(fold.open).toBe(false);
  // THE DEFAULT IS CANCELLED, which is the actual mechanism: fireEvent
  // reports what dispatchEvent did, and jsdom does not implement a
  // summary's toggle, so asserting `fold.open` afterwards would pass
  // whether or not anything prevented it.
  const dispatched = fireEvent.click(screen.getByText("Copy"));
  expect(written).toEqual([manifest]);
  expect(dispatched).toBe(false);
});

// ONE SAVE IS ONE WRITE, and only for what changed.
//
// seed() fills the form from what the engine reports, and payloadFor sent
// every value it found — so every configured section was submitted whether or
// not anybody touched it. Each POST is a revision and an epoch: nothing on
// the write path compares a submitted value against what is stored, and the
// activation advances unconditionally. Editing one seat of a three-seat app
// wrote three revisions.
test("only the section that changed is written", async () => {
  const writes: string[] = [];
  const spy = stubFetch((url, init) => {
    if (init?.method === "POST") writes.push(url);
    return new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 });
  });
  const seat = (handle: string) => ({
    name: handle,
    seat: handle,
    tool: {
      ...tool,
      configured: true,
      requirements: [],
      // A PER-SEAT APP asks its questions per seat, so the requirements
      // live on the seat rather than on the tool.
      seats: [
        {
          handle,
          satisfied: true,
          requirements: [
            req({
              field: "channel",
              label: "Channel",
              kind: "text",
              present: true,
              value: `#${handle}`,
            }),
          ],
        },
      ],
    },
  });
  render(
    <SetupDialog
      sections={[seat("ceo"), seat("cto"), seat("swe")]}
      title="Slack"
      onClose={() => {}}
      onDone={() => {}}
    />,
  );

  const boxes = screen.getAllByLabelText("Channel");
  expect(boxes).toHaveLength(3);
  const cto = boxes[1];
  if (!cto) throw new Error("the second seat has no field");
  fireEvent.change(cto, { target: { value: "#leadership" } });
  fireEvent.click(screen.getByRole("button", { name: /Save|Connect/ }));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());

  expect(writes).toHaveLength(1);
});

// AND NOTHING CHANGED IS NOTHING SUBMITTED, rather than three no-op
// revisions.
test("a save that changed nothing writes nothing", async () => {
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
              req({
                field: "url",
                label: "Instance",
                kind: "url",
                present: true,
                value: "https://gitlab.example.com",
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
  fireEvent.click(screen.getByRole("button", { name: /Save|Connect/ }));
  await vi.waitFor(() => expect(screen.getByText(/Nothing to submit/)).toBeTruthy());
  const posts = spy.mock.calls.filter(
    ([, init]) => (init as RequestInit | undefined)?.method === "POST",
  );
  expect(posts).toHaveLength(0);
});

// A SAVE SAYS IT SAVED. The title and the button branch on whether this is a
// connection or an edit, and the toast did not — so a dialog headed "Datadog
// settings", submitted with a Save button, confirmed with "Datadog
// connected".
test("saving a connected app does not say it connected", async () => {
  const spy = stubFetch(
    () => new Response(JSON.stringify({ revision_id: "r", wrote_secrets: [] }), { status: 201 }),
  );
  render(
    <ToastProvider>
      <SetupDialog
        sections={[
          {
            name: "Datadog",
            tool: {
              ...tool,
              configured: true,
              requirements: [
                req({ field: "route_to", label: "Fallback seat", kind: "handle", present: true }),
              ],
            },
          },
        ]}
        title="Datadog"
        onClose={() => {}}
        onDone={() => {}}
      />
    </ToastProvider>,
  );
  fireEvent.change(screen.getByLabelText("Fallback seat"), { target: { value: "sre-lead" } });
  fireEvent.click(screen.getByRole("button", { name: /Save|Connect/ }));
  await vi.waitFor(() => expect(spy).toHaveBeenCalled());

  await vi.waitFor(() => expect(screen.getByText(/Datadog settings saved/)).toBeTruthy());
  expect(screen.queryByText(/Datadog connected/)).toBeNull();
});

// A REFUSAL NEVER HIDES BEHIND THE FOLD.
//
// The disclosure is closed in both directions on purpose — a settings form
// that sprang open because a field inside it was unset would differ from the
// connect form — and it opens when a submission is refused for something
// inside it. That was true of exactly one error code. The ordinary refusal, a
// config validation listing the values it will not accept, went to the banner
// with the fold shut: a Datadog operator read
// `integrations.datadog.route_to required value missing` over a form with no
// such input on it, because the field is real, required, and behind "More
// settings" with nothing saying so.
test("a refusal opens the fold hiding the field it names", async () => {
  stubFetch(
    () =>
      new Response(
        JSON.stringify({
          error: "invalid_config",
          detail:
            "integrations.datadog.route_to required value missing: name the " +
            "handle of the seat an alert should wake when no monitor tag " +
            "names an owner",
        }),
        { status: 422 },
      ),
  );
  const { baseElement } = render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [
              req({
                field: "site",
                label: "Datadog region",
                kind: "choice",
                connect: true,
                config_path: "integrations.datadog.site",
              }),
              req({
                field: "api_key",
                label: "API key",
                kind: "secret",
                connect: true,
                config_path: "integrations.datadog.api_key",
              }),
              // FOLDED, because it declares no `connect`, and required. The
              // config path is what the refusal is joined on, so it has to be
              // the one the engine really sends.
              req({
                field: "route_to",
                label: "Fallback seat",
                kind: "handle",
                config_path: "integrations.datadog.route_to",
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

  const more = baseElement.querySelector("details.int-form-more") as HTMLDetailsElement;
  expect(more.open).toBe(false);

  // Something to submit, which is what an operator filling the connect fields
  // leaves behind: with nothing changed the dialog never reaches the engine.
  fireEvent.change(screen.getByLabelText(/API key/), { target: { value: "dd-api-key" } });
  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  expect(await screen.findByText(/names an owner/)).toBeDefined();

  const after = baseElement.querySelector("details.int-form-more") as HTMLDetailsElement;
  expect(after.open).toBe(true);
  // AND THE FIELD IT NAMES IS THE ONE NOW REACHABLE.
  expect(after.contains(screen.getByText("Fallback seat").closest(".field"))).toBe(true);
});

// AND A REFUSAL ABOUT SOMETHING ON THE FORM LEAVES IT SHUT, or "open it
// whenever anything is refused" would be the settings form differing from the
// connect form again, by another route.
test("a refusal naming a visible field leaves the fold shut", async () => {
  stubFetch(
    () =>
      new Response(
        JSON.stringify({
          error: "invalid_config",
          detail: "integrations.datadog.api_key required value missing: give a key",
        }),
        { status: 422 },
      ),
  );
  const { baseElement } = render(
    <SetupDialog
      sections={[
        {
          name: "Datadog",
          tool: {
            ...tool,
            requirements: [
              req({
                field: "api_key",
                label: "API key",
                kind: "secret",
                connect: true,
                config_path: "integrations.datadog.api_key",
              }),
              req({
                field: "route_to",
                label: "Fallback seat",
                kind: "handle",
                config_path: "integrations.datadog.route_to",
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
  fireEvent.change(screen.getByLabelText(/API key/), { target: { value: "dd-api-key" } });
  fireEvent.click(screen.getByRole("button", { name: /Connect|Save/ }));
  expect(await screen.findByText(/give a key/)).toBeDefined();

  const more = baseElement.querySelector("details.int-form-more") as HTMLDetailsElement;
  expect(more.open).toBe(false);
});

// A FIELD REQUIRED BY AN ANSWER IS HIDDEN UNTIL THAT ANSWER IS GIVEN.
//
// GitHub's organization token is the pair this exists for. Stated `required`
// it blocked a connect that needed nothing — a hand-minted personal access
// token demanded before anything worked. Stated optional it let the API store
// "cover every repository in the organization" with nothing able to register
// the hook, which is a company one apply later holding a demand it cannot
// meet.
const gated: SetupRequirement = {
  field: "token",
  label: "Organization token",
  kind: "secret",
  config_path: "integrations.github.token",
  required: false,
  present: false,
  required_when: { field: "provisioning.org_webhook", equals: "true" },
};

test("a gated field is hidden until its answer is chosen", () => {
  expect(shownField(gated, () => "false")).toBe(false);
  expect(shownField(gated, () => "true")).toBe(true);
});

test("a gated field is required exactly when it is shown", () => {
  // THE TWO ARE ONE DECISION. A field shown under an answer that needs it is
  // a field that needs it, and splitting them would allow "visible but
  // optional" — a form asking a question whose answer it will ignore.
  expect(neededField(gated, () => "false")).toBe(false);
  expect(neededField(gated, () => "true")).toBe(true);
  // AND AN UNGATED FIELD ANSWERS TO `required` ALONE.
  expect(neededField({ ...gated, required_when: undefined, required: true })).toBe(true);
});

// A GATE WITH NOTHING TO READ IS SHUT.
//
// A caller that only wants the hidden/mintable rule — a count, a fold — must
// not be handed a field nobody has opened. Open-by-default would put the
// token in the connect form of every company again, which is the bug it
// exists to remove.
test("a gate with no answers to read stays shut", () => {
  expect(gateOpen(gated)).toBe(false);
  expect(shownField(gated)).toBe(false);
  // AND AN UNGATED FIELD IS UNAFFECTED, so the resolver stays optional for
  // every caller that has no gates to evaluate.
  expect(gateOpen({ ...gated, required_when: undefined })).toBe(true);
});
