/**
 * The Settings panel: what an artifact is called and says about itself, what its
 * latest version lets its page do, who owns it, who has opened it, and the
 * trash.
 *
 * EVERY CONTROL IS DRAWN FOR EVERY READER and says why it is held: renaming is
 * an editor's, giving the artifact to another seat and moving it to the trash
 * are its owner's and a person's (D06), and deleting it for good is an
 * administrator's, on a trashed artifact, with a fresh proof of who they are
 * (D07), which the REST transport asks for and replays.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  ConfirmField,
  FormField,
  Input,
  Modal,
  Select,
  Textarea,
  confirmMatches,
  useToast,
} from "@crewlethq/ui";
import { artifactSectionPath } from "~/app/routes.ts";
import { useNavigator } from "~/app/router.tsx";
import { QueryState, Section } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime, utf8Bytes } from "~/lib/format.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { useQuery } from "~/lib/useQuery.ts";
import {
  ARTIFACT_DESCRIPTION_MAX_BYTES,
  ARTIFACT_ICON_MAX_BYTES,
  ARTIFACT_TITLE_MAX_BYTES,
} from "~/contract/artifacts.ts";
import type { ArtifactDetail } from "~/protocol/index.ts";
import {
  patchArtifact,
  purgeArtifact,
  transferArtifact,
  trashArtifact,
} from "~/lib/artifacts/api.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import {
  CAPABILITY_WORDS,
  TRASH_KEPT,
  purgeSentence,
  roleIncludes,
} from "~/lib/artifacts/words.ts";

/** A text field's bytes against its bound, as the sentence under it says. */
function overBy(text: string, max: number): string | undefined {
  const bytes = utf8Bytes(text);
  return bytes > max ? `This is ${bytes} bytes and the most is ${max}.` : undefined;
}

export function SettingsTab({
  detail,
  onChanged,
  focus,
}: {
  detail: ArtifactDetail;
  onChanged: () => void;
  /** A field to put the reader in on opening: the icon, from More's "Change icon". */
  focus?: "icon" | undefined;
}) {
  return (
    <div className="artifact-settings">
      <Details detail={detail} onChanged={onChanged} focus={focus} />
      <Capabilities detail={detail} />
      <Ownership detail={detail} onChanged={onChanged} />
      {roleIncludes(detail.role, "editor") && <SeenBy detail={detail} />}
      <Section title="Trash" hint="A trashed artifact stops opening for everyone">
        <p className="artifact-note">
          Its owner can restore it for {TRASH_KEPT}. After that it is deleted for good, with every
          version and its data.
        </p>
        <div className="artifact-form-actions">
          <TrashControl detail={detail} />
        </div>
      </Section>
    </div>
  );
}

/** An artifact's details as the panel holds them while they are typed. */
interface ArtifactDetails {
  readonly title: string;
  readonly description: string;
  readonly icon: string;
}

/** A detail set to a value its own field would take, or refused in that field's sentence. */
function detail(
  key: keyof ArtifactDetails,
  problem: (text: string) => string | undefined,
): Pick<DraftFieldSpec<ArtifactDetails>, "read" | "write"> {
  return {
    read: (d) => d[key],
    write: (d, value) => {
      if (typeof value !== "string") return refuse("A detail is a line of text.");
      const wrong = problem(value);
      return wrong ? refuse(wrong) : { ...d, [key]: value };
    },
  };
}

/** The fields of an artifact's details, each held to the bound its field states. */
export const DETAIL_FIELDS: readonly DraftFieldSpec<ArtifactDetails>[] = [
  {
    path: "title",
    label: "Title",
    ...detail("title", (t) =>
      t.trim() === "" ? "An artifact has a title." : overBy(t.trim(), ARTIFACT_TITLE_MAX_BYTES),
    ),
  },
  {
    path: "description",
    label: "Description",
    hint: "One sentence, shown under the title",
    ...detail("description", (t) => overBy(t.trim(), ARTIFACT_DESCRIPTION_MAX_BYTES)),
  },
  {
    path: "icon",
    label: "Icon",
    hint: "One short word, such as chart or calendar",
    ...detail("icon", (t) => overBy(t.trim(), ARTIFACT_ICON_MAX_BYTES)),
  },
];

