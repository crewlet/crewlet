/**
 * The node's own configuration file, in the words every Settings page says it
 * in.
 *
 * ONE NAME FOR ONE FILE. Settings called the file a node boots from
 * `crewlet.yaml` on Sign-in policy and File storage, "this node's bootstrap
 * file" on API tokens and "the node's file" in between, so one thing read as
 * three. A setting written there is named by what it does and where it is set;
 * the key that sets it is behind the settings table's copy control, for the
 * operator who will edit the file.
 */

/** Where a value comes from, in a settings table's source column. */
export const FROM_NODE_FILE = "Node configuration file";

/** How a value set there changes, said once on the page that shows it. */
export const NODE_FILE_CHANGE = "changed by editing that file and restarting the node";
