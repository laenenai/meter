// Package domain is the single home for every domain entity and value
// object in meter: the usage event, the two shapes a metric can take, the
// attribution dimensions below the tenant, and the range a rollup reads
// with (this file, dimensions.go, query.go, errors.go). The root package
// meter re-exports the types a consumer must name as type aliases, so this
// package stays the only one that defines them.
//
// The invariant this package holds is that a usage event is a *physical*
// measurement and nothing more: a tenant, a metric id from the consumer's
// own catalog, a quantity in a stable physical unit, when it happened, who
// below the tenant to attribute it to, and the key that makes re-emission
// a no-op. There is no price, no plan, no currency, no tier of service and
// no quota anywhere in this package, and there must never be — those are
// the charter's explicit non-contents, and they are downstream of the
// ledger rather than inside it. The reason metering is built this way from
// the first commit is that a physical measurement that was never recorded
// cannot be reconstructed afterwards, while a price can always be applied
// to a recorded measurement later.
//
// Two decisions in this file are worth reading before using it, because
// both are irreversible once a ledger has rows in it:
//
//   - Quantity is an integer, never a float. A ledger that sums floating
//     point quantities produces a different total depending on the order
//     the rows come back in, which is a billing dispute waiting to happen.
//     The way to record a fractional measurement is therefore to choose a
//     finer unit (milliseconds, not seconds; bytes, not megabytes), which
//     is exactly what "stable physical units" in the charter buys. See
//     Quantity.
//   - A metric's Unit is fixed by its entry in the consumer's metric
//     catalog and must never change for a published metric id, because a
//     rollup sums across rows written at different times and cannot know
//     that row 1 meant seconds and row 2 meant milliseconds. This package
//     validates that a Unit is well formed; it deliberately does not
//     enforce stability, because doing so would require a read before
//     every append. See Unit.
package domain

import (
	"fmt"
	"strings"
	"time"
)

// TenantID identifies the billing entity a usage event is charged to. It is
// the only dimension the ledger treats as mandatory: everything below it
// (fileset, document, principal, agent run, job, capability chain) is an
// attribution dimension, recorded so an operator can answer "what did this
// matter cost" and a tenant can charge back internally, but never required
// in order to bill.
//
// It is an opaque string rather than a parsed id because meter has three
// consumers with three id schemes and no business knowing any of them; the
// tenant registry that does know lives in the tenancy module.
type TenantID string

// MetricID names one entry in the consumer's metric catalog — "the metric
// id is a versioned, documented catalog: stable forever, added to, never
// repurposed". meter does not own the catalog (it would have to own all
// three consumers' catalogs), so it validates only the spelling rule that
// keeps ids comparable across a SQL index, a rollup key and an export
// payload: lowercase ASCII letters, digits, '.', '_', ':' and '-'.
//
// The convention the consumers follow is dotted and namespaced by the
// emitting subsystem, e.g. "cas.blob.bytes_stored", "cas.job.wall_time",
// "kgs.query.vector_searches".
type MetricID string

// Unit is the physical unit a Quantity is expressed in. It is a validated
// string rather than a closed enum for the same reason blob.Key.Tier is:
// consumers add metrics this module has never heard of, and a closed enum
// would make every new metric a change to this module.
//
// The constants below are the canonical spellings; a consumer that needs a
// unit not listed here should add it to its own catalog using the same
// style (lowercase, singular, SI-prefixed only where the prefix is part of
// the physical unit). What must not happen is two spellings of one unit —
// "ms" and "millisecond" — because a rollup groups by unit.
type Unit string

