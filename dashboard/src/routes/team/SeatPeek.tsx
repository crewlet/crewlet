/**
 * One colleague, beside the chart or the list they were opened from — the
 * person shell's peek of a seat.
 *
 * # What a peek answers
 *
 * "Who is this, and what are they doing?" — the state line first, then the
 * facts a teammate asks next: who they report to, how much work they hold, and
 * what they are for. Everything else is the profile's, one press away.
 *
 * # Only what the company published
 *
 * The state is the `agents` push's row, which a member is sent as the seat's
 * STATE alone — doing what, on which item, since when, paused by whom — and
 * the facts are the org projection's and the tracker's `work_workload`. How the
 * seat RUNS — its model, its node, its budget windows, its round, its coding
 * run — is the console's peek of the same seat (`routes/admin/agents/
 * SeatPeek.tsx`), drawn there for an admin and never fetched by a member's
 * browser.
 *
 * # Message is a task that asks
 *
 * There is no person-to-seat chat channel. Message opens the one New task
 * sheet with the seat as assignee and `ask` set, so the question is filed as
 * work the seat owes an answer on, and the answer lands in the asker's Inbox.
 */

import { useMemo } from "react";
import { ButtonLink, EmptyState, EmptyValue, StatusDot } from "@crewlethq/ui";
import { UserGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { MessageSeatButton } from "~/components/writes.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { renderInline } from "~/lib/markdown.ts";
import { unitPath } from "~/lib/orgchart.ts";
import {
  activityOf,
  handleLabel,
  indexOrg,
  nameOfIn,
  ringOf,
  seatPath,
  stateLine,
  toneOf,
  type Seat,
} from "~/lib/seats.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { AgentRow } from "~/protocol/index.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { liveRow } from "./seat/profile.ts";

export function SeatPeek({ handle }: { handle: string }) {
  const org = useOrg();
  const agents = useAgents();
  const index = useMemo(() => indexOrg(org), [org]);
  // BY THE HANDLE ALONE, as the seat's page is (`OrgIndex.byHandle`).
  const seat = index.byHandle.get(handle);
  if (!seat) {
    // NOT AN EMPTY RAIL. A `peek=seat:` reaches this from a pasted or
    // hand-edited URL as often as from a card, so the honest answer names the
    // handle that resolved to nothing rather than a header over no seat.
    return (
      <EmptyState
        size="compact"
        icon={<UserGlyph size={32} />}
        title={`Nobody called “${handle}”`}
        description="Teammates are addressed by handle. The org chart may have renamed or removed this one."
      />
    );
  }
  return <SeatPeekBody seat={seat} agent={liveRow(agents, seat)} nameOf={nameOfIn(index)} />;
}

function SeatPeekBody({
  seat,
  agent,
  nameOf,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  nameOf: (key: string) => string;
}) {
  const now = useNow();
  const human = seat.kind === "human";
  const state = human ? undefined : activityOf(agent);
  const ring = human ? undefined : ringOf(state);
  const workload = useQuery("work_workload", undefined, { pollMs: 60_000 });
  const load = workload.data?.rows?.find((r) => r.handle === seat.handle);
  const place = unitPath(seat.unit);

  return (
    <div className="seat-peek">
      <header className="seat-peek-head">
        <SeatAvatar
          name={seat.name}
          size="lg"
          kind={human ? "human" : "agent"}
          {...(ring ? { ring } : {})}
          decorative
        />
        <div className="seat-peek-who">
          <h2 className="seat-peek-name">{seat.name}</h2>
          <span className="seat-peek-sub">
            {[handleLabel(seat.handle), place || "Above every unit"].filter(Boolean).join(" · ")}
          </span>
        </div>
      </header>

      {!human && (
        // THE STATE LINE, tinted by the state — the one hue a seat carries.
        <section className="seat-peek-state" data-state={state} aria-label="Doing now">
          <span className="seat-peek-line">
            <StatusDot tone={toneOf(state)} pulse={state === "working"} />
            <span>{stateLine(agent, { now, seat, nameOf })}</span>
          </span>
        </section>
      )}

      <dl className="seat-peek-facts-grid">
        <dt>Reports to</dt>
        <dd>
          {seat.manager ? (
            <a className="seat-peek-person" href={href(seatPath(seat.manager, "person"))}>
              <SeatAvatar
                name={seat.manager.name}
                size="xs"
                kind={seat.manager.kind === "human" ? "human" : "agent"}
                decorative
              />
              {seat.manager.name}
            </a>
          ) : (
            "Nobody — the top of the chart"
          )}
        </dd>

        {human && seat.availability && (
          <>
            <dt>Available</dt>
            <dd>{seat.availability}</dd>
          </>
        )}

        <dt>Open work</dt>
        <dd>
          {workload.error ? (
            <EmptyValue label="The tracker did not answer" />
          ) : !workload.data ? (
            <EmptyValue label="Reading the tracker" />
          ) : load && load.open > 0 ? (
            <a className="t-link" href={href(["work"], { assignee: seat.handle })}>
              {plural(load.open, "task")}
              {load.blocked > 0 && ` · ${load.blocked} blocked`}
              {load.overdue > 0 && ` · ${load.overdue} overdue`}
            </a>
          ) : (
            "Nothing open"
          )}
        </dd>
      </dl>

      {seat.goal && (
        <section className="seat-peek-goal" aria-label="Goal">
          <h3>Goal</h3>
          {/* PROMPT TEXT, which is markdown: a ``code`` span is one. */}
          <p>{renderInline(seat.goal, "goal")}</p>
        </section>
      )}

      <footer className="seat-peek-foot">
        <ButtonLink variant="primary" size="small" href={href(seatPath(seat, "person"))}>
          Open profile
        </ButtonLink>
        <MessageSeatButton handle={seat.handle} />
      </footer>
    </div>
  );
}
