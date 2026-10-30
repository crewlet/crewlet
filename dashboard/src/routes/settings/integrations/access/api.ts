/**
 * Per-seat integration access: the read the Agents tab draws from, and the
 * writes it makes (`internal/api/setupapi`, the access routes).
 *
 * A save states the revision it edited (If-Match), so a colleague's save in
 * between is refused `revision_advanced` and read again rather than
 * overwritten. A `step_up_required` never reaches here: the transport asks for
 * a fresh proof of identity and replays the request (`app/StepUp.tsx`).
 *
 * A WRITE IS SENT WITHOUT A SIGNAL, so it runs to its answer whatever happens
 * to the screen: a save stopped part way would leave the person unsure
 * whether an account was disabled.
 */

import { useEffect, useState } from "react";
import type {
  AccessAnswer,
  AccessDraft,
  AccessDraftRequest,
  AccessPlanAnswer,
  AccessSeatAction,
} from "~/contract/access.ts";
import { pathSegment, rest, RestError } from "~/protocol/rest.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { managedBlock } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { OPERATOR_MANAGED } from "./words.ts";

/** How often the tab reads again while a sync runs. */
export const SYNC_POLL_MS = 1_500;

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
      { "If-Match": revision },
    )) as AccessAnswer,
  act: async (kind: string, handle: string, lane: string, action: AccessSeatAction) =>
    (await rest.post(
      `/setup/integrations/${pathSegment(kind)}/access/seats/${pathSegment(handle)}/${pathSegment(lane)}/${pathSegment(action)}`,
      {},
    )) as AccessAnswer,
};

/** The words of a refusal, whatever shape it came in. */
export function refusalWords(err: unknown): string {
  if (err instanceof RestError) {
    if (err.code === "revision_advanced") {
      return "Somebody changed these agents since you opened the tab. Read the latest settings and save again.";
    }
    return err.sentence || err.detail || err.message;
  }
  return err instanceof Error ? err.message : "Something went wrong.";
}

/**
 * One integration's access, read on mount and again every [SYNC_POLL_MS] while
 * a sync it started is running, so a row's Syncing settles on its own. Off
 * (`kind` null) for a tool that is not connected, which has no access to read.
 */
export function useAccess(kind: string | null): RestResult<AccessAnswer> {
  const [syncing, setSyncing] = useState(false);
  const read = useRest(
    kind === null ? null : `/setup/integrations/${pathSegment(kind)}/access`,
    (signal) => accessApi.read(kind!, signal),
    { pollMs: syncing ? SYNC_POLL_MS : undefined, refetchOnFocus: true },
  );
  const running = read.data?.syncing ?? false;
  useEffect(() => setSyncing(running), [running]);
  return read;
}

/**
 * The line an Agents tab is read-only with when the Kubernetes operator
 * manages the company document, or undefined. Access then lives on each
 * agent's Seat object, so the tab draws no switch, picker or action at all.
 */
export function useOperatorManaged(): string | undefined {
  const viewer = useViewer();
  return managedBlock(viewer) !== null && viewer.configManagedBy.includes("operator")
    ? OPERATOR_MANAGED
    : undefined;
}
