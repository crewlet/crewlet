/**
 * The identity directory's writes that more than one screen offers: inviting
 * somebody (Settings › People & access, and a human seat nobody holds), the
 * grants a write confers, a value shown once, and what a write came to.
 *
 * Every write goes through `lib/iamWrite.ts`, so the step-up, the operation key
 * a retry sends and the four answers are decided once. And they are offered to
 * a reader holding `people:manage` only — the grant every one of them is
 * refused without ([canManagePeople]). People & access is read by auditors
 * (`audit:read`) as well, and a dozen controls disabled with one reason is
 * noise on the record they came to read; the engine refuses whatever reaches
 * it all the same.
 */

import { useMemo, useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  Copyable,
  FormField,
  Input,
  Modal,
  Select,
  Text,
  type SelectOption,
} from "@crewlethq/ui";
import { UserPlusGlyph } from "@crewlethq/icons/glyphs";
import { GRANTS } from "~/contract/identity.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useIamGesture, type IamAnswer, type IamGesture } from "~/lib/iamWrite.ts";
import { useRest } from "~/lib/useRest.ts";
import { rest } from "~/protocol/index.ts";

/** The grant every directory write is decided on. */
export const PEOPLE_MANAGE = "people:manage";

/** Whether a reader's grants reach the directory's writes. */
export function canManagePeople(grants: readonly string[]): boolean {
  return grants.includes(PEOPLE_MANAGE);
}

/** What each grant opens, in a line — beside the grant's own name, never instead of it. */
export const GRANT_WORDS: Record<(typeof GRANTS)[number], string> = {
  "state:read": "Read the company's state: the roster, work, pages, spend",
  "audit:read": "Read transcripts, events and the identity trail",
  "config:read": "Read the company configuration",
  "secrets:read": "Reveal a stored credential's value",
  "work:write": "File and change work items",
  "knowledge:write": "Write knowledge-base pages",
  "config:write": "Change the company configuration — host access, by design",
  "secrets:write": "Store and remove credentials",
  "fleet:operate": "Operate the deployment: backups, retention, the node gate",
  "people:manage": "Invite, change and remove people — the grant that grants",
  "sandbox:run": "Start code-sandbox runs",
};

/**
 * Checkboxes over the grants. One outside `held` is drawn disabled with why —
 * the engine refuses to confer it anyway (a caller may not confer a grant they
 * do not hold), and a box that only fails on submit is a guess. `only` narrows
 * the offer (a token carries a subset of its owner's), and `withheld` greys
 * out what the engine refuses outright, with its reason.
 */
export function GrantPicker({
  value,
  onChange,
  held,
  only,
  withheld,
  legend = "Grants",
}: {
  value: readonly string[];
  onChange: (next: string[]) => void;
  /**
   * What this write may confer: the viewer's own grants — and, on an edit,
   * what the person already holds, which keeping or taking away confers
   * nothing (the engine checks only what an edit ADDS).
   */
  held: readonly string[];
  only?: readonly string[];
  withheld?: { grants: readonly string[]; reason: string };
  legend?: string;
}) {
  const offered = GRANTS.filter((g) => !only || only.includes(g));
  return (
    <fieldset className="col gap-1" style={{ border: 0, margin: 0, padding: 0 }}>
      <legend className="t-label">{legend}</legend>
      {offered.length === 0 && (
        <Text as="p" variant="caption" tone="secondary">
          Nothing to offer.
        </Text>
      )}
      {offered.map((g) => {
        const refused = withheld?.grants.includes(g)
          ? withheld.reason
          : held.includes(g)
            ? null
            : "You do not hold this grant, so you cannot confer it.";
        return (
          <Checkbox
            key={g}
            label={<span className="mono">{g}</span>}
            description={refused ?? GRANT_WORDS[g]}
            checked={value.includes(g)}
            disabled={refused !== null}
            onCheckedChange={(on) => onChange(on ? [...value, g] : value.filter((x) => x !== g))}
          />
        );
      })}
    </fieldset>
  );
}

/**
 * A value the engine shows once — an invitation's link, a reset link, a token —
 * with a copy control and the sentence saying what it is. Nothing keeps it:
 * closing the dialog is the last anybody sees of it.
 */
export function ShownOnce({
  label,
  value,
  children,
}: {
  label: string;
  value: string;
  /** What the value is, and how long it lasts. */
  children: React.ReactNode;
}) {
  return (
    <div className="col gap-2">
      <FormField label={label}>
        {() => <Copyable value={value} variant="block" monospace ariaLabel={`Copy the ${label}`} />}
      </FormField>
      <Callout variant="warning">{children}</Callout>
    </div>
  );
}

/** What a write came to, where it is not simply done. */
export function IamOutcome({ answer }: { answer: IamAnswer | null }) {
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
  if (answer.pending) {
    return (
      <Callout variant="neutral" role="status">
        Recorded. This node has not applied it yet, so the lists here catch up in a moment.
      </Callout>
    );
  }
  return null;
}

/** The label a write's button carries: a retry after an unknown answer says so. */
export function pressLabel(write: IamGesture, idle: string, busy: string): string {
  if (write.busy) return busy;
  return write.answer?.kind === "unknown" ? "Try again" : idle;
}

/** One human seat of the running company and who holds it (`GET /iam/seats`). */
export interface HumanSeat {
  handle: string;
  name: string;
  unit?: string;
  holder?: { person: string; login?: string; stage?: string };
}

