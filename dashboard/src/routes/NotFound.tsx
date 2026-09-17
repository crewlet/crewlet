import { Button, EmptyState } from "@crewlethq/ui";
import { ExploreGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator, useRoute } from "~/app/router.tsx";

export function NotFound({ what }: { what: string }) {
  const route = useRoute();
  const nav = useNavigator();
  return (
    <EmptyState
      icon={<ExploreGlyph size={32} />}
      title={`This URL names ${what}, and there is no such screen.`}
      description={
        <>
          The address was <code className="inline">{route.hash}</code>. Every screen is reachable
          from the rail on the left, and any event, trace or turn id can be pasted into the search
          box.
        </>
      }
      action={
        <Button variant="primary" onClick={() => nav.to(["inbox"])}>
          Go to the inbox
        </Button>
      }
    />
  );
}
