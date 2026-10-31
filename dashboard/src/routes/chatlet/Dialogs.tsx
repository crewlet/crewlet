/**
 * Chatlet's dialogs: a gesture in progress, finished or abandoned, never a
 * place (`nav.tsx`'s `ChatDialog`). New room, adding people and agents to a
 * room, a room's topic or description, starting it from a template, deleting
 * a message, and the keyboard shortcuts.
 *
 * # Every write is the engine's to judge, and its answer is said honestly
 *
 * Each of these is a tool called through the act transport, and its answer is
 * one of the three outcomes (`start.ts`): applied, pending (durable, not on
 * this node yet) or unknown. Only an applied answer closes a dialog as done;
 * a pending one says so; an unknown one never navigates and says how to ask
 * again. A refusal keeps the engine's own sentence, which names the rule.
 *
 * WHILE A WRITE IS OUT THE DIALOG STAYS: a stray Escape would leave somebody
 * unable to tell applied from pending from unknown, which is the distinction
 * the report exists to draw.
 *
 * # Phone sheets
 *
 * A dialog with a list in it (New room, Add people, a template, the
 * shortcuts) becomes a sheet on a phone (`phoneSheet`), so its list scrolls in
 * the height a phone has rather than a centred box's.
 */

import { useId, useRef, useState, type ReactNode } from "react";

import {
  Button,
  Checkbox,
  ChipField,
  ChoiceCards,
  FormField,
  Input,
  Kbd,
  Modal,
  PersonRow,
  Switch,
  Tag,
  Textarea,
  useToast,
} from "@crewlethq/ui";
import {
  CalendarClockGlyph,
  EyeGlyph,
  FolderKanbanGlyph,
  HashGlyph,
  KeyGlyph,
  LayoutGridGlyph,
  LockGlyph,
  PackageGlyph,
  PencilGlyph,
  TrashGlyph,
  TriangleAlertGlyph,
  UserPlusGlyph,
} from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { KEYMAP, capsOf, type KeyScope } from "~/app/keymap.ts";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { ADMIN, holds } from "~/lib/access.ts";
import { fmtTime } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import { MAX_CHANNEL_NAME, MAX_PURPOSE_CHARS, MAX_TOPIC_CHARS } from "~/contract/chat.ts";
import type { ChatChannel, ChatMessageView } from "~/protocol/index.ts";

import { MessageBody } from "./Body.tsx";
import { ChatFace } from "./Face.tsx";
import { nameRefusal, normalizeName, startable } from "./name.ts";
import { useChatFrame, useChatNav, type ChatDialog } from "./nav.tsx";
import { handlesOf, matchesPerson, useChatPeople, type ChatPerson } from "./people.ts";
import { useRoomDetail } from "./roomDetail.ts";
import { roomMark, roomTitle } from "./rooms.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { ROOM_TEMPLATES, templateNamed, type RoomTemplate } from "./templates.ts";

/** What a dialog says while the engine has not answered. */
const WAITING = "Waiting for the engine to answer.";

/** The open dialog, drawn by the frame. */
export function ChatDialogHost({
  dialog,
  onClose,
}: {
  dialog: ChatDialog | null;
  onClose: () => void;
}) {
  if (!dialog) return null;
  switch (dialog.kind) {
    case "new-room":
      return <NewRoomDialog onClose={onClose} />;
    case "shortcuts":
      return <ShortcutsDialog onClose={onClose} />;
    case "delete":
      return (
        <DeleteDialog channelID={dialog.channelID} message={dialog.message} onClose={onClose} />
      );
    case "people":
      return <AddPeopleDialog room={dialog.room} onClose={onClose} />;
    case "topic":
    case "description":
      return <RoomTextDialog field={dialog.kind} room={dialog.room} onClose={onClose} />;
    case "template":
      return <TemplateDialog room={dialog.room} onClose={onClose} />;
  }
}

/** A room as its rows name it: its kind's glyph, then its name. */
function RoomLabel({ channel }: { channel: ChatChannel }) {
  const frame = useChatFrame();
  return (
    <span className="chat-inline-mark">
      <NamedGlyph name={roomMark(channel.kind)} size="xs" />
      {roomTitle(channel, { viewer: frame.viewer, nameOf: frame.nameOf })}
    </span>
  );
}

