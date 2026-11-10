// @vitest-environment node

/**
 * An open draft as the assistant is told it, and changed through the screen.
 *
 * THE DRAFT IS THE ONE PLACE A PAGE CONTEXT CARRIES WHAT THE PERSON TYPED, so
 * it is held to the engine's own rules here
 * (`internal/assistant/wire.PageDraft`): a credential never leaves the page,
 * every value is cut at its bound, and a draft past the byte budget sheds its
 * fields rather than making a question the engine refuses.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import {
  ASSISTANT_ACTION_ARGS_DESCRIPTION_MAX_RUNES,
  ASSISTANT_ACTION_ARGS_MAX_BYTES,
  ASSISTANT_ACTION_ARGS_MAX_MEMBERS,
  ASSISTANT_PAGE_CONTEXT_MAX_BYTES,
  ASSISTANT_PAGE_DRAFT_MAX_FIELDS,
  ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES,
  type AssistantPageDraft,
} from "~/contract/assistant.ts";
import { pageContextFits, wireBytes } from "~/protocol/assistantWire.ts";
import {
  applyPageAction,
  buildPageContext,
  pageSnapshot,
  registerPage,
  resetPageContextForTest,
  setFramePage,
  undoPageAction,
  type FramePage,
  type PageActionSpec,
} from "./context.ts";
import {
  draftSetAction,
  draftSetArgs,
  draftSnapshot,
  isSecretDraftPath,
  parseDraftChange,
  refuse,
  type AssistantDraftSpec,
} from "./draft.ts";

afterEach(resetPageContextForTest);

interface Form {
  title: string;
  tags: readonly string[];
  password: string;
  apiKey: string;
}

const FORM: Form = { title: "Runbook", tags: ["ops"], password: "hunter2", apiKey: "abc" };

function spec(value: Form = FORM, apply = vi.fn()): AssistantDraftSpec<Form> {
  return {
    kind: "client",
    id: "kit-1",
    revision: 3,
    step: "details",
    help: "Change the title and the tags.",
    scope: "The form (not saved)",
    value,
    fields: [
      {
        path: "title",
        label: "Title",
        read: (f) => f.title,
        write: (f, v) =>
          typeof v === "string" && v.trim() !== "" ? { ...f, title: v } : refuse("Name it."),
      },
      {
        path: "tags",
        label: "Tags",
        read: (f) => f.tags,
        write: (f, v) => ({ ...f, tags: [...v] }),
      },
      // A CREDENTIAL THE SCREEN MARKS, and one only its path names.
      { path: "password", label: "Password", secret: true, read: (f) => f.password },
      { path: "api_key", label: "Key", read: (f) => f.apiKey, write: (f) => f },
    ],
    apply,
  };
}

describe("the draft a page sends", () => {
  test("names the draft, its step and its values, and never a credential", () => {
    expect(draftSnapshot(spec())).toEqual({
      kind: "client",
      id: "kit-1",
      revision: "3",
      step: "details",
      help: "Change the title and the tags.",
      fields: [
        { path: "title", label: "Title", kind: "text", value: "Runbook", editable: true },
        { path: "tags", label: "Tags", kind: "list", value: "ops", editable: true },
        { path: "password", label: "Password", kind: "text", value: "", redacted: true },
        { path: "api_key", label: "Key", kind: "text", value: "", redacted: true },
      ],
      more: 0,
    });
  });

  test("cuts a long value, counts fields past the bound, and leaves out what the engine refuses", () => {
    const long = "x".repeat(ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES + 5);
    const fields = Array.from({ length: ASSISTANT_PAGE_DRAFT_MAX_FIELDS + 2 }, (_, i) => ({
      path: `f${i}`,
      label: `F${i}`,
      read: () => (i === 0 ? long : "v"),
    }));
    const snapshot = draftSnapshot({
      ...spec(),
      kind: "server",
      id: null,
      step: "Not A Step",
      fields: [{ path: "Bad Path", label: "x", read: () => "" }, ...fields],
    });
    expect(snapshot.fields).toHaveLength(ASSISTANT_PAGE_DRAFT_MAX_FIELDS);
    expect(snapshot.more).toBe(3);
    expect(snapshot.fields[0]).toMatchObject({ truncated: true });
    expect([...snapshot.fields[0]!.value]).toHaveLength(ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES);
    // A SAVED DRAFT THAT NAMES NO RECORD is the page's own, and a step out
    // of the grammar is left out.
    expect(snapshot.kind).toBe("client");
    expect(snapshot.step).toBeUndefined();
  });

  test("reads a credential's path by segment, as the engine does", () => {
    expect(isSecretDraftPath("smtp.password")).toBe(true);
    expect(isSecretDraftPath("api_key")).toBe(true);
    expect(isSecretDraftPath("auth.token")).toBe(true);
    expect(isSecretDraftPath("project.key")).toBe(false);
    expect(isSecretDraftPath("ceiling")).toBe(false);
  });

  test("sheds its fields from the end, never the turn, when the page is over budget", () => {
    const page: FramePage = {
      screen: "brand-setup",
      title: "Brand › Set up",
      path: ["company"],
      params: {},
      query: {},
    };
    const draft = draftSnapshot({
      ...spec(),
      fields: Array.from({ length: 30 }, (_, i) => ({
        path: `f${i}`,
        label: `F${i}`,
        read: () => "y".repeat(900),
      })),
    });
    const context = buildPageContext(page, { draft });
    expect(pageContextFits(context)).toBe(true);
    expect(context.draft!.fields.length).toBeLessThan(30);
    expect(context.draft!.more).toBe(30 - context.draft!.fields.length);
  });
});

/** A snapshot of `n` editable text fields: labels of `label` characters, hints of `hint`, each holding `value`. */
function editable(n: number, label: number, hint: number, value = "v"): AssistantPageDraft {
  return {
    kind: "client",
    fields: Array.from({ length: n }, (_, i) => ({
      path: `f${i}`,
      label: `F${i}`.padEnd(label, "x"),
      kind: "text" as const,
      value,
      ...(hint > 0 ? { hint: "h".repeat(hint) } : {}),
      editable: true,
    })),
    more: 0,
  };
}

