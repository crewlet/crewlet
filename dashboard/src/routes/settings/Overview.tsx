/**
 * Company › Overview: who the company is. Its name and its clock, its mission
 * and its vision.
 *
 * # The company's own page, first in Company
 *
 * It was Settings › General, and it is no setting: it is what the company has
 * told every seat about itself, verbatim, in every executor's prompt, and the
 * first place a founder looks for "what did we tell them". The standing
 * policies are told to every seat too, and are their own row, Company ›
 * Policies, so this page does not draw them a second time. A unit's goals are
 * the unit's, drawn on its page under the Org chart.
 *
 * # One profile, not a card a sentence
 *
 * The company is one card: its name is the card's title, and its mission, its
 * vision and its clock are the facts under it, in one property list. Three
 * cards each holding one line spent a header on every sentence and stretched
 * the page into a column of chrome; the reading column is [CharterPage]'s.
 *
 * Read from the org projection by every reader, and drawn only once it has
 * arrived ([CharterPage]).
 */

import { Card, DescriptionList } from "@crewlethq/ui";
import { BuildingComplexGlyph } from "@crewlethq/icons/glyphs";
import type { OrgProjection } from "~/protocol/index.ts";
import { CharterPage } from "./charter.tsx";

export function Overview() {
  return (
    <CharterPage loading="Loading the company's charter" edits="charter">
      {(org) => <Charter org={org} />}
    </CharterPage>
  );
}

/** The company as the chart says it. */
function Charter({ org }: { org: OrgProjection }) {
  return (
    <Card>
      <Card.Header icon={<BuildingComplexGlyph size="sm" />}>
        <Card.Title>{org.name || <span className="muted">No name is set</span>}</Card.Title>
      </Card.Header>
      <DescriptionList>
        <DescriptionList.Item term="Mission">
          <span className="t-body">
            {org.mission || <span className="muted">No mission is set.</span>}
          </span>
        </DescriptionList.Item>
        {org.vision && (
          <DescriptionList.Item term="Vision">
            <span className="t-body">{org.vision}</span>
          </DescriptionList.Item>
        )}
        {/* THE COMPANY'S ONE CLOCK (ADR-0018), as the engine resolved it:
            every day it cuts (today, a due band, a budget's window) is cut
            on this zone, not on the reader's browser. */}
        {org.timezone && <DescriptionList.Item term="Clock">{org.timezone}</DescriptionList.Item>}
      </DescriptionList>
    </Card>
  );
}
