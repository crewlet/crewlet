/**
 * The legend: every key the dashboard answers, drawn from the one table.
 *
 * IT READS `KEYMAP` AND NOTHING ELSE, so it cannot fall behind: a row added
 * to the table is a row here, a row deleted is gone, and there is no second
 * list for a reviewer to remember. `keymap.test.ts` renders it and holds it to
 * every row.
 *
 * A KEY IS DRAWN AS THE KIT'S `Kbd`, in the platform's own notation — `⌘K` on
 * a Mac, `Ctrl+K` elsewhere — and read as words by a screen reader, for the
 * reason `dashboard-design.md` gives for every hint in the product.
 */

import { Fragment } from "react";
import { Kbd, Modal, Text } from "@crewlethq/ui";
import { KEY_SCOPES, KEYMAP, capsOf, keyShown, type KeyRow } from "./keymap.ts";
import type { ServicesRun } from "~/lib/services.ts";

/** A row's presses: each a cap, a sequence as two, a run of digits as a range. */
function Presses({ row }: { row: KeyRow }) {
  // A RUN IS DRAWN AS ITS ENDS. Nine caps for "go to that tab" is a row
  // nobody reads to the end; "1 to 9" is the same fact.
  if (row.presses.length > 2) {
    const first = row.presses[0]!;
    const last = row.presses[row.presses.length - 1]!;
    return (
      <>
        <Kbd keys={capsOf(first)} /> <span className="muted">to</span> <Kbd keys={capsOf(last)} />
      </>
    );
  }
  return (
    <>
      {row.presses.map((press, i) => (
        <Fragment key={i}>
          {i > 0 && <span className="muted"> or </span>}
          {press.after && (
            <>
              <Kbd keys={[press.after]} /> <span className="muted">then</span>{" "}
            </>
          )}
          <Kbd keys={capsOf(press)} />
        </Fragment>
      ))}
    </>
  );
}

export function KeyLegend({
  runs,
  onClose,
}: {
  /** Which services the company runs: a key to a place it does not run is left out. */
  runs: ServicesRun;
  onClose: () => void;
}) {
  return (
    <Modal open title="Keyboard shortcuts" size="lg" onClose={onClose}>
      <div className="key-legend">
        <div className="key-legend-groups">
          {KEY_SCOPES.map(({ scope, title }) => {
            const rows = KEYMAP.filter((row) => row.scope === scope && keyShown(row, runs));
            if (rows.length === 0) return null;
            return (
              <section key={scope} className="key-legend-group" aria-labelledby={`keys-${scope}`}>
                <Text as="h3" variant="label" id={`keys-${scope}`}>
                  {title}
                </Text>
                <dl>
                  {rows.map((row) => (
                    <div key={row.id} className="key-legend-row" data-key-row={row.id}>
                      <dt>{row.does}</dt>
                      <dd className="key-legend-keys">
                        <Presses row={row} />
                      </dd>
                    </div>
                  ))}
                </dl>
              </section>
            );
          })}
        </div>
      </div>
    </Modal>
  );
}
