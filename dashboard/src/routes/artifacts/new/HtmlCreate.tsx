/**
 * A new HTML page: its own create page, in the shared frame (`CreateShell`).
 *
 * TWO WAYS IN, ONE PAGE. "Describe it" asks an agent to build the page in a
 * task; "Bring the HTML" takes the page the person writes, pastes, drops or
 * picks, checks it as they write and previews it as a private draft. The way
 * in is the address's (`mode=source`), so a link opens the one it names.
 */

import { SegmentedControl } from "@crewlethq/ui";
import { useParam } from "~/app/router.tsx";
import { SourceBody } from "./html/SourceBody.tsx";
import { DescribeBody } from "./html/DescribeBody.tsx";

export function HtmlCreate() {
  const [mode, setMode] = useParam("mode", "describe");
  const source = mode === "source";
  const modeSwitch = (
    <SegmentedControl
      label="How to make it"
      semantics="radio"
      size="sm"
      options={[
        { value: "describe", label: "Describe it" },
        { value: "source", label: "Bring the HTML" },
      ]}
      value={source ? "source" : "describe"}
      onValueChange={(v: string) => setMode(v)}
    />
  );
  return source ? <SourceBody modeSwitch={modeSwitch} /> : <DescribeBody modeSwitch={modeSwitch} />;
}
