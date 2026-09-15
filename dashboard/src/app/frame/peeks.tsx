/**
 * Which component reads one object, for every kind the product can address.
 *
 * # Why this is a registry and not a prop
 *
 * `DetailRail` took its body as `children`, so the SCREEN that opened a peek
 * had to know how to render one — and only the tracker did. Nineteen kinds
 * were registered in `objects.ts` and exactly one of them, `item`, could ever
 * appear in the rail: `objects.ts`'s own doc comment says it exists so "a list
 * of any kind can open a peek on any other kind without either knowing about
 * the other", and that was true of the addressing and false of the rendering.
 *
 * With the body on the KIND rather than on the caller, a list opens a peek by
 * naming what it points at — `openPeek({kind: "node", id})` from the fleet
 * grid, `{kind: "turn", id}` from the spend table — and the rail resolves it.
 * The shell mounts the rail once, so no screen renders one and every screen
 * has one.
 *
 * # Each peek fetches its own object
 *
 * A peek is not a projection of the row that opened it. The row carries what
 * its own list needed; a peek answers "what is this thing", and the two differ
 * on every kind — a fleet row has a node's lag, the node peek has its posture,
 * its epoch and what it holds. So a peek takes an `id` and asks, which is also
 * what lets a peek be opened from a URL that was pasted rather than clicked.
 *
 * # What a peek is not
 *
 * Not a second page. It answers the question a reader asks WITHOUT leaving the
 * list — is this the one I meant, and what is it doing — and every one of them
 * ends at `Open ↗`, which the rail draws. A peek that grew tabs would be a
 * page in a narrow column, and the reason the rail exists is that the list
 * behind it stays on screen.
 */

import type { ObjectKind, ObjectRef } from "./objects.ts";
import { ItemPeek } from "~/routes/work/WorkItem.tsx";
import { SeatPeek } from "~/routes/company/Seat.tsx";
import { UnitPeek } from "~/routes/company/Company.tsx";
import { TurnPeek } from "~/routes/activity/Turn.tsx";
import { RunPeek } from "~/routes/activity/Runs.tsx";
import { ChannelPeek } from "~/routes/activity/Conversations.tsx";
import { EventPeek } from "~/routes/activity/Event.tsx";
import { SchedulePeek } from "~/routes/activity/Schedules.tsx";
import { NodePeek } from "~/routes/admin/Fleet.tsx";

/**
 * What every peek is handed.
 *
 * ONE ARGUMENT, because the rail knows nothing about any kind: the id out of
 * the `peek=` token, exactly as `objects.ts` parsed it. Anything else a peek
 * needs it asks for.
 */
export interface PeekProps {
  id: string;
}

/**
 * The body for each kind, or `undefined` where a kind has no peek.
 *
 * A KIND WITH NO ENTRY IS NOT AN ERROR. `notice` is addressed — an inbox row
 * links to one — and its "peek" is the Inbox itself, where the notice is read
 * in place; `model` has no page at all, by the same argument that a page per
 * model id is a page per typo. The rail closes rather than opening empty.
 */
export const PEEKS: Partial<Record<ObjectKind, (props: PeekProps) => React.ReactNode>> = {
  item: ({ id }) => <ItemPeek itemKey={id} chrome={{}} />,
  seat: ({ id }) => <SeatPeek handle={id} />,
  unit: ({ id }) => <UnitPeek id={id} />,
  turn: ({ id }) => <TurnPeek turnId={id} />,
  run: ({ id }) => <RunPeek turnId={id} />,
  channel: ({ id }) => <ChannelPeek id={id} />,
  event: ({ id }) => <EventPeek eventId={id} />,
  schedule: ({ id }) => <SchedulePeek scope={id} />,
  node: ({ id }) => <NodePeek id={id} />,
};

/** Whether this kind can be peeked at all. */
export function peekable(ref: ObjectRef | null): boolean {
  return Boolean(ref && PEEKS[ref.kind]);
}
