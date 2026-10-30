/**
 * Per-seat integration access: the read the Agents tab draws from, and the
 * writes it makes (`internal/api/setupapi`, the access routes).
 *
 * A save states the revision it edited (If-Match, the answer's revision id
 * as the entity tag the GET's ETag carries), so a colleague's save in between
 * is refused `revision_advanced` and read again rather than overwritten. A `step_up_required` never reaches here: the transport asks for
 * a fresh proof of identity and replays the request (`app/StepUp.tsx`).
 *
 * A WRITE IS SENT WITHOUT A SIGNAL, so it runs to its answer whatever happens
 * to the screen: a save stopped part way would leave the person unsure
 * whether an account was disabled.
 */

import { useCallback, useEffect, useState } from "react";
import type {
  AccessAnswer,
  AccessDraftRequest,
  AccessInstallLink,
  AccessPlanAnswer,
  AccessSeatAction,
} from "~/contract/access.ts";
import { pathSegment, rest, RestError } from "~/protocol/rest.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { asSentence } from "~/lib/format.ts";
import { managedBlock } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { OPERATOR_MANAGED } from "./words.ts";

/** A draft: the desired state per seat and lane, only for what changed. */
export type AccessDraft = AccessDraftRequest["seats"];

/** How often the tab reads again while a sync runs. */
export const SYNC_POLL_MS = 1_500;

/** The revision a write edited, as the entity tag the read's ETag carries. */
const ifMatch = (revision: string) => ({ "If-Match": `"${revision}"` });

export const accessApi = {
  read: async (kind: string, signal?: AbortSignal) =>
    (await rest.get(`/setup/integrations/${pathSegment(kind)}/access`, signal)) as AccessAnswer,
  plan: async (kind: string, seats: AccessDraft) =>
    (await rest.post(`/setup/integrations/${pathSegment(kind)}/access/plan`, {
      seats,
    } satisfies AccessDraftRequest)) as AccessPlanAnswer,
  save: async (kind: string, seats: AccessDraft, revision: string) =>
    (await rest.put(
      `/setup/integrations/${pathSegment(kind)}/access`,
      { seats } satisfies AccessDraftRequest,
      ifMatch(revision),
    )) as AccessAnswer,
  /**
   * One action on one seat's lane. Keep writes the vendor's setting into the
   * company document, so it states the revision it read, as a save does.
   */
  act: async (
    kind: string,
    handle: string,
    lane: string,
    action: AccessSeatAction,
    revision?: string,
  ) =>
    (await rest.post(
      `/setup/integrations/${pathSegment(kind)}/access/seats/${pathSegment(handle)}/${pathSegment(lane)}/${pathSegment(action)}`,
      {},
      revision === undefined ? undefined : ifMatch(revision),
    )) as AccessAnswer,
  /** A fresh, single-use link to install a seat's app at the vendor (Slack's approval). */
  install: async (kind: string, handle: string, lane: string) =>
    (await rest.post(
      `/setup/integrations/${pathSegment(kind)}/access/seats/${pathSegment(handle)}/${pathSegment(lane)}/install`,
      {},
    )) as AccessInstallLink,
};

/** The access routes' refusals, in the reader's words. */
const REFUSALS: Record<string, string> = {
  revision_advanced:
    "Somebody changed these agents since you opened the tab. Read the latest settings and save again.",
  pass_in_flight: "A sync is running. Try again when it finishes.",
  seat_still_on: "Turn this agent off and save before removing its account.",
  credential_external:
    "This credential was provided outside Crewlet. Rotate it where it was issued.",
  config_managed: OPERATOR_MANAGED,
};

/** Whether a write was refused because somebody saved in between, so the tab reads again. */
export function isStale(err: unknown): boolean {
  return err instanceof RestError && err.code === "revision_advanced";
}

/** The words of a refusal, whatever shape it came in. */
export function refusalWords(err: unknown): string {
  if (err instanceof RestError) {
    const words = REFUSALS[err.code];
    if (words) return words;
    // THE VENDOR'S REASON, THEN ITS REMEDY, where the engine sends both.
    const said = asSentence(err.sentence || err.detail || err.message);
    return err.hint ? `${said} ${asSentence(err.hint)}` : said;
  }
  return err instanceof Error ? err.message : "Something went wrong.";
}

export interface AccessRead extends RestResult<AccessAnswer> {
  /**
   * Draw the answer a write returned until the next read arrives: every
   * access write answers the whole tab as it now stands.
   */
  adopt: (answer: AccessAnswer) => void;
}

/**
 * One integration's access, read on mount and again every [SYNC_POLL_MS] while
 * a sync is running, so a row's Syncing settles on its own. Off (`kind` null)
 * for a tool that is not connected, which has no access to read.
 */
export function useAccess(kind: string | null): AccessRead {
  const [syncing, setSyncing] = useState(false);
  const read = useRest(
    kind === null ? null : `/setup/integrations/${pathSegment(kind)}/access`,
    (signal) => accessApi.read(kind!, signal),
    { pollMs: syncing ? SYNC_POLL_MS : undefined, refetchOnFocus: true },
  );
  // AN ADOPTED ANSWER IS NEWER than the read it was adopted over, and older
  // than any read that lands after it.
  const [adopted, setAdopted] = useState<{
    answer: AccessAnswer;
    over: AccessAnswer | null;
  } | null>(null);
  const data = adopted && adopted.over === read.data ? adopted.answer : read.data;
  const running = data?.syncing ?? false;
  useEffect(() => setSyncing(running), [running]);
  const adopt = useCallback(
    (answer: AccessAnswer) => setAdopted({ answer, over: read.data }),
    [read.data],
  );
  return { ...read, data, adopt };
}

/**
 * The line an Agents tab shows when the Kubernetes operator writes the company
 * document, or undefined: said by the access answer, or by the viewer's own
 * reading of the document. Access settings then live on each agent's Seat
 * object, so nothing that writes a revision is drawn; actions at the vendor,
 * which write none, still run.
 */
export function useOperatorManaged(answer: AccessAnswer | null | undefined): string | undefined {
  const viewer = useViewer();
  const operator =
    answer?.managed_by === "operator" ||
    (managedBlock(viewer) !== null && viewer.configManagedBy.includes("operator"));
  return operator ? OPERATOR_MANAGED : undefined;
}
