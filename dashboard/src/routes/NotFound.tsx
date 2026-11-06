import { Button, ButtonLink, EmptyState, InlineCode } from "@crewlethq/ui";
import { CompassGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import { sentence } from "~/lib/format.ts";

/**
 * A path the route table does not have.
 *
 * NAMED, AND NEVER REDIRECTED. There is no redirect table: a redirect whose
 * old path becomes a live route sends every reader of that route somewhere
 * else, permanently, with the address bar agreeing with them. So an old
 * address — a `#/company/…` from before the one-sidebar rebuild, a bare
 * `#/live/traces` — says what it asked for and, where the product knows, why
 * there is nothing there.
 *
 * `away` is the way on where the address named an object of a list (a
 * schedule nobody declared): that list, rather than Home.
 */
export function NotFound({
  what,
  hint,
  away,
}: {
  what: string;
  hint?: string;
  away?: { label: string; path: string[] };
}) {
  const route = useRoute();
  const nav = useNavigator();
  return (
    <EmptyState
      icon={<CompassGlyph size={32} />}
      title={`This URL names ${what}, and there is no such screen.`}
      description={
        <>
          {hint ? <>{sentence(hint)} </> : null}
          The address was <InlineCode>{route.hash}</InlineCode>.
        </>
      }
      action={
        away ? (
          <ButtonLink variant="primary" href={href(away.path)}>
            {away.label}
          </ButtonLink>
        ) : (
          <Button variant="primary" onClick={() => nav.to(["home"])}>
            Go home
          </Button>
        )
      }
    />
  );
}
