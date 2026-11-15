/**
 * What Settings › Configuration WRITES of the whole document: a revision's
 * revert, shown the diff it applies and asked first; a reload, open on a
 * managed document too; and the document edited as YAML, checked under the
 * read's revision before it is saved, its problems located.
 *
 * The socket answers the screen's questions and `fetch` plays the engine's
 * REST surface, as the frame mounts it (`test/settingsScreen.tsx`).
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { pageSnapshot, resetPageContextForTest, setFramePage } from "~/app/assistant/context.ts";
import { QueryError } from "~/protocol/index.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  savedAnswer,
  stubEngine,
  validAnswer,
  type EngineCall,
} from "~/test/settingsScreen.tsx";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { ConfigScreen } from "./Config.tsx";

afterEach(() => {
  cleanup();
  resetPageContextForTest();
  vi.unstubAllGlobals();
  location.hash = "";
});

const ACTIVE = "01JCFGAAAA0000000000000001";
const EARLIER = "01JCFGBBBB0000000000000002";

const REVISIONS = [
  {
    revision_id: ACTIVE,
    parent_revision_id: EARLIER,
    created_at: "2026-08-23T15:00:00Z",
    created_by: "founder",
    created_by_kind: "operator",
    operator_id: "",
    source: "api",
    summary: "connect datadog",
    is_active: true,
    activated_at: "2026-08-23T15:00:00Z",
  },
  {
    revision_id: EARLIER,
    created_at: "2026-08-22T15:00:00Z",
    created_by: "node-a",
    created_by_kind: "system",
    operator_id: "",
    source: "file",
    summary: "first import",
    is_active: false,
    activated_at: "2026-08-22T15:00:00Z",
  },
];

/** What reverting to EARLIER changes in the active revision. */
const REVERT_DIFF = {
  from: ACTIVE,
  to: EARLIER,
  changes: [{ path: "integrations.datadog.route_to", kind: "removed", from: "sre-lead" }],
  changes_total: 1,
};

const ANSWERS = {
  config: { name: "Nimbus" },
  config_audit: REVISIONS,
  config_diff: REVERT_DIFF,
  fleet: { nodes: [] },
  config_entities: { kind: "roles", ids: [] },
};

const MANAGED_VIEWER = { ...ADMIN_VIEWER, config_managed_by: ["gitops"], config_writer: false };

/** The document as `GET /config?format=yaml` writes it. */
const YAML = "name: Nimbus\nturn_engine:\n  max_iterations: 3\n";

/** A text answer the engine wrote, with its revision's tag. */
const yamlAnswer = (text = YAML): Response =>
  new Response(text, {
    status: 200,
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/yaml", ETag: `"${ACTIVE}"` },
  });

async function show(
  screenElement: ReactElement,
  hash: string,
  {
    viewer = ADMIN_VIEWER,
    answers = {},
  }: { viewer?: Record<string, unknown>; answers?: Record<string, unknown> } = {},
) {
  const mounted = mountSettings(screenElement, { ...ANSWERS, ...answers }, { hash, viewer });
  await act(afterMicrotasks);
  return mounted;
}

async function press(button: HTMLElement) {
  await act(async () => {
    fireEvent.click(button);
  });
  await act(afterMicrotasks);
}

function type(field: HTMLElement, value: string) {
  fireEvent.change(field, { target: { value } });
}

const request = (c: EngineCall) => `${c.method} ${c.path.replace(/^.*?\/config/, "/config")}`;

// ---------------------------------------------------------------------------
// Revert
// ---------------------------------------------------------------------------

