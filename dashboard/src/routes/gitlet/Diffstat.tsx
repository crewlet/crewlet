/** A diff's lines added and removed in figures and five squares, told by shape: filled added, hollow removed. */
import { fmtCount, plural } from "~/lib/format.ts";
import "~/styles/gitlet.css";

export function Diffstat({
  added,
  deleted,
  label,
}: {
  added: number;
  deleted: number;
  /** Where the lines are, after "N lines added and M removed": "in revision 3". */
  label: string;
}) {
  const lines = added + deleted;
  const plus = lines === 0 ? 0 : Math.round((5 * added) / lines);
  return (
    <span
      className="code-diffstat"
      role="img"
      aria-label={`${plural(added, "line")} added and ${fmtCount(deleted)} removed ${label}`}
    >
      {/* A SIDE WITH NO LINES is left out beside one with some: "+118". */}
      {(added > 0 || deleted === 0) && (
        <span className="code-diffstat__added">+{fmtCount(added)}</span>
      )}
      {(deleted > 0 || added === 0) && (
        <span className="code-diffstat__removed">−{fmtCount(deleted)}</span>
      )}
      {lines > 0 && (
        <span className="code-diffstat__squares">
          {[0, 1, 2, 3, 4].map((i) => (
            <span key={i} data-kind={i < plus ? "added" : "removed"} />
          ))}
        </span>
      )}
    </span>
  );
}
