/**
 * Tidying a page from its own screen: rename it, give it labels, move it under
 * another page of its space, move it to the trash, and, for a page in the
 * trash, restore it or delete it for good.
 *
 * # Each through the engine's own route
 *
 * A rename is `POST /doclet/pages/{id}/rename`, the trash `DELETE
 * /doclet/pages/{id}`, a restore `POST …/restore` and a purge `POST …/purge`
 * with the title typed back and a reason, each keyed so a retry after a lost
 * answer is the same write ([useKeyedGesture]). Labels and a move are
 * `save_page` (`labels`, `parent`), against the version on screen. No rule is
 * restated here: who may rename, trash and restore (the page's creator, its
 * space's lead, an admin) and who may purge (an admin) is the engine's to say,
 * and its refusal is drawn in the dialog that made the press. A purge is
 * offered to an admin alone, because it is refused to everybody else.
 *
 * # A page in the trash says so
 *
 * A trashed page is still read by its id, so it says it is in the trash above
 * its body with Restore beside it, and Delete permanently for an admin. A
 * space lists what it has in the trash under its outline ([SpaceTrash]), each
 * linking to the page, which is where it is restored from.
 */

import { useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Combobox,
  ConfirmField,
  FormField,
  Input,
  TagsInput,
  Text,
  confirmMatches,
  useToast,
  type ComboboxOption,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import {
  FolderInputGlyph,
  HashGlyph,
  RotateCcwGlyph,
  SearchGlyph,
  TrashGlyph,
  TypeGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import type { PageMenuEntry } from "~/app/header/PageHeader.tsx";
import { PAGE_LABEL_MAX_BYTES, PAGE_LABELS_MAX, PAGE_TITLE_MAX_BYTES } from "~/contract/pages.ts";
import { holds } from "~/lib/access.ts";
import { utf8Bytes } from "~/lib/format.ts";
import { useKeyedGesture, type KeyedAnswer } from "~/lib/keyedWrite.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { menuHold, useWriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { pathSegment, type Page } from "~/protocol/index.ts";
import { session } from "~/protocol/session.ts";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { pagePath, placesIn } from "../parts.tsx";
import { PAGES_WINDOW } from "../usePagedPages.ts";

/** What a tidy press opens. */
type Tidy = "rename" | "labels" | "move" | "trash" | "purge";

/** The page's own route, for a gesture no tool makes. */
const pageRoute = (id: string) => `/doclet/pages/${pathSegment(id)}`;

/**
 * A keyed page write that landed: the reads it moved are asked again, past
 * the position it landed at, as an act's are.
 */
function landed(answer: KeyedAnswer): boolean {
  if (answer.kind !== "done") return false;
  const position = typeof answer.body.position === "string" ? answer.body.position : null;
  session.written("pages", position, ["knowledge"]);
  return true;
}

/** What a keyed write came to, where it did not land: the engine's words. */
function KeyedNote({ answer }: { answer: KeyedAnswer | null }) {
  if (!answer || answer.kind === "done") return null;
  return (
    <Callout variant={answer.kind === "unknown" ? "warning" : "danger"} role="alert">
      {answer.text}
    </Callout>
  );
}

/**
 * The page's tidy entries for its More menu, and the dialog a press opens.
 * Nothing for a page in the trash: [TrashedNotice] holds its two gestures.
 */
export function usePageTidy(page: Page | undefined): {
  entries: PageMenuEntry[];
  dialog: ReactNode;
} {
  const canEdit = useWriteAccess("save_page");
  const [open, setOpen] = useState<Tidy | null>(null);
  const close = () => setOpen(null);
  if (!page || page.status === "trashed") return { entries: [], dialog: null };
  const entries: PageMenuEntry[] = [
    {
      key: "rename",
      label: "Rename",
      icon: <TypeGlyph size="sm" />,
      onSelect: () => setOpen("rename"),
      ...menuHold(canEdit),
    },
    {
      key: "labels",
      label: "Labels",
      icon: <HashGlyph size="sm" />,
      onSelect: () => setOpen("labels"),
      ...menuHold(canEdit),
    },
    {
      key: "move",
      label: "Move",
      icon: <FolderInputGlyph size="sm" />,
      description: "Under another page of its space",
      onSelect: () => setOpen("move"),
      ...menuHold(canEdit),
    },
    {
      key: "trash",
      label: "Move to trash",
      icon: <TrashGlyph size="sm" />,
      onSelect: () => setOpen("trash"),
      ...menuHold(canEdit),
    },
  ];
  const dialog =
    open === "rename" ? (
      <RenameDialog page={page} onClose={close} />
    ) : open === "labels" ? (
      <LabelsDialog page={page} onClose={close} />
    ) : open === "move" ? (
      <MoveDialog page={page} onClose={close} />
    ) : open === "trash" ? (
      <TrashDialog page={page} onClose={close} />
    ) : null;
  return { entries, dialog };
}

/** A new title, which is the page's address in its space. */
export function RenameDialog({ page, onClose }: { page: Page; onClose: () => void }) {
  const write = useKeyedGesture();
  const toast = useToast();
  const [title, setTitle] = useState(page.title);
  const wanted = title.trim();
  const held =
    wanted === ""
      ? "A page needs a title."
      : wanted === page.title
        ? "That is its title now."
        : utf8Bytes(wanted) > PAGE_TITLE_MAX_BYTES
          ? `A title is at most ${PAGE_TITLE_MAX_BYTES} bytes.`
          : undefined;
  const submit = async () => {
    if (held || write.busy) return;
    const answer = await write.run({
      method: "POST",
      path: `${pageRoute(page.id)}/rename`,
      body: { title: wanted },
    });
    if (answer && landed(answer)) {
      toast.ok(`Renamed to “${wanted}”`);
      onClose();
    }
  };
  return (
    <Modal
      open
      size="sm"
      title={`Rename “${page.title}”`}
      icon={<TypeGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabledReason={held} disabled={write.busy}>
            {write.busy ? "Renaming" : "Rename"}
          </Button>
        </>
      }
    >
      <Text as="p" variant="body">
        The title is the page’s address in its space. Links to the page keep working: they name it
        by its id.
      </Text>
      <FormField label="Title">
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            autoFocus
            width="full"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
          />
        )}
      </FormField>
      <KeyedNote answer={write.answer} />
    </Modal>
  );
}

