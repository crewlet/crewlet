/**
 * What a person does to a folder itself: make one, rename it, remove it, and
 * the words for who sees one. The More menu that offers these on a folder's
 * tile and its page is `FolderMenu.tsx`.
 *
 * # Who sees a new folder is said once, at the top
 *
 * A top-level folder is made with one of two presets, Everyone at the
 * company or Only you, and every folder inside it is seen by whoever sees it
 * (a subfolder is decided by its top-level folder), so the New folder dialog
 * asks at the top level and tells the reader below it. There is no folder
 * Share dialog: a folder grants nothing on what it holds, and a people
 * picker on a folder is exactly where a reader would conclude it does.
 *
 * # Remove moves things up, and deletes nothing
 *
 * Removing a folder moves every folder and artifact in it up to the folder
 * above, so the words are "Remove", never "Delete", and nobody loses access
 * to anything.
 */

import { useState } from "react";
import {
  ArtifactAccessGlyph,
  Button,
  ChoiceList,
  FormField,
  Input,
  Modal,
  useToast,
} from "@crewlethq/ui";
import { TrashGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { artifactFolderPath } from "~/app/routes.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { createFolder, removeFolder } from "~/lib/artifacts/api.ts";
import {
  announceFoldersChanged,
  folderNameHold,
  folderNameProblem,
  takenBy,
  visibilityWords,
  type FolderIndex,
  type VisibilityWords,
} from "~/lib/artifacts/folders.ts";
import type { ArtifactFolder, ArtifactFolderItem } from "~/protocol/index.ts";

/** The sentence every folder gesture ends on: what a folder is not. */
export const FOLDER_GRANTS_NOTHING =
  "A folder never shares what is in it. Each artifact keeps its own access.";

/** Who sees a folder, in this reader's words: its owner's name, the company's. */
export function useVisibilityWords(): (folder: ArtifactFolder) => VisibilityWords {
  const org = useOrg();
  const viewer = useViewer();
  const people = useArtifactPeople();
  return (folder) => {
    const owner = folder.root_owner ?? folder.owner;
    return visibilityWords(folder.visibility, {
      company: org?.name ?? "",
      mine: viewer.owner !== "" && owner === viewer.owner,
      ownerName: people.seatName(owner),
    });
  };
}

/**
 * The create every new folder goes through, from the New folder dialog and
 * from Move to…'s inline create: the write, then every reader of the tree told
 * and the reader told it was made. Answers the folder made (the chain's last),
 * or null when the engine refused it, whose words are in `refusal`.
 */
export function useCreateFolder() {
  const gesture = useKeyedGesture();
  const toast = useToast();
  const run = async (
    fields: Parameters<typeof createFolder>[0],
  ): Promise<ArtifactFolder | null> => {
    const answer = await gesture.run(createFolder(fields));
    if (answer?.kind !== "done") return null;
    const made = (answer.body.folders as ArtifactFolder[] | undefined) ?? [];
    announceFoldersChanged();
    toast.ok(`Created “${fields.name}”`);
    return made[made.length - 1] ?? null;
  };
  return {
    run,
    busy: gesture.busy,
    refusal: gesture.answer && gesture.answer.kind !== "done" ? gesture.answer.text : null,
  };
}

/** Whether the reader may make a folder in `parent` (the top level for null), and why not. */
export function useNewFolderAccess(parent: ArtifactFolderItem | null): string | null {
  // THE TOP LEVEL IS A PERSON'S, where who sees a folder is decided; below
  // it, filing (the app and a contributor on the folder) is enough.
  const access = useArtifactAccess({ app: true, person: parent === null });
  if (!access.can) return access.reason;
  if (parent && parent.role === "viewer") {
    return "You can see this folder but not file into it. Its owner can make you a contributor.";
  }
  if (parent && parent.depth >= parent.max_depth) {
    return `Folders go at most ${parent.max_depth} deep, and “${parent.name}” is as deep as they go.`;
  }
  return null;
}

/** What a new folder's form holds, and its create. */
interface NewFolderForm {
  name: string;
  setName: (name: string) => void;
  /** The top level's preset, or null below it, where the parent decides. */
  visibility: "company" | "private" | null;
  setVisibility: (visibility: "company" | "private") => void;
  /** Why Create is held, or undefined when it may go. */
  hold: string | undefined;
  busy: boolean;
  /** The engine's refusal of the last create, or null. */
  refusal: string | null;
  /** Make it; the folder made, or null when it was refused. */
  submit: () => Promise<ArtifactFolder | null>;
}

/**
 * The New folder form's state and its write, for the dialog and for the Move
 * dialog's inline create alike. `parent` is the folder it is made in, or null
 * for the top level.
 */
export function useNewFolderForm(
  parent: ArtifactFolderItem | null,
  index: FolderIndex,
  initialName = "",
): NewFolderForm {
  const [name, setName] = useState(initialName);
  const [preset, setVisibility] = useState<"company" | "private">("company");
  const create = useCreateFolder();
  const held = useNewFolderAccess(parent);
  const siblings = index.children(parent?.id ?? "");
  const trimmed = name.normalize("NFC").trim();
  const taken = trimmed ? takenBy(siblings, trimmed) : undefined;
  const hold =
    held ??
    folderNameHold(name) ??
    (taken ? `A folder called “${taken.name}” is already here. Choose another name.` : undefined);
  const submit = async (): Promise<ArtifactFolder | null> => {
    if (hold) return null;
    return create.run({
      name: trimmed,
      ...(parent ? { parent: parent.id } : { visibility: preset }),
    });
  };
  return {
    name,
    setName,
    visibility: parent ? null : preset,
    setVisibility,
    hold,
    busy: create.busy,
    refusal: create.refusal,
    submit,
  };
}

/** The New folder form's fields: the name, then who sees it. */
export function NewFolderFields({
  form,
  parent,
  onSubmit,
}: {
  form: NewFolderForm;
  parent: ArtifactFolderItem | null;
  onSubmit: () => void;
}) {
  const words = useVisibilityWords();
  const org = useOrg();
  const company = org?.name ?? "";
  // THE NAME'S OWN FAULTS are said under it once the reader has typed;
  // the others (the app, the depth) are the Create button's to say.
  const problem = form.name.trim() ? folderNameProblem(form.name) : undefined;
  return (
    <form
      className="artifacts-folder-form"
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit();
      }}
    >
      <FormField label="Name" error={problem}>
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
            width="full"
            autoFocus
            value={form.name}
            onChange={(e) => form.setName(e.target.value)}
          />
        )}
      </FormField>
      {form.visibility !== null ? (
        <FormField as="fieldset" label="Who can see this folder">
          <ChoiceList
            label="Who can see this folder"
            value={form.visibility}
            onChange={(value) => form.setVisibility(value === "private" ? "private" : "company")}
            options={[
              {
                value: "company",
                label: company ? `Everyone at ${company}` : "Everyone in the company",
                detail: "Anyone in the company can find it and file into it.",
                media: <ArtifactAccessGlyph access="company" />,
              },
              {
                value: "private",
                label: "Only you",
                detail: "Nobody else sees it, or anything you file in it, by its place.",
                media: <ArtifactAccessGlyph access="private" />,
              },
            ]}
          />
        </FormField>
      ) : parent ? (
        // BELOW THE TOP LEVEL THE PARENT DECIDES, so the dialog says who that
        // is rather than asking.
        <p className="artifacts-folder-note">
          Who can see it: {words(parent).long}, the same as “{parent.name}”.
        </p>
      ) : null}
      <p className="artifacts-folder-note">{FOLDER_GRANTS_NOTHING}</p>
      {form.refusal && <p className="artifact-refusal">{form.refusal}</p>}
    </form>
  );
}

