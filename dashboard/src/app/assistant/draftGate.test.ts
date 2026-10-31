// @vitest-environment node

/**
 * EVERY SCREEN WITH A DRAFT OR A FORM TELLS THE ASSISTANT WHAT IS OPEN.
 *
 * The assistant reads what a person is editing only from the page context,
 * and a screen that never declared its draft was invisible: asked to check
 * the brand draft open in front of them, it searched the company and found
 * nothing. So the tree is read for the marks of editing state, the dashboard's
 * own and the design system's: a leave guard (`useLeaveGuard`,
 * `useUnloadGuard`), an editing status, a setup wizard, a rich, inline or list
 * editor, a form field, a value field or a text area. A module with any of
 * them must either
 *
 *  - declare its draft itself (`useAssistantDraft`, `draft.ts`),
 *  - be listed in [PART_OF] as a piece of a screen that declares it (the
 *    brand's card editors are the setup's and the Brand page's fields), or
 *  - be listed in [NOT_A_DRAFT] with the reason it holds no draft the
 *    assistant could be asked about: a modal form (the drawer is inert while
 *    a modal holds the page, so nobody can ask), a signed-out page (there is
 *    no assistant), or a control that saves each change as it is made.
 *
 * Every listing is held to the tree too: a module listed that no longer has
 * a mark, that declares its own draft, or whose screen no longer declares
 * one fails here, so the lists cannot rot into permission. Read as syntax
 * trees (`test/source.ts`), never grepped: a comment naming a mark is not
 * one, and a call broken over lines still is.
 */

import { describe, expect, test } from "vitest";
import { modules, parse, stringValue, walk, type Module, type Node } from "~/test/source.ts";

/** The hooks whose call says a module guards unsaved work. */
const GUARD_HOOKS = new Set(["useLeaveGuard", "useUnloadGuard"]);

/** The elements whose rendering says a module edits something. */
const EDITING_ELEMENTS = new Set([
  "EditingStatus",
  "SetupWizard",
  "RichEditor",
  "InlineEditor",
  "ListInput",
  "FormField",
  // uilet's labelled value (FormField with its control), which every
  // configuration form and the dialogs that hold a value draw.
  "ValueField",
  "Textarea",
]);

/** The call that declares a draft. */
const DECLARES = "useAssistantDraft";