/** The page's whole label set, replaced in one save. */
function LabelsDialog({ page, onClose }: { page: Page; onClose: () => void }) {
  const save = useAct("save_page");
  const [labels, setLabels] = useState<string[]>(() => [...(page.labels ?? [])]);
  const same =
    labels.length === (page.labels ?? []).length &&
    labels.every((l, i) => l === (page.labels ?? [])[i]);
  const long = labels.find((l) => utf8Bytes(l) > PAGE_LABEL_MAX_BYTES);
  const held = same
    ? "Nothing has changed yet."
    : labels.length > PAGE_LABELS_MAX
      ? `A page carries at most ${PAGE_LABELS_MAX} labels.`
      : long
        ? `A label is at most ${PAGE_LABEL_MAX_BYTES} bytes.`
        : undefined;
  const submit = async () => {
    if (held || save.busy) return;
    const result = await save.run(
      { page: page.id, base_version: page.version, labels },
      { done: `Saved the labels of “${page.title}”` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Labels of “${page.title}”`}
      icon={<HashGlyph />}
      onClose={onClose}
      dismissable={!save.busy}
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={save.busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabledReason={held} disabled={save.busy}>
            {save.busy ? "Saving" : "Save labels"}
          </Button>
        </>
      }
    >
      <FormField label="Labels" helper="Type one, then Enter.">
        {() => (
          <TagsInput
            aria-label="Labels"
            value={labels}
            onChange={setLabels}
            allowCustom
            dedupe
            placeholder={labels.length ? "Add another" : "Add a label, then Enter"}
          />
        )}
      </FormField>
      <RefusalNote write={save} />
    </Modal>
  );
}

/**
 * The pages a page may move under: every page of its space but itself and
 * those beneath it, which the engine refuses as a parent.
 */
export function movePlaces<P extends { id: string; parent_id?: string | null | undefined }>(
  pages: readonly P[],
  id: string,
): P[] {
  const beneath = new Set([id]);
  for (let grew = true; grew;) {
    grew = false;
    for (const p of pages) {
      if (p.parent_id && beneath.has(p.parent_id) && !beneath.has(p.id)) {
        beneath.add(p.id);
        grew = true;
      }
    }
  }
  return pages.filter((p) => !beneath.has(p.id));
}

/** The value the list gives the space's top, which has no page id of its own. */
const TOP = "top";

/**
 * How many places the list stands tall before it scrolls: six, the kit's own
 * default and the Artifacts Move dialog's, a screenful without the dialog
 * outgrowing a laptop window.
 */
const MOVE_ROWS = 6;

/**
 * Another page of its space to file it under, or the space's top.
 *
 * THE LIST IS THE DIALOG'S BODY (the kit's inline `Combobox`, as Artifacts'
 * Move dialog draws it): always on screen under a field that filters it by
 * words from a title or its path, at one height. It was a popup `Select`,
 * whose panel opened down over the dialog's own foot and hid Cancel and Move
 * for as long as it was open.
 */
function MoveDialog({ page, onClose }: { page: Page; onClose: () => void }) {
  const save = useAct("save_page");
  const places = useQuery("pages", {
    container: page.container,
    status: "published,draft",
    skills: false,
    limit: PAGES_WINDOW,
  });
  const current = page.parent_id ?? "";
  const [parent, setParent] = useState(current);
  const [phrase, setPhrase] = useState("");
  const filed = useMemo(
    () => placesIn(movePlaces(places.data?.pages ?? [], page.id)),
    [places.data, page.id],
  );
  const words = phrase.trim().toLowerCase();
  const all = [
    { id: "", title: "Top of the space", path: "" },
    ...filed.map(({ page: p, path }) => ({ id: p.id, title: p.title, path })),
  ];
  const options: ComboboxOption[] = all
    .filter((p) => !words || `${p.path} ${p.title}`.toLowerCase().includes(words))
    .map((p) => {
      const hint = [p.path && `in ${p.path}`, p.id === current && "Current"]
        .filter(Boolean)
        .join(" · ");
      return { value: p.id || TOP, label: p.title, ...(hint ? { hint } : {}) };
    });
  const held = parent === current ? "It is filed there now." : undefined;
  const submit = async () => {
    if (held || save.busy) return;
    const result = await save.run(
      { page: page.id, base_version: page.version, parent },
      { done: `Moved “${page.title}”` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Move “${page.title}”`}
      icon={<FolderInputGlyph />}
      onClose={onClose}
      dismissable={!save.busy}
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={save.busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabledReason={held} disabled={save.busy}>
            {save.busy ? "Moving" : "Move"}
          </Button>
        </>
      }
    >
      <FormField label="File it under" helper="A page of the same space, or its top.">
        {(field) => (
          <Combobox
            id={field.id}
            aria-describedby={field.describedBy}
            label="The page it goes under"
            placeholder="Words from a title"
            leading={<SearchGlyph size="sm" />}
            width="full"
            autoFocus
            value={phrase}
            onValueChange={setPhrase}
            inline
            rows={MOVE_ROWS}
            chosen={parent || TOP}
            options={options}
            onCommit={(option) => setParent(option.value === TOP ? "" : option.value)}
            commitOn="click"
            emptyMessage={
              places.loading
                ? "Reading the space’s pages…"
                : `No page of this space matches “${phrase.trim()}”.`
            }
          />
        )}
      </FormField>
      <RefusalNote write={save} />
    </Modal>
  );
}

/** Out of every listing and search, until it is restored. */
function TrashDialog({ page, onClose }: { page: Page; onClose: () => void }) {
  const write = useKeyedGesture();
  const toast = useToast();
  const submit = async () => {
    if (write.busy) return;
    const answer = await write.run({ method: "DELETE", path: pageRoute(page.id) });
    if (answer && landed(answer)) {
      toast.ok(`Moved “${page.title}” to the trash`);
      onClose();
    }
  };
  return (
    <Modal
      open
      size="sm"
      title={`Move “${page.title}” to the trash?`}
      icon={<TrashGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Keep it
          </Button>
          <Button type="submit" variant="danger" disabled={write.busy}>
            {write.busy ? "Moving to trash" : "Move to trash"}
          </Button>
        </>
      }
    >
      <Text as="p" variant="body">
        No listing or search finds it until it is restored, from the page itself or its space’s
        trash. What stays in the trash past the company’s trash term is deleted for good.
      </Text>
      <KeyedNote answer={write.answer} />
    </Modal>
  );
}

/**
 * A page in the trash, said above its body: Restore, and Delete permanently
 * for an admin.
 */
export function TrashedNotice({ page }: { page: Page }) {
  const viewer = useViewer();
  const restore = useKeyedGesture();
  const toast = useToast();
  const [purging, setPurging] = useState(false);
  const admin = holds(viewer.grants, "admin");
  const press = async () => {
    if (restore.busy) return;
    const answer = await restore.run({ method: "POST", path: `${pageRoute(page.id)}/restore` });
    if (answer && landed(answer)) toast.ok(`Restored “${page.title}”`);
  };
  return (
    <>
      <Callout
        variant="neutral"
        title="This page is in the trash"
        action={
          <span className="knowledge-callout-actions">
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<RotateCcwGlyph size="sm" />}
              disabled={restore.busy}
              onClick={() => void press()}
            >
              {restore.busy ? "Restoring" : "Restore"}
            </Button>
            {admin && (
              <Button size="small" variant="ghost" onClick={() => setPurging(true)}>
                Delete permanently
              </Button>
            )}
          </span>
        }
      >
        No listing or search finds it. Restore it to put it back where it was filed.
        <KeyedNote answer={restore.answer} />
      </Callout>
      {purging && <PurgeDialog page={page} onClose={() => setPurging(false)} />}
    </>
  );
}

