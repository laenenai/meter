package domain_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/laenenai/meter/internal/domain"
)

// occurred is a fixed, deliberately non-UTC instant with sub-microsecond
// precision, so every test that round-trips it also exercises
// Normalize's two documented normalisations at once.
var occurred = time.Date(2026, 10, 23, 14, 5, 6, 123456789, time.FixedZone("CEST", 2*60*60))

// validEvent returns a fully populated, valid event. Tests mutate one
// field of a copy rather than restating nine fields each time.
func validEvent() domain.UsageEvent {
	return domain.UsageEvent{
		TenantID:   "t-1",
		MetricID:   "cas.blob.bytes_stored",
		Shape:      domain.ShapeGaugeDelta,
		Unit:       domain.UnitByte,
		Quantity:   4096,
		OccurredAt: occurred,
		Source:     "cas.blob.store",
		SourceRef:  "slotversion:01JB0000000000000000000000",
		Dimensions: domain.Dimensions{
			domain.DimensionTier:       "warm",
			domain.DimensionDocumentID: "doc-9",
		},
	}
}

func TestUsageEventNormalizeCanonicalisesTimeAndDimensions(t *testing.T) {
	t.Parallel()

	in := validEvent()
	got, err := in.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if got.OccurredAt.Location() != time.UTC {
		t.Errorf("OccurredAt location = %v, want UTC", got.OccurredAt.Location())
	}
	if got.OccurredAt.Nanosecond()%1000 != 0 {
		t.Errorf("OccurredAt = %v, want microsecond precision", got.OccurredAt)
	}
	if want := occurred.UTC().Truncate(time.Microsecond); !got.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, want)
	}

	// The normalized event must not alias the caller's map: a ledger that
	// stored the caller's map would see the stored event change under it.
	in.Dimensions[domain.DimensionTier] = "cold"
	if got.Dimensions[domain.DimensionTier] != "warm" {
		t.Errorf("normalized Dimensions aliased the caller's map: tier = %q", got.Dimensions[domain.DimensionTier])
	}
}

func TestUsageEventNormalizeDropsEmptyDimensions(t *testing.T) {
	t.Parallel()

	for _, dims := range []domain.Dimensions{nil, {}} {
		e := validEvent()
		e.Dimensions = dims
		got, err := e.Normalize()
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if got.Dimensions != nil {
			t.Errorf("Dimensions = %#v, want nil so that an absent and an empty dimension set compare equal", got.Dimensions)
		}
	}
}

func TestUsageEventNormalizeIsIdempotent(t *testing.T) {
	t.Parallel()

	once, err := validEvent().Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	twice, err := once.Normalize()
	if err != nil {
		t.Fatalf("second Normalize: %v", err)
	}
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("Normalize is not idempotent:\n once  = %#v\n twice = %#v", once, twice)
	}
}

func TestUsageEventNormalizeRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*domain.UsageEvent)
		wantErr error
	}{
		{"empty tenant", func(e *domain.UsageEvent) { e.TenantID = "" }, domain.ErrInvalidEvent},
		{"empty metric", func(e *domain.UsageEvent) { e.MetricID = "" }, domain.ErrInvalidEvent},
		{"metric with uppercase", func(e *domain.UsageEvent) { e.MetricID = "CAS.Bytes" }, domain.ErrInvalidEvent},
		{"metric with space", func(e *domain.UsageEvent) { e.MetricID = "cas bytes" }, domain.ErrInvalidEvent},
		{"metric over-long", func(e *domain.UsageEvent) { e.MetricID = domain.MetricID(strings.Repeat("a", 200)) }, domain.ErrInvalidEvent},
		{"unspecified shape", func(e *domain.UsageEvent) { e.Shape = 0 }, domain.ErrInvalidEvent},
		{"unknown shape", func(e *domain.UsageEvent) { e.Shape = domain.MetricShape(99) }, domain.ErrInvalidEvent},
		{"empty unit", func(e *domain.UsageEvent) { e.Unit = "" }, domain.ErrInvalidEvent},
		{"unit with uppercase", func(e *domain.UsageEvent) { e.Unit = "Byte" }, domain.ErrInvalidEvent},
		{"zero occurred-at", func(e *domain.UsageEvent) { e.OccurredAt = time.Time{} }, domain.ErrInvalidEvent},
		{"empty source", func(e *domain.UsageEvent) { e.Source = "" }, domain.ErrInvalidEvent},
		{"empty source ref", func(e *domain.UsageEvent) { e.SourceRef = "" }, domain.ErrInvalidEvent},
		{"source with control byte", func(e *domain.UsageEvent) { e.Source = "cas\x00blob" }, domain.ErrInvalidEvent},
		{"dimension key empty", func(e *domain.UsageEvent) { e.Dimensions = domain.Dimensions{"": "x"} }, domain.ErrInvalidEvent},
		{"dimension value empty", func(e *domain.UsageEvent) { e.Dimensions = domain.Dimensions{domain.DimensionTier: ""} }, domain.ErrInvalidEvent},
		{"negative quantity on an event metric", func(e *domain.UsageEvent) {
			e.Shape = domain.ShapeEvent
			e.Unit = domain.UnitCount
			e.Quantity = -1
		}, domain.ErrNegativeQuantity},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := validEvent()
			tc.mutate(&e)
			if _, err := e.Normalize(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Normalize error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// A gauge metric records signed deltas (CAS §15.3): "-n bytes in hot" is
// the only way the ledger can express a deletion, so a negative quantity
// must be accepted for exactly this shape and no other.
func TestUsageEventNormalizeAcceptsNegativeGaugeDelta(t *testing.T) {
	t.Parallel()

	e := validEvent()
	e.Shape = domain.ShapeGaugeDelta
	e.Quantity = -4096
	got, err := e.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Quantity != -4096 {
		t.Errorf("Quantity = %d, want -4096", got.Quantity)
	}
}

func TestUsageEventNormalizeAcceptsZeroQuantityForBothShapes(t *testing.T) {
	t.Parallel()

	for _, shape := range []domain.MetricShape{domain.ShapeEvent, domain.ShapeGaugeDelta} {
		e := validEvent()
		e.Shape = shape
		e.Quantity = 0
		if _, err := e.Normalize(); err != nil {
			t.Errorf("Normalize with shape %v and zero quantity: %v", shape, err)
		}
	}
}

// The validation error must name the offending dimension key and never its
// value: a dimension value is a document id, a principal id, or a matter
// reference, and CONVENTIONS forbids sensitive payload in an error string.
func TestUsageEventNormalizeErrorNamesDimensionKeyNotValue(t *testing.T) {
	t.Parallel()

	const sentinelValue = "sentinel-7c1f9a-do-not-leak-this-value"
	e := validEvent()
	e.Dimensions = domain.Dimensions{"not a valid key": sentinelValue}

	_, err := e.Normalize()
	if err == nil {
		t.Fatal("Normalize of a malformed dimension key returned nil error")
	}
	if strings.Contains(err.Error(), sentinelValue) {
		t.Fatalf("validation error leaked a dimension value: %v", err)
	}
	if !strings.Contains(err.Error(), "not a valid key") {
		t.Fatalf("validation error does not name the offending key: %v", err)
	}
}

func TestUsageEventIdempotencyKeyComposition(t *testing.T) {
	t.Parallel()

	base := validEvent()

	// Fields deliberately outside the key: a retry that recomputes its
	// timestamp, or a correction attempt that changes the quantity, must
	// collide with the original rather than silently double-count.
	for _, tc := range []struct {
		name   string
		mutate func(*domain.UsageEvent)
	}{
		{"quantity", func(e *domain.UsageEvent) { e.Quantity = 999 }},
		{"occurred-at", func(e *domain.UsageEvent) { e.OccurredAt = occurred.Add(time.Hour) }},
		{"shape", func(e *domain.UsageEvent) { e.Shape = domain.ShapeEvent }},
		{"unit", func(e *domain.UsageEvent) { e.Unit = domain.UnitCount }},
		{"dimensions", func(e *domain.UsageEvent) { e.Dimensions = domain.Dimensions{domain.DimensionTier: "cold"} }},
	} {
		t.Run("same key when only "+tc.name+" differs", func(t *testing.T) {
			t.Parallel()
			other := validEvent()
			tc.mutate(&other)
			if base.IdempotencyKey() != other.IdempotencyKey() {
				t.Fatalf("IdempotencyKey changed with %s: %#v vs %#v", tc.name, base.IdempotencyKey(), other.IdempotencyKey())
			}
		})
	}

	// Fields deliberately inside the key.
	for _, tc := range []struct {
		name   string
		mutate func(*domain.UsageEvent)
	}{
		{"tenant", func(e *domain.UsageEvent) { e.TenantID = "t-2" }},
		{"metric", func(e *domain.UsageEvent) { e.MetricID = "cas.blob.bytes_ingested" }},
		{"source", func(e *domain.UsageEvent) { e.Source = "cas.job.runner" }},
		{"source ref", func(e *domain.UsageEvent) { e.SourceRef = "slotversion:other" }},
	} {
		t.Run("different key when "+tc.name+" differs", func(t *testing.T) {
			t.Parallel()
			other := validEvent()
			tc.mutate(&other)
			if base.IdempotencyKey() == other.IdempotencyKey() {
				t.Fatalf("IdempotencyKey unchanged with a different %s: %#v", tc.name, base.IdempotencyKey())
			}
		})
	}
}

// SameBillableContent is what an adapter compares when an idempotency key
// recurs: equal content is a retry and must be a silent no-op, unequal
// content is a caller bug and must surface rather than be swallowed.
func TestUsageEventSameBillableContent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*domain.UsageEvent)
		want   bool
	}{
		{"identical", func(*domain.UsageEvent) {}, true},
		{"occurred-at differs", func(e *domain.UsageEvent) { e.OccurredAt = occurred.Add(time.Second) }, true},
		{"dimensions differ", func(e *domain.UsageEvent) { e.Dimensions = nil }, true},
		{"quantity differs", func(e *domain.UsageEvent) { e.Quantity = 1 }, false},
		{"unit differs", func(e *domain.UsageEvent) { e.Unit = domain.UnitCount }, false},
		{"shape differs", func(e *domain.UsageEvent) { e.Shape = domain.ShapeEvent }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := validEvent()
			b := validEvent()
			tc.mutate(&b)
			if got := a.SameBillableContent(b); got != tc.want {
				t.Fatalf("SameBillableContent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMetricShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		shape          domain.MetricShape
		valid          bool
		allowsNegative bool
		str            string
	}{
		{domain.ShapeUnspecified, false, false, "unspecified"},
		{domain.ShapeEvent, true, false, "event"},
		{domain.ShapeGaugeDelta, true, true, "gauge_delta"},
		{domain.MetricShape(42), false, false, "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.str, func(t *testing.T) {
			t.Parallel()
			if got := tc.shape.Valid(); got != tc.valid {
				t.Errorf("Valid = %v, want %v", got, tc.valid)
			}
			if got := tc.shape.AllowsNegative(); got != tc.allowsNegative {
				t.Errorf("AllowsNegative = %v, want %v", got, tc.allowsNegative)
			}
			if got := tc.shape.String(); !strings.Contains(got, tc.str) {
				t.Errorf("String = %q, want it to contain %q", got, tc.str)
			}
		})
	}
}