/**
 * WHAT draft.set DECLARES IT TAKES IS A SCHEMA THE ENGINE READS, or nothing.
 *
 * A member per editable field grows with the draft, and the engine refuses
 * the whole page context, and the question asked with it, for an action
 * declaring more members than one object may hold, encoding past its bytes,
 * or carrying a description past its bound
 * (`internal/assistant/wire.ActionArgs`). Past the members or the bytes,
 * `draft.set` declares nothing, which the engine takes for it alone, reading
 * the draft's editable fields instead.
 */
describe("what draft.set declares it takes", () => {
  // Mutation: drop the members check, or the bytes check, and the 33-member
  // or the twenty wide members are declared.
  test("a member per editable field while one schema may hold them, and nothing past that", () => {
    const most = draftSetArgs(editable(ASSISTANT_ACTION_ARGS_MAX_MEMBERS, 10, 0));
    expect(Object.keys(most?.properties?.values?.properties ?? {})).toHaveLength(
      ASSISTANT_ACTION_ARGS_MAX_MEMBERS,
    );
    expect(wireBytes(most)).toBeLessThanOrEqual(ASSISTANT_ACTION_ARGS_MAX_BYTES);
    expect(draftSetArgs(editable(ASSISTANT_ACTION_ARGS_MAX_MEMBERS + 1, 10, 0))).toBeUndefined();
    // TWENTY MEMBERS at the longest label and hint a field carries: under the
    // member bound, over the bytes.
    expect(draftSetArgs(editable(20, 120, 200))).toBeUndefined();
  });

  // Mutation: send the label and hint whole, and the description runs past
  // the engine's bound.
  test("cuts a description at the engine's bound, keeping the words that say its kind", () => {
    const args = draftSetArgs({
      kind: "client",
      fields: [
        {
          path: "tags",
          label: "T".repeat(120),
          hint: "h".repeat(200),
          kind: "list",
          value: "",
          editable: true,
        },
      ],
      more: 0,
    });
    const description = args?.properties?.values?.properties?.tags?.description ?? "";
    expect([...description]).toHaveLength(ASSISTANT_ACTION_ARGS_DESCRIPTION_MAX_RUNES);
    expect(description).toMatch(/h…\. A list of items, or one item per line\.$/);
  });

  // A VALUE RUNS TO A THOUSAND CHARACTERS, so the example it makes is left
  // out where it would push the schema past its bytes, and the members stay.
  // Mutation: always offer the example, and this one is declared.
  test("offers a field's own value as the example only where it still fits", () => {
    const long = draftSetArgs(
      editable(16, 120, 200, "x".repeat(ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES)),
    );
    expect(Object.keys(long?.properties?.values?.properties ?? {})).toHaveLength(16);
    expect(long?.examples).toBeUndefined();
    expect(draftSetArgs(editable(16, 120, 200))?.examples).toEqual([{ values: { f0: "v" } }]);
  });

  // WHAT draft.set DECLARES IS SHED BEFORE ANY FIELD OF THE DRAFT: the
  // fields say it again, and a field shed with its member still declared was
  // one the model was offered and the engine refused as not in the draft.
  // Mutation: shed the fields first, and fields the context had room for go.
  test("a page over its budget sheds what draft.set declares before a field of its draft", () => {
    const page: FramePage = {
      screen: "brand-setup",
      title: "Brand › Set up",
      path: ["company"],
      params: {},
      query: {},
    };
    const draft = editable(11, 100, 150, "y".repeat(ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES));
    const args = draftSetArgs(draft);
    const action = { ...draftSetAction(() => null), args } as PageActionSpec<unknown>;
    const declared = buildPageContext(page, { draft });
    // THE CASE IT MEANS: the draft fits whole, and does not beside its arguments.
    expect(pageContextFits(declared)).toBe(true);
    expect(wireBytes({ ...declared, action_args: { "draft.set": args } })).toBeGreaterThan(
      ASSISTANT_PAGE_CONTEXT_MAX_BYTES,
    );
    const context = buildPageContext(page, { draft, actions: { "draft.set": action } });
    expect(pageContextFits(context)).toBe(true);
    expect(context.accepts).toEqual(["draft.set"]);
    expect(context.action_args?.["draft.set"]).toBeUndefined();
    expect(context.draft?.fields).toHaveLength(11);
    expect(context.draft?.more).toBe(0);
  });
});