/** A piece of a screen whose draft another module declares, and which. */
const PART_OF: Readonly<Record<string, { screen: readonly string[]; why: string }>> = {
  "routes/brand/BrandEditors.tsx": {
    screen: ["routes/brand/setup/BrandSetup.tsx", "routes/brand/BrandPage.tsx"],
    why: "the brand's card editors, whose fields are the setup's and the Brand page's draft",
  },
  "routes/settings/principles/edit/PartEditor.tsx": {
    screen: ["routes/settings/principles/edit/useCharterEditing.tsx"],
    why: "one part's editor; the page's editing hook declares every part",
  },
  "routes/settings/principles/edit/PolicyListField.tsx": {
    screen: ["routes/settings/principles/edit/useCharterEditing.tsx"],
    why: "the policies' list field, a part the editing hook declares",
  },
  "routes/artifacts/new/WhoBuildsIt.tsx": {
    screen: [
      "routes/artifacts/new/DocCreate.tsx",
      "routes/artifacts/new/SlidesCreate.tsx",
      "routes/artifacts/new/SheetCreate.tsx",
      "routes/artifacts/new/DesignCreate.tsx",
      "routes/artifacts/new/DashboardCreate.tsx",
      "routes/artifacts/new/html/SourceBody.tsx",
    ],
    why: "the agent and project a create page asks, fields of each create page's draft",
  },
  "routes/artifacts/ArtifactPage.tsx": {
    screen: ["routes/artifacts/DocEditor.tsx", "routes/artifacts/SlidesView.tsx"],
    why: "its editing status is drawn from the open kind's editor, which declares the draft",
  },
  "routes/settings/models/wizard/steps/CliStep.tsx": {
    screen: ["routes/settings/models/wizard/ModelWizard.tsx"],
    why: "one step of the model wizard, whose draft the wizard declares",
  },
  "routes/settings/models/wizard/steps/LimitsStep.tsx": {
    screen: ["routes/settings/models/wizard/ModelWizard.tsx"],
    why: "one step of the model wizard, whose draft the wizard declares",
  },
  "routes/settings/models/wizard/steps/ModelStep.tsx": {
    screen: ["routes/settings/models/wizard/ModelWizard.tsx"],
    why: "one step of the model wizard, whose draft the wizard declares",
  },
  "routes/settings/models/wizard/steps/SignInStep.tsx": {
    screen: ["routes/settings/models/wizard/ModelWizard.tsx"],
    why: "one step of the model wizard, whose draft the wizard declares",
  },
  "routes/doclet/page/LinkPicker.tsx": {
    screen: ["routes/doclet/page/Editor.tsx"],
    why: "the editor's link popover; what it inserts lands in the body the editor declares",
  },
  // A PAGE WITH SEVERAL BOXES OPEN AT ONCE declares one draft whose fields are
  // all of them (`boxes.ts`): the page context carries one draft, and a box
  // that declared its own hid every other box at its next keystroke.
  "routes/chatlet/Composer.tsx": {
    screen: ["routes/chatlet/Room.tsx"],
    why: "the room's message box and an open thread's reply box, fields of the room's one draft",
  },
  "routes/chatlet/MessageRow.tsx": {
    screen: ["routes/chatlet/Room.tsx"],
    why: "a message open for a rewrite, a field of the room's one draft",
  },
  "routes/worklet/item/Body.tsx": {
    screen: ["routes/worklet/item/edit.tsx"],
    why: "a task's title and description editors, fields of the one draft its page's edits declare",
  },
};

/** A module with editing marks and no draft the assistant could be asked about, and why. */
const NOT_A_DRAFT: Readonly<Record<string, string>> = {
  "app/StepUp.tsx": "modal: the recent sign-in a write asks for, a credential and never a draft",
  "components/Composer.tsx": "modal: a reply, comment or ask written in its dialog",
  "components/people.tsx": "modal: an invitation or a person's details, in their dialogs",
  "components/writes.tsx": "modal: each write control's own dialog",
  "routes/account/Account.tsx": "modal: a password, a second factor or a token, in its dialog",
  "routes/artifacts/FolderDialogs.tsx": "modal: naming a folder",
  "routes/artifacts/HistoryTab.tsx": "modal: a version's note, in its dialog",
  "routes/artifacts/RenameDialog.tsx": "modal: renaming an artifact",
  "routes/chatlet/Attach.tsx":
    "modal: a search for a task, page or artifact to link, in its dialog",
  "routes/chatlet/RoomSettings.tsx": "modal: a room's topic and purpose, in its dialog",
  "routes/chatlet/StartDirect.tsx": "modal: opening a direct conversation, in its dialog",
  "routes/chatlet/StartRoom.tsx": "modal: making a room, in its dialog",
  "routes/inbox/NoticeActions.tsx": "modal: a snooze's time",
  "routes/settings/AccessDialogs.tsx": "modal: an access change, in its dialog",
  "routes/worklet/NewProject.tsx": "modal: a new project's owner and key",
  "routes/worklet/NewTask.tsx": "modal: the one sheet every New task opens",
  "routes/settings/SecretDialog.tsx":
    "modal: a sealed secret's value, a credential and never a draft",
  "routes/settings/TakeBackupDialog.tsx": "modal: a backup's note, in its dialog",
  "components/ConfigField.tsx":
    "not an editor: one configuration field; every host is a modal or the create-company form, which declares",
  "routes/artifacts/DashboardView.tsx":
    "not an editor: its select filters what a published board shows, and changes nothing",
  "routes/home/board/settings.tsx": "modal: a widget's settings, in a sheet over the board",
  "routes/org/builder/NodeEditor.tsx": "modal: a unit or seat, in a side sheet over the builder",
  "routes/org/builder/dialogParts.tsx": "modal: the fields of the builder's dialogs",
  "routes/signin/SignIn.tsx": "signed out: there is no assistant before a sign-in",
  "routes/signin/Invite.tsx": "signed out: accepting an invitation",
  "routes/signin/NewPassword.tsx": "signed out: a password reset",
  "routes/signin/SecondFactor.tsx": "signed out: a sign-in's second factor",
};