// The canonical units. Fractions are expressed by choosing a finer unit,
// never by a fractional Quantity (see Quantity).
const (
	// UnitByte counts bytes: stored, ingested, egressed.
	UnitByte Unit = "byte"
	// UnitCount counts occurrences: requests, jobs, deliveries, mints.
	UnitCount Unit = "count"
	// UnitMillisecond measures elapsed time, e.g. processor wall time.
	// Milliseconds rather than seconds so that sub-second work is an
	// integer quantity rather than a rounding decision.
	UnitMillisecond Unit = "millisecond"
	// UnitToken counts model tokens, as reported by the vendor.
	UnitToken Unit = "token"
	// UnitPage counts pages, e.g. OCR pages.
	UnitPage Unit = "page"
)

// Quantity is how much of Unit the event measures, as an exact integer.
//
// It is signed because a gauge metric records signed deltas (see
// ShapeGaugeDelta): "-4096 bytes in hot" is the only way an append-only
// ledger can express a deletion. ShapeEvent rejects a negative quantity —
// see UsageEvent.Normalize and ErrNegativeQuantity.
//
// It is an integer, not a float, because a ledger exists to be summed and
// floating point addition is not associative: two rollups over the same
// rows in different orders would disagree, and the disagreement would be
// invisible until a customer queried it. A measurement that is fractional
// in its natural unit is recorded in a finer unit instead.
type Quantity int64

// MetricShape is one of the two shapes a metric can have. The distinction
// is semantic, not a storage detail: it determines whether a negative
// quantity is meaningful and how a rollup must combine rows, and it cannot
// be recovered from the rows afterwards, so it is recorded on every event.
//
// Deliberately *not* modelled here: whether an adapter stores a metric as
// one row per event or compacts it into a per-(tenant, metric, dimensions,
// minute) bucket. That is a storage optimisation an adapter chooses to keep
// a high-traffic counter metric from dominating the database, and it must
// be invisible through the Ledger port: a compacting adapter still returns
// the same sums and must still pass the conformance suite unchanged. Making
// compaction a domain concept would push a storage decision into every
// consumer's metric catalog, where it could never be revised.
type MetricShape uint8

const (
	// ShapeUnspecified is the zero value and is never valid. It exists so
	// that an event with an unset Shape is rejected rather than silently
	// treated as the first real shape — the shape cannot be inferred from
	// the quantity, and guessing it wrong is a billing error.
	ShapeUnspecified MetricShape = iota

	// ShapeEvent is a metric counted as it happens: API requests by kind,
	// bytes ingested, bytes egressed, signed links minted and redeemed,
	// jobs by processor and outcome, processor wall time, OCR pages,
	// embedding tokens, vector and lexical queries, webhook deliveries and
	// retries, cold-tier restores, erasures.
	//
	// A rollup over an event metric is a plain sum over the window, and the
	// sum is monotonically non-decreasing as the window widens, so a
	// negative quantity is always a bug and is rejected.
	ShapeEvent

	// ShapeGaugeDelta is a metric whose rows are signed deltas to a level
	// rather than occurrences: stored bytes per tier, document count, chunk
	// and vector count. Every blob create, delete and tier transition emits
	// one ("+n bytes in warm", "-n bytes in hot").
	//
	// The level is not sampled periodically, it is the cumulative sum of
	// the deltas; and a byte-hours figure is the integral over the delta
	// ledger, computed exactly for any historical window rather than
	// approximated by whatever the sampler happened to catch. That is the
	// whole reason this shape exists, and it is why a negative quantity is
	// valid here and only here.
	ShapeGaugeDelta
)

// Valid reports whether s is one of the two real shapes.
func (s MetricShape) Valid() bool {
	return s == ShapeEvent || s == ShapeGaugeDelta
}

// AllowsNegative reports whether a negative Quantity is meaningful for this
// shape. Only ShapeGaugeDelta permits one.
func (s MetricShape) AllowsNegative() bool { return s == ShapeGaugeDelta }

// String returns the shape's stable wire spelling: "event" or
// "gauge_delta". These strings are part of the module's contract — an
// adapter persists them and an export carries them — so they must not be
// changed once a ledger holds rows.
func (s MetricShape) String() string {
	switch s {
	case ShapeUnspecified:
		return "unspecified"
	case ShapeEvent:
		return "event"
	case ShapeGaugeDelta:
		return "gauge_delta"
	default:
		return fmt.Sprintf("invalid(%d)", uint8(s))
	}
}