describe("draft.set", () => {
  test("sets editable fields through the screen's write and shows each one's value now", () => {
    const parsed = parseDraftChange(spec(), { values: { title: "On call", tags: "ops\npager\n" } });
    expect(parsed).toEqual({
      ok: true,
      value: {
        next: { ...FORM, title: "On call", tags: ["ops", "pager"] },
        rows: [
          { field: "Title", next: "On call", current: "Runbook" },
          // A LIST IS DRAWN AS ITS ITEMS, beside the one line a reader that
          // draws one shows.
          {
            field: "Tags",
            next: "ops; pager",
            current: "ops",
            items: ["ops", "pager"],
            currentItems: ["ops"],
          },
        ],
      },
    });
  });

  // A CHANGE IS A SET OF VALUES, WRITTEN IN THE SCREEN'S ORDER: the engine's
  // encoder sorts a proposal's values by path, so a field whose control waits
  // on another is named before it as often as after. Mutation: write in the
  // payload's order, and the access named first is refused while the switch
  // named after it is still off.
  test("writes the values in the order the screen declares its fields, not the order they are named in", () => {
    interface Access {
      on: boolean;
      level: string;
    }
    const access: AssistantDraftSpec<Access> = {
      kind: "client",
      scope: "Access (not saved)",
      value: { on: false, level: "" },
      fields: [
        {
          path: "switch",
          label: "On",
          read: (a) => (a.on ? "On" : "Off"),
          write: (a, v) => ({ ...a, on: v === "On" }),
        },
        {
          path: "access",
          label: "Access",
          read: (a) => a.level,
          write: (a, v) => (a.on ? { ...a, level: String(v) } : refuse("Turn it on first.")),
        },
      ],
      apply: vi.fn(),
    };
    expect(parseDraftChange(access, { values: { access: "Admin", switch: "On" } })).toEqual({
      ok: true,
      value: {
        next: { on: true, level: "Admin" },
        rows: [
          { field: "On", next: "On", current: "Off" },
          { field: "Access", next: "Admin", current: "(empty)" },
        ],
      },
    });
  });

  test("refuses a credential, an unknown field, the wrong shape, extra keys, and the screen's own no", () => {
    for (const raw of [
      { values: { password: "x" } },
      { values: { api_key: "x" } },
      { values: { nope: "x" } },
      { values: { title: ["a"] } },
      { values: { title: "a\u001bb" } },
      { values: { title: "x" }, save: true },
      { values: {} },
      "title",
    ]) {
      expect(parseDraftChange(spec(), raw).ok, JSON.stringify(raw)).toBe(false);
    }
    expect(parseDraftChange(spec(), { values: { title: " " } })).toEqual({
      ok: false,
      reason: "Name it.",
    });
  });

  test("applies through the page's registry, then undoes while the draft still says it", () => {
    setFramePage({
      screen: "brand-setup",
      title: "Brand › Set up",
      path: ["company"],
      params: {},
      query: {},
    });
    let value = FORM;
    const apply = vi.fn((next: Form) => {
      value = next;
    });
    const latest = () => spec(value, apply);
    registerPage({
      draft: draftSnapshot(latest()),
      actions: { "draft.set": draftSetAction(latest) as PageActionSpec<unknown> },
    });
    const revision = pageSnapshot()!.context.source_revision;
    const result = applyPageAction({
      correlation_id: "c1",
      action: "draft.set",
      payload: { values: { title: "On call" } },
      source_revision: revision,
    });
    expect(result).toMatchObject({ outcome: "applied", undoable: true });
    expect(value.title).toBe("On call");
    expect(undoPageAction("c1")).toBe("undone");
    expect(value.title).toBe("Runbook");
  });

  test("refuses every half once the screen closed its draft", () => {
    const action = draftSetAction<Form>(() => null);
    expect(action.parse({ values: { title: "x" } }).ok).toBe(false);
    expect(action.blocked({} as never)).toMatch(/no longer open/);
  });
});

