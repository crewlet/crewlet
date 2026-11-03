/**
 * The sheet a person sets an agent up in at a vendor that lets only a person
 * make the agent's account (Expo's robots): make the account at the vendor,
 * make its token there, and paste the token here for Crewlet to check and keep.
 *
 * ONE SHEET FOR EVERY HANDOFF. A new agent walks all three steps. A token
 * (its first, or a new one after the vendor refused the old one or a rotate)
 * starts at the token, since the account is already there. A role changed at
 * the vendor is one panel with both roles and the way to fix it. What the
 * person does is the engine's to say (the sync's wait, read by [handoffOf]);
 * only the vendor's nouns are this screen's.
 *
 * THE VALUE IS NEVER KEPT HERE: it lives in the field until the engine has
 * answered, is sent once, and is cleared the moment the engine accepts it. A
 * refusal is the engine's sentence beside the field, which keeps the value so
 * a person can see what they pasted is still there.
 */

import { useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Copyable,
  DescriptionList,
  FormField,
  InlineCode,
  Input,
  Modal,
  Stepper,
  Text,
  type StepperStep,
} from "@crewlethq/ui";
import type { AccessSeat } from "~/contract/access.ts";
import { marked } from "~/ui/Problems.tsx";
import type { Handoff, Lane } from "./logic.ts";
import { laneWords, type HandoffWords } from "./words.ts";

/** Which of the sheet's steps is drawn. */
type StepId = "account" | "token" | "paste";

export interface HandoffSheetProps {
  seat: AccessSeat;
  lane: Lane;
  handoff: Handoff;
  onClose: () => void;
  /** Sends the pasted token; resolves with the engine's sentence, or rejects with its refusal. */
  onPaste: (token: string) => Promise<string>;
  /** Asks the engine to read the vendor again, for a fix made there (a role changed back). */
  onCheck: () => void;
  /** Turns a refusal into the sentence drawn beside the field. */
  refusal: (err: unknown) => string;
}

export function HandoffSheet(props: HandoffSheetProps) {
  const words = laneWords(props.lane.key).handoff;
  if (!words) return null;
  return props.handoff.step === "rerole" ? (
    <RoleFix {...props} words={words} />
  ) : (
    <Walk {...props} words={words} />
  );
}

/** Opens the vendor's page for the step, in a new tab, so the sheet stays where it was. */
function OpenVendor({ lane, url }: { lane: Lane; url: string }) {
  return (
    <span className="agent-access-actions">
      <ButtonLink size="small" variant="secondary" href={url} external>
        Open {lane.name}
      </ButtonLink>
    </span>
  );
}

/** The agent's account by name where the engine names it, or what it is called otherwise. */
function accountName(handoff: Handoff, words: HandoffWords): ReactNode {
  return handoff.account ? (
    <InlineCode>{handoff.account}</InlineCode>
  ) : (
    `the agent's ${words.account}`
  );
}

