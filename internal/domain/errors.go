package domain

import "errors"

// This file declares the validation sentinels for the domain package (see
// the package doc comment in usage.go). All three are precondition errors —
// "is this input well formed enough to record at all" — asked before
// anything is written. The ledger's own operational sentinels (no
// transaction, a foreign transaction, a finished transaction, an
// idempotency conflict) belong to the port and are declared in
// meter/ledger, because they describe what happened to an append rather
// than what was wrong with an event.
//
// There is deliberately no not-found sentinel. A range read over a window
// with no events is an empty result, not an error: a tenant that used
// nothing in an hour used nothing, and a rollup must be able to record that
// without branching.

// ErrInvalidEvent is returned by UsageEvent.Normalize and
// Dimensions.Validate when an event cannot be recorded as written: a
// missing tenant, metric id, unit, source or source ref; an unspecified or
// unknown metric shape; a zero OccurredAt; a malformed metric id, unit,
// dimension key or dimension value.
//
// Every adapter returns this, wrapped, from its append path rather than
// writing a row it cannot interpret later. The error names the offending
// field, and for a dimension the offending key — never a dimension value.
var ErrInvalidEvent = errors.New("meter: invalid usage event")

// ErrNegativeQuantity is returned by UsageEvent.Normalize when a negative
// Quantity is recorded against a metric shape that cannot mean anything by
// it — in practice, any ShapeEvent metric.
//
// It is a separate sentinel rather than another ErrInvalidEvent because it
// is the one validation failure a caller is likely to want to branch on: a
// sign error in a counter is a code bug worth alerting on, while a missing
// field is a wiring bug that fails on the first call in development. Only
// ShapeGaugeDelta accepts a negative quantity, and for it a negative
// quantity is the normal case rather than an exception.
var ErrNegativeQuantity = errors.New("meter: negative quantity for a non-delta metric")

// ErrInvalidRange is returned by Range.Validate when a read cannot be
// executed as written: a missing tenant id, a missing or inverted window, an
// empty window, a negative limit, or a malformed metric filter.
var ErrInvalidRange = errors.New("meter: invalid range")
