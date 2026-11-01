/**
 * The frame every kind's create page stands in: one slim bar in the frame's
 * place, as an open artifact's page draws (the kit's `ArtifactBar`), and the
 * whole screen under it for the kind's own body.
 *
 * # One bar, the same as the artifact's
 *
 * The bar holds the way back to the gallery, what is being made (the kind's
 * glyph in its colour and "New doc"), what the page is doing now (`status`),
 * the kind's own controls (`tools`), Cancel and, at its end, the kind's one
 * primary action (`action`). A create page reads as the artifact it is about
 * to become, so the bar does not jump when the new artifact opens.
 *
 * The primary action is pressed with the command key and Enter from anywhere
 * on the page, a field included (`create.start` in `app/keymap.ts`), so every
 * kind starts the same way and none binds its own. On a narrow bar it takes
 * its short word (`short`), so "Create and ask SRE Lead" reads "Create" on a
 * phone.
 *
 * # Where it will be filed, said before it is made
 *
 * The bar's first control is Save in: the folder the new artifact is filed in
 * (the folder page New was pressed on, `?folder=`), or "Save in a folder" for
 * the top level, which opens Move to… to choose one and writes nothing until
 * the artifact is made. The way back leads to that folder, so a reader who
 * started in it returns to it.
 *
 * # Asking an agent sits beside the page
 *
 * A kind that can be handed to an agent passes `handoff`: the bar's "Ask an
 * agent" (`askTool`) and its panel (`AskAgentPanel`), which the shell lays at
 * the body's side in the open artifact's own workspace, so it is a column
 * where there is room and a drawer where there is not. The body stays mounted
 * as the panel opens and closes, so nothing the reader made is lost. The
 * control sits before Cancel; on a kind only an agent makes, it is the bar's
 * last control and its primary while the panel is closed, and Command+Enter
 * outside the panel opens it (inside, the panel sends).
 *
 * # The body is the kind's
 *
 * Writing a doc, laying out a deck and asking an agent for a dashboard share
 * nothing but this frame, so the body is full bleed: no padding, no scroll of
 * the frame's. It fills the window's height under the bar (`useFillScreen`)
 * and scrolls inside itself, so a kind can be a canvas, an editor or a
 * composer and place its own inset.
 *
 * # Its stylesheet arrives with it
 *
 * The create pages' rules are `styles/create.css`, imported here rather than
 * by `main.tsx`, so the build ships them beside the create pages' chunks and a
 * reader who only reads never downloads them (see that sheet's note).
 */