function Walk({
  seat,
  lane,
  handoff,
  onClose,
  onPaste,
  refusal,
  words,
}: HandoffSheetProps & { words: HandoffWords }) {
  const fresh = handoff.step === "create";
  const ids: StepId[] = fresh ? ["account", "token", "paste"] : ["token", "paste"];
  const [at, setAt] = useState<StepId>(ids[0]!);
  const [reached, setReached] = useState<StepId>(ids[0]!);
  const [token, setToken] = useState("");
  const [refused, setRefused] = useState("");
  const [checking, setChecking] = useState(false);
  const [said, setSaid] = useState("");

  const index = ids.indexOf(at);
  const last = index === ids.length - 1;
  const go = (id: StepId) => {
    setAt(id);
    if (ids.indexOf(id) > ids.indexOf(reached)) setReached(id);
  };
  const labels: Record<StepId, StepperStep> = {
    account: {
      id: "account",
      label: `Create the ${words.account} on ${lane.name}`,
      description: `${words.add}, named for this agent`,
    },
    token: {
      id: "token",
      label: handoff.renew ? "Create a new token" : "Create its token",
      description: `${words.mint}, shown once`,
    },
    paste: { id: "paste", label: "Paste it here", description: "Crewlet checks it and keeps it" },
  };

  const check = async () => {
    if (token.trim() === "" || checking) return;
    setChecking(true);
    setRefused("");
    try {
      const kept = await onPaste(token);
      setToken("");
      setSaid(kept);
    } catch (err) {
      setRefused(refusal(err));
    } finally {
      setChecking(false);
    }
  };

  // THE NAME AND THE ROLE ARE VALUES the engine sent, so each is drawn only
  // where it was sent; the engine's own sentence says the step otherwise.
  const facts: [ReactNode, ReactNode][] = [];
  if (handoff.account) {
    facts.push([
      "Name",
      <Copyable
        key="name"
        variant="block"
        monospace
        value={handoff.account}
        ariaLabel={`Copy the ${words.account}'s name`}
      />,
    ]);
  }
  if (handoff.role) {
    facts.push([
      "Role",
      <span key="role" className="agent-access-handoff-role">
        <Text>{handoff.role.name}</Text>
        {handoff.role.note && (
          <Text variant="caption" tone="secondary">
            {handoff.role.note}
          </Text>
        )}
      </span>,
    ]);
  }

  const content: Record<StepId, ReactNode> = {
    account: (
      <>
        {facts.length === 2 ? (
          <>
            <Text>
              On {lane.name}, open the account&apos;s access tokens and choose {words.add}. Give it
              this name and role.
            </Text>
            <DescriptionList uniform items={facts} />
          </>
        ) : (
          <Text>{marked(handoff.said)}</Text>
        )}
        <Text variant="caption" tone="secondary">
          {words.never}
        </Text>
        <OpenVendor lane={lane} url={handoff.url} />
      </>
    ),
    token: (
      <>
        <Text>
          On the row of {accountName(handoff, words)}, choose {words.mint}. {lane.name} shows the
          token&apos;s value once, so copy it before you close that dialog.
        </Text>
        <OpenVendor lane={lane} url={handoff.url} />
      </>
    ),
    paste: said ? (
      <Callout variant="success" live="polite">
        {marked(said)}
      </Callout>
    ) : (
      <FormField
        label="Token"
        helper={`Crewlet checks it at ${lane.name} and seals it. It is never shown again.`}
        error={refused || undefined}
      >
        {({ id, describedBy, invalid }) => (
          <Input
            id={id}
            type="password"
            autoFocus
            value={token}
            error={invalid}
            aria-describedby={describedBy}
            onChange={(e) => {
              setToken(e.target.value);
              if (refused) setRefused("");
            }}
          />
        )}
      </FormField>
    ),
  };

  const footer = said ? (
    <Button size="small" variant="primary" onClick={onClose}>
      Done
    </Button>
  ) : (
    <>
      {index > 0 && (
        <Button size="small" variant="ghost" onClick={() => setAt(ids[index - 1]!)}>
          Back
        </Button>
      )}
      {last ? (
        <Button
          size="small"
          variant="primary"
          loading={checking}
          disabledReason={token.trim() === "" ? "Paste the token first." : undefined}
          onClick={() => void check()}
        >
          Check and save
        </Button>
      ) : (
        <Button size="small" variant="primary" onClick={() => go(ids[index + 1]!)}>
          Continue
        </Button>
      )}
    </>
  );

  const title = fresh
    ? `Set up ${seat.name} on ${lane.name}`
    : `${handoff.renew ? "New token" : "Token"} for ${seat.name}`;

  return (
    <Modal
      open
      variant="sheet"
      size="md"
      stackBody
      onClose={onClose}
      title={title}
      subtitle={words.why}
      footer={footer}
      onSubmit={last && !said ? () => void check() : undefined}
    >
      <Stepper
        orientation="vertical"
        label={`Setting up ${seat.name} on ${lane.name}`}
        steps={ids.map((id) => labels[id])}
        current={said ? null : at}
        reached={reached}
        onSelect={said ? undefined : (id) => setAt(id as StepId)}
      />
      <section className="agent-access-handoff" aria-label={labels[at].label as string}>
        {content[at]}
      </section>
    </Modal>
  );
}

/** A role changed at the vendor: both roles, where to change it back, and the check that confirms it. */
function RoleFix({
  seat,
  lane,
  handoff,
  onClose,
  onCheck,
  words,
}: HandoffSheetProps & { words: HandoffWords }) {
  const facts: [ReactNode, ReactNode][] = [];
  if (handoff.account) {
    facts.push([words.accountLabel, <InlineCode key="r">{handoff.account}</InlineCode>]);
  }
  if (handoff.current) facts.push([`On ${lane.name}`, handoff.current]);
  if (handoff.role) facts.push(["You chose", handoff.role.name]);
  return (
    <Modal
      open
      variant="sheet"
      size="md"
      stackBody
      onClose={onClose}
      title={`${seat.name}'s role on ${lane.name}`}
      subtitle={`Crewlet cannot change a ${words.account}'s role, so the fix is yours.`}
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Close
          </Button>
          <Button
            size="small"
            variant="primary"
            onClick={() => {
              onCheck();
              onClose();
            }}
          >
            Done, check again
          </Button>
        </>
      }
    >
      {facts.length > 0 && <DescriptionList uniform items={facts} />}
      <Text>{marked(handoff.said)}</Text>
      {handoff.role && (
        <Text>
          On {lane.name}, open the menu on its row, choose {words.edit} and set its role to{" "}
          {handoff.role.name}.
        </Text>
      )}
      <OpenVendor lane={lane} url={handoff.url} />
    </Modal>
  );
}