/**
 * Making a folder: in `parent`, or at the top level when it is null. Opens
 * the new folder where `open` is set (the Folders section), and otherwise
 * leaves the reader where they are, with its tile drawn.
 */
export function NewFolderDialog({
  parent,
  index,
  onClose,
  open = false,
}: {
  parent: ArtifactFolderItem | null;
  index: FolderIndex;
  onClose: () => void;
  /** Go to the new folder once it is made. */
  open?: boolean;
}) {
  const form = useNewFolderForm(parent, index);
  const nav = useNavigator();
  const create = async () => {
    const folder = await form.submit();
    if (!folder) return;
    onClose();
    if (open) nav.to(artifactFolderPath(folder.id));
  };
  return (
    <Modal
      open
      onClose={onClose}
      title={parent ? `New folder in “${parent.name}”` : "New folder"}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={form.busy}
            disabledReason={form.hold}
            onClick={() => void create()}
          >
            Create folder
          </Button>
        </>
      }
    >
      <NewFolderFields form={form} parent={parent} onSubmit={() => void create()} />
    </Modal>
  );
}

/**
 * Removing a folder: everything in it moves up to `parent` (the top level when
 * null), and nothing is deleted. Answers no counts, so the dialog promises
 * none.
 */
export function RemoveFolderDialog({
  folder,
  parent,
  onClose,
  onRemoved,
}: {
  folder: ArtifactFolderItem;
  parent: ArtifactFolderItem | null;
  onClose: () => void;
  onRemoved: () => void;
}) {
  const gesture = useKeyedGesture();
  const toast = useToast();
  const where = parent ? parent.name : "the top level";
  const remove = async () => {
    const answer = await gesture.run(removeFolder(folder.id));
    if (answer?.kind === "done") {
      toast.ok(`Removed “${folder.name}”`);
      announceFoldersChanged();
      onClose();
      onRemoved();
    }
  };
  return (
    <Modal
      open
      onClose={onClose}
      title={`Remove “${folder.name}”?`}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="danger"
            leadingIcon={<TrashGlyph size="sm" />}
            loading={gesture.busy}
            onClick={() => void remove()}
          >
            Remove folder
          </Button>
        </>
      }
    >
      <p>Everything in it moves up to {where}. Nothing is deleted, and nobody loses access.</p>
      {gesture.answer && gesture.answer.kind !== "done" && (
        <p className="artifact-refusal">{gesture.answer.text}</p>
      )}
    </Modal>
  );
}