/** What one module's tree says: whether it edits, and whether it declares a draft. */
interface Reading {
  marks: string[];
  declares: boolean;
}

function read(module: Module): Reading {
  const marks = new Set<string>();
  let declares = false;
  walk(parse(module.text, module.lang), (node: Node) => {
    if (node.type === "CallExpression") {
      const callee = node.callee as Node;
      const name = callee.type === "Identifier" ? String(callee.name) : null;
      if (name && GUARD_HOOKS.has(name)) marks.add(name);
      if (name === DECLARES) declares = true;
    }
    if (node.type === "JSXOpeningElement") {
      const name = node.name as Node;
      const tag = name.type === "JSXIdentifier" ? String(name.name) : "";
      if (EDITING_ELEMENTS.has(tag)) marks.add(`<${tag}>`);
    }
  });
  return { marks: [...marks].sort(), declares };
}

/** Every module with editing marks that neither declares a draft nor is listed. */
function undeclared(all: ReadonlyMap<string, Reading>): string[] {
  return [...all]
    .filter(([, r]) => r.marks.length > 0 && !r.declares)
    .filter(([path]) => !(path in PART_OF) && !(path in NOT_A_DRAFT))
    .map(([path, r]) => `${path} (${r.marks.join(", ")})`);
}

const readings = new Map<string, Reading>(
  modules()
    // THE ROUTER DEFINES THE GUARDS and the registry defines the hook; a
    // test kit renders what it tests.
    .filter((m) => m.lang === "tsx" || m.lang === "ts")
    .filter((m) => !m.path.startsWith("test/") && !m.path.startsWith("app/assistant/"))
    .map((m) => [m.path, read(m)]),
);

describe("the draft gate", () => {
  test("reads the marks it gates on", () => {
    // A GATE THAT READS NOTHING PASSES EVERYTHING: the setup wizard is the
    // screen that made this gate, and it carries a mark and a declaration.
    expect(readings.get("routes/brand/setup/BrandSetup.tsx")).toEqual({
      marks: expect.arrayContaining(["<SetupWizard>", "useLeaveGuard"]),
      declares: true,
    });
  });

  test("fails a new editing screen that tells the assistant nothing, and passes it once it does", () => {
    const screen = (body: string): Module => ({
      path: "routes/probe/Probe.tsx",
      lang: "tsx",
      text: `import { FormField, Textarea } from "@crewlethq/ui";\n${body}`,
    });
    const silent = screen(
      "export function Probe() { return <FormField label='Name'><Textarea /></FormField>; }",
    );
    const declared = screen(
      "export function Probe() { useAssistantDraft(null); " +
        "return <FormField label='Name'><Textarea /></FormField>; }",
    );
    expect(undeclared(new Map([[silent.path, read(silent)]]))).toEqual([
      "routes/probe/Probe.tsx (<FormField>, <Textarea>)",
    ]);
    expect(undeclared(new Map([[declared.path, read(declared)]]))).toEqual([]);

    // A VALUE FIELD IS A FORM FIELD. A dialog or screen that holds a value
    // through uilet's ValueField edits something, exactly as one drawing the
    // FormField it is built on.
    const valued: Module = {
      path: "routes/probe/Valued.tsx",
      lang: "tsx",
      text:
        'import { ValueField } from "@crewlethq/ui";\n' +
        "export function Valued() { return <ValueField label='Name' value='' onChange={() => {}} />; }",
    };
    expect(undeclared(new Map([[valued.path, read(valued)]]))).toEqual([
      "routes/probe/Valued.tsx (<ValueField>)",
    ]);
  });

  test("every module with draft or form state declares its draft, or says why not", () => {
    const missing = undeclared(readings);
    expect(
      missing,
      "a module with editing state that tells the assistant nothing: declare its draft with " +
        "useAssistantDraft (app/assistant/draft.ts), or list it in draftGate.test.ts with why",
    ).toEqual([]);
  });

  test("every module listed as part of a screen is one, of a screen that declares", () => {
    for (const [path, { screen }] of Object.entries(PART_OF)) {
      const r = readings.get(path);
      expect(r?.marks.length ?? 0, `${path} is listed and has no editing mark`).toBeGreaterThan(0);
      expect(r?.declares, `${path} declares its own draft; take it off the list`).toBe(false);
      for (const s of screen) {
        expect(readings.get(s)?.declares, `${path} is part of ${s}, which declares no draft`).toBe(
          true,
        );
      }
    }
  });

  test("every module listed as holding no draft still has the marks it was listed for", () => {
    for (const [path, why] of Object.entries(NOT_A_DRAFT)) {
      const r = readings.get(path);
      expect(r?.marks.length ?? 0, `${path} is listed and has no editing mark`).toBeGreaterThan(0);
      expect(r?.declares, `${path} declares a draft; take it off the list`).toBe(false);
      expect(why).toMatch(/^(modal|signed out|saved per change|not an editor): /);
    }
  });
});

