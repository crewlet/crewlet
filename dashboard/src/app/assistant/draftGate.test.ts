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
 *
 * A MODAL WHOSE CHANGE OUTLIVES IT IS PART OF ITS PAGE. A modal that keeps
 * only its own transient state holds nothing anyone could ask about, as the
 * drawer is inert while it holds the page; but one whose change is written
 * into the page's draft (a dialog whose change stays in the tab until its
 * Review, a sheet whose edits stay on the board) changes what the page
 * declares, and listing it as a modal hid it from the assistant. So a modal
 * listing is held to the draft's TYPES ([refusedModals]): a screen holds
 * what it declares in branded state (`draft.ts`), and through the TypeScript
 * checker (`test/checker.ts`) a module listed modal is refused once it holds
 * a branded value, from context or a prop, or a page hands it one, or a
 * closure or a local function that reaches one. A name could not say it:
 * the setter of a form's errors is spelled like the setter of its values,
 * and a dispatch taken from context is spelled `dispatch` whatever it
 * writes. The same reading holds the brand itself: it is cast nowhere but
 * where it is minted ([forgedBrands]), and what a write does beside writing
 * (its `also`) reaches none of a draft ([writingAlso]).
 */

import { posix } from "node:path";
import { afterAll, beforeAll, describe, expect, test } from "vitest";
import {
  SymbolFlags,
  TypeFlags,
  type Symbol as TsSymbol,
  type Type,
} from "typescript/unstable/sync";
import {
  isArrowFunction,
  isAsExpression,
  isCallExpression,
  isClassDeclaration,
  isFunctionDeclaration,
  isFunctionExpression,
  isIdentifier,
  isImportDeclaration,
  isInterfaceDeclaration,
  isJsxExpression,
  isJsxNamespacedName,
  isJsxOpeningLikeElement,
  isJsxSpreadAttribute,
  isMethodDeclaration,
  isNamedImports,
  isObjectLiteralExpression,
  isPropertyAccessExpression,
  isPropertyAssignment,
  isShorthandPropertyAssignment,
  isStringLiteral,
  isTypeAssertion,
  isVariableDeclaration,
  type CallExpression,
  type InterfaceDeclaration,
  type JsxAttributeLike,
  type JsxOpeningLikeElement,
  type JsxTagNameExpression,
  type Node as TsNode,
  type SourceFile,
} from "typescript/unstable/ast";
import { nodesOf, placeOf, typedFixture, typedTree, type Typed } from "~/test/checker.ts";
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
  "routes/settings/models/wizard/steps/HarnessStep.tsx": {
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
  "routes/settings/mcp-tools/wizard/steps.tsx": {
    screen: ["routes/settings/mcp-tools/wizard/ServerWizard.tsx"],
    why: "the MCP server wizard's steps, whose draft the wizard declares",
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
  "routes/gitlet/change/Conversation.tsx": {
    screen: ["routes/gitlet/change/ChangePage.tsx"],
    why: "the comment on a change and each thread's reply box, fields of the change page's one draft",
  },
  "routes/gitlet/change/Files.tsx": {
    screen: ["routes/gitlet/change/ChangePage.tsx"],
    why: "a finding written on picked lines and each finding's reply box, fields of the change page's one draft",
  },
  // MODAL, AND STILL THE TAB'S: every change made in these dialogs lands in
  // the Agents tab's draft and outlives the dialog, until the tab's Review.
  "routes/settings/integrations/access/Dialogs.tsx": {
    screen: ["routes/settings/integrations/access/AgentsAccess.tsx"],
    why: "an agent's permissions and scope, set in their dialogs into the Agents tab's draft, which the tab declares",
  },
  // A SHEET, AND STILL THE BOARD'S: each change made in a widget's settings is
  // written into the board being arranged as it is made, through Home's own
  // setter, and Done keeps it there (Cancel takes it back).
  "routes/home/board/settings.tsx": {
    screen: ["routes/home/Home.tsx"],
    why: "a widget's settings, written into the board being arranged as they are made, which Home declares",
  },
  // A SIDE SHEET, AND STILL THE BUILDER'S: its Apply records what changed
  // in it into the builder's draft through the builder's own dispatch, kept
  // after the sheet closes until Review and save.
  "routes/org/builder/NodeEditor.tsx": {
    screen: ["routes/org/builder/Builder.tsx"],
    why: "a unit or seat edited in the side sheet, its Apply recording the change into the builder's draft, which the builder declares",
  },
};

/** A module with editing marks and no draft the assistant could be asked about, and why. */
const NOT_A_DRAFT: Readonly<Record<string, string>> = {
  "app/StepUp.tsx": "modal: the recent sign-in a write asks for, a credential and never a draft",
  "components/Composer.tsx": "modal: a reply, comment or ask written in its dialog",
  "components/people.tsx": "modal: an invitation or a person's details, in their dialogs",
  "components/writes.tsx": "modal: each write control's own dialog",
  "components/seatControls.tsx": "modal: a pause's reason, in the dialog a seat's Pause opens",
  "routes/account/Account.tsx": "modal: a password, a second factor or a token, in its dialog",
  "routes/artifacts/FolderDialogs.tsx": "modal: naming a folder",
  "routes/artifacts/HistoryTab.tsx": "modal: a version's note, in its dialog",
  "routes/artifacts/RenameDialog.tsx": "modal: renaming an artifact",
  "routes/atlas/AddEntry.tsx": "modal: a new atlas entry, in its dialog",
  "routes/atlas/EntryActions.tsx":
    "modal: a fact's value, an owner, a relation, a merge, a split or a purge, each in its dialog",
  "routes/atlas/pickers.tsx":
    "modal: the entry, owner and page fields of the atlas's dialogs, each dialog's own until its write",
  "routes/chatlet/Attach.tsx":
    "modal: a search for a task, page or artifact to link, in its dialog",
  "routes/gitlet/commit/ComparePage.tsx": "modal: opening a change of a branch, in its dialog",
  "routes/gitlet/change/Merge.tsx": "modal: an override's reason, in its dialog",
  "routes/gitlet/ImportRepo.tsx": "modal: a repository to import, in its dialog",
  "routes/gitlet/Search.tsx":
    "not an editor: its path and ref filters narrow a code search, and change nothing",
  "routes/chatlet/Dialogs.tsx":
    "modal: a new room, a room's topic or description, people to add, in their dialogs",
  "routes/inbox/NoticeActions.tsx": "modal: a snooze's time",
  "routes/org/builder/dialogParts.tsx":
    "modal: the fields of the builder's side sheet and dialogs, the sheet's or the dialog's own until its Apply, Add, Move or Delete records them",
  "routes/settings/AccessDialogs.tsx":
    "modal: a service account, or a person's login, seat and access, saved by its dialog's own write",
  "routes/settings/integrations/access/HandoffSheet.tsx":
    "modal: an agent's robot token pasted in its sheet, a credential and never a draft",
  "routes/worklet/NewProject.tsx": "modal: a new project's owner and key",
  "routes/worklet/NewTask.tsx": "modal: the one sheet every New task opens",
  "routes/settings/SecretDialog.tsx":
    "modal: a sealed secret's value, a credential and never a draft",
  "routes/settings/TakeBackupDialog.tsx": "modal: a backup's note, in its dialog",
  "routes/settings/mcp-tools/flows/SignInDialog.tsx":
    "modal: a sign-in's consent at the vendor; its one field is the callback address, read-only",
  "routes/settings/mcp-tools/flows/RemoveDialog.tsx":
    "modal: which stored values go with a server being removed, in its dialog",
  "routes/settings/mcp-tools/flows/TestDialog.tsx":
    "modal: which agent a per-agent server is tested as, in its dialog",
  "routes/settings/mcp-tools/flows/ValueDialog.tsx":
    "modal: a header or variable's new value, write-only, in its dialog",
  "components/TitleField.tsx":
    "not an editor: one field; every screen that draws it holds the value and declares the draft",
  "components/ConfigField.tsx":
    "not an editor: one configuration field; every host is a modal or the create-company form, which declares",
  "routes/artifacts/DashboardView.tsx":
    "not an editor: its select filters what a published board shows, and changes nothing",
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

// ---------------------------------------------------------------------------
// A modal whose change outlives it is part of its page.

/** The module that mints a draft's branded state, and the one place a cast to it is made. */
const DRAFT_MODULE = "app/assistant/draft.ts";

/** The kinds of type that are never a draft's branded state, answered without asking the checker. */
const NEVER_HELD =
  TypeFlags.Any |
  TypeFlags.Unknown |
  TypeFlags.Never |
  TypeFlags.StringLike |
  TypeFlags.NumberLike |
  TypeFlags.BooleanLike |
  TypeFlags.BigIntLike |
  TypeFlags.Void |
  TypeFlags.Undefined |
  TypeFlags.Null |
  TypeFlags.ESSymbolLike |
  TypeFlags.EnumLike;

/** Where a tag's component is declared when it comes from a package (the design system, React). */
const LIBRARY = "a package";

/** Where a tag's component is declared when the checker cannot place it. */
const UNPLACED = "a component the checker cannot place";

/** A component rendered in a module, and where it is declared. */
interface Site {
  element: JsxOpeningLikeElement;
  where: string;
}

/** A program read for a draft's branded state. */
interface Branded {
  typed: Typed;
  /** Whether a type is a draft's branded state: assignable to the brand `draft.ts` declares. */
  held(type: Type | undefined): boolean;
  /** Each module's render sites, placed once for every listing read. */
  sites: Map<string, Site[]>;
  places: Map<number, string>;
}

/** Why a module listed modal is refused. */
interface Refusal {
  /** The module listed modal, or [UNPLACED]. */
  module: string;
  /** Where the draft reaches it: a line of the module, or of the page rendering it. */
  at: string;
  /** What reaches it. */
  says: string;
}

/**
 * The brand as the checker reads it: a type is a draft's own when it is
 * assignable to `DraftHeld`, which every branded value carries and nothing
 * else can.
 */
function branded(typed: Typed): Branded {
  const { program, checker } = typed;
  const declared = program
    .getSourceFile(typed.src + DRAFT_MODULE)
    ?.statements.find(
      (s): s is InterfaceDeclaration => isInterfaceDeclaration(s) && s.name.text === "DraftHeld",
    );
  const symbol = declared && checker.getSymbolAtLocation(declared.name);
  if (!symbol) throw new Error(`${DRAFT_MODULE} declares no DraftHeld, the brand this gate reads`);
  const brand = checker.getDeclaredTypeOfSymbol(symbol);
  const assignable = (type: Type) =>
    !(type.flags & NEVER_HELD) && checker.isTypeAssignableTo(type, brand);
  const known = new Map<number, boolean>();
  return {
    typed,
    held(type) {
      if (!type || type.flags & NEVER_HELD) return false;
      let held = known.get(type.id);
      if (held === undefined) {
        held = assignable(type);
        // AN OPTIONAL ONE IS STILL THE DRAFT'S: `DraftSetter<D> | undefined`.
        if (!held && type.flags & TypeFlags.Union) {
          const present = checker.getNonNullableType(type);
          held = present !== undefined && present.id !== type.id && assignable(present);
        }
        known.set(type.id, held);
      }
      return held;
    },
    sites: new Map(),
    places: new Map(),
  };
}

/** The program's modules that ship, by their path under `src/`: no suite, benchmark or test kit. */
function shipped(typed: Typed): string[] {
  return typed.program
    .getSourceFileNames()
    .filter((f) => f.startsWith(typed.src) && /\.tsx?$/.test(f) && !f.endsWith(".d.ts"))
    .map((f) => f.slice(typed.src.length))
    .filter((path) => !path.startsWith("test/") && !/\.(test|bench)\./.test(path));
}

/** A module the program holds, or why the gate cannot read it. */
function sourceOf(typed: Typed, path: string): SourceFile {
  const file = typed.program.getSourceFile(typed.src + path);
  if (!file) throw new Error(`${path} is not in the program tsconfig.json builds`);
  return file;
}

/**
 * Every cast to a draft's branded state outside `draft.ts`: a brand minted
 * there would call a value a draft's own without the state that proves it.
 */
function forgedBrands(b: Branded, paths: readonly string[]): string[] {
  const out: string[] = [];
  for (const path of paths) {
    if (path === DRAFT_MODULE) continue;
    const file = sourceOf(b.typed, path);
    const casts = nodesOf(file).filter((n) => isAsExpression(n) || isTypeAssertion(n));
    if (casts.length === 0) continue;
    const types = b.typed.checker.getTypeAtLocation(casts);
    casts.forEach((cast, i) => {
      if (b.held(types[i])) out.push(`${placeOf(b.typed, file, cast)} ${cast.getText(file)}`);
    });
  }
  return out;
}

/**
 * Where each of draft.ts's mints that takes an `also` takes it: in the
 * options of `writeTo`, and after the write of `writeParts` and `writeVia`.
 */
const ALSO_AT: Readonly<Record<string, (call: CallExpression) => TsNode | undefined>> = {
  writeTo: (call) => memberOf(call.arguments[1], "also"),
  writeParts: (call) => call.arguments[2],
  writeVia: (call) => call.arguments[2],
};

/** An object literal's member, by its name: a property's value, a shorthand's name, or a method. */
function memberOf(node: TsNode | undefined, name: string): TsNode | undefined {
  if (!node || !isObjectLiteralExpression(node)) return undefined;
  for (const member of node.properties) {
    if (
      !isPropertyAssignment(member) &&
      !isShorthandPropertyAssignment(member) &&
      !isMethodDeclaration(member)
    ) {
      continue;
    }
    const key = member.name;
    if (!(isIdentifier(key) || isStringLiteral(key)) || key.text !== name) continue;
    return isPropertyAssignment(member)
      ? member.initializer
      : isShorthandPropertyAssignment(member)
        ? key
        : member;
  }
  return undefined;
}

/**
 * The names a module calls draft.ts's mints by, to the mint each names: its
 * own, or the one it imports a mint under. A module reaches a mint only by
 * importing it from draft.ts, which re-exports nothing of it.
 */
function mintsIn(path: string, file: SourceFile): Map<string, string> {
  const names = new Map<string, string>();
  for (const statement of file.statements) {
    if (!isImportDeclaration(statement) || !isStringLiteral(statement.moduleSpecifier)) continue;
    const spec = statement.moduleSpecifier.text;
    const target = spec.startsWith("~/")
      ? spec.slice(2)
      : posix.normalize(posix.join(posix.dirname(path), spec));
    const bindings = statement.importClause?.namedBindings;
    if (target !== DRAFT_MODULE || !bindings || !isNamedImports(bindings)) continue;
    for (const element of bindings.elements) {
      const imported = (element.propertyName ?? element.name).text;
      if (imported in ALSO_AT) names.set(element.name.text, imported);
    }
  }
  return names;
}

/**
 * Every `also` handed to a write's mint that reaches a draft's branded
 * state, read as a modal's hand-off is: what a screen does beside writing
 * its draft is never a write of a draft, which goes through a write of its
 * own (`writeEach`). Its type cannot say so, as a closure reaches whatever
 * its scope holds.
 */
function writingAlso(b: Branded, paths: readonly string[]): string[] {
  const out: string[] = [];
  for (const path of paths) {
    if (path === DRAFT_MODULE) continue;
    const file = sourceOf(b.typed, path);
    const mints = mintsIn(path, file);
    if (mints.size === 0) continue;
    for (const call of nodesOf(file).filter(isCallExpression)) {
      const callee = call.expression;
      const mint = isIdentifier(callee) ? mints.get(callee.text) : undefined;
      const also = mint === undefined ? undefined : ALSO_AT[mint]!(call);
      const via = also && reaches(b, file, also, new Set());
      if (via) out.push(`${placeOf(b.typed, file, call)} ${mint}'s also reaches ${via}`);
    }
  }
  return out;
}

/**
 * Every name in a module typed by a draft's branded state: what it holds of
 * a page's draft, whether it took it from context, from a prop, or minted it.
 */
function heldIn(b: Branded, path: string): Refusal[] {
  const file = sourceOf(b.typed, path);
  const names = nodesOf(file).filter(isIdentifier);
  const types = b.typed.checker.getTypeAtLocation(names);
  return names.flatMap((name, i) =>
    b.held(types[i])
      ? [{ module: path, at: placeOf(b.typed, file, name), says: `holds ${name.text}` }]
      : [],
  );
}

/** A tag the browser draws itself (`div`), or a namespaced one: never a component. */
function intrinsic(tag: JsxTagNameExpression): boolean {
  return (isIdentifier(tag) && /^[a-z]/.test(tag.text)) || isJsxNamespacedName(tag);
}

/**
 * A declaration that is a component itself: a function, a class, or a const
 * holding one, bare or through `memo` or `forwardRef`. Never `lazy`, whose
 * component is the module it imports, not the one that calls it.
 */
function declaresComponent(node: TsNode): boolean {
  if (isFunctionDeclaration(node) || isClassDeclaration(node)) return true;
  if (!isVariableDeclaration(node) || !node.initializer) return false;
  const init = node.initializer;
  if (isArrowFunction(init) || isFunctionExpression(init)) return true;
  if (!isCallExpression(init)) return false;
  const callee = init.expression;
  const name = isIdentifier(callee)
    ? callee.text
    : isPropertyAccessExpression(callee)
      ? callee.name.text
      : "";
  return name === "memo" || name === "forwardRef";
}

/** Where the component a tag's name resolves to is declared: its module, [LIBRARY] or [UNPLACED]. */
function placed(b: Branded, symbol: TsSymbol | undefined): string {
  if (!symbol) return UNPLACED;
  const known = b.places.get(symbol.id);
  if (known !== undefined) return known;
  const { checker, src } = b.typed;
  const target = symbol.flags & SymbolFlags.Alias ? checker.getAliasedSymbol(symbol) : symbol;
  const at = target.valueDeclaration ?? target.declarations[0];
  let where = UNPLACED;
  if (at?.path.includes("/node_modules/")) where = LIBRARY;
  else if (at?.path.startsWith(src)) {
    const node = at.resolve();
    if (node && declaresComponent(node)) where = at.path.slice(src.length);
  }
  b.places.set(symbol.id, where);
  return where;
}

/** Every component a module renders, placed by the checker. */
function sitesIn(b: Branded, path: string): Site[] {
  const known = b.sites.get(path);
  if (known) return known;
  const file = sourceOf(b.typed, path);
  const elements = nodesOf(file).filter(
    (n): n is JsxOpeningLikeElement => isJsxOpeningLikeElement(n) && !intrinsic(n.tagName),
  );
  const symbols =
    elements.length === 0
      ? []
      : b.typed.checker.getSymbolAtLocation(elements.map((e) => e.tagName));
  const sites = elements.map((element, i) => ({ element, where: placed(b, symbols[i]) }));
  b.sites.set(path, sites);
  return sites;
}

/** The body a local declaration runs: a declared function, or a const holding one or made by a call (a hook's callback). */
function runs(node: TsNode | undefined): TsNode | null {
  if (!node) return null;
  if (isFunctionDeclaration(node)) return node.body ?? null;
  if (isVariableDeclaration(node) && node.initializer) {
    const init = node.initializer;
    if (isArrowFunction(init) || isFunctionExpression(init) || isCallExpression(init)) return init;
  }
  return null;
}

/**
 * How an expression reaches a draft's branded state: a name in it typed by
 * one, or a local function, closure or callback it names that does, followed
 * through its own module's declarations; null where it reaches none.
 */
function reaches(b: Branded, file: SourceFile, expr: TsNode, seen: Set<number>): string | null {
  const names = nodesOf(expr).filter(isIdentifier);
  if (names.length === 0) return null;
  const { checker } = b.typed;
  const types = checker.getTypeAtLocation(names);
  const direct = names.findIndex((_, i) => b.held(types[i]));
  if (direct !== -1) return names[direct]!.text;
  const symbols = checker.getSymbolAtLocation(names);
  for (let i = 0; i < names.length; i++) {
    const symbol = symbols[i];
    if (!symbol || seen.has(symbol.id)) continue;
    seen.add(symbol.id);
    const at = symbol.valueDeclaration ?? symbol.declarations[0];
    if (!at || at.path !== file.path) continue;
    const body = runs(at.resolve());
    const via = body && reaches(b, file, body, seen);
    if (via) return `${names[i]!.text} > ${via}`;
  }
  return null;
}

/**
 * A branded member of an object handed whole, one property down: a page's
 * API, its dispatch with it. Deeper is the page's data, which carries no
 * write of its own.
 */
function heldMember(b: Branded, expr: TsNode): string | null {
  const { checker } = b.typed;
  const type = checker.getTypeAtLocation(expr);
  if (!type || !(type.flags & TypeFlags.Object)) return null;
  const members = checker.getPropertiesOfType(type);
  if (members.length === 0) return null;
  const types = checker.getTypeOfSymbol(members);
  const hit = members.findIndex((_, i) => b.held(types[i]));
  return hit === -1 ? null : `.${members[hit]!.name}`;
}

/** The expression an attribute hands over: a spread's, or the braces' of a valued one. */
function handed(attribute: JsxAttributeLike): TsNode | undefined {
  if (isJsxSpreadAttribute(attribute)) return attribute.expression;
  const value = attribute.initializer;
  return value && isJsxExpression(value) ? value.expression : undefined;
}

/**
 * Every render site in `paths` that hands a draft's branded state to a
 * module in `modal`, or to a component the checker cannot place: an
 * attribute whose value is one, or carries one a property down, or names a
 * closure, a local function or a callback that reaches one.
 */
function handedOver(b: Branded, paths: readonly string[], modal: ReadonlySet<string>): Refusal[] {
  const out: Refusal[] = [];
  for (const path of paths) {
    if (!path.endsWith(".tsx")) continue;
    for (const { element, where } of sitesIn(b, path)) {
      if (where !== UNPLACED && !modal.has(where)) continue;
      if (where === path) continue;
      const file = sourceOf(b.typed, path);
      for (const attribute of element.attributes.properties) {
        const value = handed(attribute);
        if (!value) continue;
        const via = reaches(b, file, value, new Set()) ?? heldMember(b, value);
        if (!via) continue;
        const name = isJsxSpreadAttribute(attribute) ? "{...}" : attribute.name.getText(file);
        out.push({
          module: where,
          at: placeOf(b.typed, file, element),
          says: `<${element.tagName.getText(file)}> ${name} via ${via}`,
        });
      }
    }
  }
  return out;
}

/**
 * Every refusal of a listing's modals over `paths`: what each holds of a
 * page's draft, what a page hands each of it, and what is handed to a
 * component the checker cannot place, which no listing can answer for.
 */
function refusedModals(
  b: Branded,
  listing: Readonly<Record<string, string>>,
  paths: readonly string[],
): Refusal[] {
  const modal = new Set(
    Object.entries(listing)
      .filter(([, why]) => why.startsWith("modal: "))
      .map(([path]) => path),
  );
  return [...[...modal].flatMap((path) => heldIn(b, path)), ...handedOver(b, paths, modal)];
}

/** Refusals as their module, the file the draft reaches it from, and what reaches it: lines move. */
const said = (refusals: readonly Refusal[]) =>
  refusals.map((r) => `${r.module} <- ${r.at.replace(/:\d+$/, "")} ${r.says}`);

describe("read through the checker", () => {
  let tree: Typed | undefined;
  let brand: Branded;
  let paths: string[];
  beforeAll(() => {
    tree = typedTree();
    brand = branded(tree);
    paths = shipped(tree);
  });
  afterAll(() => tree?.close());

  /** A listing's refusals with `path` listed modal too. */
  const listed = (path: string) =>
    said(refusedModals(brand, { ...NOT_A_DRAFT, [path]: "modal: listed by this case" }, paths));

  test("a module listed modal holds none of a page's draft, and no page hands it any", () => {
    expect(
      said(refusedModals(brand, NOT_A_DRAFT, paths)),
      "a module listed modal that holds a page's draft, or is handed it: list it in PART_OF as " +
        "a part of the page its change outlives it in, or keep the draft out of it",
    ).toEqual([]);
  });

  // THE AGENTS TAB'S DIALOGS write every change into the tab's draft, which
  // outlives them until the tab's Review: listed modal, the hand-off at each
  // render site is read through the tab's own closures to its setter.
  test("a modal is refused once a page hands it its draft: the Agents tab's dialogs", () => {
    const dialogs = "routes/settings/integrations/access/Dialogs.tsx";
    const from = `${dialogs} <- routes/settings/integrations/access/AgentsAccess.tsx`;
    const refused = listed(dialogs);
    for (const site of [
      "<PermissionsDialog> onChange",
      "<ScopeDialog> onChange",
      "<ReviewDialog> onSaved",
    ]) {
      expect(
        refused.some((r) => r.startsWith(`${from} ${site} via `)),
        site,
      ).toBe(true);
    }
  });

  // HOME'S WIDGET SHEET writes each setting into the board being arranged as
  // it is made: its Cancel takes back what the sheet wrote, through Home's own.
  test("a modal is refused once a page hands it its draft: Home's widget sheet", () => {
    const sheet = "routes/home/board/settings.tsx";
    const from = `${sheet} <- routes/home/Home.tsx`;
    const refused = listed(sheet);
    for (const site of ["<WidgetSettings> onChange", "<WidgetSettings> onCancel"]) {
      expect(
        refused.some((r) => r.startsWith(`${from} ${site} via `)),
        site,
      ).toBe(true);
    }
  });

  // THE BUILDER'S SIDE SHEET is handed nothing: its Apply records through
  // the builder's dispatch, which it takes from context.
  test("a modal is refused once it holds the draft it takes from context: the org builder's side sheet", () => {
    const editor = "routes/org/builder/NodeEditor.tsx";
    expect(listed(editor)).toContain(`${editor} <- ${editor} holds dispatch`);
  });

  // THE FIELDS OF THE BUILDER'S SHEET AND DIALOGS are drawn where the draft
  // is recorded, and hold only what is typed in them until the sheet's or
  // the dialog's own press records it: a modal's transient state, which is
  // why it stays listed as one.
  test("a modal keeping its dialog's own state passes: the builder's dialog fields", () => {
    const fields = "routes/org/builder/dialogParts.tsx";
    expect(NOT_A_DRAFT[fields]).toMatch(/^modal: /);
    expect(listed(fields).filter((r) => r.startsWith(`${fields} <- `))).toEqual([]);
  });

  test("a draft's branded state is cast nowhere but where it is minted", () => {
    expect(
      forgedBrands(brand, paths),
      `a cast to a draft's branded state outside ${DRAFT_MODULE}: hold the draft in ` +
        "useDraftState, useDraftReducer or a DraftStore and make its write from that",
    ).toEqual([]);
  });

  test("a write's also reaches none of a draft", () => {
    expect(
      writingAlso(brand, paths),
      "an also that writes a draft: write that state through a write of its own " +
        "(writeEach), and keep in also only what the screen does beside writing",
    ).toEqual([]);
  });
});

/**
 * The checker's reading on modules held only in memory, so each rule has a
 * case that fails without it: a page with a draft and a non-draft state of
 * the draft's own type, a modal that keeps its own, a field that writes a
 * draft taken from context, an object handed whole, a component nothing
 * names, a forged brand, and writes whose `also` writes a draft or does not.
 */
const FIXTURE: Readonly<Record<string, string>> = {
  "routes/probe/Page.tsx": `
import { useCallback, useState } from "react";
import { useDraftState } from "~/app/assistant/draft.ts";
import { Sheet } from "./Sheet.tsx";

export function Page() {
  const [values, setValues] = useDraftState<Record<string, string>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const forget = () => setErrors({});
  const keep = useCallback((name: string) => setValues((was) => ({ ...was, name })), [setValues]);
  return (
    <>
      <Sheet title="Errors" onDone={forget} />
      <Sheet title="Values" onDone={keep} />
      <output>{Object.keys(values).length + Object.keys(errors).length}</output>
    </>
  );
}
`,
  "routes/probe/Sheet.tsx": `
import { useState } from "react";

export function Sheet({ title, onDone }: { title: string; onDone: (name: string) => void }) {
  const [name, setName] = useState("");
  return (
    <label>
      {title}
      <input value={name} onChange={(event) => setName(event.target.value)} onBlur={() => onDone(name)} />
    </label>
  );
}
`,
  "routes/probe/Board.tsx": `
import { createContext, useContext } from "react";
import type { DraftDispatch } from "~/app/assistant/draft.ts";

export const BoardContext = createContext<{ dispatch: DraftDispatch<string> } | null>(null);
export const useBoard = () => useContext(BoardContext);
`,
  "routes/probe/Field.tsx": `
import { useBoard } from "./Board.tsx";

export function Field() {
  const board = useBoard();
  return <button type="button" onClick={() => board?.dispatch("add")}>Add</button>;
}
`,
  "routes/probe/Picked.tsx": `
import { useDraftState } from "~/app/assistant/draft.ts";
import { Sheet } from "./Sheet.tsx";

const SHEETS = [Sheet];

export function Picked() {
  const [, setValue] = useDraftState("");
  const Shown = SHEETS[0]!;
  return <Shown title="Picked" onDone={setValue} />;
}
`,
  "routes/probe/Handed.tsx": `
import { useDraftReducer } from "~/app/assistant/draft.ts";
import { Panel } from "./Panel.tsx";

export function Handed() {
  const [count, dispatch] = useDraftReducer((n: number, by: number) => n + by, 0);
  const api = { count, dispatch };
  return <Panel api={api} />;
}
`,
  "routes/probe/Panel.tsx": `
export function Panel({ api }: { api: { count: number } }) {
  return <output>{api.count}</output>;
}
`,
  "routes/probe/Also.tsx": `
import { useState } from "react";
import { useDraftState, writeTo, writeVia as reshaped } from "~/app/assistant/draft.ts";

export function Also() {
  const [value, setValue] = useDraftState("");
  const [other, setOther] = useDraftState("");
  const [said, setSaid] = useState("");
  const clear = () => setOther("");
  const writes = [
    writeTo(setValue, { also: () => setSaid("") }),
    writeTo(setValue, { also: clear }),
    reshaped(setValue, (next: string) => next.trim(), () => setOther("")),
  ];
  return <output>{value}{other}{said}{writes.length}</output>;
}
`,
  "routes/probe/Forged.tsx": `
import type { DraftWrite } from "~/app/assistant/draft.ts";

export const forged = ((next: string) => void next) as DraftWrite<string>;
`,
};

describe("the checker's reading of a draft", () => {
  let fixture: Typed | undefined;
  let brand: Branded;
  beforeAll(() => {
    fixture = typedFixture(FIXTURE);
    brand = branded(fixture);
  });
  afterAll(() => fixture?.close());

  const sheet = { "routes/probe/Sheet.tsx": "modal: a name, in its sheet" };

  // A STATE OF THE DRAFT'S OWN TYPE IS NOT THE DRAFT: the errors' setter is
  // spelled as the values' is, and only the values' is branded.
  test("passes a modal keeping its own state, and refuses one handed a closure over the draft", () => {
    expect(said(refusedModals(brand, sheet, ["routes/probe/Page.tsx"]))).toEqual([
      "routes/probe/Sheet.tsx <- routes/probe/Page.tsx <Sheet> onDone via keep > setValues",
    ]);
  });

  test("refuses a modal that holds the draft it takes from context", () => {
    const field = { "routes/probe/Field.tsx": "modal: an addition, in its dialog" };
    expect(said(refusedModals(brand, field, ["routes/probe/Field.tsx"]))).toEqual([
      "routes/probe/Field.tsx <- routes/probe/Field.tsx holds dispatch",
    ]);
  });

  // AN OBJECT HANDED WHOLE carries what it holds: a page's API, its dispatch with it.
  test("refuses a modal handed an object that carries the draft's dispatch", () => {
    const panel = { "routes/probe/Panel.tsx": "modal: a count, in its panel" };
    expect(said(refusedModals(brand, panel, ["routes/probe/Handed.tsx"]))).toEqual([
      "routes/probe/Panel.tsx <- routes/probe/Handed.tsx <Panel> api via .dispatch",
    ]);
  });

  test("refuses a draft handed to a component it cannot place, whatever is listed", () => {
    expect(said(refusedModals(brand, {}, ["routes/probe/Picked.tsx"]))).toEqual([
      `${UNPLACED} <- routes/probe/Picked.tsx <Shown> onDone via setValue`,
    ]);
  });

  // A WRITE'S ALSO IS THE SCREEN'S, never a write: one that clears a message
  // passes, and one that writes another draft, directly or through a local
  // function, under the mint's own name or the one it is imported as, is not.
  test("refuses a write's also that writes a draft, and passes one that does not", () => {
    expect(
      writingAlso(brand, ["routes/probe/Also.tsx"]).map((r) => r.replace(/:\d+ /, " ")),
    ).toEqual([
      "routes/probe/Also.tsx writeTo's also reaches clear > setOther",
      "routes/probe/Also.tsx writeVia's also reaches setOther",
    ]);
  });

  test("refuses a cast to a draft's brand", () => {
    expect(forgedBrands(brand, ["routes/probe/Forged.tsx", "routes/probe/Page.tsx"])).toEqual([
      expect.stringMatching(
        /^routes\/probe\/Forged\.tsx:4 \(\(next: string\) => void next\) as DraftWrite<string>$/,
      ),
    ]);
  });
});

/**
 * A SAVED DRAFT IS THE ONLY ONE HELD NOWHERE ON THE PAGE. `readOnlySaved()`
 * proves nothing about the page's state, so a spec says it only of a draft
 * saved at the engine (`kind: "server"`): said of any other, a draft the page
 * holds would be declared as one it does not, and no modal it hands it to
 * could be found. Mutation: say it of a client draft.
 */
function savedClaims(module: Module): string[] {
  const all = new Set<Node>();
  const kept = new Set<Node>();
  walk(parse(module.text, module.lang === "tsx" ? "tsx" : "ts"), (node) => {
    if (node.type === "CallExpression") {
      const callee = node.callee as Node;
      if (callee.type === "Identifier" && callee.name === "readOnlySaved") all.add(node);
    }
    if (node.type !== "ObjectExpression") return;
    const props = new Map<string, Node>();
    for (const p of node.properties as Node[]) {
      if (p.type !== "Property") continue;
      const key = p.key as Node;
      const name = key.type === "Identifier" ? String(key.name) : stringValue(key);
      if (name) props.set(name, p.value as Node);
    }
    const write = props.get("write");
    const kind = props.get("kind");
    if (write && kind && stringValue(kind) === "server") kept.add(write);
  });
  return [...all].filter((call) => !kept.has(call)).map(() => module.path);
}

describe("a saved draft", () => {
  test("is said of a server draft alone", () => {
    const probe = (spec: string): Module => ({
      path: "routes/probe/Saved.ts",
      lang: "ts",
      text: `const spec = ${spec};`,
    });
    expect(savedClaims(probe('{ kind: "server", write: readOnlySaved() }'))).toEqual([]);
    expect(savedClaims(probe('{ kind: "client", write: readOnlySaved() }'))).toEqual([
      "routes/probe/Saved.ts",
    ]);
    expect(savedClaims(probe("{ write: readOnlySaved() }"))).toEqual(["routes/probe/Saved.ts"]);
    const found = modules()
      .filter((m) => m.lang === "tsx" || m.lang === "ts")
      .flatMap(savedClaims);
    expect(found, "readOnlySaved() said of a draft not saved at the engine").toEqual([]);
  });
});
