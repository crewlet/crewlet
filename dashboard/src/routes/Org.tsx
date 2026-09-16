/**
 * The company as a structure.
 *
 * THE SHELL OF A MULTI-LENS SCREEN: the header, the lens control and one
 * index, with each lens in its own module under `routes/org/`. The lens is in
 * the URL as a section, so a chart someone is looking at is a link they can
 * send and Back walks out through the lenses the reader opened.
 *
 * - Chart (`routes/org/Chart.tsx`): the units nested as they are, with the
 *   seats in each. `unit=` and `seat=` select, and a link carrying one reveals
 *   it on arrival.
 * - Directory (`routes/org/Directory.tsx`): every seat as a sortable row.
 * - Charter (`routes/org/Charter.tsx`): mission, vision, policies and unit
 *   goals.
 * - Builder (`routes/org/builder/Builder.tsx`): editing the organization, and
 *   creating the company where none exists. Operator-gated: it reads and
 *   writes the guarded configuration, and says what it needs when refused.
 *
 * ONE INDEX FOR THE THREE READ LENSES, built from the anonymous org
 * projection: who the seats are as the document writes them, and the
 * hierarchy the engine derived for them. No lens computes a reporting line, a
 * lead or a placement of its own. The Builder edits the configuration
 * document instead, and asks the engine for the hierarchy of its draft. A
 * lens this build does not know shows the chart rather than an empty page
 * under a header.
 */

import { useId, useMemo } from "react";
import { useParam } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { Charter } from "./org/Charter.tsx";
import { Chart } from "./org/Chart.tsx";
import { Directory } from "./org/Directory.tsx";
import { PreviousRevisionNote } from "./org/builder/AfterSaveStrip.tsx";
import { Builder } from "./org/builder/Builder.tsx";
import { builderSurfaces } from "./org/builder/surfaces.ts";
import { AccountTreeGlyph, EditGlyph, FlagGlyph, GroupGlyph } from "@crewlethq/icons/glyphs";
import { PageHeader, SegmentedControl, TabPanel, Tag } from "@crewlethq/ui";

type Lens = "chart" | "directory" | "charter" | "builder";

const LENSES: readonly Lens[] = ["chart", "directory", "charter", "builder"];

export function OrgScreen() {
  const org = useOrg();
  const [param, setLens] = useParam("lens", "chart", "section");
  const lens: Lens = (LENSES as readonly string[]).includes(param) ? (param as Lens) : "chart";
  const index = useMemo(() => indexOrg(org), [org]);
  const panel = useId();

  return (
    <>
      <PageHeader
        title={org?.name ? `${org.name} org chart` : "Org chart"}
        description="The hierarchy is the execution graph: knowledge, delegation and routing all follow it."
        /* HOW MANY SEATS THE COMPANY HAS, which the directory's own count line
           used to carry under its rows. It is a fact about the organization
           rather than about one lens, so it is said once here. Not on the
           builder: that lens draws a DRAFT, and this index is the revision
           this node has applied, so the two numbers differ exactly while
           somebody is editing. */
        {...(lens === "builder"
          ? {}
          : { badges: <Tag appearance="outline">{plural(index.seats.length, "seat")}</Tag> })}
        actions={
          <SegmentedControl<Lens>
            label="Org view"
            semantics="tabs"
            panelId={panel}
            value={lens}
            onValueChange={setLens}
            options={[
              { value: "chart", label: "Chart", icon: <AccountTreeGlyph /> },
              { value: "directory", label: "Directory", icon: <GroupGlyph /> },
              { value: "charter", label: "Charter", icon: <FlagGlyph /> },
              { value: "builder", label: "Builder", icon: <EditGlyph /> },
            ]}
          />
        }
      />
      {/* A save is not an apply: until this node applies the revision the
          builder saved, the projection every read lens draws is the previous
          one, and the chart that has not moved would read as a save that did
          nothing. */}
      {lens !== "builder" && <PreviousRevisionNote />}
      {/* NAMED BY THE SCREEN, because one lens has a layout of its own: the
          builder's chart fills the window, and the stylesheet that says so
          needs a handle on the panel between the shell and the builder. */}
      <TabPanel id={panel} value={lens} className="org-lens">
        {lens === "chart" && <Chart index={index} />}
        {lens === "directory" && <Directory index={index} />}
        {lens === "charter" && <Charter org={org ?? {}} index={index} />}
        {lens === "builder" && <Builder surfaces={builderSurfaces} />}
      </TabPanel>
    </>
  );
}
