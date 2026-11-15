/**
 * An agent's request for one permission at an integration, answered from its
 * question (ADR-0118): choosing "Grant for this change" or "Add to this agent
 * permanently" opens what the grant will be, built from the request as the
 * engine stored it, never from the question's words. Grant saves it on the
 * integration's Agents tab, which asks for a fresh sign-in where the
 * permission can change things, and only then is the question answered, so
 * the agent is told yes only once it holds the permission.
 */

import { useEffect, useState } from "react";
import { Button, Callout, DescriptionList, Link, Stack, Text } from "@crewlethq/ui";
import type { AccessAnswer, AccessDraftRequest, AccessGrantRequest } from "~/contract/access.ts";
import { Modal } from "./Modal.tsx";
import { pathSegment, rest } from "~/protocol/rest.ts";
import { refusalText } from "~/lib/refusal.ts";
import { grantEntry, grantReach, hoursWords, newGrant, type RequestOption } from "~/lib/grants.ts";

/** What the dialog has read: the request, and the integration's Agents tab it saves on. */
type Read =
  | { state: "reading" }
  | { state: "gone"; words: string }
  | { state: "read"; request: AccessGrantRequest; answer: AccessAnswer };

export function RequestGrantDialog({
  option,
  question,
  onClose,
  onApplied,
}: {
  /** The option chosen: the request's id, and whether for good. */
  option: RequestOption;
  /** The decision's question, the dialog's title. */
  question: string;
  onClose: () => void;
  /** Called once the grant is saved, for the question to be answered. */
  onApplied: () => void;
}) {
  const [read, setRead] = useState<Read>({ state: "reading" });
  const [saving, setSaving] = useState(false);
  const [refused, setRefused] = useState("");
  useEffect(() => {
    const ctl = new AbortController();
    void (async () => {
      try {
        const request = (await rest.get(
          `/setup/access/requests/${pathSegment(option.id)}`,
          ctl.signal,
        )) as AccessGrantRequest;
        // THE AGENTS TAB'S OWN ROUTE, read and saved as the tab does
        // (`accessApi`), each path written out so the dev server's proxy gate
        // reads it.
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

  const save = async () => {
    if (missing) return;
    setSaving(true);
    setRefused("");
    try {
      // ONE SEAT'S LANE, AS THE TAB SAVES IT: held to the revision read, so
      // a save in between is refused rather than overwritten, and a fresh
      // sign-in asked by the transport where the permission is a write.
      await rest.put(
        `/setup/integrations/${pathSegment(answer.kind)}/access`,
        {
          seats: {
            [seat.handle]: {
              [lane.lane]: {
                ...saved.desired,
                grants: [...(saved.desired.grants ?? []), { ...grant, request: request.id }],
              },
            },
          },
        } satisfies AccessDraftRequest,
        { "If-Match": `"${answer.revision}"` },
      );
      onApplied();
    } catch (err) {
      setRefused(refusalText(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      onClose={onClose}
      title={question}
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            onClick={() => void save()}
            disabled={missing || saving}
          >
            {saving ? "Granting…" : "Grant"}
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
              ["How long", option.permanent ? hoursWords(0) : hoursWords(request.hours)],
              ["Why", request.reason],
            ]}
          />
        )}
        {entry?.write && (
          <Callout variant="warning">
            {`${entry.name} can change things there, so granting it asks you to sign in again.`}
          </Callout>
        )}
        {entry?.unavailable && (
          <Callout variant="warning" title="The company's own credential has to hold it first">
            {`${entry.unavailable}. Add ${(entry.missing ?? [entry.name]).join(", ")} to it, then grant.`}{" "}
            {entry.action_url && (
              <Link href={entry.action_url} external>
                Open the API tokens page
              </Link>
            )}
          </Callout>
        )}
        {refused && (
          <Callout variant="danger" title="The grant was not saved">
            {refused}
          </Callout>
        )}
      </Stack>
    </Modal>
  );
}