describe("a file field", () => {
  const FILE = "0b7f1c3e-8a4d-4e2b-9c1a-3f5e6d7a8b9c";
  const CONVERSATION = "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f";
  const ref = (over: Record<string, unknown> = {}) => ({
    kind: "file",
    file_id: FILE,
    conversation_id: CONVERSATION,
    name: "logo.svg",
    media_type: "image/svg+xml",
    ...over,
  });
  interface Kit {
    logo: string;
  }
  const kit: AssistantDraftSpec<Kit> = {
    kind: "client",
    scope: "The kit (not saved)",
    value: { logo: "" },
    fields: [
      {
        path: "logo",
        label: "Logo",
        read: (k) => k.logo,
        file: {
          accepts: ["image/svg+xml", "image/png"],
          write: (k, r) => (r.name.startsWith("x") ? refuse("Not that one.") : { logo: r.name }),
        },
      },
    ],
    apply: vi.fn(),
  };

  test("is sent as a file field, by its name, that the assistant may set", () => {
    expect(draftSnapshot(kit).fields).toEqual([
      {
        path: "logo",
        label: "Logo",
        kind: "file",
        value: "",
        hint: "A file: image/svg+xml, image/png",
        editable: true,
      },
    ]);
  });

  test("takes a reference to a file and shows it by name, type and the reference", () => {
    expect(parseDraftChange(kit, { values: { logo: ref() } })).toEqual({
      ok: true,
      value: {
        next: { logo: "logo.svg" },
        rows: [
          { field: "Logo", next: "logo.svg (image/svg+xml)", current: "(empty)", file: ref() },
        ],
      },
    });
  });

  test("refuses markup, a path, another type, extra keys, a bad id and the screen's own no", () => {
    for (const value of [
      '<svg xmlns="http://www.w3.org/2000/svg" onload="x()"/>',
      ref({ name: "../logo.svg" }),
      ref({ media_type: "text/html" }),
      ref({ bytes: "PHN2Zz4=" }),
      ref({ file_id: "logo" }),
      ref({ kind: "text" }),
    ]) {
      expect(parseDraftChange(kit, { values: { logo: value } }).ok, JSON.stringify(value)).toBe(
        false,
      );
    }
    expect(parseDraftChange(kit, { values: { logo: ref({ name: "x.svg" }) } })).toEqual({
      ok: false,
      reason: "Not that one.",
    });
  });
});
