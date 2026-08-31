/**
 * Route dispatch.
 *
 * A flat switch rather than a route table with lazy chunks: there are twenty
 * screens, the whole application is ~180 KB gzipped, and it is served from the
 * same binary as the API — so a code-split chunk buys a round trip against a
 * server that is already answering. The switch is also what makes the screen
 * list readable in one place.
 */

import { Shell } from "./Shell.tsx";
import { useRoute } from "./router.tsx";
import { Overview } from "~/routes/Overview.tsx";
import { People } from "~/routes/People.tsx";
import { SeatScreen } from "~/routes/Seat.tsx";
import { OrgScreen } from "~/routes/Org.tsx";
import { Runs } from "~/routes/Runs.tsx";
import { Conversations } from "~/routes/Conversations.tsx";
import { Schedules } from "~/routes/Schedules.tsx";
import { ModelActivity } from "~/routes/Model.tsx";
import { Activity } from "~/routes/Activity.tsx";
import { Knowledge } from "~/routes/Knowledge.tsx";
import { Spend } from "~/routes/Spend.tsx";
import { Fleet } from "~/routes/Fleet.tsx";
import { Integrations } from "~/routes/Integrations.tsx";
import { Tools } from "~/routes/Tools.tsx";
import { ConfigScreen } from "~/routes/Config.tsx";
import { Secrets } from "~/routes/Secrets.tsx";
import { TraceScreen } from "~/routes/Trace.tsx";
import { EventScreen } from "~/routes/Event.tsx";
import { TurnScreen } from "~/routes/Turn.tsx";
import { NotFound } from "~/routes/NotFound.tsx";

function Screen() {
  const route = useRoute();
  const [head, id] = route.path;
  switch (head) {
    case undefined:
      return <Overview />;
    case "people":
      return <People />;
    case "seats":
      return id ? <SeatScreen handle={id} /> : <People />;
    case "org":
      return <OrgScreen />;
    case "runs":
      return <Runs />;
    case "conversations":
      return <Conversations />;
    case "schedules":
      return <Schedules />;
    case "model":
      return <ModelActivity />;
    case "activity":
      return <Activity />;
    case "knowledge":
      return <Knowledge />;
    case "spend":
      return <Spend />;
    case "fleet":
      return <Fleet />;
    case "integrations":
      return <Integrations />;
    case "tools":
      return <Tools />;
    case "config":
      return <ConfigScreen />;
    case "secrets":
      return <Secrets />;
    case "traces":
      return id ? <TraceScreen traceId={id} /> : <NotFound what="a trace id" />;
    case "events":
      return id ? <EventScreen eventId={id} /> : <Activity />;
    case "turns":
      return id ? <TurnScreen turnId={id} /> : <NotFound what="a turn id" />;
    default:
      return <NotFound what={`the screen “${head}”`} />;
  }
}

export function App() {
  return (
    <Shell>
      <Screen />
    </Shell>
  );
}
