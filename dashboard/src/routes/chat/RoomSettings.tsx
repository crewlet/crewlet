/**
 * What a room is for.
 *
 * # Topic and purpose, and deliberately nothing else
 *
 * A patch may also archive a room and move its retention horizon. Neither is
 * here: archiving closes a company's conversation to new messages for ever
 * after, and a retention override decides how long a year of it survives on
 * every node — both are operator gestures with a blast radius, and both belong
 * beside the other destructive ones rather than behind the same button as
 * fixing a typo in a topic.
 *
 * THERE IS NO NAME FIELD, and that is the value layer's own rule rather than
 * an omission: a room's name is the address its create claimed and this build
 * has no record that moves one. A field here would be a control whose only
 * outcome is a refusal.
 *
 * THERE IS NO MEMBERSHIP EDITOR EITHER, and for a direct conversation that is
 * a correctness rule rather than a scope decision: its id IS its participant
 * set, so adding somebody does not widen this room — it names a different one,
 * which the engine refuses to pretend otherwise about. Opening the larger
 * conversation is how that is done; see `StartDirect.tsx`.
 *
 * # An empty patch is refused, so Save is what tells you nothing changed
 *
 * The engine refuses a patch that sets no field — an empty one is a record on
 * the log, a history row and a wake for a change nobody made — so the button
 * is disabled until something actually differs. A patch that sets fields to
 * what they already hold is legal and publishes nothing, but there is no
 * reason to ask for it.
 */

import { useState } from "react";

import { Button, FormField, Input, Modal, Textarea } from "@crewlethq/ui";
import { EditGlyph } from "@crewlethq/icons/glyphs";
import type { ChatChannel } from "~/protocol/index.ts";

import { membershipNote } from "./directory.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { patchChannel, type ChannelPatch } from "./writes.ts";

/**
 * The caps the engine holds these to — `chat.MaxTopic` and `chat.MaxPurpose`.
 *
 * BYTES, NOT CHARACTERS, which is why they are measured with an encoder: the
 * engine bounds the record's own field, and an emoji in a topic is four of
 * these and one of those. REFUSED RATHER THAN CUT, like every other value with
 * a limit in this tree: cutting somebody's sentence to fit is losing what they
 * wrote.
 */
export const MAX_TOPIC_BYTES = 256;
export const MAX_PURPOSE_BYTES = 600;

const encoder = new TextEncoder();

export function RoomSettings({
  channel,
  member,
  onClose,
  onWrote,
}: {
  channel: ChatChannel;
  /** Whether the viewer is in the room, for the sentence about who may change
   *  what — the gate itself is the server's. */
  member: boolean;
  onClose: () => void;
  onWrote: () => void;
}) {
  const [topic, setTopic] = useState(channel.topic ?? "");
  const [purpose, setPurpose] = useState(channel.purpose ?? "");
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<StartReport | null>(null);

  const topicBytes = encoder.encode(topic).length;
  const purposeBytes = encoder.encode(purpose).length;
  const tooLong = topicBytes > MAX_TOPIC_BYTES || purposeBytes > MAX_PURPOSE_BYTES;
  const moved = topic !== (channel.topic ?? "") || purpose !== (channel.purpose ?? "");
  const ready = moved && !tooLong;

  async function submit() {
    if (busy || !ready) return;
    setBusy(true);
    setReport(null);
    try {
      // ONLY WHAT MOVED TRAVELS. Every field on a patch is absent-means-
      // unchanged, which is the only shape that tells "set this to empty"
      // from "leave it alone" — so sending a field nobody touched would be
      // this dialog asserting a value it merely read.
      const patch: ChannelPatch = {};
      if (topic !== (channel.topic ?? "")) patch.topic = topic;
      if (purpose !== (channel.purpose ?? "")) patch.purpose = purpose;
      const answer = reportFor("topic", await patchChannel(channel.id, patch));
      if (answer.tone === "success") {
        onWrote();
        onClose();
        return;
      }
      if (answer.tone === "pending") onWrote();
      setReport(answer);
    } finally {
      setBusy(false);
    }
  }

  const note = membershipNote({ kind: channel.kind, member });

  return (
    <Modal
      open
      title="What this room is for"
      icon={<EditGlyph size="md" />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the log to acknowledge."
      size="md"
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="tertiary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="primary" onClick={() => void submit()} disabled={busy || !ready}>
            {busy ? "Saving" : "Save"}
          </Button>
        </>
      }
    >
      <FormField
        label="Topic"
        helper="One line, shown beside the room's name wherever it appears."
        error={
          topicBytes > MAX_TOPIC_BYTES
            ? `That is ${topicBytes.toLocaleString()} bytes against a cap of ${MAX_TOPIC_BYTES.toLocaleString()}. Nothing is cut to fit — shorten it.`
            : ""
        }
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
            width="full"
            autoFocus
            value={topic}
            error={field.invalid}
            onChange={(event) => setTopic(event.target.value)}
            placeholder="Shipping the October release"
          />
        )}
      </FormField>

      <FormField
        label="Purpose"
        helper="What the room is for, for somebody deciding whether to be in it. Longer than a topic and read far less often."
        error={
          purposeBytes > MAX_PURPOSE_BYTES
            ? `That is ${purposeBytes.toLocaleString()} bytes against a cap of ${MAX_PURPOSE_BYTES.toLocaleString()}. Nothing is cut to fit — shorten it.`
            : ""
        }
      >
        {(field) => (
          <Textarea
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
            value={purpose}
            rows={4}
            onChange={(event) => setPurpose(event.target.value)}
            placeholder="Everything about getting the release out: what is blocking, what shipped, who is on what."
          />
        )}
      </FormField>

      <StartNote report={report} />

      {note && <p className="t-caption muted">{note}</p>}
      <p className="t-caption muted">
        A room’s name is the address its create claimed, and there is no rename — so it is not on
        this form. Archiving a room and changing how long its messages are kept are not here either:
        both decide what survives on every node, which is an operator’s gesture rather than an edit.
      </p>
    </Modal>
  );
}
