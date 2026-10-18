/**
 * The links a message carries, drawn under its body in the room and in a
 * thread alike, so the two cannot come to draw one link two ways.
 *
 * BY THE DASHBOARD'S LINK RULES, the ones every address it draws passes: a link
 * the dashboard's scheme allowlist refuses (`safeHref`) is drawn as its words,
 * and every other is the address token (`AddressLink`) read `checked`,
 * WHOEVER WROTE THE MESSAGE: a seat chooses its links in a tool's arguments,
 * and a person's message carries only the links their assistant or a tool
 * proposed for them, since the composer attaches none. So an address on a host
 * the company configured opens as the dashboard rebuilds it, in a new tab, and
 * any other is drawn whole and inert with the host it names, as the Inbox draws
 * the same address in an agent's words (ADR-0061).
 *
 * CUT TO THE ROW, never wrapped: a link may be two kibibytes and an inert one
 * is drawn whole, so one wider than the row is cut where the row ends, and
 * keeps the whole address as its title.
 */

import { safeHref } from "@crewlethq/ui";
import { AddressLink } from "~/components/InlineToken.tsx";

export function MessageLinks({ links }: { links: readonly string[] | undefined }) {
  if (!links || links.length === 0) return null;
  return (
    <div className="row wrap gap-2">
      {links.map((link, index) => {
        const address = safeHref(link);
        return (
          // BY PLACE AND ADDRESS: a message may carry one link twice.
          <span key={`${index}:${link}`} className="t-caption truncate">
            {address === null ? link : <AddressLink address={address} links="checked" />}
          </span>
        );
      })}
    </div>
  );
}
