/**
 * Company › Policies: the standing policies every agent reads on every turn.
 *
 * # Plain words, and only those
 *
 * A policy is a sentence the company writes and every executor is given,
 * verbatim: "every amount is exact to the cent". The numbers the engine holds
 * a turn to (its review rounds, tool rounds, delegation depth) are not
 * policies the company writes but caps the engine enforces, and live once, in
 * Settings › Limits; this page offers that page as a related row and draws
 * none of them.
 *
 * # A page to read
 *
 * The page says what a policy is, then lists them numbered (a reader cites
 * "policy 2"), one a row with a rule between them, in a column a sentence can
 * be read across. The edit control in the bar is named for what it edits and
 * opens the company's editor in the builder at its Policies; an empty list
 * says so and offers the same way in.
 *
 * Read from the org projection by every reader, and drawn only once it has
 * arrived ([CharterPage]).
 */

import { useId } from "react";
import { Card, EmptyState, List, ListItem } from "@crewlethq/ui";
import { GaugeGlyph, ShieldGlyph } from "@crewlethq/icons/glyphs";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { RelatedRow } from "~/app/frame/RelatedRow.tsx";
import type { OrgProjection } from "~/protocol/index.ts";
import { EditCharterButton } from "~/routes/agents/header.tsx";
import { CharterPage } from "./charter.tsx";

/** Where the caps the engine holds every turn to are read. */
const LIMITS_PATH = ["settings", "limits"];

export function Policies() {
  return (
    <CharterPage loading="Loading the company's policies" edits="policies">
      {(org) => <PolicyList org={org} />}
    </CharterPage>
  );
}

function PolicyList({ org }: { org: OrgProjection }) {
  const policies = org.policies ?? [];
  const titleID = useId();
  return (
    <>
      <PageNote>
        The standing instructions every agent is given, word for word, at the start of every turn.
      </PageNote>
      <Card padding="none">
        <Card.Header icon={<ShieldGlyph size="sm" />}>
          <Card.Title id={titleID}>Standing policies</Card.Title>
        </Card.Header>
        {policies.length > 0 ? (
          <List variant="numbered-divided" aria-labelledby={titleID}>
            {policies.map((policy, i) => (
              <ListItem key={i}>{policy}</ListItem>
            ))}
          </List>
        ) : (
          <EmptyState
            size="compact"
            icon={<ShieldGlyph size={32} />}
            title="No policies are set"
            description="Write the rules every agent follows, such as how to handle money or when to ask first."
            action={<EditCharterButton part="policies" />}
          />
        )}
      </Card>
      {/* THE ENGINE'S CAPS, the one related place, for a reader who may open it. */}
      <RelatedRow
        path={LIMITS_PATH}
        icon={<GaugeGlyph size="sm" />}
        title="Engine limits"
        description="The caps the engine holds every turn to, in Settings › Limits."
      />
    </>
  );
}
