/**
 * Delegated decisions on the screens that answer a held call (ADR-0125).
 *
 * AN ESCALATION READS ITS DELEGATES FIRST. A call that reached people over
 * its delegates is answered by somebody who should know what the delegates
 * made of it before they read the call: who escalated it and under which
 * rule, each delegate's verdict, confidence, reason and findings (most
 * serious first, as given), then the engine's own sentences on why it
 * reached them. [EscalationNote] is that block, drawn before the call by the
 * Inbox's pane and the decision row alike.
 *
 * A DELEGATE'S WORDS ARE A MODEL'S, so they are drawn as plain text: no
 * Markdown, no links, nothing a request could have planted in them acts. So
 * is everything the requesting agent wrote into a held merge or deploy
 * ([ReleaseRequestFacts]): its change, repository, rollback and reason.
 */

import {
  Callout,
  Copyable,
  DescriptionList,
  FindingList,
  Tag,
  Text,
  type DescriptionListEntry,
} from "@crewlethq/ui";
import { BotGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import type { HeldDelegation } from "~/contract/delegated.ts";
import { shortOID } from "~/lib/code.ts";
import {
  confidenceWords,
  delegatedMarkWords,
  escalatedByWords,
  mergeMethodWords,
  releaseAllowWords,
  releaseApproverWords,
  verdictWords,
  withDelegateWords,
  type ReleaseRequest,
} from "~/lib/delegation.ts";

/** A handle as a person reads it. */
type Name = (handle: string) => string;

/**
 * Who escalated a held call and what each delegate answered, then why it
 * reached people. Draw it only for a delegation that is escalated.
 */
export function EscalationNote({ delegation, name }: { delegation: HeldDelegation; name: Name }) {
  const engine = delegation.escalated?.engine ?? [];
  return (
    <Callout
      variant="warning"
      icon={<UsersGlyph size="md" />}
      title={escalatedByWords(delegation, name)}
      className="delegation-note"
    >
      {delegation.given.length === 0 ? (
        <Text as="p" variant="caption">
          No delegate answered.
        </Text>
      ) : (
        delegation.given.map((g, i) => {
          const verdict = verdictWords(g.verdict);
          const who = name(g.by);
          return (
            <div key={`${g.by}:${i}`} className="col gap-1 delegation-answer">
              <span className="row gap-2 wrap">
                <strong>{who}</strong>
                <Tag size="xs" variant={verdict.tone}>
                  {verdict.label}
                </Tag>
                {confidenceWords(g.confidence) && (
                  <Text as="span" variant="caption">
                    {confidenceWords(g.confidence)}
                  </Text>
                )}
              </span>
              {g.reason && (
                <Text as="p" variant="body">
                  {g.reason}
                </Text>
              )}
              {g.findings.length > 0 && (
                <FindingList
                  label={`What ${who} found`}
                  findings={g.findings.map((f, n) => ({
                    key: String(n),
                    tone: "warning" as const,
                    message: f,
                  }))}
                  toneLabels={{ warning: "Finding" }}
                />
              )}
            </div>
          );
        })
      )}
      {engine.length > 0 && (
        <div className="col gap-1 delegation-answer">
          <strong>Why it reached you</strong>
          <ul className="delegation-reasons">
            {engine.map((sentence, i) => (
              <li key={i}>{sentence}</li>
            ))}
          </ul>
        </div>
      )}
    </Callout>
  );
}

/**
 * A held call that waits on its delegates: nobody is asked yet, and it reaches
 * people only if they escalate it or do not answer in time.
 */
export function DelegateWaitNote({ delegation, name }: { delegation: HeldDelegation; name: Name }) {
  return (
    <Callout variant="info" icon={<BotGlyph size="md" />}>
      {withDelegateWords(delegation, name)}. It reaches people only if{" "}
      {delegation.delegates.length > 1 ? "a delegate escalates it" : "the delegate escalates it"} or
      does not answer in time.
    </Callout>
  );
}

/** The mark an answer a delegate gave carries: "Decided under delegation, rule 2". */
export function DelegatedMark({ rule }: { rule: number }) {
  return (
    <Tag size="xs" appearance="outline">
      {delegatedMarkWords(rule)}
    </Tag>
  );
}

/** An agent's words, as written: kept to their lines, never read as Markdown. */
function Written({ text }: { text: string }) {
  return <span className="release-request__text">{text}</span>;
}

/** A commit by its short id, the whole id on hover and copied whole. */
function Commit({ oid }: { oid: string }) {
  return (
    <Copyable
      value={oid}
      display={shortOID(oid)}
      variant="inline"
      monospace
      ariaLabel="Copy the whole commit id"
      title="Copy the whole commit id"
    />
  );
}

/**
 * A held merge or deploy, read from the call's arguments: what it would merge
 * or deploy, where, and why, each value as the agent sent it.
 */
export function ReleaseRequestFacts({ request }: { request: ReleaseRequest }) {
  const items: DescriptionListEntry[] =
    request.act === "pr_merge"
      ? [
          ["Change", <Written text={request.change} />],
          ["Head", <Commit oid={request.head} />],
          ["Method", mergeMethodWords(request.method)],
          ["Reason", <Written text={request.reason} />],
        ]
      : [
          ["Repository", <Written text={request.repo} />],
          ["Commit", <Commit oid={request.commit} />],
          ["Changes", <Written text={request.changes.join(", ")} />],
          ["Environment", <Written text={request.environment} />],
          ["Rollback", <Written text={request.rollback} />],
          ["Reason", <Written text={request.reason} />],
        ];
  return <DescriptionList className="release-request" items={items} />;
}

/**
 * Who may answer a held merge or deploy and the sign-in its answer needs,
 * after what Allow once does where the request is drawn from its arguments
 * (`drawn`); the engine's own words say that where they are drawn instead.
 */
export function ReleaseAnswerNote({
  act,
  agent,
  rule,
  drawn,
}: {
  act: "pr_merge" | "deploy";
  /** The requesting agent, by name. */
  agent: string;
  /** The rule its delegation names, or null for a call put to people at once. */
  rule: number | null;
  drawn: boolean;
}) {
  return (
    <Text as="p" variant="caption" className="release-request">
      {drawn ? `${releaseAllowWords(act, agent)} ` : ""}
      {releaseApproverWords(act, agent, rule)}
    </Text>
  );
}
