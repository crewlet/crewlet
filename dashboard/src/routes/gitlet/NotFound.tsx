/**
 * What a Gitlet page says of an address naming a change, a repository or a
 * commit the engine does not hold: what is missing, in the page's own words,
 * and the ways on from it. The frame's generic "Not found" named nothing and
 * offered nothing, so a reader who followed an old link had no step but Back.
 */

import { ButtonLink, EmptyState } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { REPO_PARAM, repoHref } from "./paths.ts";

export function CodeNotFound({
  title,
  description,
  repo,
  changes = false,
}: {
  title: string;
  description: string;
  /** The repository the address names, where it exists: its own page is a way on. */
  repo?: string;
  /** Offer the repository's changes rather than its page (the address named a change). */
  changes?: boolean;
}) {
  return (
    <EmptyState
      title={title}
      description={description}
      action={
        <div className="row gap-2 wrap">
          {repo && (
            <ButtonLink
              variant="secondary"
              href={changes ? href(["gitlet"], { [REPO_PARAM]: repo }) : repoHref(repo)}
            >
              {changes ? `Open ${repo}'s changes` : `Open ${repo}`}
            </ButtonLink>
          )}
          <ButtonLink variant="ghost" href={href(["gitlet", "repos"])}>
            Every repository
          </ButtonLink>
        </div>
      }
    />
  );
}
