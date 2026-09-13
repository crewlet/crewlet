/**
 * Route dispatch.
 *
 * A flat switch rather than a route table with lazy chunks: there are twenty
 * screens, the whole application is ~180 KB gzipped, and it is served from the
 * same binary as the API — so a code-split chunk buys a round trip against a
 * server that is already answering. The switch is also what makes the screen
 * list readable in one place.
 */

import { Component, type ReactNode } from "react";
import { Shell } from "./Shell.tsx";
import { ToastProvider } from "~/ui/Toast.tsx";
import { Button, Code, Empty } from "~/ui/primitives.tsx";
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

interface BoundaryProps {
  /** What the reader is looking at. A new value is a new chance to render. */
  resetKey: string;
  children: ReactNode;
}

interface BoundaryState {
  error: Error | null;
  resetKey: string;
}

/**
 * A screen that throws takes itself down, and nothing else.
 *
 * WITHOUT ONE, A RENDER ERROR UNMOUNTS THE WHOLE APPLICATION. That is React's
 * contract, and it is what a seat whose `llm` was a per-phase mapping did: one
 * field of one seat reached a component as an object, and the reader was left
 * with a blank page, no navigation and no way to learn which screen or which
 * field. Around the routed screen, and only there, because the shell is what
 * a reader needs to get somewhere else.
 *
 * It resets when the reader goes somewhere (any change of the hash, a filter
 * included, since a different lens or selection may not reach the fault) and
 * when they ask to try again. React reports the caught error to the console
 * itself, so it is not logged twice here.
 */
export class ScreenBoundary extends Component<BoundaryProps, BoundaryState> {
  override state: BoundaryState = { error: null, resetKey: this.props.resetKey };

  static getDerivedStateFromError(error: unknown): Partial<BoundaryState> {
    return { error: error instanceof Error ? error : new Error(String(error)) };
  }

  static getDerivedStateFromProps(
    props: BoundaryProps,
    state: BoundaryState,
  ): Partial<BoundaryState> | null {
    return props.resetKey === state.resetKey ? null : { error: null, resetKey: props.resetKey };
  }

  override render(): ReactNode {
    const { error } = this.state;
    if (!error) return this.props.children;
    return (
      <Empty
        icon="alert"
        title="This screen could not be drawn"
        hint="Something it received did not have the shape it expects. The rest of the dashboard keeps working, and the message below is what to include in a report."
        action={
          <div className="col gap-3" style={{ alignItems: "center" }}>
            <Code plain>{error.message || error.name}</Code>
            <Button onClick={() => this.setState({ error: null })}>Try again</Button>
          </div>
        }
      />
    );
  }
}

export function App() {
  const route = useRoute();
  return (
    // The toast host wraps the shell rather than sitting inside a screen: an
    // outcome has to survive the navigation the write causes, and a provider
    // mounted per screen is unmounted by exactly that.
    <ToastProvider>
      <Shell>
        <ScreenBoundary resetKey={route.hash}>
          <Screen />
        </ScreenBoundary>
      </Shell>
    </ToastProvider>
  );
}
