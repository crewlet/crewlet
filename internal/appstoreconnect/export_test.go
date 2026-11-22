package appstoreconnect

import "time"

// SetApplyBound cuts p's acts under their lease at d, so a test meets the
// bound without waiting out growth.ApplyBound.
func SetApplyBound(p *Product, d time.Duration) { p.applyBound = d }
