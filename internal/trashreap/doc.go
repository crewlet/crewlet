// Package trashreap is the trash reaper: the fleet duty that purges what has
// been trashed past its term, as the system, for every domain that keeps a
// trash.
//
// A trashed page was documented as recoverable for thirty days and then kept
// for ever, because nothing purged it. The reaper keeps that promise for pages
// and, through the same duty, for artifacts: each source answers what it holds
// trashed before a cutoff and purges one item under an operation id the reaper
// derives, so a moved duty or a retried pass never purges anything twice.
package trashreap
