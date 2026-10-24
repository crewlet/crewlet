/**
 * Move to…: one dialog for filing artifacts (one from a menu or Details,
 * several from the selection bar), for moving a folder, and for choosing where
 * a new artifact will be filed (a create page's Save in), which writes
 * nothing until the artifact is made.
 *
 * # A searchable list of paths, never a tree to open
 *
 * Every place the reader may file into is one row, its path in words
 * ("Marketing › Campaigns › Q3"), the top level first and the tree in its own
 * order after it, found by typing any part of the path (`Combobox`). The
 * place it is in now is marked Current. A folder the reader cannot see is
 * never listed, and neither is one they may only view: every row is one the
 * engine would file into.
 *
 * # Making a place to move to
 *
 * A phrase that names no folder offers "Create “Launch” in Marketing" at the
 * end of the list: in the folder the reader is browsing, or in the folder a
 * path names ("Marketing › Launch"). A new top-level folder asks who sees it
 * first, in the same dialog, since the top level is where that is decided.
 *
 * # Moving never changes who can open anything
 *
 * The dialog says so under the list. Moving a folder into one more people
 * see is the one move that changes who sees something (the folder, never
 * what is in it), so the engine refuses it until the reader has seen that
 * said and confirmed it here.
 */

import { useMemo, useState, type ReactNode } from "react";
import { Button, Combobox, Modal, useToast, type ComboboxOption } from "@crewlethq/ui";
import {
  FolderGlyph,
  FolderPlusGlyph,
  LayersGlyph,
  LockGlyph,
  SearchGlyph,
} from "@crewlethq/icons/glyphs";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { moveFolder } from "~/lib/artifacts/api.ts";
import {
  PATH_SEPARATOR,
  announceFoldersChanged,
  destinations,
  folderNameProblem,
  matchDestinations,
  takenBy,
  useFolderTree,
  type Destination,
  type FolderIndex,
} from "~/lib/artifacts/folders.ts";
import type { ArtifactFolderItem } from "~/protocol/index.ts";
import { useMoveArtifacts, type Movable, type MoveTarget } from "./moves.ts";
import {
  NewFolderFields,
  useCreateFolder,
  useNewFolderForm,
  useVisibilityWords,
} from "./FolderDialogs.tsx";

/**
 * What a move files: artifacts (their id, title and the folder each is in
 * now), one folder, or nothing yet (`place`: a new artifact's Save in, whose
 * choice is handed back and filed by the create).
 */
export type MoveSubject =
  | {
      kind: "artifacts";
      items: readonly { id: string; title: string; folder?: string | undefined }[];
    }
  | { kind: "folder"; folder: ArtifactFolderItem }
  | { kind: "place"; current: string };

/**
 * How many places the list draws for one phrase: a screenful and a scroll.
 * Past it the list says how many more match, so a company at its 2000 folders
 * is narrowed by typing rather than by scrolling through all of them.
 */
const SHOWN = 50;

/** The option values that are not a folder's id (which is a uuid, so never one of these). */
const TOP = "@top";
const CREATE = "@create";
const MORE = "@more";

/** Where a phrase would make a folder, and under what name, or null where it makes none. */
interface Making {
  parent: ArtifactFolderItem | null;
  name: string;
}

/**
 * What a phrase would create: its last name, in the folder the path before it
 * names ("Marketing › Launch", "Marketing/Launch"), or in `within` for a bare
 * name. Null where the phrase names a folder already, names a parent the reader
 * cannot file into, or is no name the engine would take.
 */
export function makingOf(phrase: string, index: FolderIndex, within: string): Making | null {
  const parts = phrase
    .split(/\/|›/)
    .map((p) => p.trim())
    .filter(Boolean);
  const name = parts[parts.length - 1];
  if (!name || folderNameProblem(name)) return null;
  let parent: ArtifactFolderItem | null = within ? (index.get(within) ?? null) : null;
  if (parts.length > 1) {
    const path = parts.slice(0, -1).join(PATH_SEPARATOR).toLocaleLowerCase();
    parent = index.folders.find((f) => index.label(f.id).toLocaleLowerCase() === path) ?? null;
    if (!parent) return null;
  }
  if (parent && (parent.role === "viewer" || parent.depth >= parent.max_depth)) return null;
  if (takenBy(index.children(parent?.id ?? ""), name)) return null;
  return { parent, name };
}

/** One row of the list: the place's glyph, the folders above it quietly, then its name. */
function placeLabel(d: Destination): ReactNode {
  const glyph =
    d.id === "" ? (
      <LayersGlyph size="sm" />
    ) : d.visibility === "private" ? (
      <LockGlyph size="sm" />
    ) : (
      <FolderGlyph size="sm" />
    );
  return (
    <span className="artifacts-move-place">
      <span className="artifacts-move-glyph" aria-hidden="true">
        {glyph}
      </span>
      <span className="artifacts-move-words">
        {d.above && (
          <span className="artifacts-move-above">
            {d.above}
            {PATH_SEPARATOR}
          </span>
        )}
        {d.name}
      </span>
    </span>
  );
}

