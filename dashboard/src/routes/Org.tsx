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
import { ScreenHead } from "~/app/Shell.tsx";
import { useParam } from "~/app/router.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { Segmented, TabPanel } from "~/ui/primitives.tsx";
import { Charter } from "./org/Charter.tsx";
import { Chart } from "./org/Chart.tsx";
import { Directory } from "./org/Directory.tsx";
import { PreviousRevisionNote } from "./org/builder/AfterSaveStrip.tsx";
import { Builder } from "./org/builder/Builder.tsx";
import { builderSurfaces } from "./org/builder/surfaces.ts";

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
      <ScreenHead
        title={org?.name ? `${org.name} org chart` : "Org chart"}
        sub="The hierarchy is the execution graph: knowledge, delegation and routing all follow it."
        actions={
          <Segmented<Lens>
            ariaLabel="Org view"
            semantics="tabs"
            panelId={panel}
            value={lens}
            onChange={setLens}
            options={[
              { value: "chart", label: "Chart", icon: "sitemap" },
              { value: "directory", label: "Directory", icon: "users" },
              { value: "charter", label: "Charter", icon: "flag" },
              { value: "builder", label: "Builder", icon: "pencil" },
            ]}
          />
        }
      />
      {/* A save is not an apply: until this node applies the revision the
          builder saved, the projection every read lens draws is the previous
          one, and the chart that has not moved would read as a save that did
          nothing. */}
      {lens !== "builder" && <PreviousRevisionNote />}
      <TabPanel id={panel} value={lens}>
        {lens === "chart" && <Chart index={index} />}
        {lens === "directory" && <Directory index={index} />}
        {lens === "charter" && <Charter org={org ?? {}} index={index} />}
        {lens === "builder" && <Builder surfaces={builderSurfaces} />}
      </TabPanel>
    </>
  );
}