/* Delete -------------------------------------------------------------- */

/** What the delete dialog's button says while a delete is out. */
export const DELETE_IN_FLIGHT = "Deleting";

/**
 * Delete a message, asked once: the message as the room draws it, and what
 * deleting it does. The row stays as a tombstone, so a thread keeps its root.
 */
export function DeleteDialog({
  channelID,
  message: view,
  onClose,
}: {
  channelID: string;
  message: ChatMessageView;
  onClose: () => void;
}) {
  const frame = useChatFrame();
  const people = useChatPeople();
  const remove = useAct("delete_message");
  const toast = useToast();
  const [report, setReport] = useState<StartReport | null>(null);
  const { message } = view;
  const author = people.person(message.author, message.author_kind);

  async function press() {
    if (!pressable(remove)) return;
    setReport(null);
    const result = await remove.run(
      { channel: channelID, message_id: message.id },
      { done: "Deleted the message", quiet: true },
    );
    if (result === null) return;
    if (result.kind === "applied" || result.kind === "pending") {
      toast.show({ variant: "success", message: "Message deleted" });
      onClose();
      return;
    }
    setReport(reportFor("delete", result));
  }

  return (
    <Modal
      open
      onClose={onClose}
      role="alertdialog"
      size="sm"
      icon={<TrashGlyph size="md" />}
      title="Delete this message?"
      dismissable={!remove.busy}
      closeDisabledReason={WAITING}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={remove.busy}>
            Cancel
          </Button>
          <WriteButton
            variant="danger"
            write={remove}
            showRefusal={false}
            onPress={() => void press()}
          >
            {remove.busy ? DELETE_IN_FLIGHT : "Delete"}
          </WriteButton>
        </>
      }
    >
      <div className="chat-delete-preview">
        <ChatFace person={author} size={28} status={false} />
        <div className="chat-delete-preview__text">
          <div className="chat-delete-preview__head">
            <span className="chat-delete-preview__name">{author.name}</span>
            <time className="chat-delete-preview__time" dateTime={message.created_at}>
              {fmtTime(message.created_at)}
            </time>
          </div>
          <div className="chat-delete-preview__body">
            <MessageBody message={message} viewer={frame.viewer} nameOf={people.nameOf} />
          </div>
        </div>
      </div>
      <StartNote report={report} />
      <p className="chat-dialog-note">
        Everyone in the room sees that a message was deleted, and by whom. This can&rsquo;t be
        undone.
      </p>
    </Modal>
  );
}

/* New room ------------------------------------------------------------ */

/**
 * Making a room.
 *
 * THE NAME IS AN ADDRESS, CLAIMED RATHER THAN SET. A create arbitrates on the
 * name: two people typing `#launch` contend and exactly one wins, and the
 * loser is told the name is TAKEN, which this dialog draws as a name to change
 * rather than as a failure. There is no rename, so this is the one moment the
 * name can be got right; it is checked against the engine's own grammar while
 * somebody types (`name.ts`).
 *
 * TWO KINDS. A unit's room is the org chart's: naming a channel on a unit is
 * what makes the engine create it, with a membership it keeps reconciled, so
 * the dialog says where that gesture lives instead of offering it.
 *
 * WHO IS IN IT FROM THE START is on the form because a private room's
 * membership is the only way into it: one made with nobody in it is a room
 * only its author can reach. The visibility opens on the company's own
 * default (`default_private`), so the choice shows what will happen.
 */
