/**
 * An agent's grant request answered from its question (ADR-0118): shows the grant
 * from the stored request, and Grant sends one write, the answer, which the engine
 * saves the grant for first. The transport confirms a stale sign-in and resends.
 */

import { useEffect, useState, type ReactNode } from "react";
import { Button, Callout, DescriptionList, Link, Stack, Text } from "@crewlethq/ui";
import type { AccessAnswer, AccessGrantRequest } from "~/contract/access.ts";
import { Modal } from "./Modal.tsx";
import { pathSegment, rest } from "~/protocol/rest.ts";
import { refusalText } from "~/lib/refusal.ts";
import {
  grantCaution,
  grantEntry,
  grantReach,
  hoursWords,
  newGrant,
  type RequestOption,
} from "~/lib/grants.ts";

/** What the dialog has read: the request, and the integration's Agents tab that draws it. */
type Read =
  | { state: "reading" }
  | { state: "gone"; words: string }
  | { state: "read"; request: AccessGrantRequest; answer: AccessAnswer };

export function RequestGrantDialog({
  option,
  question,
  onClose,
  onGrant,
  granting,
  refusal,
}: {
  /** The option chosen: the request's id, and whether for good. */
  option: RequestOption;
  /** The decision's question, the dialog's title. */
  question: string;
  onClose: () => void;
  /** Answers the question with the option chosen, which saves the grant first. */
  onGrant: () => void;
  /** Whether that answer is on its way. */
  granting: boolean;
  /** Why the answer was refused, drawn under the grant; nothing while it was not. */
  refusal?: ReactNode;
}) {
  const [read, setRead] = useState<Read>({ state: "reading" });
  useEffect(() => {
    const ctl = new AbortController();
    void (async () => {
      try {
        const request = (await rest.get(
          `/setup/access/requests/${pathSegment(option.id)}`,
          ctl.signal,
        )) as AccessGrantRequest;
        // The Agents tab's route, read only to draw the grant; paths written out
        // for the dev server's proxy gate.
        const answer = (await rest.get(
          `/setup/integrations/${pathSegment(request.kind)}/access`,
          ctl.signal,
        )) as AccessAnswer;
        setRead({ state: "read", request, answer });
      } catch (err) {
        if (!ctl.signal.aborted) setRead({ state: "gone", words: refusalText(err) });
      }
    })();
    return () => ctl.abort();
  }, [option.id]);

  if (read.state !== "read") {
    return (
      <Modal open onClose={onClose} title={question}>
        {read.state === "gone" ? (
          <Callout variant="danger" title="This request can't be granted now">
            {read.words}
          </Callout>
        ) : (
          <Text as="p">Reading the request…</Text>
        )}
      </Modal>
    );
  }

  const { request, answer } = read;
  const lane = answer.options.find((o) => o.lane === request.lane);
  const seat = answer.seats.find((s) => s.handle === request.handle);
  const saved = seat?.lanes[request.lane];
  const grant = newGrant(
    request.permission,
    request.scope ?? [],
    option.permanent ? 0 : request.hours,
    request.reason,
    Date.now(),
  );
  const entry = lane ? grantEntry(lane, request.permission) : undefined;
  const missing = !lane || !seat || !saved;

  return (
    <Modal
      open
      onClose={onClose}
      title={question}
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose} disabled={granting}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            onClick={onGrant}
            disabled={missing || granting || Boolean(entry?.unavailable)}
          >
            {granting ? "Granting…" : "Grant"}
          </Button>
        </>
      }
    >
      <Stack gap={3}>
        {missing ? (
          <Callout variant="danger" title="This agent is not on the integration's Agents tab">
            The agent left the company or its access there was removed, so there is nothing to add
            the grant to.
          </Callout>
        ) : (
          <DescriptionList
            items={[
              ["Agent", seat.name],
              ["Permission", entry ? `${entry.name} (${entry.id})` : request.permission],
              ["Where", grantReach(lane, grant)],
              ...(request.target
                ? [["For one change to", request.target] as [string, string]]
                : []),
              ["How long", option.permanent ? hoursWords(0) : hoursWords(request.hours)],
              ["Why", request.reason],
            ]}
          />
        )}
        {(entry?.write || grantCaution(entry)) && (
          <Callout variant="warning">
            {[
              grantCaution(entry),
              entry?.write
                ? `${entry.name} can change things there, so granting it asks you to sign in again.`
                : "",
            ]
              .filter(Boolean)
              .join(" ")}
          </Callout>
        )}
        {entry?.unavailable && (
          <Callout
            variant="warning"
            title="The company's own credential has to hold it first"
            action={
              entry.action_url && (
                <Link href={entry.action_url} external>
                  Open the API tokens page
                </Link>
              )
            }
          >
            {`${entry.unavailable}. Add ${(entry.missing ?? [entry.name]).join(", ")} to it, then grant.`}
          </Callout>
        )}
        {refusal}
      </Stack>
    </Modal>
  );
}