// IdempotencyKey is the tuple that makes emission idempotent: appending an
// event whose key is already in the ledger counts once, not twice. It is
// comparable, so it is usable directly as a Go map key and maps one-to-one
// onto a SQL unique constraint.
//
// The composition, and the reasoning for each part:
//
//   - TenantID. A SourceRef is unique within its Source, but two tenants'
//     sources are two different systems in a pooled deployment; including
//     the tenant means a SourceRef collision can never move usage between
//     billing entities, and it keeps the unique index tenant-local, which
//     is what a tenant-partitioned table and a row-level-security
//     predicate both want.
//   - MetricID. One state change legitimately emits several metrics — a
//     finished job reports wall time, embedding tokens and OCR pages from
//     the same row — so without the metric in the key the second and third
//     metrics of a job would be swallowed as duplicates of the first.
//   - Source: the emitting code path, e.g. "cas.blob.store". Namespacing
//     by emitter means two subsystems cannot collide by both choosing
//     "job:42", and it makes an unexpected duplicate traceable to the code
//     that produced it.
//   - SourceRef: the reference that identifies the metered fact within
//     that source. It must be derived from the state change being metered
//     (the slot version id, the job id, the request id) and never from the
//     clock or a fresh random value, because a retry must recompute the
//     same ref or the key cannot deduplicate anything.
//
// Deliberately *not* in the key: OccurredAt, Quantity and Dimensions.
// Excluding OccurredAt is what lets a retry that recomputes its timestamp
// still deduplicate. Excluding Quantity is the load-bearing one: if the
// quantity were in the key, a bug that doubled a charge would write two
// rows that both look legitimate, whereas excluding it turns the same bug
// into a detectable collision (see UsageEvent.SameBillableContent).
type IdempotencyKey struct {
	TenantID  TenantID
	MetricID  MetricID
	Source    string
	SourceRef string
}

// String returns a human-readable rendering for error messages and logs.
// It names the key's four components only: a key never contains a
// quantity, a timestamp or a dimension value, so it is always safe to log.
func (k IdempotencyKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.TenantID, k.MetricID, k.Source, k.SourceRef)
}

// UsageEvent is one physical measurement, appended to the ledger inside the
// transaction that performed the work it measures. Events are never edited
// and never deleted: a correction is a new event, which for ShapeGaugeDelta
// is a compensating delta and for ShapeEvent is a separately-identified
// adjustment event.
//
// Zero fields are never meaningful here. Build an event with every field
// set and pass it through Normalize, which both validates it and returns
// the canonical form an adapter stores.
type UsageEvent struct {
	// TenantID is the billing entity. Required.
	TenantID TenantID

	// MetricID names the catalog entry being measured. Required.
	MetricID MetricID

	// Shape says whether this metric is counted as it happens or is a
	// signed delta to a level. Required; ShapeUnspecified is rejected.
	Shape MetricShape

	// Unit is the physical unit Quantity is expressed in. Required, and
	// fixed for the lifetime of MetricID.
	Unit Unit

	// Quantity is the measurement, as an exact integer count of Unit. May
	// be negative only when Shape.AllowsNegative.
	Quantity Quantity

	// OccurredAt is when the metered work happened, which is not when the
	// row was written: a job that ran at 23:59 and committed at 00:01
	// belongs to the earlier hour's rollup. Required.
	//
	// Normalize converts it to UTC and truncates it to microsecond
	// precision, because that is the finest precision a Postgres
	// timestamptz can hold; without the truncation an event would not
	// round-trip equal through a Postgres adapter while round-tripping
	// equal through an in-memory one, and the conformance suite would pass
	// for the wrong reason.
	OccurredAt time.Time

	// Source is the emitting code path, and SourceRef the reference to the
	// state change being metered. Together with TenantID and MetricID they
	// form the IdempotencyKey; both are required.
	Source    string
	SourceRef string

	// Dimensions are the attribution dimensions below the tenant, plus the
	// descriptive dimensions a rollup groups by (tier, lane, processor,
	// model, region). Optional, and nil when absent.
	Dimensions Dimensions
}