export function NewRoomDialog({ onClose }: { onClose: () => void }) {
  const nav = useChatNav();
  const frame = useChatFrame();
  const people = useChatPeople();
  const toast = useToast();
  const create = useAct("create_channel");
  const [typed, setTyped] = useState("");
  const [kind, setKind] = useState<"public" | "private">(
    frame.rail.data?.default_private ? "private" : "public",
  );
  const [topic, setTopic] = useState("");
  const [members, setMembers] = useState<string[]>([]);
  const [report, setReport] = useState<StartReport | null>(null);

  const name = normalizeName(typed);
  const refusal = nameRefusal(typed);
  const topicLength = characters(topic.trim());
  const blocked = !startable(typed)
    ? "Name the room first."
    : topicLength > MAX_TOPIC_CHARS
      ? "Shorten the topic first."
      : undefined;
  const everyone = people.everyone.filter((person) => person.handle !== frame.viewer);

  async function submit() {
    if (!pressable(create, blocked)) return;
    setReport(null);
    const result = await create.run(
      {
        // `channel`, as every chat tool names its room: the name the room
        // claims is the address it answers to from the moment it exists.
        channel: name,
        kind,
        ...(topic.trim() ? { topic: topic.trim() } : {}),
        ...(members.length ? { members: handlesOf(members) } : {}),
      },
      { done: `Created #${name}`, quiet: true },
    );
    if (result === null) return;
    const answer = reportFor("channel", result);
    if (answer.go) {
      onClose();
      nav.openRoom(answer.go);
      toast.show({ variant: "success", message: `#${name} is ready` });
      return;
    }
    setReport(answer);
  }

  return (
    <Modal
      open
      onClose={onClose}
      size="md"
      phoneSheet
      icon={<HashGlyph size="md" />}
      title="New room"
      subtitle="A room is where a topic or a project is talked about. A team’s own room comes from the org chart."
      dismissable={!create.busy}
      closeDisabledReason={WAITING}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={create.busy}>
            Cancel
          </Button>
          <WriteButton
            variant="primary"
            write={create}
            blocked={blocked}
            showRefusal={false}
            onPress={() => void submit()}
          >
            {create.busy ? "Creating" : "Create room"}
          </WriteButton>
        </>
      }
    >
      <div className="chat-form">
        <FormField
          label="Name"
          required
          helper={
            name !== "" && name !== typed.trim()
              ? `It will be #${name}. A room can’t be renamed.`
              : `Lower-case letters, digits, hyphens and underscores, at most ${MAX_CHANNEL_NAME}. A room can’t be renamed.`
          }
          // THE GRAMMAR FIRST, THEN THE ENGINE: a name somebody else holds is
          // the report's, said once on the field somebody has to change.
          error={refusal || (report?.taken ? report.note : "")}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              aria-required={field.required}
              width="full"
              autoFocus
              leading={<span aria-hidden="true">#</span>}
              placeholder="launch"
              value={typed}
              error={field.invalid}
              onChange={(event) => setTyped(event.target.value)}
            />
          )}
        </FormField>
        <ChoiceCards
          label="Who can read it"
          density="compact"
          value={kind}
          onChange={(value) => setKind(value as "public" | "private")}
          options={[
            {
              value: "public",
              label: "Public",
              media: <HashGlyph size="md" />,
              description: "Anyone in the company can find, read and join it.",
            },
            {
              value: "private",
              label: "Private",
              media: <LockGlyph size="md" />,
              description: "Only the people you add can read it. Nobody can join on their own.",
            },
          ]}
        />
        <FormField
          label="Topic"
          optional
          helper="One line saying what is talked about. You can change it later."
          limit={{ length: topicLength, max: MAX_TOPIC_CHARS }}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="full"
              placeholder="Shipping the October release"
              value={topic}
              onChange={(event) => setTopic(event.target.value)}
            />
          )}
        </FormField>
        <FormField
          label="Add people"
          optional={kind !== "private"}
          as="fieldset"
          helper={
            kind === "private"
              ? "You’re in it either way. Anyone not chosen here has to be added by someone already in it."
              : "You’re in it either way, and anyone in the company can join a public room."
          }
        >
          {() => (
            <div className="chat-pick-grid">
              {everyone.map((person) => (
                <Checkbox
                  key={person.handle}
                  label={
                    <span className="chat-inline-mark">
                      <ChatFace person={person} size="xs" status={false} /> {person.name}
                    </span>
                  }
                  checked={members.includes(person.handle)}
                  onCheckedChange={(checked) =>
                    setMembers((chosen) =>
                      checked
                        ? [...chosen, person.handle]
                        : chosen.filter((handle) => handle !== person.handle),
                    )
                  }
                />
              ))}
            </div>
          )}
        </FormField>
        <StartNote report={report?.taken ? null : report} />
      </div>
    </Modal>
  );
}

/* Add people ------------------------------------------------------------ */