/** The title, description and icon word. */
function Details({
  detail,
  onChanged,
  focus,
}: {
  detail: ArtifactDetail;
  onChanged: () => void;
  focus: "icon" | undefined;
}) {
  const access = useArtifactAccess({ role: "editor", held: detail.role });
  // AN ORDINARY EFFECT, so it runs after the panel's own layout effect has
  // put focus on its heading: the field asked for is where the reader lands.
  const iconField = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (focus === "icon") iconField.current?.focus();
  }, [focus]);
  const gesture = useKeyedGesture();
  const toast = useToast();
  const [title, setTitle] = useState(detail.title);
  const [description, setDescription] = useState(detail.description ?? "");
  const [icon, setIcon] = useState(detail.icon ?? "");
  useEffect(() => {
    setTitle(detail.title);
    setDescription(detail.description ?? "");
    setIcon(detail.icon ?? "");
  }, [detail.title, detail.description, detail.icon]);
  const titleWrong =
    title.trim() === ""
      ? "An artifact has a title."
      : overBy(title.trim(), ARTIFACT_TITLE_MAX_BYTES);
  const descriptionWrong = overBy(description.trim(), ARTIFACT_DESCRIPTION_MAX_BYTES);
  const iconWrong = overBy(icon.trim(), ARTIFACT_ICON_MAX_BYTES);
  const changed =
    title.trim() !== detail.title ||
    description.trim() !== (detail.description ?? "") ||
    icon.trim() !== (detail.icon ?? "");
  const save = async () => {
    const fields: { title?: string; description?: string; icon?: string } = {};
    if (title.trim() !== detail.title) fields.title = title.trim();
    if (description.trim() !== (detail.description ?? "")) fields.description = description.trim();
    if (icon.trim() !== (detail.icon ?? "")) fields.icon = icon.trim();
    const answer = await gesture.run(patchArtifact(detail.id, fields));
    if (answer?.kind === "done") {
      toast.ok("Saved");
      onChanged();
    }
  };
  const held = access.can ? null : access.reason;
  // WHAT IS OPEN, for the assistant, to an editor: the details as typed, set
  // through the panel's own fields and held to their own rules; Save stays
  // the editor's.
  useAssistantDraft<ArtifactDetails>(
    access.can
      ? {
          kind: "client",
          id: detail.id,
          revision: detail.head_number,
          step: "details",
          help:
            "You can set the title, the one-sentence description and the icon word. Saving " +
            "renames the artifact for everyone it is shared with, and stays the person's.",
          scope: `The details of “${detail.title}” (not saved)`,
          value: { title, description, icon },
          fields: DETAIL_FIELDS,
          apply: (next) => {
            setTitle(next.title);
            setDescription(next.description);
            setIcon(next.icon);
          },
          blocked: () => (gesture.busy ? "The details are being saved." : null),
        }
      : null,
  );
  return (
    <Section title="Details" hint="Its name in the gallery, a tab and a link">
      <div className="artifact-form">
        <FormField label="Title" error={titleWrong}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="full"
              value={title}
              disabled={held !== null}
              onChange={(e) => setTitle(e.target.value)}
            />
          )}
        </FormField>
        <FormField
          label="Description"
          optional
          error={descriptionWrong}
          helper="One sentence, shown under the title."
        >
          {(field) => (
            <Textarea
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              value={description}
              disabled={held !== null}
              rows={2}
              onChange={(e) => setDescription(e.target.value)}
            />
          )}
        </FormField>
        <FormField
          label="Icon"
          optional
          error={iconWrong}
          helper="One short word, such as chart or calendar."
        >
          {(field) => (
            <Input
              ref={iconField}
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="sm"
              value={icon}
              disabled={held !== null}
              onChange={(e) => setIcon(e.target.value)}
            />
          )}
        </FormField>
        {gesture.answer && gesture.answer.kind !== "done" && (
          <p className="artifact-refusal">{gesture.answer.text}</p>
        )}
        <div className="artifact-form-actions">
          <Button
            size="small"
            variant="primary"
            loading={gesture.busy}
            disabledReason={
              held ??
              (titleWrong || descriptionWrong || iconWrong
                ? "Fix the fields marked first."
                : !changed
                  ? "Nothing has changed."
                  : undefined)
            }
            onClick={() => void save()}
          >
            Save
          </Button>
        </div>
      </div>
    </Section>
  );
}