/** The folder every item is in, or null when they are in different places. */
function commonFolder(items: readonly { folder?: string | undefined }[]): string | null {
  const first = items[0]?.folder ?? "";
  return items.every((i) => (i.folder ?? "") === first) ? first : null;
}

export function MoveDialog({
  subject,
  within,
  onClose,
  onMoved,
}: {
  subject: MoveSubject;
  /** The folder the reader is browsing, where a bare name is created; the items' own folder when left out. */
  within?: string | undefined;
  onClose: () => void;
  /** The move landed (in part, for several): the caller's listing moved. */
  onMoved: (target: MoveTarget) => void;
}) {
  const tree = useFolderTree();
  const index = tree.index;
  const toast = useToast();
  const words = useVisibilityWords();
  const items: Movable[] = useMemo(
    () =>
      subject.kind === "artifacts"
        ? subject.items.map((i) => ({ id: i.id, title: i.title, folder: i.folder ?? "" }))
        : [],
    [subject],
  );
  const current =
    subject.kind === "folder"
      ? (subject.folder.parent ?? "")
      : subject.kind === "place"
        ? subject.current
        : commonFolder(subject.items);
  const context = within ?? current ?? "";
  const all = useMemo(
    () =>
      destinations(index, subject.kind === "folder" ? { folder: subject.folder.id } : undefined),
    [index, subject],
  );

  const [phrase, setPhrase] = useState("");
  const [listOpen, setListOpen] = useState(true);
  const [chosen, setChosen] = useState<MoveTarget | null>(null);
  const [step, setStep] = useState<"choose" | "create" | "widen">("choose");
  const artifacts = useMoveArtifacts();
  const folderGesture = useKeyedGesture();
  // A NEW TOP-LEVEL FOLDER is the form's (it asks who sees it); a subfolder
  // made straight from the list is the same create without the form.
  const creating = useNewFolderForm(null, index);
  const subfolder = useCreateFolder();
  const busy = artifacts.busy || folderGesture.busy || creating.busy || subfolder.busy;

  // WHAT THE FIELD SAYS once a place is chosen is that place's path, so the
  // list filters by the phrase only while the reader is still typing one.
  const typing = chosen === null || phrase !== chosenWords(chosen, all);
  const matches = typing ? matchDestinations(all, phrase) : all;
  const make =
    subject.kind !== "folder" && typing && phrase.trim() ? makingOf(phrase, index, context) : null;
  const options: ComboboxOption[] = [
    ...matches.slice(0, SHOWN).map((d) => ({
      value: d.id || TOP,
      label: placeLabel(d),
      ...(current !== null && d.id === current ? { hint: "Current" } : {}),
    })),
    ...(matches.length > SHOWN
      ? [
          {
            value: MORE,
            label: `${matches.length - SHOWN} more match. Keep typing to narrow them.`,
            disabled: true,
          },
        ]
      : []),
    ...(make
      ? [
          {
            value: CREATE,
            label: (
              <span className="artifacts-move-place">
                <span className="artifacts-move-glyph" aria-hidden="true">
                  <FolderPlusGlyph size="sm" />
                </span>
                <span className="artifacts-move-words">
                  {make.parent
                    ? `Create “${make.name}” in ${make.parent.name}`
                    : `Create “${make.name}” at the top level…`}
                </span>
              </span>
            ),
          },
        ]
      : []),
  ];

  // BACK TO THE LIST, open on the phrase the reader typed.
  const back = () => {
    setStep("choose");
    setListOpen(true);
  };

  const choose = (target: MoveTarget, path: string) => {
    setChosen(target);
    setPhrase(path);
    setListOpen(false);
  };

  const commit = async (option: ComboboxOption) => {
    if (option.value === CREATE && make) {
      if (!make.parent) {
        // THE TOP LEVEL ASKS WHO SEES IT, so a new top-level folder is made
        // in a step of its own rather than straight from the list.
        creating.setName(make.name);
        setStep("create");
        return;
      }
      const folder = await subfolder.run({ name: make.name, parent: make.parent.id });
      if (folder) {
        choose(
          { id: folder.id, name: folder.name },
          `${index.label(make.parent.id)}${PATH_SEPARATOR}${folder.name}`,
        );
      }
      return;
    }
    const id = option.value === TOP ? "" : option.value;
    const d = all.find((x) => x.id === id);
    if (d) choose({ id: d.id, name: d.name }, d.path);
  };

  const move = async (confirmWiden = false) => {
    if (!chosen) return;
    if (subject.kind === "place") {
      onMoved(chosen);
      return;
    }
    if (subject.kind === "artifacts") {
      const report = await artifacts.move(items, chosen);
      if (report && report.moved.length > 0) onMoved(chosen);
      else if (report) onClose();
      return;
    }
    const folder = subject.folder;
    const answer = await folderGesture.run(moveFolder(folder.id, chosen.id, confirmWiden));
    if (answer?.kind === "done") {
      announceFoldersChanged();
      const back = folder.parent ?? "";
      toast.ok(`Moved “${folder.name}” to ${chosen.id ? chosen.name : "the top level"}`, {
        action: {
          label: "Undo",
          onClick: () =>
            void folderGesture.run(moveFolder(folder.id, back)).then((undone) => {
              announceFoldersChanged();
              if (undone?.kind === "done") toast.ok(`Moved “${folder.name}” back`);
              else if (undone) {
                toast.show({
                  variant: "warning",
                  title: "It was not moved back",
                  message: undone.text,
                });
              }
            }),
        },
      });
      onMoved(chosen);
    } else if (answer?.kind === "refused" && answer.error?.code === "folder_widens") {
      setStep("widen");
    }
  };

  const title =
    subject.kind === "folder"
      ? `Move “${subject.folder.name}”`
      : subject.kind === "place"
        ? "Save in"
        : items.length === 1
          ? `Move “${items[0]!.title}”`
          : `Move ${items.length} artifacts`;
  const hold = !chosen
    ? "Choose where it goes."
    : current !== null && chosen.id === current && subject.kind !== "place"
      ? items.length > 1
        ? "They are already there."
        : "It is already there."
      : undefined;
  const refusal =
    (folderGesture.answer && folderGesture.answer.kind !== "done" && step === "choose"
      ? folderGesture.answer.text
      : null) ?? subfolder.refusal;

  if (step === "create") {
    const createTop = async () => {
      const folder = await creating.submit();
      if (!folder) return;
      setStep("choose");
      choose({ id: folder.id, name: folder.name }, folder.name);
    };
    return (
      <Modal
        open
        onClose={onClose}
        title="New folder"
        placement="top"
        onBack={back}
        backLabel="Back to Move to…"
        footer={
          <>
            <Button size="small" variant="ghost" onClick={back}>
              Back
            </Button>
            <Button
              size="small"
              variant="primary"
              loading={creating.busy}
              disabledReason={creating.hold}
              onClick={() => void createTop()}
            >
              Create folder
            </Button>
          </>
        }
      >
        <NewFolderFields form={creating} parent={null} onSubmit={() => void createTop()} />
      </Modal>
    );
  }

  if (step === "widen" && chosen && subject.kind === "folder") {
    const destination = index.get(chosen.id);
    const who = destination ? words(destination).long : "More people";
    return (
      <Modal
        open
        onClose={onClose}
        title={`Let more people see “${subject.folder.name}”?`}
        placement="top"
        footer={
          <>
            <Button size="small" variant="ghost" onClick={() => setStep("choose")}>
              Back
            </Button>
            <Button
              size="small"
              variant="primary"
              loading={folderGesture.busy}
              onClick={() => void move(true)}
            >
              Move it
            </Button>
          </>
        }
      >
        <p>
          In {chosen.name}, {who === "More people" ? "more people" : who} will see “
          {subject.folder.name}” and the folders in it. A folder never shares what is in it: each
          artifact keeps its own access.
        </p>
        {folderGesture.answer && folderGesture.answer.kind !== "done" && (
          <p className="artifact-refusal">{folderGesture.answer.text}</p>
        )}
      </Modal>
    );
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={title}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={busy}
            disabledReason={hold}
            onClick={() => void move()}
          >
            {subject.kind === "place" ? "Save here" : "Move here"}
          </Button>
        </>
      }
    >
      <form
        className={listOpen ? "artifacts-move is-listing" : "artifacts-move"}
        onSubmit={(e) => {
          e.preventDefault();
          if (!hold) void move();
        }}
      >
        <Combobox
          label="Folders"
          placeholder="Find a folder"
          aria-label="Find a folder"
          leading={<SearchGlyph size="sm" />}
          width="full"
          autoFocus
          value={phrase}
          onValueChange={(next) => {
            setPhrase(next);
            setListOpen(true);
          }}
          open={listOpen}
          onOpenChange={setListOpen}
          options={options}
          onCommit={(option) => void commit(option)}
          commitOn="click"
          emptyMessage={
            tree.loading
              ? "Reading the folders…"
              : `No folder you can file into matches “${phrase.trim()}”.`
          }
        />
        <p className="artifacts-folder-note">
          {subject.kind === "folder"
            ? "Moving a folder never changes who can open what is in it."
            : subject.kind === "place"
              ? "Where it is filed never changes who can open it."
              : items.length > 1
                ? "Moving never changes who can open them."
                : "Moving never changes who can open it."}
        </p>
        {refusal && <p className="artifact-refusal">{refusal}</p>}
      </form>
    </Modal>
  );
}

/** The words the field holds once a place is chosen: its whole path. */
function chosenWords(chosen: MoveTarget, all: readonly Destination[]): string {
  return all.find((d) => d.id === chosen.id)?.path ?? chosen.name;
}