/**
 * Add people and agents to a room: find by name, suggestions with their faces
 * and what they are doing, the chosen as chips, and Add.
 *
 * A ROOM'S PEOPLE ARE THE ENGINE'S TO ADD. A unit room's are the org chart's,
 * so the dialog says where to change them and adds nobody; a direct
 * conversation is its people and never reaches here. `add_channel_members`
 * answers who it actually put in, which is what the toast counts: a person
 * already in the room is not news.
 *
 * LEAVING A NEW ROOM'S DIALOG WITH NOBODY CHOSEN ASKS ONCE whether to skip,
 * because a room is best with a few teammates in it from the start.
 *
 * THE AUTO-ADD SWITCH IS AN ADMIN'S, ON A PUBLIC ROOM, and drawn only there:
 * the engine refuses `auto_join` to anybody else and on any other kind (a
 * private room's membership is the only way in, a unit room's is the org
 * chart's), and a switch nobody there could move is a control whose only
 * outcome is a refusal.
 */
export function AddPeopleDialog({ room, onClose }: { room: string; onClose: () => void }) {
  const frame = useChatFrame();
  const people = useChatPeople();
  const toast = useToast();
  const detail = useRoomDetail(room);
  const channel = detail.data?.channel;
  const members = detail.data?.members ?? [];
  const add = useAct("add_channel_members");
  const settings = useAct("update_channel");
  const held = useHeldGrants();
  const admin = held !== null && holds(held, ADMIN);
  const [q, setQ] = useState("");
  const [chosen, setChosen] = useState<string[]>([]);
  const [confirmSkip, setConfirmSkip] = useState(false);
  const [report, setReport] = useState<StartReport | null>(null);
  const field = useRef<HTMLInputElement>(null);
  const listId = useId();

  if (!channel) return null;
  const name = `#${channel.name ?? ""}`;
  const unit = channel.kind === "unit";
  const inRoom = new Set(members.map((member) => member.handle));
  const outside = people.everyone.filter(
    (person) => !inRoom.has(person.handle) && person.handle !== frame.viewer,
  );
  const matches = outside.filter((person) => matchesPerson(person, q));
  // A NEW ROOM is one nobody has said anything in yet: the rail previews only
  // what somebody said, never a room event.
  const fresh = !frame.channels.find((row) => row.channel.id === room)?.last;
  const askSkip = fresh && !unit && chosen.length === 0;
  const leave = () => (askSkip && !confirmSkip ? setConfirmSkip(true) : onClose());
  const busy = add.busy || settings.busy;

  const pick = (handle: string) => {
    setChosen((list) =>
      list.includes(handle) ? list.filter((h) => h !== handle) : [...list, handle],
    );
    // A PICK FROM A SEARCH CLEARS IT, and the field keeps focus so the next
    // name can be typed straight away.
    setQ("");
    field.current?.focus();
  };

  async function submit() {
    if (!pressable(add, chosen.length ? undefined : "Choose someone to add first.")) return;
    setReport(null);
    const result = await add.run(
      { channel: room, members: handlesOf(chosen) },
      { done: `Added people to ${name}`, quiet: true },
    );
    if (result === null) return;
    if (result.kind === "applied" || result.kind === "pending") {
      const added = (result.receipt as { added?: string[] } | null)?.added;
      const count = added?.length ?? chosen.length;
      toast.show({
        variant: "success",
        message:
          count === 0
            ? `Everyone you chose is already in ${name}`
            : `Added ${count === 1 ? people.nameOf(added?.[0] ?? chosen[0] ?? "") : count} to ${name}`,
      });
      onClose();
      return;
    }
    setReport(reportFor("members", result));
  }

  function setAutoJoin(on: boolean) {
    if (!pressable(settings)) return;
    void settings
      .run(
        { channel: room, auto_join: on },
        {
          done: on
            ? `New people and agents join ${name}`
            : `New people and agents no longer join ${name}`,
        },
      )
      .then((result) => {
        if (result && result.kind !== "applied" && result.kind !== "pending") {
          setReport(reportFor("topic", result));
        }
      });
  }

  if (confirmSkip) {
    return (
      <Modal
        open
        onClose={() => setConfirmSkip(false)}
        size="sm"
        icon={<UserPlusGlyph size="md" />}
        title="Skip without adding anyone?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmSkip(false)}>
              Back
            </Button>
            <Button variant="primary" onClick={onClose}>
              Skip for now
            </Button>
          </>
        }
      >
        <p className="chat-dialog-note">
          You can add people and agents later, but a room works best with a few teammates in it from
          the start.
        </p>
      </Modal>
    );
  }

  return (
    <Modal
      open
      onClose={leave}
      size="md"
      phoneSheet
      icon={<UserPlusGlyph size="md" />}
      title={`Add people to ${name}`}
      subtitle="Add people and agents from the company. Everyone you add sees the room’s history."
      dismissable={!busy}
      closeDisabledReason={WAITING}
      footer={
        <>
          <Button variant="ghost" onClick={leave} disabled={busy}>
            {unit ? "Close" : askSkip ? "Skip for now" : "Cancel"}
          </Button>
          {!unit && (
            <WriteButton
              variant="primary"
              write={add}
              blocked={chosen.length ? undefined : "Choose someone to add first."}
              showRefusal={false}
              onPress={() => void submit()}
            >
              {add.busy ? "Adding" : chosen.length > 1 ? `Add ${chosen.length}` : "Add"}
            </WriteButton>
          )}
        </>
      }
    >
      <div className="chat-form">
        {unit ? (
          <p className="chat-dialog-note">
            A team room&rsquo;s members come from the org chart. Change the team there to change who
            is in this room.
          </p>
        ) : (
          <>
            <ChipField
              variant="boxed"
              label="Find people and agents"
              inputRef={field}
              autoFocus
              placeholder={chosen.length ? "Add someone else" : "Enter a name or role"}
              query={q}
              onQueryChange={setQ}
              items={chosen.map((handle) => {
                const person = people.person(handle);
                return {
                  id: handle,
                  label: person.name,
                  leading: <ChatFace person={person} size="xs" status={false} />,
                };
              })}
              onRemove={pick}
              onCommit={() => {
                const first = matches.find((person) => !chosen.includes(person.handle));
                if (first) pick(first.handle);
              }}
              aria-controls={listId}
            />
            <div className="chat-suggest">
              <h3 className="chat-suggest__label">{q ? "Matches" : "Suggestions"}</h3>
              {matches.length === 0 ? (
                <p className="chat-dialog-note">
                  {outside.length
                    ? `Nobody else matches “${q.trim()}”.`
                    : `Everyone in the company is already in ${name}.`}
                </p>
              ) : (
                <ul className="chat-suggest__list" id={listId}>
                  {matches.map((person) => (
                    <li key={person.handle}>
                      <SuggestedPerson
                        person={person}
                        selected={chosen.includes(person.handle)}
                        onSelect={() => pick(person.handle)}
                      />
                    </li>
                  ))}
                </ul>
              )}
            </div>
            <StartNote report={report} />
            {admin && channel.kind === "public" && (
              <div className="chat-admin-setting">
                <span className="chat-admin-setting__label">
                  <EyeGlyph size="xs" /> Only admins can see this setting
                </span>
                <Switch
                  label={
                    <>
                      When new agents and people join the company, add them to{" "}
                      <span className="chat-nowrap">{name}</span>
                    </>
                  }
                  checked={!!channel.auto_join}
                  disabled={settings.busy}
                  onCheckedChange={setAutoJoin}
                />
              </div>
            )}
          </>
        )}
      </div>
    </Modal>
  );
}

