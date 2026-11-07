/**
 * The way to the ceiling that refused something, or who can raise it.
 *
 * Drawn wherever a refusal or a notice a token budget caused is shown: the
 * assistant's refusal of a question and its turn stopped mid-answer, a
 * knowledge answer the palette could not write, the Inbox's stopped seats
 * and spent company budget, and Structure's model limits. The remedy names
 * the setting (`protocol/remedy.ts`) and `lib/remedy.ts` resolves it, behind
 * the same gate as the editor it opens.
 *
 * A CALLOUT TAKES THE TWO APART (`LimitCallout`): the link is its action, and
 * who can change it, for a reader who may not, is said in its body. A
 * sentence in the action's place is no control, and beside the message it
 * squeezed it onto a third line at 768 px.
 */

import type { ReactNode } from "react";
import { ButtonLink, Callout, Text, type CalloutProps } from "@crewlethq/ui";
import { ArrowRightGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { arriveAgain, namesSection } from "~/lib/arrival.ts";
import {
  SECTION_PARAM,
  remedyDestination,
  remedyOffer,
  type RemedyDestination,
  type RemedyOffer,
} from "~/lib/remedy.ts";
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

/** What the remedy offers the person reading, now. */
function useRemedyOffer(remedy: Remedy): RemedyOffer {
  const accessFor = useOrgWriteAccessFor();
  return remedyOffer(remedy, accessFor(remedyDestination(remedy).target));
}

function Offer({ remedy, label }: { remedy: Remedy; label: string | undefined }) {
  const offer = useRemedyOffer(remedy);
  switch (offer.kind) {
    case "pending":
      return null;
    case "who":
      return (
        <Text variant="caption" tone="secondary">
          {offer.sentence}
        </Text>
      );
    case "open":
      return <RemedyLink destination={offer.destination} label={label} />;
  }
}

/**
 * A callout about a limit: the way to its ceiling as the callout's action, or,
 * for a reader who may not change it, who can, as the last line of its body.
 */
export function LimitCallout({
  remedy,
  label,
  children,
  ...callout
}: Omit<CalloutProps, "action"> & {
  remedy: Remedy | null;
  /** The press's words where the screen names the scope itself. */
  label?: string;
}) {
  return remedy === null ? (
    <Callout {...callout}>{children}</Callout>
  ) : (
    <OfferedCallout remedy={remedy} label={label} callout={callout}>
      {children}
    </OfferedCallout>
  );
}

function OfferedCallout({
  remedy,
  label,
  callout,
  children,
}: {
  remedy: Remedy;
  label: string | undefined;
  callout: Omit<CalloutProps, "action" | "children">;
  children: ReactNode;
}) {
  const offer = useRemedyOffer(remedy);
  return (
    <Callout
      {...callout}
      action={
        offer.kind === "open" ? (
          <RemedyLink destination={offer.destination} label={label} />
        ) : undefined
      }
    >
      {children}
      {offer.kind === "who" && <p>{offer.sentence}</p>}
    </Callout>
  );
}

function RemedyLink({
  destination,
  label,
}: {
  destination: RemedyDestination;
  label: string | undefined;
}) {
  const { path, query } = destination;
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
      {label ?? destination.label}
    </ButtonLink>
  );
}
