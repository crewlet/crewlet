import { ScreenHead } from "~/app/Shell.tsx";
import { Empty, Button } from "~/ui/primitives.tsx";
import { useNavigator, useRoute } from "~/app/router.tsx";

export function NotFound({ what }: { what: string }) {
  const route = useRoute();
  const nav = useNavigator();
  return (
    <>
      <ScreenHead title="Not a screen" />
      <Empty
        icon="compass"
        title={`This URL names ${what}, and there is no such screen.`}
        hint={
          <>
            The address was <code className="inline">{route.hash}</code>. Every screen is reachable
            from the sidebar, and any event, trace or turn id can be pasted into the search box.
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
