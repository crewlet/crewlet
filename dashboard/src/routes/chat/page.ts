/**
 * How much of a room one read asks for.
 *
 * FIFTY — `chat.DefaultLimit`, which the engine clamps to anyway. It is a
 * screen and a bit at this density, so opening a room is one round trip rather
 * than two, and paging back is a deliberate gesture rather than something that
 * happens while somebody scrolls. The engine's ceiling is five hundred and is
 * for an export; asking for it here would make every room open by reading ten
 * screens nobody will look at.
 *
 * ONE CONSTANT for the transcript, the thread and the mention feed, because
 * they are the same question — how much conversation is a page — and three
 * numbers would be three answers that drift.
 */
export const PAGE = 50;