/** A person or agent as a pick list offers them: face, name, Agent, what they are doing. */
export function SuggestedPerson({
  person,
  selected,
  onSelect,
}: {
  person: ChatPerson;
  selected?: boolean;
  onSelect: () => void;
}) {
  return (
    <PersonRow
      avatar={<ChatFace person={person} size={32} />}
      name={person.name}
      tag={person.kind === "agent" ? <Tag size="xs">Agent</Tag> : undefined}
      detail={person.doing || person.team || undefined}
      mark={selected === undefined ? "none" : "toggle"}
      selected={selected}
      onSelect={onSelect}
    />
  );
}

/* Topic and description --------------------------------------------------- */

/** The two words a room carries under its name, each edited the same way. */
const ROOM_TEXT = {
  topic: {
    label: "Topic",
    add: "Add a topic",
    edit: "Edit topic",
    helper: "What the room is talking about now. It shows beside the room’s name.",
    placeholder: "Shipping the October release",
    saved: "Topic saved",
    max: MAX_TOPIC_CHARS,
    multiline: false,
  },
  description: {
    label: "Description",
    add: "Add a description",
    edit: "Edit description",
    helper:
      "Let people know what this room is for. Agents in the room read it as context for their replies.",
    placeholder: "What is this room for?",
    saved: "Description saved",
    max: MAX_PURPOSE_CHARS,
    multiline: true,
  },
} as const;

