/**
 * A room's lifecycle and its record, each asked once in the page: archiving
 * the room or opening it again, and an administrator's three destructions of
 * what it said (how long it keeps messages, deleting everything before a day,
 * erasing a message for good). The rules and the requests are `roomLifecycle.ts`.
 *
 * # Said before it is done
 *
 * Each dialog says what the gesture does to everybody in the room before it
 * is pressed, and a destruction that cannot be undone asks for the room's
 * name, or a reason the erase record keeps, typed back. While a write is out
 * the dialog stays, so applied, pending and unknown are always told apart
 * (`Dialogs.tsx`' rule); a step-up the engine asks for is confirmed and the
 * same request is replayed (`protocol/rest.ts`).
 */

import { useState } from "react";

import {
  Button,
  Callout,
  ChoiceCards,
  ConfirmField,
  FormField,
  Input,
  Textarea,
  confirmMatches,
  useToast,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import {
  CalendarClockGlyph,
  ClockGlyph,
  PackageGlyph,
  RotateCcwGlyph,
  SettingsGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";

import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useLeadScope } from "~/lib/leadScope.ts";
import { useConnection } from "~/lib/store-hooks.ts";
import { WRITE_REASONS, linkOf, useWriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { fmtDate, fmtTime, fromWall, toWall } from "~/lib/format.ts";
import { useKeyedGesture, type KeyedAnswer, type KeyedGesture } from "~/lib/keyedWrite.ts";
import { useAct } from "~/lib/useAct.ts";
import { useWaiting } from "~/lib/waiting.ts";
import type { ChatChannel, ChatMessageView } from "~/protocol/index.ts";

import { MessageBody } from "./Body.tsx";
import { ChatFace } from "./Face.tsx";
import { useChatFrame } from "./nav.tsx";
import { useChatPeople } from "./people.ts";
import { useRoomDetail } from "./roomDetail.ts";
import { roomWhere } from "./rooms.ts";
import {
  ARCHIVERS,
  archivable,
  archiveArgs,
  archiveHold,
  eraseRequest,
  keepDays,
  keptFor,
  pruneRequest,
  retentionRequest,
} from "./roomLifecycle.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";

/** What a dialog says while the engine has not answered. */
const WAITING = "Waiting for the engine to answer.";

/** The room's name as a sentence names it, once its answer has come. */
function useRoomName(room: string): { where: string; name: string } {
  const detail = useRoomDetail(room);
  const channel = detail.data?.channel;
  const name = channel?.name ?? room;
  return { where: channel ? roomWhere(channel.kind, name) : "this room", name };
}

/* Who may ----------------------------------------------------------- */

/** What the reader may do to a room's lifecycle and record, and why not. */
export interface RoomGestures {
  /** Whether the room offers archiving at all: a named room. */
  archivable: boolean;
  /** Why archiving or reopening is held for this reader, or "". */
  archive: string;
  /**
   * Whether the reader is an administrator, the one reader the three
   * destructions are drawn for (`dashboard-design.md`, the third set of
   * writes drawn for admin only).
   */
  admin: boolean;
  /** Why an administrator's destruction is held right now (offline, say), or "". */
  destroy: string;
}

/**
 * What the reader may do to `channel`'s lifecycle and record: the room's own
 * rule for an archive ([archiveHold]), then the browser's reason for its tool,
 * as every room gesture is held (`scope.tsx`).
 */
export function useRoomGestures(channel: ChatChannel | undefined): RoomGestures {
  const viewer = useViewer();
  const lead = useLeadScope();
  const access = useWriteAccess("update_channel");
  const link = linkOf(useConnection());
  const destroy =
    link !== "open" ? WRITE_REASONS[link] : viewer.loading ? WRITE_REASONS.loading : "";
  if (!channel) return { archivable: false, archive: "", admin: viewer.admin, destroy };
  const rule = archiveHold(channel, {
    handle: viewer.handle,
    admin: viewer.admin,
    leads: lead.units,
  });
  return {
    archivable: archivable(channel),
    archive: rule || (access.can ? "" : access.reason),
    admin: viewer.admin,
    destroy,
  };
}

/* Archive ------------------------------------------------------------- */

/**
 * Archive a room, or open it again: what it does for everybody in it, and who
 * can undo it.
 */
export function ArchiveDialog({
  room,
  archive,
  onClose,
}: {
  room: string;
  /** True archives the room; false opens it again. */
  archive: boolean;
  onClose: () => void;
}) {
  const update = useAct("update_channel");
  const toast = useToast();
  const { where } = useRoomName(room);
  const [report, setReport] = useState<StartReport | null>(null);

  async function press() {
    if (!pressable(update)) return;
    setReport(null);
    const result = await update.run(archiveArgs(room, archive), {
      done: archive ? `Archived ${where}` : `Reopened ${where}`,
      quiet: true,
    });
    if (result === null) return;
    const answered = reportFor("archive", result);
    if (answered.tone === "success" || answered.tone === "pending") {
      toast.show({
        variant: answered.tone === "success" ? "success" : "info",
        message:
          answered.tone === "success"
            ? archive
              ? `${where} is archived`
              : `${where} is open again`
            : answered.note,
      });
      onClose();
      return;
    }
    setReport(answered);
  }

  return (
    <Modal
      open
      onClose={onClose}
      role="alertdialog"
      size="sm"
      icon={archive ? <PackageGlyph size="md" /> : <RotateCcwGlyph size="md" />}
      title={archive ? `Archive ${where}?` : `Reopen ${where}?`}
      dismissable={!update.busy}
      closeDisabledReason={WAITING}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={update.busy}>
            Cancel
          </Button>
          <WriteButton
            variant={archive ? "danger" : "primary"}
            write={update}
            showRefusal={false}
            onPress={() => void press()}
          >
            {update.busy ? (archive ? "Archiving" : "Reopening") : archive ? "Archive" : "Reopen"}
          </WriteButton>
        </>
      }
    >
      <p className="chat-dialog-note">
        {archive
          ? `Everyone in it can still read and search it, and it takes no new messages from anybody. It leaves the room list, and stays under Browse rooms, Archived. Only ${ARCHIVERS} can reopen it.`
          : "It takes new messages again, and comes back to the room list of everybody in it."}
      </p>
      <StartNote report={report} />
    </Modal>
  );
}

/* What a keyed write came to ------------------------------------------ */

/** The answer of an administrator's write, in the dialog that made it. */
function AdminOutcome({ answer }: { answer: KeyedAnswer | null }) {
  if (!answer) return null;
  if (answer.kind === "refused") {
    return (
      <Callout variant="danger" role="alert">
        {answer.text}
      </Callout>
    );
  }
  if (answer.kind === "unknown") {
    return (
      <Callout variant="warning" role="alert" title="Not confirmed">
        {answer.text}
      </Callout>
    );
  }
  return null;
}

/** What a done answer tells the reader, as the toast that closes the dialog. */
function doneWords(answer: Extract<KeyedAnswer, { kind: "done" }>, applied: string): string {
  return answer.pending ? `${applied} This page shows it in a moment.` : applied;
}

/** A keyed write's button label: busy, waiting for the reader's confirmation, or a retry. */
function pressWords(gesture: KeyedGesture, idle: string, busy: string, asked: boolean): string {
  if (gesture.busy) return asked ? "Waiting for you" : busy;
  return gesture.answer?.kind === "unknown" ? "Try again" : idle;
}

/* Retention ----------------------------------------------------------- */

type Horizon = "company" | "forever" | "days";

/** What a reader types back to set a horizon that may delete. */
const DELETE_WORD = "delete";

/**
 * How long a room keeps its messages: the company's setting, for ever, or a
 * number of days. A shorter time deletes what it no longer covers, so the
 * dialog says so and asks it to be ticked off before saving.
 */
export function RetentionDialog({ room, onClose }: { room: string; onClose: () => void }) {
  const detail = useRoomDetail(room);
  const channel = detail.data?.channel;
  const { where } = useRoomName(room);
  const current = channel?.retention_days;
  const [horizon, setHorizon] = useState<Horizon>(
    current === 0 ? "forever" : typeof current === "number" ? "days" : "company",
  );
  const [typed, setTyped] = useState(
    typeof current === "number" && current > 0 ? String(current) : "30",
  );
  const [confirmed, setConfirmed] = useState("");
  const gesture = useKeyedGesture();
  const waiting = useWaiting();
  const toast = useToast();

  const read = horizon === "days" ? keepDays(typed) : null;
  const days: number | null | undefined =
    horizon === "company"
      ? null
      : horizon === "forever"
        ? 0
        : read && "days" in read
          ? read.days
          : undefined;
  // EVERY CHOICE BUT FOR EVER MAY DELETE: the company's horizon and a number of
  // days both cover less than a room kept for ever, and whether this one
  // shortens what the room keeps is the store's to judge, not this dialog's.
  const destroys = horizon !== "forever";
  const ready = days !== undefined && (!destroys || confirmMatches(confirmed, DELETE_WORD));

  async function save() {
    if (!ready || days === undefined || gesture.busy) return;
    const answer = await gesture.run(retentionRequest(room, days));
    if (answer?.kind === "done") {
      toast.show({
        variant: answer.pending ? "info" : "success",
        message: doneWords(answer, `${where} keeps messages: ${keptFor(days).toLowerCase()}.`),
      });
      onClose();
    }
  }

  return (
    <Modal
      open
      size="sm"
      icon={<SettingsGlyph size="md" />}
      title={`How long ${where} keeps messages`}
      onClose={onClose}
      dismissable={!gesture.busy}
      closeDisabledReason={waiting.reason}
      stackBody
      onSubmit={() => void save()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={gesture.busy}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant={destroys ? "danger" : "primary"}
            disabled={gesture.busy || !ready}
          >
            {pressWords(gesture, "Save", "Saving", waiting.asked)}
          </Button>
        </>
      }
    >
      <p className="chat-dialog-note">Now: {keptFor(current)}.</p>
      <ChoiceCards
        label="Keep messages"
        density="compact"
        value={horizon}
        onChange={(value) => {
          setHorizon(value as Horizon);
          setConfirmed("");
        }}
        options={[
          {
            value: "company",
            label: "The company’s setting",
            media: <SettingsGlyph size="md" />,
            description: "As long as every room without its own setting.",
          },
          {
            value: "forever",
            label: "For ever",
            media: <ClockGlyph size="md" />,
            description: "Nothing is deleted for its age.",
          },
          {
            value: "days",
            label: "A number of days",
            media: <CalendarClockGlyph size="md" />,
            description: "Older messages are deleted.",
          },
        ]}
      />
      {horizon === "days" && (
        <FormField label="Days" error={read && "problem" in read ? read.problem : undefined}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              inputMode="numeric"
              value={typed}
              error={field.invalid}
              onChange={(event) => {
                setTyped(event.target.value);
                setConfirmed("");
              }}
            />
          )}
        </FormField>
      )}
      {destroys && (
        <ConfirmField
          phrase={DELETE_WORD}
          helper="Messages older than this are deleted for good, for everyone in the room."
          value={confirmed}
          onValueChange={setConfirmed}
        />
      )}
      <AdminOutcome answer={gesture.answer} />
    </Modal>
  );
}

