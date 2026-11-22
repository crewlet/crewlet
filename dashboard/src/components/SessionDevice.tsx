/**
 * The device a session was opened from, beside the address it signed in from
 * (`iamapi.sessionView`'s `user_agent` and `remote`): what a person reads to
 * choose the session to sign out, on their Account and in an administrator's
 * view of somebody's sessions alike.
 */
import { TableTwoLineCell } from "@crewlethq/ui";

import { deviceOf } from "~/lib/userAgent.ts";
import type { SessionRow } from "~/routes/settings/Access.tsx";

/**
 * The device a session was opened from and the address it signed in from,
 * as the kit's two-line cell: "Firefox on macOS" over "203.0.113.4". A
 * session that named no browser (a token's sign-in, or one an earlier engine
 * opened) says so.
 */
export function SessionDevice({ row }: { row: Pick<SessionRow, "user_agent" | "remote"> }) {
  const device = deviceOf(row.user_agent) || "No browser named";
  return (
    // The browser's words on the device's line alone, so a cut address keeps its own hover text.
    <TableTwoLineCell
      value={row.user_agent ? <span title={row.user_agent}>{device}</span> : device}
      detail={row.remote}
    />
  );
}
