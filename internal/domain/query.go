package domain

import (
	"fmt"
	"time"
)

// Range selects the events a rollup, a reconciliation or an export reads. A
// read is always scoped to one tenant and one bounded window: there is no
// cross-tenant read, both because every query in a pooled deployment
// carries its tenant so that a missing predicate fails closed, and because
// an unscoped scan of a ninety-day raw-event partition is not a query
// anyone wants to be able to write by accident.
//
// The window is half-open on OccurredAt — [From, To) — so that consecutive
// windows tile a timeline without double-counting the instant they meet.
// Getting that wrong is the classic double-charge: an hourly rollup with
// inclusive bounds bills the event at the hour boundary twice, and the
// error is invisible in any single hour's figures.
//
// A read is ordered by CompareUsageEvents and is not paginated in this
// increment; see Limit.
type Range struct {
	// TenantID scopes the read. Required.
	TenantID TenantID

	// MetricID narrows the read to one metric. Optional: the zero value
	// reads every metric in the window, which is what an hourly rollup
	// over all of a tenant's metrics wants.
	MetricID MetricID

	// From is the inclusive start and To the exclusive end of the window,
	// compared against each event's OccurredAt. Both are required and To
	// must be strictly after From.
	From time.Time
	To   time.Time

	// Limit caps how many events are returned, after ordering. Zero means
	// no cap.
	//
	// A Limit is not a cursor: this type carries no resume token, so a
	// caller that hits the cap cannot ask for the next page and must
	// narrow its window instead. That is a deliberate omission rather than
	// an oversight — a keyset cursor over (OccurredAt, MetricID, Source,
	// SourceRef) is the obvious shape and CompareUsageEvents already pins
	// the order it would page through, but adding it before a rollup exists
	// to use it would be designing against an imagined caller. Limit is
	// here so that a read is bounded at all.
	Limit int
}

// Validate reports whether r is a well-formed read, returning an error
// wrapping ErrInvalidRange otherwise.
//
// An empty window (To equal to From) is rejected rather than returning no
// rows. A rollup loop that computes an empty window has an off-by-one in
// its clock arithmetic, and silently returning zero events would make that
// bug look like a quiet hour.
func (r Range) Validate() error {
	if r.TenantID == "" {
		return fmt.Errorf("%w: tenant id is required", ErrInvalidRange)
	}
	if r.MetricID != "" {
		if err := validateMetricID(r.MetricID); err != nil {
			return fmt.Errorf("%w: metric filter: %w", ErrInvalidRange, err)
		}
	}
	if r.From.IsZero() || r.To.IsZero() {
		return fmt.Errorf("%w: both From and To are required", ErrInvalidRange)
	}
	if !r.From.Before(r.To) {
		return fmt.Errorf("%w: From must be strictly before To", ErrInvalidRange)
	}
	if r.Limit < 0 {
		return fmt.Errorf("%w: limit %d is negative", ErrInvalidRange, r.Limit)
	}
	return nil
}

// Matches reports whether e falls inside r: same tenant, the metric filter
// if one is set, and OccurredAt within the half-open window.
//
// The predicate lives here rather than in each adapter so that a Postgres
// WHERE clause and the in-memory adapter's filter cannot drift apart on the
// boundaries — the one place where a disagreement between two adapters
// would be a billing discrepancy rather than a test failure. An adapter
// that pushes the filter into SQL for efficiency must implement exactly
// this predicate, and the conformance suite pins it.
func (r Range) Matches(e UsageEvent) bool {
	if e.TenantID != r.TenantID {
		return false
	}
	if r.MetricID != "" && e.MetricID != r.MetricID {
		return false
	}
	if e.OccurredAt.Before(r.From) {
		return false
	}
	return e.OccurredAt.Before(r.To)
}