/** The seats an invitation, a create or a bind may name: those nobody holds. */
export function useUnheldSeats() {
  return useRest("/iam/seats?unheld=true", async (signal) => {
    const answer = (await rest.get("/iam/seats?unheld=true", signal)) as {
      seats?: HumanSeat[] | null;
    } | null;
    return answer?.seats ?? [];
  });
}

/** The value of the "nobody" option in a seat select. */
export const NO_SEAT = "";

/** A seat select's options: nobody, then each seat by name with its handle. */
export function seatOptions(seats: readonly HumanSeat[]): SelectOption[] {
  return [
    { value: NO_SEAT, label: "No seat", description: "Acts under their own login" },
    ...seats.map((s) => ({
      value: s.handle,
      label: s.name || s.handle,
      description: s.handle,
      text: `${s.name} ${s.handle}`,
    })),
  ];
}

/**
 * Invite somebody: an address, the seat redeeming it binds them to, and the
 * grants it confers. On success the link is shown ONCE — it is the credential,
 * and the engine keeps only a hash of its secret.
 */
export function InviteDialog({
  held,
  seat = NO_SEAT,
  onClose,
  onDone,
}: {
  /** The viewer's own grants. */
  held: readonly string[];
  /** A seat to propose — the seat page's own. */
  seat?: string;
  onClose: () => void;
  /** After an invitation landed, so the lists are read again. */
  onDone: () => void;
}) {
  const write = useIamGesture();
  const seats = useUnheldSeats();
  const [email, setEmail] = useState("");
  const [bind, setBind] = useState(seat);
  const [grants, setGrants] = useState<string[]>([]);
  const options = useMemo(() => {
    const list = seats.data ?? [];
    // THE PROPOSED SEAT STAYS OFFERED while the read is out, so a dialog
    // opened from a seat's page does not drop it on the first render.
    const named = list.some((s) => s.handle === seat) || seat === NO_SEAT;
    return seatOptions(named ? list : [...list, { handle: seat, name: seat }]);
  }, [seats.data, seat]);

  const done = write.answer?.kind === "done" ? write.answer.body : null;
  const url = typeof done?.url === "string" ? done.url : "";
  const expires = typeof done?.expires_at === "string" ? done.expires_at : "";

  async function submit() {
    if (email.trim() === "" || done) return;
    const answer = await write.run({
      method: "POST",
      path: "/iam/invitations",
      body: { email: email.trim(), grants, ...(bind ? { seat: bind } : {}) },
    });
    if (answer?.kind === "done") onDone();
  }

  return (
    <Modal
      open
      size="md"
      title="Invite a person"
      icon={<UserPlusGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason="Waiting for the engine to answer."
      stackBody
      onSubmit={() => void submit()}
      footer={
        done ? (
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={write.busy}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" disabled={write.busy || email.trim() === ""}>
              {pressLabel(write, "Invite", "Inviting")}
            </Button>
          </>
        )
      }
    >
      {done ? (
        <ShownOnce label="Invitation link" value={url}>
          This link is the credential: it works once, for {email.trim()}, and expires{" "}
          {fmtDateTime(expires)}. It is shown only now — send it yourself; this engine sends no
          mail. Opening it is where they choose a login and a password.
        </ShownOnce>
      ) : (
        <>
          <FormField label="Email address">
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                type="email"
                autoFocus
                width="full"
                spellCheck={false}
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            )}
          </FormField>
          <FormField
            label="Seat"
            optional
            helper="A human seat nobody holds. Redeeming the invitation binds them to it, so they act as that seat."
          >
            {(field) => (
              <Select
                id={field.id}
                ariaLabel="Seat"
                searchable
                value={bind}
                options={options}
                onChange={(next) => setBind(String(next))}
              />
            )}
          </FormField>
          {seats.error && (
            <Text as="p" variant="caption" tone="secondary">
              The seats nobody holds could not be read: {seats.error.message}
            </Text>
          )}
          <GrantPicker value={grants} onChange={setGrants} held={held} />
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}

/**
 * A gesture that needs only a yes: what it does, said before it is done, and
 * — for a removal — the login typed back, because it cannot be undone.
 */
export function ConfirmDialog({
  title,
  confirm,
  danger,
  typeToConfirm,
  write,
  onConfirm,
  onClose,
  children,
}: {
  title: string;
  /** The button's word: "Suspend", "Remove". */
  confirm: string;
  danger?: boolean;
  /** A value the person types to confirm, where the gesture cannot be undone. */
  typeToConfirm?: string;
  write: IamGesture;
  onConfirm: () => void;
  onClose: () => void;
  children: React.ReactNode;
}) {
  const [typed, setTyped] = useState("");
  const ready = !typeToConfirm || typed.trim() === typeToConfirm;
  return (
    <Modal
      open
      size="sm"
      title={title}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason="Waiting for the engine to answer."
      stackBody
      onSubmit={() => ready && onConfirm()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant={danger ? "danger" : "primary"}
            disabled={write.busy || !ready}
          >
            {pressLabel(write, confirm, "Working")}
          </Button>
        </>
      }
    >
      <div className="t-body">{children}</div>
      {typeToConfirm && (
        <FormField label={`Type ${typeToConfirm} to confirm`}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              autoFocus
              width="full"
              spellCheck={false}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
            />
          )}
        </FormField>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}