/**
 * How many characters a text holds, as the engine counts them: Unicode code
 * points, so an emoji is one. Never `text.length`, which counts it twice.
 */
export function characters(text: string): number {
  return [...text].length;
}

/**
 * A room's topic or description: the room under the title, the text with its
 * count, and Save once something has changed. ONLY THE FIELD THAT MOVED
 * TRAVELS: a patch's fields are absent-means-unchanged, which is the only shape
 * that tells "set this to empty" from "leave it alone". A topic is one line,
 * so Enter saves it.
 */
export function RoomTextDialog({
  field,
  room,
  onClose,
}: {
  field: keyof typeof ROOM_TEXT;
  room: string;
  onClose: () => void;
}) {
  const toast = useToast();
  const channel = useRoomDetail(room).data?.channel;
  const update = useAct("update_channel");
  const was = (field === "topic" ? channel?.topic : channel?.purpose) ?? "";
  const [text, setText] = useState<string | null>(null);
  const [report, setReport] = useState<StartReport | null>(null);
  if (!channel) return null;
  const value = text ?? was;
  const copy = ROOM_TEXT[field];
  const length = characters(value.trim());
  const changed = value.trim() !== was;
  const blocked = !changed
    ? "Nothing has changed yet."
    : length > copy.max
      ? "Shorten it first."
      : undefined;

  async function submit() {
    if (!pressable(update, blocked)) return;
    setReport(null);
    const result = await update.run(
      {
        channel: room,
        ...(field === "topic" ? { topic: value.trim() } : { purpose: value.trim() }),
      },
      { done: copy.saved, quiet: true },
    );
    if (result === null) return;
    const answer = reportFor("topic", result);
    if (answer.settled && answer.tone !== "warning") {
      toast.show({ variant: "success", message: copy.saved });
      onClose();
      return;
    }
    setReport(answer);
  }

  return (
    <Modal
      open
      onClose={onClose}
      size="md"
      icon={<PencilGlyph size="md" />}
      title={was ? copy.edit : copy.add}
      subtitle={<RoomLabel channel={channel} />}
      dismissable={!update.busy}
      closeDisabledReason={WAITING}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={update.busy}>
            Cancel
          </Button>
          <WriteButton
            variant="primary"
            type="submit"
            write={update}
            blocked={blocked}
            showRefusal={false}
            onPress={() => void submit()}
          >
            {update.busy ? "Saving" : "Save"}
          </WriteButton>
        </>
      }
    >
      <FormField
        label={copy.label}
        hideLabel
        helper={copy.helper}
        limit={{ length, max: copy.max }}
      >
        {(f) =>
          copy.multiline ? (
            <Textarea
              id={f.id}
              aria-describedby={f.describedBy}
              aria-invalid={f.invalid}
              autoFocus
              autoResize
              rows={3}
              placeholder={copy.placeholder}
              value={value}
              onChange={(event) => setText(event.target.value)}
            />
          ) : (
            <Input
              id={f.id}
              aria-describedby={f.describedBy}
              aria-invalid={f.invalid}
              width="full"
              autoFocus
              placeholder={copy.placeholder}
              value={value}
              onChange={(event) => setText(event.target.value)}
            />
          )
        }
      </FormField>
      <StartNote report={report} />
    </Modal>
  );
}

/* Templates ------------------------------------------------------------- */

const TEMPLATE_GLYPH: Record<RoomTemplate["id"], ReactNode> = {
  project: <FolderKanbanGlyph size="md" />,
  incident: <TriangleAlertGlyph size="md" />,
  release: <PackageGlyph size="md" />,
  standup: <CalendarClockGlyph size="md" />,
};

/**
 * Start a room from a template: it sets the room's topic and description in
 * one change, recorded as one template line in the transcript. The preview
 * reads as the room's head will: its name, the topic under it, then the
 * description.
 */
