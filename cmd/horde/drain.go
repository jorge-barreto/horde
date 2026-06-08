package main

import (
	"sort"

	"github.com/jorge-barreto/horde/internal/store"
)

// sortByDrainOrder orders runs the way the drain claims them: highest priority
// first, then oldest enqueued_at. Used by `horde queue list` for an accurate
// preview of what launches next, and by the lazy drain's peek.
func sortByDrainOrder(runs []*store.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		oi, oj := runs[i].Priority.Ordinal(), runs[j].Priority.Ordinal()
		if oi != oj {
			return oi > oj
		}
		return runs[i].EnqueuedAt.Before(runs[j].EnqueuedAt)
	})
}