// IdempotencyKey returns the tuple that identifies this event for
// deduplication. See IdempotencyKey for the composition and why each part
// is in or out.
func (e UsageEvent) IdempotencyKey() IdempotencyKey {
	return IdempotencyKey{
		TenantID:  e.TenantID,
		MetricID:  e.MetricID,
		Source:    e.Source,
		SourceRef: e.SourceRef,
	}
}

// SameBillableContent reports whether e and other measure the same thing to
// the same amount — that is, whether they agree on Shape, Unit and
// Quantity.
//
// This is the comparison an adapter makes when an IdempotencyKey recurs.
// Agreement means the append is a retry of work already recorded and must
// be a silent no-op. Disagreement means two different measurements were
// emitted under one key, which is a caller bug that no downstream rollup or
// reconciliation could ever detect, so an adapter must surface it rather
// than pick a winner.
//
// OccurredAt and Dimensions are deliberately not compared: a retry may
// recompute its timestamp, and a retry from a different code path may carry
// a differently-populated dimension set, yet neither changes what is being
// charged. The first write's values stand.
func (e UsageEvent) SameBillableContent(other UsageEvent) bool {
	return e.Shape == other.Shape && e.Unit == other.Unit && e.Quantity == other.Quantity
}

// Normalize validates e and returns its canonical form: OccurredAt in UTC
// truncated to microsecond precision, and Dimensions cloned so the returned
// event shares no map with the caller, with an empty set reduced to nil so
// that an absent and an empty dimension set compare equal.
//
// It returns ErrNegativeQuantity when the quantity's sign is not allowed by
// the shape, and ErrInvalidEvent for every other malformed field. The
// returned error names the offending field, and for a dimension the
// offending *key* — never a dimension value, which may be a document id, a
// principal id or a matter reference (see dimensions.go).
//
// Normalize is idempotent: normalizing an already-normalized event returns
// an identical value. Every Ledger implementation calls it on the way in,
// so a caller never has to; calling it beforehand is how a test builds the
// value it expects to read back.
func (e UsageEvent) Normalize() (UsageEvent, error) {
	if e.TenantID == "" {
		return UsageEvent{}, fmt.Errorf("%w: tenant id is required", ErrInvalidEvent)
	}
	if err := validateMetricID(e.MetricID); err != nil {
		return UsageEvent{}, err
	}
	if !e.Shape.Valid() {
		return UsageEvent{}, fmt.Errorf("%w: metric shape %s", ErrInvalidEvent, e.Shape)
	}
	if err := validateUnit(e.Unit); err != nil {
		return UsageEvent{}, err
	}
	if e.Quantity < 0 && !e.Shape.AllowsNegative() {
		return UsageEvent{}, fmt.Errorf("%w: quantity %d on %s metric %q",
			ErrNegativeQuantity, e.Quantity, e.Shape, e.MetricID)
	}
	if e.OccurredAt.IsZero() {
		return UsageEvent{}, fmt.Errorf("%w: occurred-at is required", ErrInvalidEvent)
	}
	if err := validateRef("source", e.Source); err != nil {
		return UsageEvent{}, err
	}
	if err := validateRef("source ref", e.SourceRef); err != nil {
		return UsageEvent{}, err
	}
	if err := e.Dimensions.Validate(); err != nil {
		return UsageEvent{}, err
	}

	e.OccurredAt = e.OccurredAt.UTC().Truncate(time.Microsecond)
	if len(e.Dimensions) == 0 {
		e.Dimensions = nil
	} else {
		e.Dimensions = e.Dimensions.Clone()
	}
	return e, nil
}

