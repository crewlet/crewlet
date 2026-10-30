/**
 * What a room is for.
 *
 * # Topic and purpose, and deliberately nothing else
 *
 * A room can also be archived, and its retention horizon moved. Neither is
 * here: archiving closes a company's conversation to new messages, and a
 * retention override decides how long a year of it survives on every data
 * node — both gestures with a blast radius, which do not belong behind the
 * same button as fixing a typo in a topic. Archiving is `update_channel`'s
 * `archived`, the room's creator's, its unit's lead's or an admin's, which the
 * dashboard's own assistant proposes for them to approve (or their own MCP
 * client sends over `/operator/mcp`); a horizon is an admin's gesture on the
 * engine's write surface (`PUT /chatlet/channels/{id}/retention`), which asks a
 * fresh proof that it is them, proposed by the assistant the same way or sent
 * directly. THIS FORM OFFERS NEITHER, and the dialog says so and where each is
 * made, rather than leaving a person to look for them.
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

import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { Button, FormField, InlineCode, Input, Modal, Textarea } from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import { MAX_PURPOSE_BYTES, MAX_TOPIC_BYTES } from "~/contract/chat.ts";
import { textBudget } from "~/lib/format.ts";
import type { ChatChannel } from "~/protocol/index.ts";

import { membershipNote } from "./directory.ts";
import { reportFor, SHORTEN, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";

export function RoomSettings({
  channel,
  member,
  onClose,
}: {
  channel: ChatChannel;
  /** Whether the viewer is in the room, for the sentence about who may change
   *  what — the gate itself is the server's. */
  member: boolean;
  onClose: () => void;
}) {
  const update = useAct("update_channel");
  const [topic, setTopic] = useState(channel.topic ?? "");
  const [purpose, setPurpose] = useState(channel.purpose ?? "");
  const [report, setReport] = useState<StartReport | null>(null);
  const busy = update.busy;

  // HELD TO THE CONTRACT'S CAPS IN BYTES, of what is sent — the trimmed
  // value — through the one measure every form in the dashboard uses
  // (`textBudget`), so this dialog and New room cannot disagree about a topic.
  const topicBudget = textBudget(topic, MAX_TOPIC_BYTES, "a topic", SHORTEN);
  const purposeBudget = textBudget(purpose, MAX_PURPOSE_BYTES, "a purpose", SHORTEN);
  const tooLong = topicBudget.over || purposeBudget.over;
  const moved =
    topic.trim() !== (channel.topic ?? "") || purpose.trim() !== (channel.purpose ?? "");
  const ready = moved && !tooLong;
  const blocked = !moved ? "Nothing has changed yet." : tooLong ? "Shorten it first." : undefined;

  async function submit() {
    if (!ready || !pressable(update, blocked)) return;
    setReport(null);
    // ONLY WHAT MOVED TRAVELS. Every field on a patch is absent-means-
    // unchanged, which is the only shape that tells "set this to empty" from
    // "leave it alone" — so sending a field nobody touched would be this
    // dialog asserting a value it merely read. The room's header and the rail
    // both draw what moved, and both are asked again at the write's floor.
    const patch: { topic?: string; purpose?: string } = {};
    if (topic.trim() !== (channel.topic ?? "")) patch.topic = topic.trim();
    if (purpose.trim() !== (channel.purpose ?? "")) patch.purpose = purpose.trim();
    const result = await update.run(
      { channel: channel.id, ...patch },
      { done: "Saved what the room is for", quiet: true },
    );
    if (result === null) return;
    const answer = reportFor("topic", result);
    if (answer.tone === "success") {
      onClose();
      return;
    }
    setReport(answer);
  }

  const note = membershipNote({ kind: channel.kind, member, unit: channel.unit });

  return (
    <Modal
      open
      title="What this room is for"
      icon={<PencilGlyph size="md" />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the engine to answer."
      size="md"
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <WriteButton
            variant="primary"
            write={update}
            blocked={blocked}
            showRefusal={false}
            onPress={() => void submit()}
          >
            {busy ? "Saving" : "Save"}
          </WriteButton>
        </>
      }
    >
      <FormField
        label="Topic"
        helper={
          topicBudget.over
            ? undefined
            : (topicBudget.line ?? "One line, shown beside the room's name wherever it appears.")
        }
        error={topicBudget.over ? topicBudget.line : ""}
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
        helper={
          purposeBudget.over
            ? undefined
            : (purposeBudget.line ?? "What the room is for, for someone deciding whether to join.")
        }
        error={purposeBudget.over ? purposeBudget.line : ""}
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
        A room can&rsquo;t be renamed. Archiving it and how long it keeps messages aren&rsquo;t on
        this form. Ask Crewlet to archive it: the room&rsquo;s creator, its team&rsquo;s lead or an
        admin approves the change it proposes. An admin can ask it to change how long the room keeps
        messages, or send <InlineCode>PUT /chatlet/channels/{"{id}"}/retention</InlineCode>.
      </p>
    </Modal>
  );
}
