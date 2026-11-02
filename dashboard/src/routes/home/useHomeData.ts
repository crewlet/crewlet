/**
 * What Home asks and derives, as one hook, so the reader's board can draw
 * Home's cards in any order and as often as they place them without asking
 * anything twice.
 *
 * # Three questions, and the rest is pushes
 *
 * `work_flow` (Worklet's history replayed backward from today's census,
 * asked only with Worklet), `token_series` (the spend over the window, by
 * team) and `decisions` (what waits on this person). Who is working, the
 * budget meter and the fleet are the pushes every screen already holds, so
 * nothing here polls for them. The feed asks its own question (`Feed`).
 *
 * ONE PER PAGE. `Home` calls it once and hands it down through
 * `HomeDataProvider`; a card reads it with `useSharedHomeData`.
 */

import { useServicesAsk } from "~/lib/services.ts";
import { createContext, useContext, useMemo } from "react";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useAssistantContext } from "~/app/assistant/useAssistantContext.ts";
import { choiceKey, urlAction } from "~/app/assistant/urlAction.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { ownName, useViewer } from "~/lib/viewer.ts";
import { managedBlock, mayChangeConfig } from "~/lib/useWriteAccess.ts";
import { useNow } from "~/lib/clock.ts";
import { useAttention } from "~/lib/useAttention.ts";
import { useAgents, useConnection, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { seatDecisionsFor } from "~/components/DecisionRow.tsx";
import { seatConditionsOf } from "~/lib/decisions.ts";
import { useLeadScope } from "~/lib/leadScope.ts";
import { conditionKey, conditionsToDecide } from "~/lib/attention.ts";
import {
  DEFAULT_RANGE,
  HOME_RANGES,
  crewOf,
  flowPoints,
  rangeOf,
  statusSentence,
  waitingOf,
} from "./model.ts";
import { gapsOf } from "./board/kit.tsx";

/** How often the decisions are asked again: a person answers one and another
 *  lands, and the push carries neither. */
const DECISIONS_POLL_MS = 30_000;

/** The series and the spend move on the scale of minutes. */
const SLOW_POLL_MS = 60_000;

/** The leads a managed document honours: none, since it refuses a lead's write too. */
const NO_LEADS: ReadonlySet<string> = new Set();

export function useHomeData() {
  const now = useNow();
  const org = useOrg();
  const viewer = useViewer();
  // THE READER IS GREETED BY THEIR OWN NAME, which their directory row holds
  // and the frame's `GET /auth/session` answered (`ownName`), never the
  // seat's, which is the chart's name for a place and often a role: a person
  // with no name was greeted "Good afternoon, Founder". No name, no name.
  const own = ownName(useFrameSession().answer);
  const agents = useAgents();
  const engine = useEngineHealth();
  const { connected, authRejected } = useConnection();
  const attention = useAttention();
  const [rangeValue, setRange] = useParam("range", DEFAULT_RANGE);
  const range = rangeOf(rangeValue);
  // --- assistant page actions (WP12) ---
  // THE PAGE'S ONE RANGE, which every widget with a time axis reads.
  const nav = useNavigator();
  useAssistantContext({
    actions: urlAction("home.range.set", {
      scope: "Home's window (view only)",
      keys: { range: choiceKey("Window", HOME_RANGES, "filter", DEFAULT_RANGE) },
      nav,
    }),
  });
  // --- end assistant page actions (WP12) ---

  const decisions = useQuery("decisions", undefined, {
    enabled: viewer.owner !== "",
    pollMs: DECISIONS_POLL_MS,
  });
  // WORKLET'S HISTORY, asked only of a reader holding Worklet: the
  // engine refuses it to anybody else, and the tiles and chart it feeds say
  // what would open them rather than drawing a refusal as a broken read.
  // AND OF A COMPANY RUNNING WORKLET, where the history is: elsewhere
  // the tiles and chart it feeds are not drawn (`Kpis.tsx`'s `KPI_SERVICES`).
  const ask = useServicesAsk();
  const flow = useQuery(
    "work_flow",
    { bucket: "day", points: flowPoints(range) },
    { pollMs: SLOW_POLL_MS, enabled: viewer.worklet && ask.tracker },
  );
  const spend = useQuery(
    "token_series",
    { days: range.days, group: "unit", bucket: "day" },
    { pollMs: SLOW_POLL_MS },
  );

  const crew = useMemo(() => crewOf(agents), [agents]);
  // THE STOPPED SEATS, SPLIT BY WHO CAN ACT: the ones this reader can raise
  // or hand on are decisions waiting on them; the rest are conditions they
  // can see and nothing more. See `seatDecisionsFor`.
  // A LEAD raises a seat they lead whose OWN window refused (never the
  // company's ceiling), and on a MANAGED document nobody outside its writers
  // raises anything, a lead included: the engine refuses their `/config` write
  // `config_managed` exactly as it refuses anybody's (ADR-0030).
  const leads = useLeadScope().seats;
  const managed = managedBlock(viewer) !== null;
  const seats = useMemo(
    () =>
      seatDecisionsFor(
        seatConditionsOf(agents),
        { configWrite: mayChangeConfig(viewer), acts: viewer.acts },
        managed ? NO_LEADS : leads,
      ),
    [agents, viewer, leads, managed],
  );
  // WHAT WAITS ON THE READER: the engine's count of their asks and parked
  // runs, and the stopped seats they can act on. Unknown (nobody signed in,
  // or not answered yet) is null, never zero.
  const waiting = viewer.owner
    ? waitingOf(decisions.data, seats.mine, gapsOf([decisions.data]).length > 0)
    : null;
  // THE OTHER CONDITIONS: what the engine raised that a person decides and
  // that is not one of this reader's own decisions: a seat stopped for
  // another reason, a budget near its ceiling. Exactly the Inbox's condition
  // rows (`conditionsToDecide`): the engine's own rows are the sentence's
  // precedence and the engine row's, a stalled round and the parked runs are
  // Live's, and a budget-stopped seat is one of the decisions.
  const raised = conditionsToDecide(attention);
  // A stopped seat counts here exactly when it is not counted as one of the
  // reader's decisions: every one while nothing waits is known, else the ones
  // they cannot act on.
  const conditions =
    raised.length + (waiting ? seats.others.length : seats.mine.length + seats.others.length);
  // WHERE THEY ARE LISTED: the Inbox's "Needs you" group, opened on the
  // one condition when there is exactly one.
  const conditionsHref =
    conditions === 1 && raised.length === 1
      ? href(["inbox"], { row: conditionKey(raised[0]!.id) })
      : href(["inbox"]);

  const sentence = statusSentence({
    company: org?.name ?? "",
    connected,
    authRejected,
    configured: engine?.configured,
    posture: engine?.posture,
    draining: engine?.shutting_down ?? false,
    nodes: engine?.nodes,
    waiting,
    conditions,
    conditionsHref,
    working: crew.working,
  });

  return {
    now,
    own,
    agents,
    viewer,
    range,
    setRange,
    decisions,
    flow,
    spend,
    crew,
    seats,
    waiting,
    sentence,
  };
}

export type HomeData = ReturnType<typeof useHomeData>;

/**
 * The one HomeData a page holds, for the widgets drawn on it: Home's own cards
 * share one set of questions however many of them the reader places.
 */
const HomeDataContext = createContext<HomeData | null>(null);
export const HomeDataProvider = HomeDataContext.Provider;

export function useSharedHomeData(): HomeData {
  const data = useContext(HomeDataContext);
  if (!data) throw new Error("A Home widget was drawn outside HomeDataProvider.");
  return data;
}
