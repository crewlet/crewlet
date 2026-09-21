/**
 * Opening a direct conversation.
 *
 * # It has no name, and it cannot collide
 *
 * A direct conversation's id is DERIVED from the sorted handles of the people
 * in it. So this is not a create that can be beaten to a name: two people
 * opening the same conversation from two nodes converge on one room, and
 * opening one that already exists is not an error at all — it is that room,
 * and this dialog goes to it. What the engine reports is whether the call had
 * to create it, which changes nothing a person does next.
 *
 * # Adding somebody is a DIFFERENT conversation, so this is the only way in
 *
 * Because the id IS the participant set, adding a person to an existing
 * conversation does not widen it — it names another room. The engine refuses
 * the gesture outright, and **this screen must never offer it**: a room
 * settings dialog that showed a membership editor for a direct conversation
 * would be a control whose only possible outcome is a refusal. Starting the
 * larger conversation here is what "adding somebody" actually is, and the note
 * at the foot of this dialog says so.
 */

import { useState } from "react";

import { useNavigator } from "~/app/router.tsx";
import { Button, FormField, Modal, TagsInput } from "@crewlethq/ui";
import { PersonAddGlyph } from "@crewlethq/icons/glyphs";

import { handlesOf, type PersonOption } from "./people.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { openDirect } from "./writes.ts";

/**
 * The most people one conversation may hold — `chat.MaxDMParticipants`, eight,
 * THE AUTHOR INCLUDED.
 *
 * So seven others is the ceiling here, and the engine's own sentence for going
 * past it is the right one to echo: past that a conversation is a room, and a
 * room has a name somebody can join by. The cap is `tracker.MaxCollaborators`
 * in the engine — the same number the company already uses for the people
 * attached to one piece of work.
 */
export const MAX_DIRECT_PARTICIPANTS = 8;

export function StartDirect({
  people,
  onClose,
  onWrote,
}: {
  people: PersonOption[];
  onClose: () => void;
  onWrote: () => void;
}) {
  const nav = useNavigator();
  const [chosen, setChosen] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<StartReport | null>(null);

  const handles = handlesOf(chosen);
  // THE AUTHOR IS ADDED SERVER-SIDE, so the cap here is one short of the
  // engine's: seven others plus you is the eight it will count.
  const others = MAX_DIRECT_PARTICIPANTS - 1;
  const tooMany = handles.length > others;
  const ready = handles.length >= 1 && !tooMany;

  async function submit() {
    if (busy || !ready) return;
    setBusy(true);
    setReport(null);
    try {
      const result = await openDirect(handles);
      const answer = reportFor("direct", result);
      if (result.outcome !== "refused") onWrote();
      if (answer.go) {
        nav.to(["chat", answer.go]);
        onClose();
        return;
      }
      setReport(answer);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      title={handles.length > 1 ? "New group conversation" : "New direct message"}
      icon={<PersonAddGlyph size="md" />}
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
            {busy ? "Opening" : "Open"}
          </Button>
        </>
      }
    >
      <FormField
        label="Who with"
        required
        as="fieldset"
        helper="You are in it — the server adds you, because a caller may never name a seat. Anybody in the company can be addressed directly, seats included: that is what the company runs on."
        error={
          tooMany
            ? `That is ${handles.length + 1} people counting you, and a conversation holds at most ${MAX_DIRECT_PARTICIPANTS}. Past that a conversation is a room, and a room has a name somebody can join by.`
            : ""
        }
      >
        <TagsInput
          value={chosen}
          onChange={setChosen}
          options={people}
          allowCustom={false}
          focusOnMount
          label="The people in this conversation"
          aria-label="The people in this conversation"
          placeholder="Search the company"
          emptyMessage={(query) => `Nobody in the company matches “${query}”.`}
        />
      </FormField>

      <StartNote report={report} />

      <p className="t-caption muted">
        A conversation IS the people in it: its address is derived from their handles, so opening
        one that already exists takes you straight to it. Adding somebody later is not an edit to
        this conversation — it is a different one, opened the same way.
      </p>
    </Modal>
  );
}
