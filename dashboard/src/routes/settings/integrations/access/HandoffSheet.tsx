/**
 * The sheet a person sets an agent up in at a vendor that lets only a person
 * make the agent's credential: make the agent's account there where it has
 * one (Expo's robots), make its token there, and paste the token here for
 * Crewlet to check and keep.
 *
 * ONE SHEET FOR EVERY HANDOFF. A new agent with an account to make walks
 * three steps; where the token is the agent's whole identity (DigitalOcean's)
 * a new agent walks two. A token (its first, or a new one after the vendor
 * refused the old one) starts at the token, since the account is already
 * there, and a rotate starts at the paste, its token step one step back. A
 * role changed at the vendor is one panel with both roles and the way to fix
 * it. What the person does is the engine's to say (the sync's wait, read by
 * [handoffOf]); only the vendor's nouns are this screen's.
 *
 * THE VALUE IS NEVER KEPT HERE: it lives in the field until the engine has
 * answered, is sent once, and is cleared the moment the engine accepts it. A
 * refusal is the engine's words beside the field, which keeps the value so a
 * person can see what they pasted is still there.
 */

import { Fragment, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Copyable,
  DescriptionList,
  FormField,
  InlineCode,
  Input,
  List,
  Modal,
  Stepper,
  Text,
  type StepperStep,
} from "@crewlethq/ui";
import type { AccessSeat } from "~/contract/access.ts";
import type { SetupVerdict } from "~/protocol/index.ts";
import { marked } from "~/ui/Problems.tsx";
import { VerdictResult } from "../../VerdictResult.tsx";
import type { RefusalParts } from "./api.ts";
import { scopeGuide, type Handoff, type Lane, type ScopeSection } from "./logic.ts";
import {
  laneWords,
  type HandoffAccountWords,
  type HandoffFormWords,
  type HandoffWords,
} from "./words.ts";

/** Which of the sheet's steps is drawn. */
type StepId = "account" | "token" | "paste";

export interface HandoffSheetProps {
  seat: AccessSeat;
  lane: Lane;
  handoff: Handoff;
  onClose: () => void;
  /** Sends the pasted token; resolves with the engine's verdict, or rejects with its refusal. */
  onPaste: (token: string) => Promise<SetupVerdict>;
  /** Asks the engine to read the vendor again, for a fix made there (a role changed back). */
  onCheck: () => void;
  /** Turns a refusal into what is drawn beside the field and what to do below it. */
  refusal: (err: unknown) => RefusalParts;
}

export function HandoffSheet(props: HandoffSheetProps) {
  const words = laneWords(props.lane.key).handoff;
  if (!words) return null;
  return props.handoff.step === "rerole" && words.account ? (
    <RoleFix {...props} account={words.account} />
  ) : (
    <Walk {...props} words={words} />
  );
}

/** Opens the vendor's page for the step, in a new tab, so the sheet stays where it was; nothing where the engine named none. */
function OpenVendor({ lane, url }: { lane: Lane; url: string | undefined }) {
  if (!url) return null;
  return (
    <span className="agent-access-actions">
      <ButtonLink size="small" variant="secondary" href={url} external>
        Open {lane.name}
      </ButtonLink>
    </span>
  );
}

/** The agent's account by name where the engine names it, or what it is called otherwise. */
function accountName(handoff: Handoff, account: HandoffAccountWords): ReactNode {
  return handoff.account ? (
    <InlineCode>{handoff.account}</InlineCode>
  ) : (
    `the agent's ${account.noun}`
  );
}

/** A value over the vendor's own line on it: a role and what it allows, a choice and what never to choose. */
function Noted({ value, note }: { value: string; note: string | undefined }) {
  return (
    <span className="agent-access-handoff-role">
      <Text>{value}</Text>
      {note && (
        <Text variant="caption" tone="secondary">
          {note}
        </Text>
      )}
    </span>
  );
}

/**
 * The name to give what the person makes, to copy, as a fact. ONLY A NAME THE
 * ENGINE SENT: without one there is no row, never a name made up here.
 */