/* Prune --------------------------------------------------------------- */

/** Today, at the start of the day, as the date field writes it. */
function today(now: number): string {
  return toWall(now).slice(0, 10);
}

/** The start of a typed day, as an RFC 3339 instant, or null for a day that is none. */
export function cutoffOf(day: string): string | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return null;
  const at = fromWall(`${day}T00:00`);
  return at === null ? null : new Date(at).toISOString();
}

/**
 * Delete everything a room said before a day, for good: the day, and the
 * room's name typed back.
 */
export function PruneDialog({ room, onClose }: { room: string; onClose: () => void }) {
  const { where, name } = useRoomName(room);
  const [day, setDay] = useState(() => today(Date.now()));
  const [typed, setTyped] = useState("");
  const gesture = useKeyedGesture();
  const waiting = useWaiting();
  const toast = useToast();
  const cutoff = cutoffOf(day);
  const ready = cutoff !== null && confirmMatches(typed, name);

  async function prune() {
    if (!ready || cutoff === null || gesture.busy) return;
    const answer = await gesture.run(pruneRequest(room, cutoff));
    if (answer?.kind === "done") {
      toast.show({
        variant: answer.pending ? "info" : "success",
        message: doneWords(answer, `Deleted what ${where} said before ${fmtDate(cutoff)}.`),
      });
      onClose();
    }
  }

  return (
    <Modal
      open
      size="sm"
      role="alertdialog"
      icon={<TrashGlyph size="md" />}
      title={`Delete older messages in ${where}?`}
      onClose={onClose}
      dismissable={!gesture.busy}
      closeDisabledReason={waiting.reason}
      stackBody
      onSubmit={() => void prune()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={gesture.busy}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={gesture.busy || !ready}>
            {pressWords(gesture, "Delete", "Deleting", waiting.asked)}
          </Button>
        </>
      }
    >
      <p className="chat-dialog-note">
        Everything posted in {where} before the start of this day is deleted for good, for everyone,
        threads included. The room stays, with what was said since.
      </p>
      <FormField
        label="Delete messages before"
        error={cutoff === null ? "Choose a day." : undefined}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
            type="date"
            value={day}
            error={field.invalid}
            onChange={(event) => setDay(event.target.value)}
          />
        )}
      </FormField>
      <ConfirmField phrase={name} value={typed} onValueChange={setTyped} />
      <AdminOutcome answer={gesture.answer} />
    </Modal>
  );
}