/** What the latest version declared its page may do, in words. */
function Capabilities({ detail }: { detail: ArtifactDetail }) {
  const declared = detail.capabilities;
  return (
    <Section title="What it can do" hint="What its latest version declared">
      <p className="artifact-note">
        Its page cannot do more than this, and the engine checks every call it makes.
      </p>
      {declared.length === 0 ? (
        <p className="artifact-note">Nothing beyond showing itself.</p>
      ) : (
        <ul className="artifact-list" role="list">
          {declared.map((c) => (
            <li key={c}>{CAPABILITY_WORDS[c]}</li>
          ))}
        </ul>
      )}
      {detail.contract && <p className="artifact-note">Runtime contract {detail.contract}.</p>}
    </Section>
  );
}

/** Who owns it, and giving it to another seat. */
function Ownership({ detail, onChanged }: { detail: ArtifactDetail; onChanged: () => void }) {
  const people = useArtifactPeople();
  const access = useArtifactAccess({ role: "owner", held: detail.role, person: true });
  const [open, setOpen] = useState(false);
  const [to, setTo] = useState("");
  const gesture = useKeyedGesture();
  const toast = useToast();
  const seats = useMemo(
    () =>
      people.index.seats
        .filter((s) => s.handle !== detail.owner)
        .map((s) => ({ value: s.handle, label: s.name, description: s.handle })),
    [people, detail.owner],
  );
  const transfer = async () => {
    const answer = await gesture.run(transferArtifact(detail.id, to));
    if (answer?.kind === "done") {
      toast.ok(`Gave it to ${people.seatName(to)}`);
      setOpen(false);
      onChanged();
    }
  };
  return (
    <Section title="Owner" hint="Who shares it, trashes it and gives it away">
      <p className="artifact-note">{people.seatName(detail.owner)} owns it.</p>
      <div className="artifact-form-actions">
        <Button
          size="small"
          variant="secondary"
          disabledReason={access.can ? undefined : access.reason}
          onClick={() => setOpen(true)}
        >
          Give it to another seat
        </Button>
      </div>
      <Modal
        open={open}
        onClose={() => setOpen(false)}
        title={`Give “${detail.title}” to another seat`}
        placement="top"
        footer={
          <>
            <Button size="small" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              size="small"
              variant="primary"
              loading={gesture.busy}
              disabledReason={to ? undefined : "Pick who gets it."}
              onClick={() => void transfer()}
            >
              Give it to them
            </Button>
          </>
        }
      >
        <FormField
          label="New owner"
          helper="They become its owner, and you keep no role on it unless they share it with you."
          error={gesture.answer && gesture.answer.kind !== "done" ? gesture.answer.text : undefined}
        >
          {(field) => (
            <Select
              id={field.id}
              value={to}
              onChange={(value) => setTo(String(value))}
              options={seats}
              searchable
              placeholder="Pick a seat"
              ariaLabel="New owner"
            />
          )}
        </FormField>
      </Modal>
    </Section>
  );
}

/**
 * Who has opened it (D21): its owner's and editors', and only while the
 * company shows it, which the engine decides. A reader it withholds the list
 * from is answered `shown: false` rather than refused, and the section is left
 * out for them.
 */
function SeenBy({ detail }: { detail: ArtifactDetail }) {
  const people = useArtifactPeople();
  const now = useNow();
  const seen = useQuery("artifact_seen_by", { id: detail.id });
  if (seen.data && !seen.data.shown) return null;
  const reads = seen.data?.reads ?? [];
  return (
    <Section title="Seen by" hint="Who has opened it, most recent first">
      <QueryState error={seen.error} refusal={seen.refusal} loading={seen.loading}>
        {reads.length === 0 ? (
          <p className="artifact-note">Nobody has opened it yet.</p>
        ) : (
          <ul className="artifact-list" role="list">
            {reads.map((read) => (
              <li key={read.seat}>
                {people.seatName(read.seat)}, {relTime(read.opened_at, now)}
              </li>
            ))}
          </ul>
        )}
      </QueryState>
    </Section>
  );
}

