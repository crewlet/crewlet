/**
 * A `decisions` answer as the engine states it, from a fixture written as a
 * list of what is PUT TO the reader: every item and its run are the reader's
 * own (`put_to_you`, `may_answer`, `needs_you`), where the fixture did not say
 * otherwise, and nothing is answerable for somebody else unless it says so.
 * A fixture that states any of them is passed through untouched.
 */
export function asReaders(answer: unknown): unknown {
  if (
    typeof answer !== "object" ||
    answer === null ||
    !Array.isArray((answer as { items?: unknown }).items)
  ) {
    return answer;
  }
  const a = answer as { items: Record<string, unknown>[]; answerable?: number };
  const items = a.items.map((item) => {
    if ("needs_you" in item) return item;
    const run = item.run as Record<string, unknown> | undefined;
    const facts = { put_to_you: true, may_answer: true, needs_you: true };
    return {
      ...item,
      ...facts,
      ...(run && !("needs_you" in run)
        ? { run: { ...run, put_to: run.audience_handles ?? [], ...facts } }
        : {}),
    };
  });
  return { answerable: 0, ...a, items };
}
