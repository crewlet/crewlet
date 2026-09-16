import { Button, EmptyState, InlineCode, PageHeader } from "@crewlethq/ui";
import { useNavigator, useRoute } from "~/app/router.tsx";
import { ExploreGlyph } from "@crewlethq/icons/glyphs";

export function NotFound({ what }: { what: string }) {
  const route = useRoute();
  const nav = useNavigator();
  return (
    <>
      <PageHeader title="Not a screen" />
      <EmptyState
        icon={<ExploreGlyph />}
        title={`This URL names ${what}, and there is no such screen.`}
        description={
          <>
            The address was <InlineCode>{route.hash}</InlineCode>. Every screen is reachable from
            the sidebar, and any event, trace or turn id can be pasted into the search box.
          </>
        }
        action={
          <Button variant="primary" onClick={() => nav.to([])}>
            Go to the overview
          </Button>
        }
      />
    </>
  );
}