/** Deleted for good, with its history: the title typed back, and why. */
export function PurgeDialog({ page, onClose }: { page: Page; onClose: () => void }) {
  const write = useKeyedGesture();
  const toast = useToast();
  const nav = useNavigator();
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");
  const held = !confirmMatches(typed, page.title)
    ? "Type the page’s title to confirm."
    : reason.trim() === ""
      ? "Say why: it is all that is kept."
      : undefined;
  const submit = async () => {
    if (held || write.busy) return;
    const answer = await write.run({
      method: "POST",
      path: `${pageRoute(page.id)}/purge`,
      query: { confirm: page.title, reason: reason.trim() },
    });
    if (answer && landed(answer)) {
      toast.ok(`Deleted “${page.title}” for good`);
      onClose();
      nav.to(["doclet", page.container]);
    }
  };
  return (
    <Modal
      open
      size="sm"
      title={`Delete “${page.title}” permanently?`}
      icon={<TrashGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Keep it
          </Button>
          <Button type="submit" variant="danger" disabledReason={held} disabled={write.busy}>
            {write.busy ? "Deleting" : "Delete permanently"}
          </Button>
        </>
      }
    >
      <Text as="p" variant="body">
        The page, its revisions and its comments are erased on every node, and nothing undoes it.
        The reason is all that is kept.
      </Text>
      <ConfirmField phrase={page.title} value={typed} onValueChange={setTyped} autoFocus />
      <FormField label="Why">
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            width="full"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        )}
      </FormField>
      <KeyedNote answer={write.answer} />
    </Modal>
  );
}

/**
 * What a space holds in the trash, under its outline: each page by its title,
 * linking to the page, where it is restored from. Nothing at all where the
 * trash is empty.
 */
export function SpaceTrash({ space }: { space: string }) {
  const trashed = useQuery("pages", {
    container: space,
    status: "trashed",
    skills: false,
    limit: PAGES_WINDOW,
  });
  const pages = trashed.data?.pages ?? [];
  if (pages.length === 0) return null;
  return (
    <section className="knowledge-trash" aria-labelledby={`trash-${space}`}>
      <h2 className="knowledge-side-title" id={`trash-${space}`}>
        In the trash · {pages.length.toLocaleString()}
      </h2>
      <ul className="knowledge-links" role="list">
        {pages.map((p) => (
          <li key={p.id}>
            <a className="knowledge-link" href={href(pagePath(p.id))}>
              <TrashGlyph size="sm" />
              <span className="knowledge-link-title">{p.title}</span>
            </a>
          </li>
        ))}
      </ul>
    </section>
  );
}
