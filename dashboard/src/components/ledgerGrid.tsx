/**
 * A ledger's columns: when, what changed, who, and the turn or the one action a
 * row carries. Worklet's, Doclet's and Artifacts' histories record the same
 * facts about their own objects, so they read the same way.
 *
 * What changed is the one cell of prose, so it wraps, and on a phone it leads
 * the row with the facts on the line under it. PLAIN HEADS: a ledger is the
 * engine's walk, newest first, and a press could only order what is loaded
 * while reading as an order over the whole record.
 */

import type { ReactNode } from "react";
import { TableDateCell, type DataGridColumn } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";

export interface LedgerFacts<T> {
  /** When it happened, as an instant. */
  at: (row: T) => string;
  /** Which object a row is about, in a ledger over many of them. */
  item?: ((row: T) => ReactNode) | undefined;
  what: (row: T) => ReactNode;
  /** The whole of what changed on hover, where `what` is its short form. */
  whatTitle?: ((row: T) => string | undefined) | undefined;
  who: (row: T) => ReactNode;
  /** Who sized to its names and giving way, on a screen that has the room for them. */
  whoFits?: boolean | undefined;
  /** The turn that made the change, where a row has one. */
  turn?: ((row: T) => string | undefined) | undefined;
  /** The one thing a row can do. */
  action?: ((row: T) => ReactNode) | undefined;
}

export function ledgerColumns<T>(facts: LedgerFacts<T>, now: number): DataGridColumn<T>[] {
  const { at, item, what, whatTitle, who, whoFits, turn, action } = facts;
  return [
    {
      key: "when",
      header: "When",
      align: "right",
      shrink: true,
      cell: (row) => <TableDateCell value={at(row)} now={now} />,
    },
    ...(item ? [{ key: "item", header: "Item", width: "max-content", cell: item }] : []),
    {
      key: "what",
      header: "What changed",
      floor: "12rem",
      overflow: "wrap",
      phoneLead: true,
      title: whatTitle,
      cell: what,
    },
    // A WIDTH RATHER THAN CONTENT: in a side panel the row cannot hold it beside
    // the sentence, so it becomes a card (the sentence, then when and who) with
    // the name whole, where a content track cut it to a letter or two.
    whoFits
      ? { key: "who", header: "Who", shrink: true, drop: 2, cell: who }
      : { key: "who", header: "Who", width: "9rem", cell: who },
    ...(turn
      ? [
          {
            key: "turn",
            header: "Turn",
            shrink: true,
            drop: 1,
            phoneOmit: true,
            cell: (row: T) => {
              const id = turn(row);
              return id ? (
                <a className="t-link" href={href(["live", "turns", id])}>
                  Open
                </a>
              ) : null;
            },
          },
        ]
      : []),
    ...(action ? [{ key: "action", header: "", action: true, shrink: true, cell: action }] : []),
  ];
}
