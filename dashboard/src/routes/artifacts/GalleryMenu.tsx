/**
 * A listed artifact's own actions, on a gallery card's More and a list row's:
 * pin or unpin, copy its link, rename it, move it to a folder and move it to
 * the trash, or, for one in the trash, restore it.
 *
 * ONLY WHAT THE READER MAY DO is offered. Renaming and filing in a folder are
 * an editor's who holds the artifacts app (the folder's own say is the
 * engine's, decided at the move), and the trash and a restore are the
 * owner's and a person's (D06), the same rules the artifact's own Settings
 * hold them to; a menu item the reader could not use would only say no after
 * they chose it.
 */

import { useState } from "react";
import { Menu, useToast, type MenuEntry } from "@crewlethq/ui";
import {
  CopyGlyph,
  EllipsisGlyph,
  FolderInputGlyph,
  PencilGlyph,
  PinGlyph,
  RotateCcwGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import type { ArtifactListItem } from "~/protocol/index.ts";
import { ARTIFACT_TITLE_MAX_BYTES } from "~/contract/artifacts.ts";
import { utf8Bytes } from "~/lib/format.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { patchArtifact, untrashArtifact } from "~/lib/artifacts/api.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { usePinToggle } from "~/lib/artifacts/pinToggle.ts";
import { copyAddress } from "./parts.tsx";
import { TrashDialog } from "./SettingsTab.tsx";
import { RenameDialog } from "./RenameDialog.tsx";
import { MoveDialog } from "./MoveDialog.tsx";

export function ArtifactMenu({
  item,
  onChanged,
}: {
  item: ArtifactListItem;
  /** The listing moved: a rename, a move, the trash or a restore. */
  onChanged: () => void;
}) {
  const toast = useToast();
  const pin = usePinToggle(item.id, item.pinned ?? false);
  const rename = useArtifactAccess({ role: "editor", held: item.role });
  const file = useArtifactAccess({ app: true, role: "editor", held: item.role });
  const own = useArtifactAccess({ role: "owner", held: item.role, person: true });
  const restore = useKeyedGesture();
  const [open, setOpen] = useState<"rename" | "move" | "trash" | null>(null);
  const trashed = item.status === "trashed";

  const copy: MenuEntry = {
    key: "copy",
    label: "Copy link",
    icon: <CopyGlyph size="sm" />,
    onSelect: () => void copyAddress(item.id, toast),
  };
  const items: MenuEntry[] = trashed
    ? [
        ...(own.can
          ? [
              {
                key: "restore",
                label: "Restore",
                icon: <RotateCcwGlyph size="sm" />,
                onSelect: () =>
                  void restore.run(untrashArtifact(item.id)).then((answer) => {
                    if (answer?.kind === "done") {
                      toast.ok(`Restored “${item.title}”`);
                      onChanged();
                    } else if (answer) {
                      toast.show({
                        variant: "warning",
                        title: "It was not restored",
                        message: answer.text,
                      });
                    }
                  }),
              },
            ]
          : []),
        copy,
      ]
    : [
        {
          key: "pin",
          label: pin.label,
          icon: <PinGlyph size="sm" />,
          onSelect: () => void pin.toggle(),
        },
        copy,
        ...(rename.can
          ? [
              {
                key: "rename",
                label: "Rename",
                icon: <PencilGlyph size="sm" />,
                onSelect: () => setOpen("rename"),
              },
            ]
          : []),
        ...(file.can
          ? [
              {
                key: "move",
                label: "Move to…",
                icon: <FolderInputGlyph size="sm" />,
                onSelect: () => setOpen("move"),
              },
            ]
          : []),
        ...(own.can
          ? [
              { kind: "separator" as const, key: "sep" },
              {
                key: "trash",
                label: "Move to the trash",
                icon: <TrashGlyph size="sm" />,
                danger: true,
                onSelect: () => setOpen("trash"),
              },
            ]
          : []),
      ];

  return (
    <>
      <Menu
        label={`More for “${item.title}”`}
        icon={<EllipsisGlyph size="sm" />}
        triggerVariant="ghost"
        align="end"
        items={items}
      />
      {open === "rename" && (
        <RenameDialog
          label="Title"
          initial={item.title}
          problem={artifactTitleProblem}
          request={(title) => patchArtifact(item.id, { title })}
          onClose={() => setOpen(null)}
          onRenamed={onChanged}
        />
      )}
      {open === "move" && (
        <MoveDialog
          subject={{ kind: "artifacts", items: [item] }}
          onClose={() => setOpen(null)}
          onMoved={() => {
            setOpen(null);
            onChanged();
          }}
        />
      )}
      <TrashDialog
        detail={item}
        open={open === "trash"}
        onClose={() => setOpen(null)}
        onTrashed={onChanged}
      />
    </>
  );
}

/** What is wrong with an artifact's title, as its rename says it, or undefined. */
export function artifactTitleProblem(title: string): string | undefined {
  const bytes = utf8Bytes(title);
  if (title === "") return "An artifact has a title.";
  return bytes > ARTIFACT_TITLE_MAX_BYTES
    ? `The title is ${bytes} bytes and the most is ${ARTIFACT_TITLE_MAX_BYTES}.`
    : undefined;
}