// CompareUsageEvents orders two events the way every Ledger must return a
// range: ascending by OccurredAt, then by MetricID, Source and SourceRef.
//
// The tiebreakers are not cosmetic. Within one tenant the four of them
// together are a total order, because (tenant, metric, source, source ref)
// is the ledger's uniqueness constraint, so two distinct events can never
// compare equal and no adapter is free to return a different sequence. A
// rollup, a reconciliation and an export therefore see the same sequence
// from a Postgres adapter and from the in-memory one, which is what makes
// the conformance suite able to assert on order at all.
func CompareUsageEvents(a, b UsageEvent) int {
	if !a.OccurredAt.Equal(b.OccurredAt) {
		if a.OccurredAt.Before(b.OccurredAt) {
			return -1
		}
		return 1
	}
	if c := strings.Compare(string(a.MetricID), string(b.MetricID)); c != 0 {
		return c
	}
	if c := strings.Compare(a.Source, b.Source); c != 0 {
		return c
	}
	return strings.Compare(a.SourceRef, b.SourceRef)
}

// maxMetricIDLen and maxRefLen bound the fields that end up in the ledger's
// uniqueness constraint. They are generous relative to any real id and
// exist to reject a deliberately-oversized value at this boundary rather
// than let it reach an index whose key size is limited.
const (
	maxMetricIDLen = 128
	maxUnitLen     = 32
	maxRefLen      = 256
)

// validateMetricID enforces the catalog's spelling rule. Rejecting rather
// than lower-casing is deliberate: a consumer that writes "CAS.Bytes" in
// one place and "cas.bytes" in another has two catalog entries in its head
// and needs to find that out now, not after a rollup has split one metric
// into two.
func validateMetricID(id MetricID) error {
	if id == "" {
		return fmt.Errorf("%w: metric id is required", ErrInvalidEvent)
	}
	if len(id) > maxMetricIDLen {
		return fmt.Errorf("%w: metric id length %d exceeds %d", ErrInvalidEvent, len(id), maxMetricIDLen)
	}
	for _, r := range string(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == ':', r == '-':
		default:
			return fmt.Errorf("%w: metric id %q contains %q, want lowercase ASCII, digits, '.', '_', ':' or '-'",
				ErrInvalidEvent, id, r)
		}
	}
	return nil
}

// validateUnit enforces the canonical spelling style for a unit. The set is
// open (see Unit) but the charset is not, so "byte" and "Byte" cannot
// become two units that a rollup groups separately.
func validateUnit(u Unit) error {
	if u == "" {
		return fmt.Errorf("%w: unit is required", ErrInvalidEvent)
	}
	if len(u) > maxUnitLen {
		return fmt.Errorf("%w: unit length %d exceeds %d", ErrInvalidEvent, len(u), maxUnitLen)
	}
	for _, r := range string(u) {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		default:
			return fmt.Errorf("%w: unit %q contains %q, want lowercase ASCII or '_'", ErrInvalidEvent, u, r)
		}
	}
	return nil
}

// validateRef checks a Source or SourceRef: present, bounded, and free of
// control characters. The value is echoed in the error because both are
// code-path identifiers chosen by the emitting subsystem, not user content
// — unlike a dimension value, which is never echoed.
func validateRef(label, s string) error {
	if s == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidEvent, label)
	}
	if len(s) > maxRefLen {
		return fmt.Errorf("%w: %s length %d exceeds %d", ErrInvalidEvent, label, len(s), maxRefLen)
	}
	if i := strings.IndexFunc(s, isControl); i >= 0 {
		return fmt.Errorf("%w: %s contains a control character at byte %d", ErrInvalidEvent, label, i)
	}
	return nil
}

// isControl reports whether r is a C0 or C1 control character or DEL. These
// are rejected everywhere in an event because they survive into a log line,
// a CSV export and a terminal, where they are at best unreadable and at
// worst an injection.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}