/**
 * A DRAFT FIELD'S LABEL IS THE PERSON'S WORDS, NEVER ITS PATH.
 *
 * A card row is named by its field's label, and a label that is the path
 * ("colors.dark_ground"), or any dotted key in it, puts a raw path in front
 * of the person. Every object literal that declares a draft field (a `path`
 * beside a `label`) is read, and the constant parts of its label are held to
 * that. Mutation: label a field with its path.
 */
function rawLabels(module: Module): string[] {
  const out: string[] = [];
  walk(parse(module.text, module.lang === "tsx" ? "tsx" : "ts"), (node) => {
    if (node.type !== "ObjectExpression") return;
    const props = new Map<string, Node>();
    for (const p of node.properties as Node[]) {
      if (p.type !== "Property") continue;
      const key = p.key as Node;
      const name = key.type === "Identifier" ? String(key.name) : stringValue(key);
      if (name) props.set(name, p.value as Node);
    }
    const label = props.get("label");
    const path = props.get("path");
    if (!label || !path) return;
    const pathText = stringValue(path);
    const parts =
      label.type === "TemplateLiteral"
        ? (label.quasis as Node[]).map((q) => String((q.value as { raw: string }).raw))
        : [stringValue(label) ?? ""];
    const whole = stringValue(label);
    if (
      parts.some((part) => /[a-z0-9_]\.[a-z]/i.test(part)) ||
      (whole !== null && whole === pathText)
    ) {
      out.push(`${module.path}: ${whole ?? parts.join("…")}`);
    }
  });
  return out;
}

describe("a draft field's label", () => {
  test("is never a raw path", () => {
    const probe: Module = {
      path: "routes/probe/Probe.ts",
      lang: "ts",
      text:
        'const a = { path: "colors.primary", label: "colors.primary" };\n' +
        'const b = { path: "colors.ground", label: `${"Dark"} colors.ground` };\n' +
        'const c = { path: "colors.text", label: "Text colour" };',
    };
    expect(rawLabels(probe)).toEqual([
      "routes/probe/Probe.ts: colors.primary",
      "routes/probe/Probe.ts: … colors.ground",
    ]);
    const found = modules()
      .filter((m) => (m.lang === "tsx" || m.lang === "ts") && !m.path.startsWith("test/"))
      .filter((m) => !m.path.endsWith(".test.ts") && !m.path.endsWith(".test.tsx"))
      .flatMap(rawLabels);
    expect(found, "a draft field labelled with a path: give it the words the screen shows").toEqual(
      [],
    );
  });
});