import "~/styles/create.css";
import { useState, type ReactNode } from "react";
import { ArtifactBar, ArtifactWorkspace, type ArtifactBarTool } from "@crewlethq/ui";
import { FolderGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { FrameNavToggle, OwnPageHeader } from "~/app/frame/OwnHeader.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { capsOf, keyRow, useKeymap } from "~/app/keymap.ts";
import { href, useNavigator } from "~/app/router.tsx";
import { artifactFolderPath, artifactSectionPath } from "~/app/routes.ts";
import { useServicesRun } from "~/lib/services.ts";
import { KIND_WORDS, NEW_KIND_TITLES, type MakeKind } from "~/lib/artifacts/words.ts";
import { MoveDialog } from "../MoveDialog.tsx";
import { useSaveIn, type SaveIn } from "./create.ts";

/** Where a create page leads back to when it files at the top level: the gallery. */
const GALLERY = artifactSectionPath("all");

/** Where a create page leads back to: the folder it files in, or the gallery. */
function wayBack(saveIn: SaveIn): { path: string[]; label: string } {
  return saveIn.folder && saveIn.name
    ? { path: artifactFolderPath(saveIn.folder), label: `Back to ${saveIn.name}` }
    : { path: GALLERY, label: "Back to artifacts" };
}

/** The kind's primary action, drawn as the bar's last control: "Start the doc". */
export interface CreateAction extends Omit<
  ArtifactBarTool,
  "key" | "labelled" | "emphasis" | "pressed" | "keys" | "shortLabel"
> {
  /** Its word on a narrow bar, where the full one would push the title out: "Create". */
  short?: string | undefined;
}

export interface CreateShellProps {
  kind: MakeKind;
  /** The kind's one primary action. Left out while the page offers none. */
  action?: CreateAction | undefined;
  /** What the page is doing now, before the controls: "Draft saved", "Starting…". */
  status?: ReactNode;
  /** The kind's own bar controls, before Cancel: a template picker, a preview. */
  tools?: readonly ArtifactBarTool[] | undefined;
  /**
   * What Cancel does. Left out, it returns to the gallery; a kind that holds
   * something the reader typed asks first, then leaves with [useLeaveCreate].
   */
  onCancel?: (() => void) | undefined;
  /** Asking an agent: the bar's control and, while it is open, its panel. */
  handoff?: CreateHandoff | undefined;
  /** The kind's whole body, full bleed under the bar. */
  children: ReactNode;
}

/** A create page's hand-off to an agent, as the shell places it. */
export interface CreateHandoff {
  /** The bar's "Ask an agent" (`askTool`). */
  tool: ArtifactBarTool;
  /** The panel while it is open (`AskAgentPanel`), else null. */
  panel: ReactNode;
  /** Opens the panel: what Command+Enter does on a page with no other action. */
  open: () => void;
}

/**
 * Leave a create page for where it was started (the folder it files in, or
 * the gallery), replacing it: Back should not land on a half-made artifact.
 */
export function useLeaveCreate(): () => void {
  const nav = useNavigator();
  const saveIn = useSaveIn();
  return () => nav.replace(wayBack(saveIn).path);
}

export function CreateShell({
  kind,
  action,
  status,
  tools = [],
  onCancel,
  handoff: offered,
  children,
}: CreateShellProps) {
  // ASKING AN AGENT FILES A TASK to its seat, so where the company runs no
  // Worklet the page offers none: every way a person makes the
  // artifact here stays (`lib/services.ts`).
  const runs = useServicesRun();
  const handoff = runs.tracker ? offered : undefined;
  useFillScreen(true);
  const leave = useLeaveCreate();
  const saveIn = useSaveIn();
  const back = wayBack(saveIn);
  const [choosing, setChoosing] = useState(false);
  useKeymap({
    "create.start": {
      run: (e) => {
        e.preventDefault();
        if (action) action.onSelect();
        else handoff?.open();
      },
      when: action ? action.disabledReason === undefined : handoff !== undefined,
    },
  });
  // THE HAND-OFF IS THE LAST CONTROL where it is the page's only way to make
  // the kind, and it stays there as its panel opens: a control that moved
  // when pressed would leave the pointer on Cancel.
  const last = handoff !== undefined && action === undefined;
  const bar: ArtifactBarTool[] = [
    {
      key: "save-in",
      // WHILE THE TREE IS READ the folder has no name yet, and "Save in a
      // folder" would say the artifact is going nowhere.
      label: saveIn.path
        ? `Save in ${saveIn.path}`
        : saveIn.reading
          ? "Save in…"
          : "Save in a folder",
      shortLabel: saveIn.name || "Folder",
      icon: <FolderGlyph size="sm" />,
      onSelect: () => setChoosing(true),
      labelled: true,
    },
    ...tools,
    ...(handoff && !last ? [handoff.tool] : []),
    {
      key: "cancel",
      label: "Cancel",
      icon: <XGlyph size="sm" />,
      onSelect: onCancel ?? leave,
      labelled: true,
    },
    ...(action ? [primaryTool(action)] : []),
    ...(handoff && last ? [handoff.tool] : []),
  ];
  return (
    <div className="artifact-create" data-kind={kind}>
      <OwnPageHeader>
        <ArtifactBar
          title={NEW_KIND_TITLES[kind]}
          kind={kind}
          kindLabel={KIND_WORDS[kind]}
          start={<FrameNavToggle />}
          back={{ href: href(back.path), label: back.label }}
          headingLevel={1}
          status={status}
          kindTools={bar}
        />
      </OwnPageHeader>
      <div className="artifact-create-body">
        {/* ALWAYS THE WORKSPACE, panel or none, so the body is never
            remounted (and never loses what was typed) as the panel opens. */}
        <ArtifactWorkspace
          className="artifact-create-workspace"
          bodyClassName="artifact-create-canvas"
          panel={handoff?.panel ?? null}
        >
          {children}
        </ArtifactWorkspace>
      </div>
      {choosing && (
        <MoveDialog
          subject={{ kind: "place", current: saveIn.folder }}
          onClose={() => setChoosing(false)}
          onMoved={(target) => {
            setChoosing(false);
            saveIn.set(target.id);
          }}
        />
      )}
    </div>
  );
}

/** The page's action as the bar draws it: primary, labelled, with its key and its short word. */
function primaryTool({ short, ...action }: CreateAction): ArtifactBarTool {
  return {
    ...action,
    key: "create",
    labelled: true,
    emphasis: "primary",
    keys: capsOf(keyRow("create.start").presses[0]!),
    ...(short ? { shortLabel: short } : {}),
  };
}