function nameFact(handoff: Handoff, label: string, what: string): [ReactNode, ReactNode][] {
  if (!handoff.name) return [];
  return [
    [
      label,
      <Copyable
        key="name"
        variant="block"
        monospace
        value={handoff.name}
        ariaLabel={`Copy the ${what}'s name`}
      />,
    ],
  ];
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
  const { account, token: tokenWords } = words;
  const fresh = handoff.step === "create";
  const ids: StepId[] = fresh && account ? ["account", "token", "paste"] : ["token", "paste"];
  const start: StepId = handoff.rotating ? "paste" : ids[0]!;
  const [at, setAt] = useState<StepId>(start);
  const [reached, setReached] = useState<StepId>(start);
  const [token, setToken] = useState("");
  const [refused, setRefused] = useState<RefusalParts | null>(null);
  const [checking, setChecking] = useState(false);
  const [said, setSaid] = useState<SetupVerdict | null>(null);

  // A ROTATE RENEWS IN PLACE where the vendor can: the same token, its name
  // and scopes kept, and the old value dead at once.
  const regenerate = handoff.rotating ? tokenWords.regenerate : undefined;
  const mint = regenerate ?? tokenWords.mint;

  const index = ids.indexOf(at);
  const last = index === ids.length - 1;
  const go = (id: StepId) => {
    setAt(id);
    if (ids.indexOf(id) > ids.indexOf(reached)) setReached(id);
  };
  const labelOf = (id: StepId): StepperStep => {
    switch (id) {
      // Only in `ids` where the lane's words have an account.
      case "account":
        return {
          id,
          label: `Create the ${account!.noun} on ${lane.name}`,
          description: `${account!.add}, named for this agent`,
        };
      case "token":
        return {
          id,
          label: regenerate
            ? "Regenerate its token"
            : `${tokenWords.verb} ${handoff.renew ? "a new" : "its"} token`,
          description: `${mint}, shown once`,
        };
      case "paste":
        return { id, label: "Paste it here", description: "Crewlet checks it and keeps it" };
    }
  };

  const check = async () => {
    if (token.trim() === "" || checking) return;
    setChecking(true);
    setRefused(null);
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
  const accountFacts: [ReactNode, ReactNode][] = account
    ? [
        ...nameFact(handoff, "Name", account.noun),
        ...(handoff.role
          ? [
              ["Role", <Noted key="role" value={handoff.role.name} note={handoff.role.note} />] as [
                ReactNode,
                ReactNode,
              ],
            ]
          : []),
      ]
    : [];

  const content: Record<StepId, ReactNode> = {
    account: account && (
      <>
        {accountFacts.length === 2 ? (
          <>
            <Text>
              On {lane.name}, open the account&apos;s access tokens and choose {account.add}. Give
              it this name and role.
            </Text>
            <DescriptionList uniform items={accountFacts} />
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
    token: tokenWords.form ? (
      <TokenForm
        lane={lane}
        handoff={handoff}
        words={words}
        form={tokenWords.form}
        regenerate={regenerate}
      />
    ) : (
      <>
        <Text>
          {account ? (
            <>
              On the row of {accountName(handoff, account)}, choose {tokenWords.mint}.
            </>
          ) : (
            <>
              On {lane.name}, choose {tokenWords.mint}.
            </>
          )}{" "}
          {tokenWords.once}
        </Text>
        <OpenVendor lane={lane} url={handoff.url} />
      </>
    ),
    paste: said ? (
      <div className="int-key-check">
        <VerdictResult verdict={said} />
      </div>
    ) : (
      <>
        <FormField
          label="Token"
          helper={`Crewlet checks it at ${lane.name} and seals it. It is never shown again.`}
          error={refused?.said}
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
                if (refused) setRefused(null);
              }}
            />
          )}
        </FormField>
        {refused?.action && (
          <Callout variant="danger" live="polite">
            {marked(refused.action, "inherit")}
          </Callout>
        )}
      </>
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

  // A FIRST TOKEN IS A SET-UP wherever the token is the agent's whole
  // identity, since nothing else exists for it at the vendor yet.
  const setUp = fresh || (!account && !handoff.renew);
  const title = setUp
    ? `Set up ${seat.name} on ${lane.name}`
    : `${handoff.renew ? "New token" : "Token"} for ${seat.name}`;
  const label = labelOf(at);

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
        steps={ids.map(labelOf)}
        current={said ? null : at}
        reached={reached}
        onSelect={said ? undefined : (id) => setAt(id as StepId)}
      />
      <section className="agent-access-handoff" aria-label={label.label as string}>
        {content[at]}
      </section>
    </Modal>
  );
}

/** Scope names as code, in a sentence. */
function Scopes({ scopes }: { scopes: readonly string[] }) {
  return (
    <>
      {scopes.map((s, i) => (
        <Fragment key={s}>
          {i > 0 && ", "}
          <InlineCode>{s}</InlineCode>
        </Fragment>
      ))}
    </>
  );
}

/** What to tick in one section of the vendor's scope picker. */
function sectionWords(section: ScopeSection): ReactNode {
  if (section.tick.length > 0) return <Scopes scopes={section.tick} />;
  if (section.untick.length === 0) return "The whole section";
  return (
    <>
      The whole section, then untick <Scopes scopes={section.untick} />
    </>
  );
}

/**
 * Where the token is made on a form of the vendor's: switch to what the
 * company connected, name it, set its expiry, and tick the chosen level's
 * scopes in the vendor's picker. A rotate the vendor renews in place is one
 * control on the token's row instead.
 */
function TokenForm({
  lane,
  handoff,
  words,
  form,
  regenerate,
}: {
  lane: Lane;
  handoff: Handoff;
  words: HandoffWords;
  form: HandoffFormWords;
  regenerate: string | undefined;
}) {
  if (regenerate) {
    return (
      <>
        <Text>
          On {lane.name}&apos;s tokens page, open the menu on the row of{" "}
          {handoff.name ? <InlineCode>{handoff.name}</InlineCode> : "the agent's token"} and choose{" "}
          {regenerate}. The token keeps its name and scopes. {words.token.once}
        </Text>
        <OpenVendor lane={lane} url={handoff.url} />
      </>
    );
  }
  const guide = scopeGuide(lane, handoff.level);
  const facts: [ReactNode, ReactNode][] = [
    ...nameFact(handoff, form.name, "token"),
    [form.expiry.label, form.expiry.advice],
    [form.scopes.label, <Noted key="scopes" value={form.scopes.custom} note={words.never} />],
  ];
  return (
    <>
      <Text>
        {form.switchTo(lane.vendor_account)} Then choose {words.token.mint} and fill it in.
      </Text>
      <DescriptionList uniform items={facts} />
      {guide && (
        <List variant="numbered" density="compact" aria-label={`Ticking the ${lane.name} scopes`}>
          <List.Item>
            <Text>
              Under {form.bulk.control}, choose {form.bulk.reads}.
            </Text>
          </List.Item>
          {guide.refused.length > 0 && (
            <List.Item>
              <Text>
                Untick these. Crewlet refuses a token that holds any of them:{" "}
                <Scopes scopes={guide.refused} />.
              </Text>
            </List.Item>
          )}
          {guide.unneeded.length > 0 && (
            <List.Item>
              <Text>
                Untick these too. Agents do not need them: <Scopes scopes={guide.unneeded} />.
              </Text>
            </List.Item>
          )}
          {guide.sections.length > 0 && (
            <List.Item>
              <Text>Find each of these sections with {form.search}, and tick:</Text>
              <DescriptionList
                uniform
                items={guide.sections.map((s): [ReactNode, ReactNode] => [
                  <InlineCode key={s.resource}>{s.resource}</InlineCode>,
                  sectionWords(s),
                ])}
              />
              <Text variant="caption" tone="secondary">
                Never use {form.bulk.others}: they select scopes Crewlet refuses.
              </Text>
            </List.Item>
          )}
        </List>
      )}
      {form.confirm && (
        <Text variant="caption" tone="secondary">
          {form.confirm}
        </Text>
      )}
      <Text>{words.token.once}</Text>
      <OpenVendor lane={lane} url={handoff.url} />
    </>
  );
}

/** A role changed at the vendor: both roles, where to change it back, and the check that confirms it. */
function RoleFix({
  seat,
  lane,
  handoff,
  onClose,
  onCheck,
  account,
}: HandoffSheetProps & { account: HandoffAccountWords }) {
  const facts: [ReactNode, ReactNode][] = [];
  if (handoff.account) {
    facts.push([account.label, <InlineCode key="r">{handoff.account}</InlineCode>]);
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
      subtitle={`Crewlet cannot change a ${account.noun}'s role, so the fix is yours.`}
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
          On {lane.name}, open the menu on its row, choose {account.edit} and set its role to{" "}
          {handoff.role.name}.
        </Text>
      )}
      <OpenVendor lane={lane} url={handoff.url} />
    </Modal>
  );
}
