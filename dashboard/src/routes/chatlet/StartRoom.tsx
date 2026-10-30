/**
 * Making a room.
 *
 * # The name is an address, and it is claimed rather than set
 *
 * A create arbitrates on the NAME at the broker: two people typing `#launch`
 * contend and exactly one wins. So the field is checked against the engine's
 * own grammar while somebody types — see `name.ts` for why that copy is safe —
 * and the loser of the race is told the name is TAKEN, which this dialog draws
 * as a name to change rather than as a failure. There is no rename anywhere in
 * the value layer, so this is the one moment the name can be got right.
 *
 * # Two kinds, and the third is not this screen's to make
 *
 * `public` and `private` are what a person creates. A UNIT'S ROOM IS THE ORG
 * CHART'S: naming a channel on a unit is what makes the engine create it, with
 * every seat in the unit's subtree as members and a membership it keeps
 * reconciled — so offering "unit" here would offer a room whose membership
 * this screen cannot maintain and whose unit field has to name a box on a
 * chart. The dialog says where that gesture lives instead of hiding it.
 *
 * # Why the founding membership is on this form
 *
 * A PRIVATE ROOM'S MEMBERSHIP IS THE ONLY WAY INTO IT — the engine refuses a
 * join outright — so a private room created with nobody in it is a room only
 * its author can ever reach, and the person who made it has no way to find
 * that out except by waiting for somebody to say they cannot get in.
 */

import { useState } from "react";

import { useNavigator } from "~/app/router.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { Button, FormField, Input, Modal, Select, TagsInput } from "@crewlethq/ui";
import { HashGlyph } from "@crewlethq/icons/glyphs";
import { MAX_CHANNEL_NAME, MAX_TOPIC_BYTES } from "~/contract/chat.ts";
import { textBudget } from "~/lib/format.ts";
import type { ChatKind } from "~/protocol/index.ts";