/* Erase --------------------------------------------------------------- */

/**
 * Erase one message for good: the message as the room draws it, and the
 * reason the erase record keeps, which is all that is kept about it.
 */
export function EraseDialog({
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
  const [reason, setReason] = useState("");
  const gesture = useKeyedGesture();
  const waiting = useWaiting();
  const toast = useToast();
  const { message } = view;
  const author = people.person(message.author, message.author_kind);
  const ready = reason.trim() !== "";

  async function erase() {
    if (!ready || gesture.busy) return;
    const answer = await gesture.run(eraseRequest(channelID, [message.id], reason));
    if (answer?.kind === "done") {
      toast.show({
        variant: answer.pending ? "info" : "success",
        message: doneWords(answer, "Erased the message."),
      });
      onClose();
    }
  }

  return (
    <Modal
      open
      size="sm"
      role="alertdialog"
      icon={<TrashGlyph size="md" />}
      title="Erase this message for good?"
      onClose={onClose}
      dismissable={!gesture.busy}
      closeDisabledReason={waiting.reason}
      stackBody
      onSubmit={() => void erase()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={gesture.busy}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={gesture.busy || !ready}>
            {pressWords(gesture, "Erase", "Erasing", waiting.asked)}
          </Button>
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
      <p className="chat-dialog-note">
        Unlike a delete, nothing of it is kept: no trace in the room, and no copy in any record. The
        reason you give is the only account of it that stays.
      </p>
      <FormField label="Why it is erased" required>
        {(field) => (
          <Textarea
            id={field.id}
            aria-describedby={field.describedBy}
            aria-required={field.required}
            value={reason}
            rows={2}
            onChange={(event) => setReason(event.target.value)}
          />
        )}
      </FormField>
      <AdminOutcome answer={gesture.answer} />
    </Modal>
  );
}