export function TemplateDialog({ room, onClose }: { room: string; onClose: () => void }) {
  const toast = useToast();
  const frame = useChatFrame();
  const channel = useRoomDetail(room).data?.channel;
  const update = useAct("update_channel");
  const [pick, setPick] = useState<RoomTemplate["id"]>(ROOM_TEMPLATES[0].id);
  const [report, setReport] = useState<StartReport | null>(null);
  if (!channel) return null;
  const template = templateNamed(pick);

  async function submit() {
    if (!pressable(update)) return;
    setReport(null);
    const result = await update.run(
      { channel: room, topic: template.topic, purpose: template.purpose, template: template.name },
      { done: `${template.name} template applied`, quiet: true },
    );
    if (result === null) return;
    const answer = reportFor("topic", result);
    if (answer.settled && answer.tone !== "warning") {
      toast.show({ variant: "success", message: `${template.name} template applied` });
      onClose();
      return;
    }
    setReport(answer);
  }

  return (
    <Modal
      open
      onClose={onClose}
      size="md"
      phoneSheet
      icon={<LayoutGridGlyph size="md" />}
      title="Start from a template"
      subtitle="Sets the room’s topic and description. You can change both later."
      dismissable={!update.busy}
      closeDisabledReason={WAITING}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={update.busy}>
            Cancel
          </Button>
          <WriteButton
            variant="primary"
            write={update}
            showRefusal={false}
            onPress={() => void submit()}
          >
            {update.busy ? "Applying" : "Use template"}
          </WriteButton>
        </>
      }
    >
      <div className="chat-form">
        <ChoiceCards
          label="Templates"
          density="compact"
          value={pick}
          onChange={(value) => setPick(value as RoomTemplate["id"])}
          options={ROOM_TEMPLATES.map((entry) => ({
            value: entry.id,
            label: entry.name,
            media: TEMPLATE_GLYPH[entry.id],
            description: entry.blurb,
          }))}
        />
        {/* THE ROOM AS IT WILL READ once the template is used. */}
        <figure className="chat-template-preview">
          <figcaption className="chat-template-preview__caption">Preview</figcaption>
          <div className="chat-template-preview__room">
            <p className="chat-template-preview__name">
              <NamedGlyph name={roomMark(channel.kind)} size="sm" />
              {roomTitle(channel, { viewer: frame.viewer, nameOf: frame.nameOf })}
            </p>
            <p className="chat-template-preview__topic">{template.topic}</p>
          </div>
          <div className="chat-template-preview__about">
            <span className="chat-template-preview__label">Description</span>
            <p className="chat-template-preview__text">{template.purpose}</p>
          </div>
        </figure>
        <StartNote report={report} />
      </div>
    </Modal>
  );
}

/* Shortcuts ------------------------------------------------------------- */

/**
 * The keymap's scopes the dialog lists, under the names a reader looks for
 * them by. THE KEYS ARE THE KEYMAP'S (`app/keymap.ts`), the one place a key is
 * spelled, so the dialog, the `?` legend and the handlers cannot disagree.
 */
export const SHORTCUT_GROUPS: readonly { scope: KeyScope; title: string }[] = [
  { scope: "chat", title: "Moving around" },
  { scope: "chatbox", title: "In the message box" },
  { scope: "message", title: "On a focused message" },
];

/** Chatlet's keys, in the groups a reader looks for them in. */
export function ShortcutsDialog({ onClose }: { onClose: () => void }) {
  return (
    <Modal
      open
      onClose={onClose}
      size="lg"
      phoneSheet
      icon={<KeyGlyph size="md" />}
      title="Keyboard shortcuts"
      subtitle="Chatlet’s own keys, alongside the dashboard’s."
    >
      <div className="chat-shortcuts">
        {SHORTCUT_GROUPS.map(({ scope, title }) => {
          const rows = KEYMAP.filter((row) => row.scope === scope);
          if (rows.length === 0) return null;
          return (
            <section key={scope} className="chat-shortcuts__group" aria-label={title}>
              <h3 className="chat-suggest__label">{title}</h3>
              <dl className="chat-shortcuts__list">
                {rows.map((row) => (
                  <div key={row.id} className="chat-shortcuts__row">
                    <dt>{row.does}</dt>
                    <dd className="chat-shortcuts__keys">
                      {row.presses.map((press, index) => (
                        <Kbd key={index} keys={capsOf(press)} />
                      ))}
                    </dd>
                  </div>
                ))}
              </dl>
            </section>
          );
        })}
      </div>
    </Modal>
  );
}
