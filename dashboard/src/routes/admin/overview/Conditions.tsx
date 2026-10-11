/**
 * "Needs an admin" — the admin half of the attention queue, on the Overview.
 *
 * WHAT IT HOLDS: the conditions the engine raised that a person decides
 * (`conditionsToDecide`) — a seat that failed or was paused, a budget refusing
 * or nearly spent — and the budget-stopped seats that are NOT this reader's
 * decision (`seatDecisionsFor`'s `others`), which the decisions card below it
 * leaves out. The engine's own conditions (no configuration, a draining node,
 * a refused token) are the status sentence's and the health card's, and a
 * stalled round or a parked run is Live's (`WHERE_OF`).
 *
 * ITS OWN CARD, ON THIS PAGE. These rows used to be the Inbox's "Needs a
 * decision" group, and the status sentence's "n conditions need an admin" sent
 * an admin there — out of the console and into a person's inbox, which is the
 * person shell's and asks only what a member is served (`app/shell.test.ts`).
 * Every row here is about the machine, in the engine's words and with a way
 * into the console, so they are listed where the sentence that counts them is.
 *
 * Drawn only when there is something in it: the sentence above says when
 * nothing needs an admin, and an empty card under it would say it twice.
 */

import { Card, Tag } from "@crewlethq/ui";
import { ArrowRightGlyph, InfoGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { relTime } from "~/lib/format.ts";
import type { Attention } from "~/lib/attention.ts";
import { SeatRow, type SeatCondition } from "./seatDecisions.tsx";

export function Conditions({
  items,
  seats,
  now,
}: {
  /** The conditions a person decides: `conditionsToDecide` over the queue. */
  items: readonly Attention[];
  /** The budget-stopped seats the reader cannot act on. */
  seats: readonly SeatCondition[];
  now: number;
}) {
  const total = items.length + seats.length;
  if (total === 0) return null;
  const critical = items.some((a) => a.severity === "critical") || seats.length > 0;
  return (
    <Card padding="none" className="home-card" aria-label="Needs an admin">
      <Card.Header icon={<TriangleAlertGlyph size="sm" className="home-decisions-icon" />}>
        <Card.Title as="h3">
          Needs an admin
          <Tag size="xs" variant={critical ? "danger" : "warning"} className="home-card-count">
            {total.toLocaleString()}
          </Tag>
        </Card.Title>
      </Card.Header>
      <ul className="decision-list">
        {items.map((item) => (
          <ConditionRow key={item.id} item={item} now={now} />
        ))}
        {seats.map((seat) => (
          <SeatRow key={`seat:${seat.row.id}`} seat={seat} now={now} />
        ))}
      </ul>
      <p className="home-card-foot">
        <InfoGlyph size="sm" aria-hidden="true" />
        <span>
          Computed from the live state — nothing here is stored or marked. A row leaves when its
          condition clears.
        </span>
      </p>
    </Card>
  );
}

/** One condition: its mark, what happened, what to do about it, and where. */
function ConditionRow({ item, now }: { item: Attention; now: number }) {
  return (
    <li className="decision-row">
      <span className="attention-icon" data-severity={item.severity}>
        <Mark name={item.icon} size="sm" />
      </span>
      <div className="decision-body">
        <span className="decision-title">{item.title}</span>
        <span className="decision-sub">
          {item.detail}
          {item.at ? ` · ${relTime(item.at, now)}` : ""}
        </span>
      </div>
      {item.path && (
        <div className="decision-actions">
          <a className="t-link row gap-1" href={href(item.path, item.query)}>
            Go to it <ArrowRightGlyph size="sm" />
          </a>
        </div>
      )}
    </li>
  );
}
