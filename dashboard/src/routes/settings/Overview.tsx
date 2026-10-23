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
 * Read from the org projection by every reader, and drawn only once it has
 * arrived ([CharterPage]).
 */

import { Card, DescriptionList } from "@crewlethq/ui";
import { BuildingComplexGlyph, CompassGlyph, TargetGlyph } from "@crewlethq/icons/glyphs";
import type { OrgProjection } from "~/protocol/index.ts";
import { CharterPage } from "./charter.tsx";

export function Overview() {
  return (
    <CharterPage loading="Loading the company's charter">
      {(org) => <Charter org={org} />}
    </CharterPage>
  );
}

/** The company as the chart says it. */
function Charter({ org }: { org: OrgProjection }) {
  return (
    <div className="col gap-4">
      <Card>
        <Card.Header icon={<BuildingComplexGlyph size="sm" />}>
          <Card.Title>The company</Card.Title>
        </Card.Header>
        <DescriptionList>
          <DescriptionList.Item term="Name">
            {org.name || <span className="muted">No name is set.</span>}
          </DescriptionList.Item>
          {/* THE COMPANY'S ONE CLOCK (ADR-0018), as the engine resolved it:
              every day it cuts (today, a due band, a budget's window) is cut
              on this zone, not on the reader's browser. */}
          {org.timezone && <DescriptionList.Item term="Clock">{org.timezone}</DescriptionList.Item>}
        </DescriptionList>
      </Card>
      <Card>
        <Card.Header icon={<TargetGlyph size="sm" />}>
          <Card.Title>Mission</Card.Title>
        </Card.Header>
        <p className="t-body measure">
          {org.mission || <span className="muted">No mission is set.</span>}
        </p>
      </Card>
      {org.vision && (
        <Card>
          <Card.Header icon={<CompassGlyph size="sm" />}>
            <Card.Title>Vision</Card.Title>
          </Card.Header>
          <p className="t-body measure">{org.vision}</p>
        </Card>
      )}
    </div>
  );
}