/** Moving the artifact to the trash. */
export function TrashControl({ detail }: { detail: ArtifactDetail }) {
  const access = useArtifactAccess({ role: "owner", held: detail.role, person: true });
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button
        size="small"
        variant="danger"
        disabledReason={access.can ? undefined : access.reason}
        onClick={() => setOpen(true)}
      >
        Move to the trash
      </Button>
      <TrashDialog detail={detail} open={open} onClose={() => setOpen(false)} />
    </>
  );
}

/**
 * Moving an artifact to the trash, said before it is done: from Settings'
 * own button, the artifact bar's More and a gallery card's More. Its
 * owner's, and a person's; the caller offers it only to whoever may.
 *
 * WHERE THE READER GOES AFTER is the caller's (`onTrashed`): from the
 * artifact's own page, to Yours, since the page it stood on stops opening;
 * from the gallery, nowhere, since the card leaves the list in place.
 */
export function TrashDialog({
  detail,
  open,
  onClose,
  onTrashed,
}: {
  detail: Pick<ArtifactDetail, "id" | "title">;
  open: boolean;
  onClose: () => void;
  onTrashed?: () => void;
}) {
  const gesture = useKeyedGesture();
  const toast = useToast();
  const nav = useNavigator();
  const trash = async () => {
    const answer = await gesture.run(trashArtifact(detail.id));
    if (answer?.kind === "done") {
      toast.ok(`Moved “${detail.title}” to the trash`);
      onClose();
      if (onTrashed) onTrashed();
      else nav.to(artifactSectionPath("yours"));
    }
  };
  return (
    <Modal
      open={open}
      onClose={onClose}
      title={`Move “${detail.title}” to the trash?`}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button size="small" variant="danger" loading={gesture.busy} onClick={() => void trash()}>
            Move to the trash
          </Button>
        </>
      }
    >
      <p>
        It stops opening for everybody it is shared with, at once.{" "}
        {purgeSentence(new Date().toISOString())} After that every version and its data are gone.
      </p>
      {gesture.answer && gesture.answer.kind !== "done" && (
        <p className="artifact-refusal">{gesture.answer.text}</p>
      )}
    </Modal>
  );
}

export function PurgeControl({ detail }: { detail: ArtifactDetail }) {
  const access = useArtifactAccess({ admin: true, person: true });
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const gesture = useKeyedGesture();
  const toast = useToast();
  const nav = useNavigator();
  const purge = async () => {
    const answer = await gesture.run(purgeArtifact(detail.id));
    if (answer?.kind === "done") {
      toast.ok(`Deleted “${detail.title}”`);
      setOpen(false);
      nav.to(["artifacts"]);
    }
  };
  return (
    <>
      <Button
        size="small"
        variant="danger"
        disabledReason={access.can ? undefined : access.reason}
        onClick={() => setOpen(true)}
      >
        Delete for good
      </Button>
      <Modal
        open={open}
        onClose={() => setOpen(false)}
        title={`Delete “${detail.title}”?`}
        placement="top"
        footer={
          <>
            <Button size="small" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              size="small"
              variant="danger"
              loading={gesture.busy}
              disabledReason={
                confirmMatches(typed, detail.title) ? undefined : "Type the title to confirm."
              }
              onClick={() => void purge()}
            >
              Delete for good
            </Button>
          </>
        }
      >
        <p>This deletes every version and its data for everyone. It cannot be undone.</p>
        <ConfirmField phrase={detail.title} value={typed} onValueChange={setTyped} autoFocus />
        {gesture.answer && gesture.answer.kind !== "done" && (
          <p className="artifact-refusal">{gesture.answer.text}</p>
        )}
      </Modal>
    </>
  );
}
