/**
 * A listed artifact's own actions, on a gallery card's More and a list row's:
 * pin or unpin, copy its link, rename it and move it to the trash, or, for
 * one in the trash, restore it.
 *
 * ONLY WHAT THE READER MAY DO is offered. Renaming is an editor's, and the
 * trash and a restore are the owner's and a person's (D06), the same rules
 * the artifact's own Settings hold them to; a menu item the reader could not
 * use would only say no after they chose it.
 */

import { useState } from "react";
import { Button, FormField, Input, Menu, Modal, useToast, type MenuEntry } from "@crewlethq/ui";
import {
  CopyGlyph,
  EllipsisGlyph,
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

export function ArtifactMenu({
  item,
  onChanged,
}: {
  item: ArtifactListItem;
  /** The listing moved: a rename, the trash or a restore. */
  onChanged: () => void;
}) {
  const toast = useToast();
  const pin = usePinToggle(item.id, item.pinned ?? false);
  const rename = useArtifactAccess({ role: "editor", held: item.role });
  const own = useArtifactAccess({ role: "owner", held: item.role, person: true });
  const restore = useKeyedGesture();
  const [open, setOpen] = useState<"rename" | "trash" | null>(null);
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
        <RenameDialog item={item} onClose={() => setOpen(null)} onRenamed={onChanged} />
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

/** Renaming an artifact in place: its title, as its Settings' Details names it. */
function RenameDialog({
  item,
  onClose,
  onRenamed,
}: {
  item: Pick<ArtifactListItem, "id" | "title">;
  onClose: () => void;
  onRenamed: () => void;
}) {
  const [title, setTitle] = useState(item.title);
  const gesture = useKeyedGesture();
  const toast = useToast();
  const trimmed = title.trim();
  const bytes = utf8Bytes(trimmed);
  const wrong =
    trimmed === ""
      ? "An artifact has a title."
      : bytes > ARTIFACT_TITLE_MAX_BYTES
        ? `The title is ${bytes} bytes and the most is ${ARTIFACT_TITLE_MAX_BYTES}.`
        : undefined;
  const save = async () => {
    if (wrong || trimmed === item.title) return onClose();
    const answer = await gesture.run(patchArtifact(item.id, { title: trimmed }));
    if (answer?.kind === "done") {
      toast.ok("Renamed");
      onClose();
      onRenamed();
    }
  };
  return (
    <Modal
      open
      onClose={onClose}
      title="Rename"
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={gesture.busy}
            disabledReason={wrong}
            onClick={() => void save()}
          >
            Save
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <FormField label="Title" error={title === item.title ? undefined : wrong}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="full"
              autoFocus
              value={title}
              onChange={(e) => setTitle(e.target.value)}
            />
          )}
        </FormField>
      </form>
      {gesture.answer && gesture.answer.kind !== "done" && (
        <p className="artifact-refusal">{gesture.answer.text}</p>
      )}
    </Modal>
  );
}