// THE DIFF IT WOULD APPLY, before anything is sent: the revision against the
// active one is exactly what a revert changes.
test("a revision's page reverts to it after showing what it changes", async () => {
  const calls = stubEngine(() => savedAnswer());
  const { asked } = await show(
    <ConfigScreen revision={EARLIER} revisions />,
    `#/settings/config/revisions/${EARLIER}`,
  );
  await press(screen.getByRole("button", { name: /Revert to this revision/ }));
  // NAMED BY WHEN IT WAS WRITTEN, as the history lists it, never by a
  // clipped id.
  const ask = screen.getByRole("dialog", { name: /Revert to the revision from .+\?/ });
  expect(screen.queryByRole("dialog", { name: /01JCFG/ })).toBeNull();
  expect(asked).toContain("config_diff");
  expect(within(ask).getByText("integrations.datadog.route_to")).toBeTruthy();
  expect(calls).toEqual([]);

  // A SUMMARY IS ASKED FOR, and nothing is sent without one.
  await press(within(ask).getByRole("button", { name: "Revert" }));
  expect(calls).toEqual([]);
  expect(within(ask).getByText(/Say why, in one line/)).toBeTruthy();

  type(within(ask).getByLabelText("Summary"), "back to the first import: datadog pages nobody");
  await press(within(ask).getByRole("button", { name: "Revert" }));
  expect(calls.map(request)).toEqual([`POST /config/revisions/${EARLIER}/revert`]);
  expect(calls[0]!.headers.get("X-Summary")).toBe("back to the first import: datadog pages nobody");
  expect(calls[0]!.headers.get("If-Match")).toBeNull();
  expect(screen.queryByRole("dialog", { name: /Revert to the revision from/ })).toBeNull();
});

test("the active revision offers no revert, and says why", async () => {
  await show(<ConfigScreen revision={ACTIVE} revisions />, `#/settings/config/revisions/${ACTIVE}`);
  const revert = screen.getByRole("button", { name: /Revert to this revision/ });
  expect(revert.getAttribute("title")).toMatch(/This is the active revision/);
});

// A SUMMARY TRAVELS IN A HEADER, as its UTF-8 bytes: a summary in any script
// is sent, and the engine reads back the text written.
test("a summary in any script is sent whole", async () => {
  const calls = stubEngine(() => savedAnswer());
  await show(
    <ConfigScreen revision={EARLIER} revisions />,
    `#/settings/config/revisions/${EARLIER}`,
  );
  await press(screen.getByRole("button", { name: /Revert to this revision/ }));
  const ask = screen.getByRole("dialog", { name: /Revert to the revision from/ });
  type(within(ask).getByLabelText("Summary"), "zurück zum Import");
  await press(within(ask).getByRole("button", { name: "Revert" }));
  const sent = calls[0]!.headers.get("X-Summary")!;
  expect(new TextDecoder().decode(Uint8Array.from(sent, (c) => c.charCodeAt(0)))).toBe(
    "zurück zum Import",
  );
});

