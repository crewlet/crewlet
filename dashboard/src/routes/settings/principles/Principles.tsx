/**
 * Company › Principles: who the company is and the rules it works by, the
 * Company landing (`#/company`, and `#/company/principles`).
 *
 * # One page for what every agent is told about the company
 *
 * The company's mission, vision and standing policies are what every executor
 * is given verbatim at the start of every turn. They are one page, in the
 * order an agent reads them.
 *
 * # Three cards
 *
 * The mission and the vision are two equal cards side by side, since neither
 * outranks the other, and the standing policies one card under them across
 * the page, numbered and divided. Each card names its part with a glyph and a
 * title, and holds what the company wrote or an inline empty state saying what
 * the part is for. Under the cards, for a reader who may open them, the
 * engine's caps: not principles the company writes but limits the engine
 * enforces, so they are a way to another page rather than a card here.
 *
 * Nothing else stands on the page: the company's name is the shell's and its
 * clock is the company document's, and neither is a principle.
 *
 * # Who writes it
 *
 * The page places what the charter's writer hands each card
 * ([PrinciplesCardsEdit]): the words to show, the head's Edit, the empty
 * state's Add, the editor while a part is being written, and a note under it.
 * A reader who may not write the charter is handed no control, so the page
 * carries none at all rather than a disabled one beside every part.
 *
 * # Nothing is claimed before the chart arrives
 *
 * "No mission yet" is a statement about the company, and before the org push
 * lands this browser knows nothing about it at all. So the page draws a
 * skeleton under its heading until the projection is in, and only then the
 * cards. It reads the org PROJECTION, which every reader holds, rather than
 * the guarded configuration: the charter is the company's own voice and no
 * secret. The builder's "still applying" note is not drawn here: the builder
 * no longer writes the charter, and a save made on this page says itself,
 * in its card, when this node has applied it.
 */

import type { ReactNode } from "react";
import { Card, HeadingLevelProvider, Skeleton, Stack } from "@crewlethq/ui";
import { GaugeGlyph, ScrollTextGlyph, TargetGlyph, TelescopeGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { RelatedRow } from "~/app/frame/RelatedRow.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import {
  PolicyList,
  PrincipleCard,
  StatementText,
  UnwrittenPart,
  type PrincipleCardEdit,
  type PrinciplesCardsEdit,
} from "./PrincipleCard.tsx";

/** Where the caps the engine holds every turn to are read. */
const LIMITS_PATH = ["settings", "limits"];

/** What each unwritten part is for, said where its words would be. */
const UNWRITTEN = {
  mission: {
    title: "No mission yet",
    description: "One sentence on what the company is for tells every agent why its work matters.",
  },
  vision: {
    title: "No vision yet",
    description: "Where the company is heading helps agents choose between two good options.",
  },
  policies: {
    title: "No policies yet",
    description: "The rules every agent follows, such as how to handle money or when to ask first.",
  },
} as const;

/** One card as the company wrote it, with no way to write it. */
function reading<T>(shown: T): PrincipleCardEdit<T> {
  return { shown, actions: null, add: null, editor: null, note: null };
}

/** The charter as every reader reads it. */
function readOnly(org: OrgProjection): PrinciplesCardsEdit {
  return {
    mission: reading(org.mission ?? ""),
    vision: reading(org.vision ?? ""),
    policies: reading<readonly string[]>(org.policies ?? []),
    notice: null,
  };
}

export function Principles() {
  const org = useOrg();
  return (
    // THE PAGE'S OWN STEP between its heading and its cards, the 20px every
    // screen's blocks stand apart.
    <Stack gap={5}>
      <ObjectHeader
        kind="Company"
        icon="building-complex"
        title="Principles"
        description="Who the company is and the rules every agent works by."
      />
      {org ? (
        <Charter org={org} />
      ) : (
        <div aria-busy="true">
          <Card>
            <Skeleton variant="text" rows={4} label="Loading the company's principles" />
          </Card>
        </div>
      )}
    </Stack>
  );
}

/** The three cards, and the engine's caps under them, from a chart the engine has sent. */
function Charter({ org }: { org: OrgProjection }) {
  const edit = readOnly(org);
  const policies = edit.policies;
  const writing = policies.editor != null;
  return (
    <>
      {edit.notice}
      {/* ONE LEVEL UNDER THE PAGE'S TITLE: every card's title heads a part
          of the page the title names. */}
      <HeadingLevelProvider level={3}>
        <div className="principles">
          <div className="principles-pair">
            <Statement
              part="mission"
              title="Mission"
              icon={<TargetGlyph size="sm" />}
              edit={edit.mission}
            />
            <Statement
              part="vision"
              title="Vision"
              icon={<TelescopeGlyph size="sm" />}
              edit={edit.vision}
            />
          </div>
          <PrincipleCard
            part="policies"
            title="Standing policies"
            icon={<ScrollTextGlyph size="sm" />}
            count={policies.shown.length > 0 ? policies.shown.length : undefined}
            actions={policies.actions}
            editing={writing}
            note={policies.note}
            flush={!writing && policies.shown.length > 0}
          >
            {writing ? (
              policies.editor
            ) : policies.shown.length > 0 ? (
              <PolicyList policies={policies.shown} label="Standing policies" />
            ) : (
              <UnwrittenPart {...UNWRITTEN.policies} action={policies.add} />
            )}
          </PrincipleCard>
          {/* THE ENGINE'S CAPS are not principles the company writes but
              limits the engine enforces, and live once, in Settings › Limits,
              for a reader who may open it. */}
          <RelatedRow
            path={LIMITS_PATH}
            icon={<GaugeGlyph size="sm" />}
            title="Engine limits"
            description="The caps the engine holds every turn to, whatever the policies say."
          />
        </div>
      </HeadingLevelProvider>
    </>
  );
}

/** The mission's or the vision's card: what the company wrote, or that it wrote nothing. */
function Statement({
  part,
  title,
  icon,
  edit,
}: {
  part: "mission" | "vision";
  title: string;
  icon: ReactNode;
  edit: PrincipleCardEdit<string>;
}) {
  const writing = edit.editor != null;
  return (
    <PrincipleCard
      part={part}
      title={title}
      icon={icon}
      actions={edit.actions}
      editing={writing}
      note={edit.note}
    >
      {writing ? (
        edit.editor
      ) : edit.shown ? (
        <StatementText>{edit.shown}</StatementText>
      ) : (
        <UnwrittenPart {...UNWRITTEN[part]} action={edit.add} />
      )}
    </PrincipleCard>
  );
}
