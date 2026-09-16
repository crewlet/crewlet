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
import { Button, CodeBlock, LayerHost, ToastProvider } from "@crewlethq/ui";
import { Shell } from "./Shell.tsx";
import { useRoute } from "./router.tsx";
import { Overview } from "~/routes/Overview.tsx";
import { Goals } from "~/routes/Goals.tsx";
import { People } from "~/routes/People.tsx";
import { SeatScreen } from "~/routes/Seat.tsx";
import { OrgScreen } from "~/routes/Org.tsx";
import { Runs } from "~/routes/Runs.tsx";
import { Work, WorkItem } from "~/routes/Work.tsx";
import { Sprints } from "~/routes/Sprints.tsx";
import { MyWork } from "~/routes/MyWork.tsx";
import { Pages, PageView } from "~/routes/Pages.tsx";
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
import { ErrorGlyph } from "@crewlethq/icons/glyphs";
import { EmptyState } from "@crewlethq/ui";
import { RECORD_MAX_HEIGHT } from "~/components/common.tsx";

function Screen() {
  const route = useRoute();
  const [head, id] = route.path;
  switch (head) {
    // KEYED ON THE SUBJECT, every screen that has one.
    //
    // A hash change re-renders this switch rather than remounting it, so
    // `#/turns/A` → `#/turns/B` reconciles: React keeps the same component
    // instance and every piece of per-SUBJECT state in it outlives the subject
    // it describes. The Turn header's controls are where that shows — a
    // refusal holds until the next click now, so a "Download failed" left over
    // from turn A greeted the reader of turn B — but the hazard is the shape,
    // not the control: a disclosure left open, a tab left selected and a filter
    // left set are all the same bug waiting for someone to notice.
    case undefined:
      return <Overview />;
    case "people":
      return <People />;
    case "seats":
      return id ? <SeatScreen key={id} handle={id} /> : <People />;
    case "org":
      return <OrgScreen />;
    case "runs":
      return <Runs />;
    // A key or an id BOTH resolve, because the reader has whichever they were
    // shown: a person pastes ENG-42 out of chat, and every internal link
    // carries the id. The engine's own Get takes either, so the route does
    // not have to know which it was handed.
    case "work":
      // `me` IS NOT A TASK. Keys are `<PROJECT>-<n>` and ids are uuids, so
      // neither can collide with it — and `#/work/me` is the address this
      // screen has had since it was designed.
      if (id === "me") return <MyWork />;
      return id ? <WorkItem key={id} id={id} /> : <Work />;
    case "goals":
      return <Goals />;
    // ITS OWN SCREEN rather than a tab of the board, for the reason the view
    // strip is its own question: a sprint report is about the CONTAINER over
    // time and the board is about the rows in it now.
    case "sprints":
      return <Sprints />;
    case "pages":
      return id ? <PageView key={id} id={id} /> : <Pages />;
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
      return id ? <TraceScreen key={id} traceId={id} /> : <NotFound what="a trace id" />;
    case "events":
      return id ? <EventScreen key={id} eventId={id} /> : <Activity />;
    case "turns":
      return id ? <TurnScreen key={id} turnId={id} /> : <NotFound what="a turn id" />;
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
      <EmptyState
        icon={<ErrorGlyph />}
        title="This screen could not be drawn"
        description="Something it received did not have the shape it expects. The rest of the dashboard keeps working, and the message below is what to include in a report."
        action={
          <div className="col gap-3" style={{ alignItems: "center" }}>
            <CodeBlock
              plain
              wrap
              code={error.message || error.name}
              maxHeight={RECORD_MAX_HEIGHT}
            />
            <Button variant="secondary" onClick={() => this.setState({ error: null })}>
              Try again
            </Button>
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
      {/* Every overlay portals into the nearest LayerHost, so the one at the
          root is what puts a dialog over the whole application rather than
          inside the screen that opened it. The builder's fullscreen container
          mounts a second one, because a fullscreen element renders only its
          own subtree. */}
      <LayerHost>
        <Shell>
          <ScreenBoundary resetKey={route.hash}>
            <Screen />
          </ScreenBoundary>
        </Shell>
      </LayerHost>
    </ToastProvider>
  );
}