// A WRITE THAT STORED ITS REVISION and lost its activation's answer may have
// landed: sending it again would make a second revision, so the dialog says
// how to look instead.
test("a revert whose activation went unanswered says which revision it stored", async () => {
  stubEngine(() =>
    engineAnswer(
      {
        error: "unavailable",
        outcome: "unknown",
        stored_revision_id: "01JCFGCCCC0000000000000003",
        node: "node-a",
        your_base: ACTIVE,
      },
      503,
    ),
  );
  await show(
    <ConfigScreen revision={EARLIER} revisions />,
    `#/settings/config/revisions/${EARLIER}`,
  );
  await press(screen.getByRole("button", { name: /Revert to this revision/ }));
  const ask = screen.getByRole("dialog", { name: /Revert to the revision from/ });
  type(within(ask).getByLabelText("Summary"), "back to the first import");
  await press(within(ask).getByRole("button", { name: "Revert" }));
  expect(
    within(ask).getByText(/stored as revision 01JCFGCCCC on node node-a.*Don't send it again/),
  ).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Reload
// ---------------------------------------------------------------------------

test("a reload is asked first and carries the summary the person wrote", async () => {
  const calls = stubEngine(() => savedAnswer());
  await show(<ConfigScreen />, "#/settings/config");
  await press(screen.getByRole("button", { name: "Reload" }));
  const ask = screen.getByRole("dialog", { name: "Reload the configuration?" });
  type(within(ask).getByLabelText("Summary"), "rotate the leaked GitLab token");
  await press(within(ask).getByRole("button", { name: "Reload" }));
  expect(calls.map(request)).toEqual(["POST /config/reload"]);
  expect(calls[0]!.headers.get("X-Summary")).toBe("rotate the leaked GitLab token");
});

// THE BREAK-GLASS STAYS OPEN on a managed document: a reload changes nothing
// the managing system renders. Editing it is the managing system's.
test("on a managed document the reload stays open and the edit says who manages it", async () => {
  await show(<ConfigScreen />, "#/settings/config", { viewer: MANAGED_VIEWER });
  const reload = screen.getByRole("button", { name: "Reload" });
  expect(reload.getAttribute("aria-disabled")).not.toBe("true");
  expect(screen.getByRole("button", { name: "Edit" }).getAttribute("title")).toBe(
    "Managed by gitops. Change it there.",
  );
});

// ---------------------------------------------------------------------------
// The document, as YAML
// ---------------------------------------------------------------------------

test("the document is checked under the read's revision, and a problem is placed on its line", async () => {
  const problem = "line 3: cannot unmarshal !!str `many` into int";
  const calls = stubEngine((call) => {
    if (call.method === "GET") return yamlAnswer();
    return engineAnswer(
      {
        error: "invalid_body",
        detail: problem,
        problems: [{ path: "", segments: null, kind: "shape", message: problem, line: 3 }],
      },
      400,
    );
  });
  await show(<ConfigScreen />, "#/settings/config");
  await press(screen.getByRole("button", { name: "Edit" }));
  const editor = screen.getByLabelText("The company document, as YAML") as HTMLTextAreaElement;
  expect(editor.value).toBe(YAML);
  type(editor, YAML.replace("3", "many"));
  await press(screen.getByRole("button", { name: "Check" }));

  const check = calls[1]!;
  expect(request(check)).toBe("PUT /config?dry_run=true");
  expect(check.headers.get("If-Match")).toBe(`"${ACTIVE}"`);
  expect(check.headers.get("Content-Type")).toBe("application/yaml");
  expect(check.body).toBe(YAML.replace("3", "many"));
  expect(screen.getByText(problem)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Line 3" })).toBeTruthy();
});

test("a save checks first, then stores the document under the read's revision with its summary", async () => {
  const calls = stubEngine((call) => {
    if (call.method === "GET") return yamlAnswer();
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show(<ConfigScreen />, "#/settings/config");
  await press(screen.getByRole("button", { name: "Edit" }));
  const edited = YAML.replace("3", "5");
  type(screen.getByLabelText("The company document, as YAML"), edited);
  type(screen.getByLabelText("Summary"), "more review rounds");
  await press(screen.getByRole("button", { name: "Save" }));

  expect(calls.map(request)).toEqual([
    "GET /config?format=yaml",
    "PUT /config?dry_run=true",
    "PUT /config",
  ]);
  const save = calls[2]!;
  expect(save.headers.get("If-Match")).toBe(`"${ACTIVE}"`);
  expect(save.headers.get("X-Summary")).toBe("more review rounds");
  expect(save.body).toBe(edited);
  // SAVED: the editor gives way to the document as the engine now serves it.
  expect(screen.queryByLabelText("The company document, as YAML")).toBeNull();
});

// THE DOCUMENT NEVER REACHES THE ASSISTANT: a person writes every credential
// in it as its value, so the editor declares it open with no value, and the
// summary is the one field the assistant may propose.
test("the editor tells the assistant the document is open and never sends its text", async () => {
  setFramePage({
    screen: "config",
    title: "Settings › Configuration",
    path: ["settings", "config"],
    params: {},
    query: {},
  });
  stubEngine(() => yamlAnswer());
  await show(<ConfigScreen />, "#/settings/config");
  await press(screen.getByRole("button", { name: "Edit" }));
  type(screen.getByLabelText("The company document, as YAML"), `${YAML}api_key: SENTINEL\n`);
  type(screen.getByLabelText("Summary"), "rotate the key");
  const field = (path: string) =>
    pageSnapshot()?.context.draft?.fields.find((f) => f.path === path);
  // THE SNAPSHOT IS THE LIVE DRAFT: it carries the summary typed a moment ago,
  // so an absent sentinel means the text was left out, not that nothing was read.
  await waitFor(() => expect(field("summary")?.value).toBe("rotate the key"));
  expect(field("summary")?.editable).toBe(true);
  expect(field("document")).toMatchObject({ redacted: true, value: "" });
  expect(field("document")?.editable).toBeUndefined();
  expect(JSON.stringify(pageSnapshot())).not.toContain("SENTINEL");
});

// A WARNING THE DOCUMENT BRINGS is said before it is stored, and only a
// second press saves it; one the company already carried is not news.
test("a warning the document introduces stops the save until it is confirmed", async () => {
  const brought = {
    kind: "advisory",
    ref: "",
    path: "roles",
    segments: null,
    seat: "",
    unit: "",
    from: "",
    to: "",
    message: "No human seat: nobody can be admitted.",
    introduced: true,
  };
  const held = { ...brought, message: "An old warning.", introduced: false };
  const calls = stubEngine((call) => {
    if (call.method === "GET") return yamlAnswer();
    if (call.path.includes("dry_run=true")) return validAnswer([brought, held]);
    return savedAnswer();
  });
  await show(<ConfigScreen />, "#/settings/config");
  await press(screen.getByRole("button", { name: "Edit" }));
  type(screen.getByLabelText("The company document, as YAML"), "name: Nimbus\n");
  type(screen.getByLabelText("Summary"), "drop the people");
  await press(screen.getByRole("button", { name: "Save" }));
  expect(screen.getByText(brought.message)).toBeTruthy();
  expect(screen.queryByText(held.message)).toBeNull();
  expect(calls.filter((c) => c.method === "PUT" && !c.path.includes("dry_run"))).toEqual([]);
  await press(screen.getByRole("button", { name: "Save anyway" }));
  expect(calls.filter((c) => c.method === "PUT" && !c.path.includes("dry_run"))).toHaveLength(1);
});

test("a document saved as it was read sends nothing", async () => {
  const calls = stubEngine(() => yamlAnswer());
  await show(<ConfigScreen />, "#/settings/config");
  await press(screen.getByRole("button", { name: "Edit" }));
  type(screen.getByLabelText("Summary"), "nothing really");
  await press(screen.getByRole("button", { name: "Save" }));
  expect(calls.map(request)).toEqual(["GET /config?format=yaml"]);
  expect(screen.getByText("Nothing changed since the document was opened.")).toBeTruthy();
});

// AN UNSEALED ACTIVE REVISION opens to nobody; a whole document replaces it,
// conditional on the revision the refusal named.
test("an unsealed active revision is replaced by a whole document, under its own tag", async () => {
  const unsealed = {
    error: "unsealed_revision",
    revision_id: "01JCFGUUUU0000000000000009",
    detail: "The active revision is stored without a seal, so it can't be shown or edited.",
    hint: "Send the whole document with PUT /config.",
  };
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(unsealed, 409);
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show(<ConfigScreen />, "#/settings/config", {
    answers: {
      config: () => Promise.reject(new QueryError("unsealed_revision", null, unsealed.detail)),
    },
  });
  await press(screen.getByRole("button", { name: "Replace the document" }));
  expect(screen.getByText(unsealed.detail)).toBeTruthy();
  // THE API'S ADVICE IS NOT DRAWN: the screen says what to do in its own words.
  expect(document.body.textContent).not.toContain("PUT /config");
  const editor = screen.getByLabelText("The company document, as YAML") as HTMLTextAreaElement;
  expect(editor.value).toBe("");
  type(editor, YAML);
  type(screen.getByLabelText("Summary"), "replace the unsealed revision");
  await press(screen.getByRole("button", { name: "Save" }));
  const save = calls.find((c) => c.method === "PUT" && !c.path.includes("dry_run"))!;
  expect(save.headers.get("If-Match")).toBe('"01JCFGUUUU0000000000000009"');
});
