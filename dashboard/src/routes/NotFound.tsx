import { Button, EmptyState, Kbd } from "@crewlethq/ui";
import { CompassGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator, useRoute } from "~/app/router.tsx";

/**
 * A path the route table does not have.
 *
 * NAMED, AND NEVER REDIRECTED. There is no redirect table: a redirect whose
 * old path becomes a live route sends every reader of that route somewhere
 * else, permanently, with the address bar agreeing with them. So an old
 * address — a `#/company/…` from before the one-sidebar rebuild, a bare
 * `#/live/traces` — says what it asked for and, where the product knows, why
 * there is nothing there.
 */
export function NotFound({ what, hint }: { what: string; hint?: string }) {
  const route = useRoute();
  const nav = useNavigator();
  return (
    <EmptyState
      icon={<CompassGlyph size={32} />}
      title={`This URL names ${what}, and there is no such screen.`}
      description={
        <>
          {hint ? <>{capitalise(hint)}. </> : null}
          The address was <code className="inline">{route.hash}</code>. Every screen is reachable
          from the sidebar, and any event, trace or turn id can be pasted into the command{" "}
          {/* THE PLATFORM'S OWN CHORD, drawn by the part the sidebar's search
              draws it with: "⌘K" on a machine whose sidebar says Ctrl + K was
              two answers to one question. Kept on one line with the word before
              it, so the chord is never a line of its own. */}
          <span className="nowrap">
            palette, <Kbd keys={["Mod", "k"]} subtle />.
          </span>
        </>
      }
      action={
        <Button variant="primary" onClick={() => nav.to(["home"])}>
          Go home
        </Button>
      }
    />
  );
}

function capitalise(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
