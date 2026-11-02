/**
 * The way to the ceiling that refused something, or who can raise it.
 *
 * Drawn wherever a refusal or a notice a token budget caused is shown: the
 * assistant's refusal of a question and its turn stopped mid-answer, a
 * knowledge answer the palette could not write, and the Inbox's stopped seats
 * and spent company budget. The remedy names the setting (`protocol/remedy.ts`)
 * and `lib/remedy.ts` resolves it, behind the same gate as the editor it opens.
 */

import { ButtonLink, Text } from "@crewlethq/ui";
import { ArrowRightGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { arriveAgain, namesSection } from "~/lib/arrival.ts";
import { SECTION_PARAM, remedyDestination, remedyOffer } from "~/lib/remedy.ts";
import { useOrgWriteAccessFor } from "~/lib/useWriteAccess.ts";
import type { Remedy } from "~/protocol/remedy.ts";

export function LimitRemedy({
  remedy,
  label,
}: {
  remedy: Remedy | null;
  /** The press's words where the screen names the scope itself. */
  label?: string;
}) {
  // NOTHING TO ASK THE GATE about a refusal no limit caused: most refusals
  // are drawn by surfaces that never mount a viewer for this.
  return remedy === null ? null : <Offer remedy={remedy} label={label} />;
}

function Offer({ remedy, label }: { remedy: Remedy; label: string | undefined }) {
  const accessFor = useOrgWriteAccessFor();
  const offer = remedyOffer(remedy, accessFor(remedyDestination(remedy).target));
  switch (offer.kind) {
    case "pending":
      return null;
    case "who":
      return (
        <Text variant="caption" tone="secondary">
          {offer.sentence}
        </Text>
      );
    case "open": {
      const { path, query } = offer.destination;
      return (
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(path, { ...query })}
          // THE SECTION ALREADY OPEN ARRIVES AGAIN: a press on it moves
          // nothing the section is read from, whatever sort or peek the
          // screen added to the address since.
          onClick={() => {
            if (namesSection(location.hash, path, query[SECTION_PARAM] ?? "")) arriveAgain();
          }}
          trailingIcon={<ArrowRightGlyph size="sm" />}
        >
          {label ?? offer.destination.label}
        </ButtonLink>
      );
    }
  }
}
