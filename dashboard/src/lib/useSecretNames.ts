/**
 * The names of the company's sealed secrets, offered when somebody types `$`
 * into a field that takes a `${NAME}` reference.
 *
 * NAMES ONLY, read when the form opens. A refused read offers no names and
 * leaves the fields exactly as they were: the completion is a convenience,
 * and a form that could not be filled in because a second request failed
 * would be worse than one with no completion at all.
 *
 * ONE COPY, for every form that takes a reference — the integration setup
 * form and the MCP server form alike — because the freshness rule below is
 * the part that is easy to get wrong twice.
 */

import { useCallback, useMemo, useRef } from "react";
import { rest } from "~/protocol/index.ts";
import { useRestRead } from "./restRead.ts";

/**
 * How long a read of the names stays fresh enough that a completion opening
 * does not ask again.
 *
 * THREE SECONDS: the list is asked for whenever a completion opens, and a
 * form where somebody types several references would otherwise spend a
 * request on each. Long enough to collapse one person's typing, short enough
 * that leaving to create an entry and coming back gets the new name.
 */
export const SECRET_NAMES_FRESH_MS = 3_000;

export function useSecretNames(): {
  /** The sealed entries' names; empty until read, or when refused. */
  names: string[];
  /** Ask again, quietly, unless a read is fresher than [SECRET_NAMES_FRESH_MS]. */
  refresh: () => void;
} {
  // THE ONE REST LOADER (`./restRead.ts`): a refusal — a reader without the
  // grant the listing takes — holds no names, and anything else keeps the
  // last reading through the failure.
  const secrets = useRestRead(
    "/secrets",
    (signal) => rest.get("/secrets", signal) as Promise<{ secrets?: { name?: string }[] } | null>,
  );
  const names = useMemo(
    () => (secrets.data?.secrets ?? []).map((s) => s.name ?? "").filter(Boolean),
    [secrets.data],
  );
  // WHEN THE LIST WAS LAST ASKED FOR. There is no push for the secret store,
  // so a read taken when the form opened goes stale the moment somebody adds
  // an entry in another tab, which is exactly what a person does on finding
  // the name they wanted is not there.
  const askedAt = useRef(Date.now());
  const { refetch } = secrets;
  const refresh = useCallback(() => {
    const now = Date.now();
    if (now - askedAt.current < SECRET_NAMES_FRESH_MS) return;
    askedAt.current = now;
    refetch();
  }, [refetch]);
  return { names, refresh };
}
