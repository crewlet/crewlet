/**
 * Company › Principles: who the company is and the rules it works by, the
 * Company landing (`#/company`, and `#/company/principles`).
 *
 * # One page for what every agent is told about the company
 *
 * The company's mission, vision and standing policies are what every executor
 * is given verbatim at the start of every turn. They were two pages (Overview
 * and Policies) answering one question, "what did we tell them", so a reader
 * went to one to read half of it. They are one page, in the order an agent
 * reads them.
 *
 * # The statements in the main column, the facts beside them
 *
 * The page heading says what the page is. The main column is the three things
 * the company says, each a kit `Statement` under the same label: the mission
 * and the vision set a step over the body at the reading measure, and the
 * policies numbered (a reader cites "policy 2") and divided. Beside them, in
 * an `AsideLayout` that follows the main column when the page is narrow, the
 * facts about the company (its name and its clock, with the time it reads
 * now), how agents use the page, and the engine's caps as a related row for a
 * reader who may open them.
 *
 * A MISSION AND A STATEMENT ARE NOT A KEY AND A VALUE. In a description list
 * the mission weighed what "Clock: UTC" weighed, and an unwritten vision was
 * grey text standing in its place. An unwritten part is an inline empty
 * state with its own way to write it ("Add a mission", which opens the
 * builder in that field), offered only to a reader who may: the page bar
 * already says once why the charter is not somebody's to edit.
 *
 * Read from the org projection by every reader, and drawn only once it has
 * arrived ([CharterPage]).
 */

import { useId } from "react";
import {
  AsideLayout,
  Card,
  HeadingLevelProvider,
  List,
  ListItem,
  Stack,
  Statement,
  Text,
} from "@crewlethq/ui";
import { GaugeGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { RelatedRow } from "~/app/frame/RelatedRow.tsx";
import { useNow } from "~/lib/clock.ts";
import { clockIn } from "~/lib/format.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { CharterPartLink } from "~/routes/agents/header.tsx";
import { CharterPage } from "./charter.tsx";

/** Where the caps the engine holds every turn to are read. */
const LIMITS_PATH = ["settings", "limits"];

export function Principles() {
  return (
    <CharterPage loading="Loading the company's principles">
      {(org) => (
        // THE PAGE'S OWN STEP between its heading and its columns, the 20px
        // every screen's blocks stand apart.
        <Stack gap={5}>
          <ObjectHeader
            kind="Company"
            icon="building-complex"
            title="Principles"
            description="Who the company is and the rules every agent works by."
          />
          {/* ONE LEVEL UNDER THE PAGE'S TITLE: every label below heads a part
              of the page the title names. */}
          <HeadingLevelProvider level={3}>
            <AsideLayout asideLabel="About the company" aside={<About org={org} />}>
              <Statement
                label="Mission"
                empty={{
                  title: "No mission yet",
                  description:
                    "One sentence on what the company is for tells every agent why its work matters.",
                  action: <CharterPartLink part="mission" label="Add a mission" />,
                }}
              >
                {org.mission}
              </Statement>
              <Statement
                label="Vision"
                empty={{
                  title: "No vision yet",
                  description:
                    "Where the company is heading helps agents choose between two good options.",
                  action: <CharterPartLink part="vision" label="Add a vision" />,
                }}
              >
                {org.vision}
              </Statement>
              <Policies policies={org.policies ?? []} />
            </AsideLayout>
          </HeadingLevelProvider>
        </Stack>
      )}
    </CharterPage>
  );
}

/**
 * The standing policies every agent reads on every turn, numbered and divided.
 * NO COUNT beside the label: the numbers are the count.
 */
function Policies({ policies }: { policies: string[] }) {
  const labelID = useId();
  return (
    <Statement
      label={<span id={labelID}>Standing policies</span>}
      empty={{
        title: "No policies yet",
        description:
          "The rules every agent follows, such as how to handle money or when to ask first.",
        action: <CharterPartLink part="policies" label="Add a policy" />,
      }}
    >
      {policies.length > 0 ? (
        // ONE CARD, THE RULES ACROSS IT: each policy a numbered row a reader
        // cites ("policy 2"), divided from the next across the card's width.
        <Card padding="none">
          <List
            variant="numbered-divided"
            aria-labelledby={labelID}
            className="principles-policies"
          >
            {policies.map((policy, i) => (
              <ListItem key={i}>{policy}</ListItem>
            ))}
          </List>
        </Card>
      ) : null}
    </Statement>
  );
}

/**
 * Beside the statements: the company's own facts, how its agents use this
 * page, and the engine's caps for a reader who may open them.
 */
function About({ org }: { org: OrgProjection }) {
  const now = useNow();
  const zone = org.timezone ?? "";
  // THE TIME IT IS THERE, so a reader in another zone knows what "today"
  // means to every agent: the engine cuts every day on this clock (ADR-0018).
  const time = zone ? clockIn(now, zone) : null;
  const usesID = useId();
  return (
    <>
      <PropertiesRail
        headings="eyebrow"
        groups={[
          {
            name: "Company",
            properties: [
              { label: "Name", value: org.name || undefined },
              {
                label: "Clock",
                value: zone ? (
                  <span className="principles-clock">
                    <span>{zone}</span>
                    {time && (
                      <Text as="span" variant="caption">
                        {time} there now
                      </Text>
                    )}
                  </span>
                ) : undefined,
                title:
                  "Every day the engine cuts (today, a due date, a budget's window) is cut on this clock.",
              },
            ],
          },
        ]}
      />
      <section className="principles-uses" aria-labelledby={usesID}>
        <Text as="h3" variant="label" id={usesID}>
          How agents use this
        </Text>
        <Text as="p" variant="caption">
          Every agent is given the mission, the vision and the policies, word for word, at the start
          of every turn. They weigh their choices against the first two and follow each policy as a
          standing rule.
        </Text>
      </section>
      {/* THE ENGINE'S CAPS are not principles the company writes but limits
          the engine enforces, and live once, in Settings › Limits. */}
      <RelatedRow
        path={LIMITS_PATH}
        icon={<GaugeGlyph size="sm" />}
        title="Engine limits"
        description="The caps the engine holds every turn to."
      />
    </>
  );
}