import { nameRefusal, normalizeName, startable } from "./name.ts";
import type { PersonOption } from "./people.ts";
import { handlesOf } from "./people.ts";
import { reportFor, SHORTEN, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";

export function StartRoom({
  people,
  defaultPrivate = false,
  onClose,
}: {
  /** Everybody who can be put in the room from the start, minus the author. */
  people: PersonOption[];
  /**
   * The company's `chat.chatlet.default_channel_private`, which is what a room
   * created without a stated visibility becomes.
   *
   * IT ONLY DECIDES WHAT THIS FORM OPENS ON. The server resolves an omitted
   * kind itself, so nothing here can make a room more or less private than the
   * policy allows — what it buys is that the selector shows the truth. Opening
   * on "public" under a company whose default is private told the person the
   * opposite of what would happen, on the one control where that matters.
   */
  defaultPrivate?: boolean;
  onClose: () => void;
}) {
  const nav = useNavigator();
  const create = useAct("create_channel");
  const [typed, setTyped] = useState("");
  const [kind, setKind] = useState<ChatKind>(defaultPrivate ? "private" : "public");
  const [topic, setTopic] = useState("");
  const [members, setMembers] = useState<string[]>([]);
  const [report, setReport] = useState<StartReport | null>(null);
  const busy = create.busy;

  const name = normalizeName(typed);
  const refusal = nameRefusal(typed);
  const ready = startable(typed);
  // THE TOPIC IS HELD TO THE ENGINE'S CAP HERE, as the room's own Topic
  // dialog holds it, rather than sent to be refused: measured in bytes of
  // what is sent (`textBudget`), and said on the field.
  const topicBudget = textBudget(topic, MAX_TOPIC_BYTES, "a topic", SHORTEN);
  const blocked = !ready
    ? "Name the room first."
    : topicBudget.over
      ? "Shorten the topic first."
      : undefined;

  async function submit() {
    if (!pressable(create, blocked)) return;
    setReport(null);
    // QUIET: the answer is this dialog's own note (`start.ts`) — a room that
    // landed opens, a name somebody holds goes back to its field — and the
    // rail is asked again at the create's floor, so it needs no nudge. A
    // BARE HANDLE is a founding member who follows only what names them,
    // which is what this dialog has always made.
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
      nav.to(["chatlet", answer.go]);
      onClose();
      return;
    }
    setReport(answer);
  }

  return (
    <Modal
      open
      title="New room"
      icon={<HashGlyph size="md" />}
      onClose={onClose}
      // A CREATE IS A RECORD ON THE LOG and its outcome is three-valued, so a
      // stray Escape while it is in flight would leave somebody unable to tell
      // applied from pending from unknown — which is the distinction the
      // report below exists to draw.
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
            write={create}
            blocked={blocked}
            showRefusal={false}
            onPress={() => void submit()}
          >
            {busy ? "Creating" : "Create"}
          </WriteButton>
        </>
      }
    >
      <FormField
        label="Name"
        required
        helper={`Lower-case letters, digits, hyphens and underscores, at most ${MAX_CHANNEL_NAME}. A room can’t be renamed.`}
        // THE GRAMMAR FIRST, THEN THE ENGINE. The typed value is refused here
        // before a round trip; a name the engine refused for a reason this
        // client cannot know — somebody else holds it — is the report's.
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
            value={typed}
            error={field.invalid}
            onChange={(event) => setTyped(event.target.value)}
            placeholder="launch"
            leading={<span aria-hidden="true">#</span>}
          />
        )}
      </FormField>
      {name !== "" && name !== typed.trim() && (
        // WHAT WILL ACTUALLY BE CLAIMED. A name is lower-cased and trimmed
        // before it is arbitrated on, so somebody typing `Launch` is creating
        // `#launch` — and finding that out from the rail afterwards reads as
        // the engine having renamed their room.
        <p className="t-caption muted">
          This will be <strong>#{name}</strong>.
        </p>
      )}

      <FormField
        label="Who can read it"
        helper="Anyone in the company can read a public room, joined or not. Only members can read a private one, and nobody can join it on their own."
      >
        {(field) => (
          <Select
            id={field.id}
            aria-describedby={field.describedBy}
            width="full"
            value={kind}
            onChange={(value) => setKind(String(value) as ChatKind)}
            options={[
              { value: "public", label: "Public", description: "Anybody in the company" },
              { value: "private", label: "Private", description: "Its members, and nobody else" },
            ]}
          />
        )}
      </FormField>

      <FormField
        label="Topic"
        optional
        helper={
          topicBudget.over
            ? undefined
            : (topicBudget.line ??
              "One line saying what’s being talked about. You can change it from the room.")
        }
        error={topicBudget.over ? topicBudget.line : ""}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
            error={field.invalid}
            width="full"
            value={topic}
            onChange={(event) => setTopic(event.target.value)}
            placeholder="Shipping the October release"
          />
        )}
      </FormField>

      <FormField
        label="Who is in it"
        optional={kind !== "private"}
        as="fieldset"
        helper={
          kind === "private"
            ? "You’re in it either way. Anyone not named here has to be added by someone already in it."
            : "You’re in it either way, and anyone in the company can join a public room."
        }
      >
        <TagsInput
          value={members}
          onChange={setMembers}
          options={people}
          // ONLY SEATS THE COMPANY HAS. A handle typed freehand is dropped by
          // the write path rather than refused, so a room would quietly be
          // created without the person somebody meant to put in it.
          allowCustom={false}
          label="The people in this room from the start"
          aria-label="The people in this room from the start"
          placeholder="Search the company"
          emptyMessage={(query) => `Nobody in the company matches “${query}”.`}
        />
      </FormField>

      {/* A TAKEN NAME IS SAID ONCE, ON THE FIELD SOMEBODY HAS TO CHANGE.
          The same sentence in a callout as well is one dialog saying one
          thing twice, and the copy a reader acts on is the one beside the
          box their caret is in. Every other answer is a callout: none of them
          is about a field. */}
      <StartNote report={report?.taken ? null : report} />

      <p className="t-caption muted">
        A team&rsquo;s own room comes from the org chart. Give the team a channel there, and its
        room is made with everyone in the team in it.
      </p>
    </Modal>
  );
}
