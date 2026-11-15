/**
 * The device a session was opened from, beside the address it signed in from
 * (`iamapi.sessionView`'s `user_agent` and `remote`): what a person reads to
 * choose the session to sign out, on their Account and in an administrator's
 * view of somebody's sessions alike.
 */
import { Text } from "@crewlethq/ui";

import { deviceOf } from "~/lib/userAgent.ts";
import type { SessionRow } from "~/routes/settings/Access.tsx";

/**
 * The device a session was opened from and the address it signed in from,
 * as two lines: "Firefox on macOS" over "203.0.113.4". A session that named
 * no browser (a token's sign-in, or one an earlier engine opened) says so.
 */
export function SessionDevice({ row }: { row: Pick<SessionRow, "user_agent" | "remote"> }) {
  const device = deviceOf(row.user_agent);
  return (
    <div className="col">
      <Text as="span" variant="body" title={row.user_agent || undefined}>
        {device || "No browser named"}
      </Text>
      {row.remote && (
        <Text as="span" variant="caption" tone="secondary">
          {row.remote}
        </Text>
      )}
    </div>
  );
}
