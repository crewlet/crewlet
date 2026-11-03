/**
 * A repository's Checks tab: every attempt its rows keep, newest ended first
 * and every one not yet ended after them (`code_checks` with `repo=`), each
 * opening in the pane every checks listing shares. The company-wide window is
 * Code › Checks.
 */

import { useMemo } from "react";
import { CheckListing } from "./CheckList.tsx";

export function RepoChecks({ repo }: { repo: string }) {
  const params = useMemo(() => ({ repo }), [repo]);
  return (
    <CheckListing
      params={params}
      label={`Checks of ${repo}`}
      showRepo={false}
      empty={{
        title: `No check has run on ${repo}`,
        hint: "A repository's checks are the ones .crewlet/checks.yaml defines on the branch a commit is checked for; they are kept thirty days after they end.",
      }}
    />
  );
}
