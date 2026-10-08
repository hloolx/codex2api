package database

// The request observation time is captured BEFORE issuing GET /models.
// Preserve the last opaque inference hint when /models omits it. A hint that
// arrived during the request wins, including its outstanding invalidation.
func mergeGrokCatalogHint(next *GrokModelCatalogSnapshot, old GrokModelCatalogSnapshot) {
	if old.ETagHintObservedAt.After(next.ObservedAt) {
		next.ETagHint = old.ETagHint
		next.ETagHintObservedAt = old.ETagHintObservedAt
		if old.ETagHint != next.RequestETagHint && !old.ExpiresAt.After(old.ETagHintObservedAt) && old.ExpiresAt.Before(next.ExpiresAt) {
			next.ExpiresAt = old.ExpiresAt
		}
	} else if next.ETagHint == "" {
		next.ETagHint = old.ETagHint
		next.ETagHintObservedAt = old.ETagHintObservedAt
	}
}
