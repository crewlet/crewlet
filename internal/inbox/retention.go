package inbox

// RetentionKey is the epoch key a person's inbox horizon reaches every
// domain's applier under: the company's `inbox.retention_days`, in whole
// days, with its default applied.
//
// ONE KEY FOR EVERY DOMAIN THAT KEEPS AN INBOX, because the horizon is the
// company's and not any one domain's: an applier declines to write a notice
// older than it, and a node's sweep deletes the notices that have aged past
// it, and the two must hold ONE horizon or an inbox grows back on every
// replay by the width of their disagreement. A key per domain would be a
// horizon per domain, and one person's inbox aging out on as many clocks as
// it has sources.
const RetentionKey = "inbox.retention_days"
