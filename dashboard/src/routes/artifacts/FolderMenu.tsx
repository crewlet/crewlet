/**
 * A folder's own actions, on its tile, its list row and its page: Copy link
 * for anyone who sees it, and Rename, Move to… and Remove for its owner, the
 * owner's lead or an administrator (`role: "owner"`), each a person's
 * gesture. Only what the reader may do is offered.
 */

import { useMemo, useState, type ReactNode } from "react";
import { Menu, useToast, type MenuEntry } from "@crewlethq/ui";
import {
  CopyGlyph,
  EllipsisGlyph,
  FolderInputGlyph,
  PencilGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { artifactFolderPath, artifactFoldersPath } from "~/app/routes.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { renameFolder } from "~/lib/artifacts/api.ts";
import {
  announceFoldersChanged,
  folderNameProblem,
  takenBy,
  type FolderIndex,
} from "~/lib/artifacts/folders.ts";
import type { ArtifactFolderItem } from "~/protocol/index.ts";
import { copyPageAddress } from "./parts.tsx";
import { RenameDialog } from "./RenameDialog.tsx";
import { MoveDialog } from "./MoveDialog.tsx";
import { RemoveFolderDialog } from "./FolderDialogs.tsx";

/** What is wrong with a folder's new name, as its rename says it, or undefined. */
function renameProblem(index: FolderIndex, folder: ArtifactFolderItem) {
  return (name: string): string | undefined => {
    if (name === "") return "A folder has a name.";
    const problem = folderNameProblem(name);
    if (problem) return problem;
    const taken = takenBy(index.children(folder.parent ?? ""), name, folder.id);
    return taken
      ? `A folder called “${taken.name}” is already here. Choose another name.`
      : undefined;
  };
}

export function FolderMenu({
  folder,
  index,
  onRemoved,
  trigger,
}: {
  folder: ArtifactFolderItem;
  index: FolderIndex;
  /** The folder is gone: a page drawing it leaves for the folder above. */
  onRemoved?: (() => void) | undefined;
  /** The menu's trigger words, where it is a labelled button rather than a glyph. */
  trigger?: ReactNode;
}) {
  const toast = useToast();
  const manage = useArtifactAccess({ person: true });
  const [open, setOpen] = useState<"rename" | "move" | "remove" | null>(null);
  const parent = folder.parent ? (index.get(folder.parent) ?? null) : null;
  const managing = folder.role === "owner" && manage.can;
  const problem = useMemo(() => renameProblem(index, folder), [index, folder]);
  const items: MenuEntry[] = [
    {
      key: "copy",
      label: "Copy link",
      icon: <CopyGlyph size="sm" />,
      onSelect: () => void copyPageAddress(artifactFolderPath(folder.id), toast),
    },
    ...(managing
      ? [
          {
            key: "rename",
            label: "Rename",
            icon: <PencilGlyph size="sm" />,
            onSelect: () => setOpen("rename"),
          },
          {
            key: "move",
            label: "Move to…",
            icon: <FolderInputGlyph size="sm" />,
            onSelect: () => setOpen("move"),
          },
          { kind: "separator" as const, key: "sep" },
          {
            key: "remove",
            label: "Remove",
            icon: <TrashGlyph size="sm" />,
            danger: true,
            onSelect: () => setOpen("remove"),
          },
        ]
      : []),
  ];
  return (
    <>
      <Menu
        label={`More for the folder “${folder.name}”`}
        {...(trigger ? { trigger } : {})}
        icon={<EllipsisGlyph size="sm" />}
        triggerVariant="ghost"
        align="end"
        items={items}
      />
      {open === "rename" && (
        <RenameDialog
          label="Name"
          initial={folder.name}
          problem={problem}
          request={(name) => renameFolder(folder.id, name)}
          onClose={() => setOpen(null)}
          onRenamed={() => announceFoldersChanged()}
        />
      )}
      {open === "move" && (
        <MoveDialog
          subject={{ kind: "folder", folder }}
          onClose={() => setOpen(null)}
          onMoved={() => setOpen(null)}
        />
      )}
      {open === "remove" && (
        <RemoveFolderDialog
          folder={folder}
          parent={parent}
          onClose={() => setOpen(null)}
          onRemoved={() => onRemoved?.()}
        />
      )}
    </>
  );
}

/** Where a page leaves to once its folder is removed: the folder above, or Folders. */
export function aboveRemoved(folder: ArtifactFolderItem): string[] {
  return folder.parent ? artifactFolderPath(folder.parent) : artifactFoldersPath();
}
