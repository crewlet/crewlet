/**
 * New, as a menu of the kinds: each row the kind's glyph in its colour and its
 * word, opening that kind's own create page. The gallery's header draws it,
 * a folder's page draws it with the folder (`folder`), so what is made there
 * is filed there, and a task's rail draws it with the task (`item`), so what
 * is made there is listed on that task.
 *
 * IN A FOLDER, "Folder" COMES FIRST, set apart from the kinds: a subfolder is
 * made where the reader stands, and a kind's page opens carrying the folder
 * (`?folder=`), which its "Save in" control shows and lets them change.
 */

import { ArtifactKindGlyph, Button, Menu, type MenuEntry } from "@crewlethq/ui";
import { ChevronDownGlyph, FolderPlusGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { newArtifactPath } from "~/app/routes.ts";
import { KIND_WORDS } from "~/lib/artifacts/words.ts";
import { offeredKinds } from "~/app/nav.ts";
import { useServicesRun } from "~/lib/services.ts";

export interface NewArtifactMenuProps {
  /** The task the new artifact is made for, carried to its page as `item=`. */
  item?: string | undefined;
  /** The folder the new artifact is filed in, carried to its page as `folder=`. */
  folder?: string | undefined;
  /** Makes a folder here: offered first, above the kinds, where the reader may. */
  onNewFolder?: (() => void) | undefined;
  /** A quiet trigger, for a rail; the primary button otherwise. */
  quiet?: boolean;
}

export function NewArtifactMenu({
  item,
  folder,
  onNewFolder,
  quiet = false,
}: NewArtifactMenuProps) {
  const nav = useNavigator();
  const runs = useServicesRun();
  const query = {
    ...(item ? { item } : {}),
    ...(folder ? { folder } : {}),
  };
  const kinds: MenuEntry[] = offeredKinds(runs).map((kind) => ({
    key: kind,
    label: KIND_WORDS[kind],
    icon: <ArtifactKindGlyph kind={kind} size="sm" />,
    onSelect: () =>
      nav.to(newArtifactPath(kind), Object.keys(query).length > 0 ? query : undefined),
  }));
  return (
    <Menu
      label={item ? `New artifact for ${item}` : "New artifact"}
      trigger={quiet ? "New artifact" : "New"}
      icon={<PlusGlyph size="sm" />}
      trailingIcon={<ChevronDownGlyph size="sm" />}
      triggerVariant={quiet ? "ghost" : "primary"}
      align="end"
      items={
        onNewFolder
          ? [
              {
                key: "folder",
                label: "Folder",
                icon: <FolderPlusGlyph size="sm" />,
                onSelect: onNewFolder,
              },
              { kind: "separator", key: "kinds" },
              ...kinds,
            ]
          : kinds
      }
    />
  );
}

/**
 * New in a page bar: the menu for a reader who may make an artifact, and for
 * one who may not, the same button held with the reason (`held`), so the
 * control is never hidden and always says why.
 */
export function NewArtifactButton({
  held,
  ...menu
}: NewArtifactMenuProps & { held: string | null }) {
  if (held === null) return <NewArtifactMenu {...menu} />;
  return (
    <Button
      size="small"
      variant="primary"
      leadingIcon={<PlusGlyph size="sm" />}
      disabledReason={held}
      title={held}
    >
      New
    </Button>
  );
}
