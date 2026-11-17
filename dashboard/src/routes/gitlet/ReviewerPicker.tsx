/**
 * Who is asked to review a change, PICKED from the chart rather than typed:
 * every seat by its name, agents and people alike, with its handle under it,
 * since a review is asked of a seat by its handle (`code_open_change`'s and
 * `code_request_review`'s `reviewers`).
 *
 * THE READER IS NOT OFFERED. Whoever opens a change is its author, and an
 * author's review counts for nothing (`own_change`): the engine refuses an ask
 * of one, so the picker never lists the one choice it would refuse. A change's
 * other authors are left out by the caller (`exclude`).
 */

import { useMemo } from "react";
import { Select } from "@crewlethq/ui";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ChoiceFace } from "~/ui/SeatAvatar.tsx";

export function ReviewerPicker({
  id,
  describedBy,
  value,
  onChange,
  exclude = [],
}: {
  id?: string;
  describedBy?: string;
  /** The handles picked so far. */
  value: readonly string[];
  onChange: (handles: string[]) => void;
  /** Handles never offered: a change's authors. */
  exclude?: readonly string[];
}) {
  const org = useOrg();
  const viewer = useViewer();
  const options = useMemo(
    () =>
      indexOrg(org)
        .seats.filter((s) => s.handle !== viewer.handle && !exclude.includes(s.handle))
        .map((s) => ({
          value: s.handle,
          label: s.name || s.handle,
          description: s.handle,
          leading: <ChoiceFace seat={s} />,
        })),
    [org, viewer.handle, exclude],
  );
  return (
    <Select
      id={id}
      aria-describedby={describedBy}
      ariaLabel="Reviewers"
      width="full"
      multiple
      searchable
      placeholder="Nobody yet"
      emptyMessage="No seat matches"
      value={[...value]}
      options={options}
      onChange={(next) => onChange((Array.isArray(next) ? next : [next]).map(String))}
    />
  );
}
