package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/laenen-ai/meter/internal/domain"
)

var (
	t0 = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

func TestRangeValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		r       domain.Range
		wantErr bool
	}{
		{"tenant and window", domain.Range{TenantID: "t-1", From: t0, To: t1}, false},
		{"with metric filter", domain.Range{TenantID: "t-1", MetricID: "cas.a", From: t0, To: t1}, false},
		{"with limit", domain.Range{TenantID: "t-1", From: t0, To: t1, Limit: 10}, false},
		{"no tenant", domain.Range{From: t0, To: t1}, true},
		{"zero from", domain.Range{TenantID: "t-1", To: t1}, true},
		{"zero to", domain.Range{TenantID: "t-1", From: t0}, true},
		{"to before from", domain.Range{TenantID: "t-1", From: t1, To: t0}, true},
		{"empty window", domain.Range{TenantID: "t-1", From: t0, To: t0}, true},
		{"negative limit", domain.Range{TenantID: "t-1", From: t0, To: t1, Limit: -1}, true},
		{"malformed metric filter", domain.Range{TenantID: "t-1", MetricID: "NOPE", From: t0, To: t1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.r.Validate()
			if tc.wantErr {
				if !errors.Is(err, domain.ErrInvalidRange) {
					t.Fatalf("Validate error = %v, want ErrInvalidRange", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// Matches lives in the domain rather than in each adapter so that a
// Postgres WHERE clause and the in-memory adapter's filter cannot drift
// apart on the window's boundaries.
func TestRangeMatchesIsHalfOpen(t *testing.T) {
	t.Parallel()

	r := domain.Range{TenantID: "t-1", From: t0, To: t1}
	ev := func(tenant domain.TenantID, metric domain.MetricID, at time.Time) domain.UsageEvent {
		return domain.UsageEvent{TenantID: tenant, MetricID: metric, OccurredAt: at}
	}

	cases := []struct {
		name string
		e    domain.UsageEvent
		want bool
	}{
		{"at From is included", ev("t-1", "cas.a", t0), true},
		{"inside", ev("t-1", "cas.a", t0.Add(time.Minute)), true},
		{"one microsecond before To", ev("t-1", "cas.a", t1.Add(-time.Microsecond)), true},
		{"at To is excluded", ev("t-1", "cas.a", t1), false},
		{"after To", ev("t-1", "cas.a", t1.Add(time.Minute)), false},
		{"before From", ev("t-1", "cas.a", t0.Add(-time.Nanosecond)), false},
		{"other tenant", ev("t-2", "cas.a", t0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := r.Matches(tc.e); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}

	filtered := domain.Range{TenantID: "t-1", MetricID: "cas.a", From: t0, To: t1}
	if filtered.Matches(ev("t-1", "cas.b", t0)) {
		t.Error("a metric filter did not exclude a different metric")
	}
	if !filtered.Matches(ev("t-1", "cas.a", t0)) {
		t.Error("a metric filter excluded its own metric")
	}
}

// CompareUsageEvents defines the total order every adapter must return a
// range in, so a rollup gets the same sequence from any adapter.
func TestCompareUsageEventsIsATotalOrder(t *testing.T) {
	t.Parallel()

	mk := func(at time.Time, metric domain.MetricID, source, ref string) domain.UsageEvent {
		return domain.UsageEvent{TenantID: "t-1", MetricID: metric, OccurredAt: at, Source: source, SourceRef: ref}
	}
	want := []domain.UsageEvent{
		mk(t0, "cas.a", "s1", "r1"),
		mk(t0, "cas.a", "s1", "r2"),
		mk(t0, "cas.a", "s2", "r1"),
		mk(t0, "cas.b", "s1", "r1"),
		mk(t1, "cas.a", "s1", "r1"),
	}

	shuffled := []domain.UsageEvent{want[3], want[0], want[4], want[2], want[1]}
	slices.SortFunc(shuffled, domain.CompareUsageEvents)
	for i := range want {
		if shuffled[i].IdempotencyKey() != want[i].IdempotencyKey() || !shuffled[i].OccurredAt.Equal(want[i].OccurredAt) {
			t.Fatalf("sorted[%d] = %+v, want %+v", i, shuffled[i], want[i])
		}
	}

	if domain.CompareUsageEvents(want[0], want[0]) != 0 {
		t.Error("CompareUsageEvents of an event with itself is not 0")
	}
	if domain.CompareUsageEvents(want[0], want[1]) >= 0 || domain.CompareUsageEvents(want[1], want[0]) <= 0 {
		t.Error("CompareUsageEvents is not antisymmetric")
	}
}
