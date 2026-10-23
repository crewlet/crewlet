/**
 * New, as a menu of the kinds: each row the kind's glyph in its colour and its
 * word, opening that kind's own create page. The gallery's header draws it,
 * and a task's rail draws it with the task (`item`), so what is made there is
 * listed on that task.
 */

import { ArtifactKindGlyph, Menu } from "@crewlethq/ui";
import { ChevronDownGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { newArtifactPath } from "~/app/routes.ts";
import { KIND_WORDS } from "~/lib/artifacts/words.ts";
import { offeredKinds } from "~/app/nav.ts";
import { useServicesRun } from "~/lib/services.ts";

export function NewArtifactMenu({
  item,
  quiet = false,
}: {
  /** The task the new artifact is made for, carried to its page as `item=`. */
  item?: string | undefined;
  /** A quiet trigger, for a rail; the primary button otherwise. */
  quiet?: boolean;
}) {
  const nav = useNavigator();
  const runs = useServicesRun();
  return (
    <Menu
      label={item ? `New artifact for ${item}` : "New artifact"}
      trigger={quiet ? "New artifact" : "New"}
      icon={<PlusGlyph size="sm" />}
      trailingIcon={<ChevronDownGlyph size="sm" />}
      triggerVariant={quiet ? "ghost" : "primary"}
      align="end"
      items={offeredKinds(runs).map((kind) => ({
        key: kind,
        label: KIND_WORDS[kind],
        icon: <ArtifactKindGlyph kind={kind} size="sm" />,
        onSelect: () => nav.to(newArtifactPath(kind), item ? { item } : undefined),
      }))}
    />
  );
}
