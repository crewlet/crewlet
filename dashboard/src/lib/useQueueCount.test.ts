// @vitest-environment node
import { expect, test } from "vitest";

import { queueCountParams } from "./useQueueCount.ts";
import { NO_FILTERS, buildItemsParams } from "./work.ts";

// ONE FIGURE, ONE QUESTION. The sidebar's My work figure and the Queue tab's
// are this read; the Queue's own "N in Open" line is the list's read over its
// Open segment, locked to the person. The three disagreed once — "My work 1",
// "Queue 2", five rows — and the scope they are counted over was spelled
// twice, in two files, with nothing tying the spellings together. So the count
// is held to what the Queue's list actually asks on the keys that decide which
// tasks are counted.
test("the Queue's count asks what the Queue's own Open list asks", () => {
  const list = buildItemsParams({
    container: "workspace",
    shape: "list",
    view: {},
    filters: { ...NO_FILTERS, scope: "open" },
    lock: { assignee: "ada" },
  });
  const count = queueCountParams("ada");
  for (const key of ["container", "assignee", "status_group"]) {
    expect(count[key], key).toEqual(list[key]);
  }
});
