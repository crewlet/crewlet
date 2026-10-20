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
 * # The body is the kind's
 *
 * Writing a doc, laying out a deck and asking an agent for a dashboard share
 * nothing but this frame, so the body is full bleed: no padding, no scroll of
 * the frame's. It fills the window's height under the bar (`useFillScreen`)
 * and scrolls inside itself, so a kind can be a canvas, an editor or a
 * composer and place its own inset.
 */

import type { ReactNode } from "react";
import { ArtifactBar, type ArtifactBarTool } from "@crewlethq/ui";
import { XGlyph } from "@crewlethq/icons/glyphs";
import { FrameNavToggle, OwnPageHeader } from "~/app/frame/OwnHeader.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { capsOf, keyRow, useKeymap } from "~/app/keymap.ts";
import { href, useNavigator } from "~/app/router.tsx";
import { artifactSectionPath } from "~/app/routes.ts";
import { KIND_WORDS, NEW_KIND_TITLES } from "~/lib/artifacts/words.ts";
import type { ArtifactKind } from "~/protocol/index.ts";

/** Where a create page leads back to: the gallery it was started from. */
const GALLERY = artifactSectionPath("all");

/** The kind's primary action, drawn as the bar's last control: "Start the doc". */
export interface CreateAction extends Omit<
  ArtifactBarTool,
  "key" | "labelled" | "emphasis" | "pressed" | "keys" | "shortLabel"
> {
  /** Its word on a narrow bar, where the full one would push the title out: "Create". */
  short?: string | undefined;
}

export interface CreateShellProps {
  kind: ArtifactKind;
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
  /** The kind's whole body, full bleed under the bar. */
  children: ReactNode;
}

/** Leave a create page for the gallery, replacing it: Back should not land on a half-made artifact. */
export function useLeaveCreate(): () => void {
  const nav = useNavigator();
  return () => nav.replace(GALLERY);
}

export function CreateShell({
  kind,
  action,
  status,
  tools = [],
  onCancel,
  children,
}: CreateShellProps) {
  useFillScreen(true);
  const leave = useLeaveCreate();
  useKeymap({
    "create.start": {
      run: (e) => {
        e.preventDefault();
        action?.onSelect();
      },
      when: action !== undefined && action.disabledReason === undefined,
    },
  });
  const bar: ArtifactBarTool[] = [
    ...tools,
    {
      key: "cancel",
      label: "Cancel",
      icon: <XGlyph size="sm" />,
      onSelect: onCancel ?? leave,
      labelled: true,
    },
    ...(action ? [primaryTool(action)] : []),
  ];
  return (
    <div className="artifact-create" data-kind={kind}>
      <OwnPageHeader>
        <ArtifactBar
          title={NEW_KIND_TITLES[kind]}
          kind={kind}
          kindLabel={KIND_WORDS[kind]}
          start={<FrameNavToggle />}
          back={{ href: href(GALLERY), label: "Back to artifacts" }}
          headingLevel={1}
          status={status}
          kindTools={bar}
        />
      </OwnPageHeader>
      <div className="artifact-create-body">{children}</div>
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
