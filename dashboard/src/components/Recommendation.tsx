/**
 * What an ask recommends, as the words its answer is drawn beside: the
 * recommended option, or what the ask is answered with where it recommends
 * nothing.
 *
 * WHAT IT ASKS FOR, NEVER WHAT HAPPENED: an ask with no options was drawn as
 * "Answered in words", which under a question nobody has answered read as
 * though somebody had. One drawing for `line`, which says it beside its own controls, and
 * for a decision row, which says it beside the ask's key.
 */
export function Recommendation({
  options,
  recommended,
}: {
  options: readonly { id: string; label: string }[];
  recommended?: string | undefined;
}) {
  const rec = options.find((o) => o.id === recommended);
  return (
    <span className="decision-rec">
      {rec ? (
        <>
          Recommended: <strong>{rec.label}</strong>
        </>
      ) : options.length === 0 ? (
        "Asks for a written answer"
      ) : (
        "No recommendation"
      )}
    </span>
  );
}
