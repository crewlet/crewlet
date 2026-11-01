/**
 * The assistant's panel, as a chunk of its own (`app/lazyScreen.ts`): the
 * drawer, the conversation's store, its reducer, the cards and the stream
 * client. The entry keeps only what the frame needs before anybody opens it:
 * the Ask button, the page context registry, the bus and the one-word status.
 */

export { Drawer } from "./Drawer.tsx";
