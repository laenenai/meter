// Package metertest holds the conformance suite for the ledger.Ledger
// port: the authoritative description of the contract's edge-case
// behaviour, and the thing a consumer runs to demonstrate that it has
// actually adopted this module rather than merely saying so.
//
// It is a sibling of package ledger rather than part of it, which is the
// relationship httptest has to http and testing/fstest has to io/fs, and
// both halves of the reason are forced. The suite has to be callable from
// another package and from another module — a consumer's own CI, against
// that consumer's own wiring — which rules out a _test.go file, since
// identifiers there are not importable. And a non-test file that imports
// testing makes testing a dependency of whatever package holds it, so
// keeping it out of package ledger is what stops every consumer's
// production binary from linking the testing package and inheriting its
// flag registrations.
//
// Nothing in here is production code, and nothing in production may import
// it.
package metertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laenenai/meter/internal/domain"
	"github.com/laenenai/meter/ledger"
)

// RunLedgerSuite runs the contract every ledger.Ledger adapter must satisfy.
// newLedger is called once per subtest, receiving that subtest's own *T, so
// an adapter that needs per-test setup and teardown (a scratch schema, a
// fresh connection pool, a transaction that rolls back) can register it
// with t.Cleanup inside newLedger and get fresh, isolated state for every
// subtest.
//
// Every assertion here must hold for any ledger: an in-memory map, a
// Postgres table with a unique index, or an adapter that compacts a counter
// metric into per-minute buckets. Nothing here inspects storage directly,
// counts rows, or assumes one row per event — compaction is a storage
// decision the port hides, so the suite asserts on sums and on what
// ReadRange returns, never on how it is kept.
//
// Subtests do not call t.Parallel: an adapter may be backed by a single
// database with a bounded connection pool, and the suite's own concurrency
// subtests already exercise parallel use within one adapter instance.
func RunLedgerSuite(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	t.Helper()

	t.Run("AppendThenReadRangeRoundTrip", func(t *testing.T) {
		runRoundTrip(t, newLedger)
	})

	t.Run("BothMetricShapes", func(t *testing.T) {
		runBothMetricShapes(t, newLedger)
	})

	t.Run("QuantitySignFollowsMetricShape", func(t *testing.T) {
		runQuantitySign(t, newLedger)
	})

	t.Run("IdempotentReEmission", func(t *testing.T) {
		runIdempotentReEmission(t, newLedger)
	})

	t.Run("AttributionPreserved", func(t *testing.T) {
		runAttributionPreserved(t, newLedger)
	})

	t.Run("RangeReadIsStableUnderConcurrentAppends", func(t *testing.T) {
		runRangeStability(t, newLedger)
	})

	t.Run("RollbackLeavesNoTrace", func(t *testing.T) {
		runRollbackLeavesNoTrace(t, newLedger)
	})

	t.Run("AppendRequiresATransaction", func(t *testing.T) {
		runTransactionRequired(t, newLedger)
	})

	t.Run("AppendIsAllOrNothingWithinACall", func(t *testing.T) {
		runAppendAtomicity(t, newLedger)
	})

	t.Run("RangeIsTenantScopedAndHalfOpen", func(t *testing.T) {
		runRangeSemantics(t, newLedger)
	})

	t.Run("InvalidRangeIsRejected", func(t *testing.T) {
		runInvalidRange(t, newLedger)
	})

	t.Run("NoDimensionValuesLeakInErrors", func(t *testing.T) {
		runNoDimensionValueLeak(t, newLedger)
	})
}

// runRoundTrip pins that what goes in comes back out in canonical form:
// UTC at microsecond precision, an empty dimension set as nil, and in the
// order domain.CompareUsageEvents defines regardless of append order.
func runRoundTrip(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	// Deliberately appended out of order, and with a non-UTC,
	// sub-microsecond timestamp, so the read exercises both the ordering
	// and the normalisation guarantee.
	later := eventAt(suiteStart.Add(30*time.Minute).In(time.FixedZone("CEST", 2*60*60)).Add(123*time.Nanosecond), "ref-later")
	earlier := eventAt(suiteStart.Add(time.Minute), "ref-earlier")

	mustAppendCommit(ctx, t, led, later, earlier)

	got := mustRead(ctx, t, led, fullWindow())
	want := []domain.UsageEvent{mustNormalize(t, earlier), mustNormalize(t, later)}
	assertEvents(t, got, want)

	for _, e := range got {
		if e.OccurredAt.Location() != time.UTC {
			t.Errorf("read-back OccurredAt location = %v, want UTC", e.OccurredAt.Location())
		}
		if e.OccurredAt.Nanosecond()%1000 != 0 {
			t.Errorf("read-back OccurredAt = %v, want microsecond precision", e.OccurredAt)
		}
	}

	// An empty window inside the retained range is an empty result, not an
	// error: a tenant that used nothing used nothing.
	quiet := domain.Range{TenantID: suiteTenant, From: suiteStart.Add(10 * time.Hour), To: suiteStart.Add(11 * time.Hour)}
	if quiet := mustRead(ctx, t, led, quiet); len(quiet) != 0 {
		t.Errorf("read of a window with no events returned %d events", len(quiet))
	}
}

// runBothMetricShapes exercises the two shapes of cas §15.3 through the
// port, and asserts the property that distinguishes them: an event
// metric's sum only grows as the window widens, while a gauge metric's
// cumulative sum is a level that can fall.
func runBothMetricShapes(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	t.Run("EventMetricSumsAndNeverDecreases", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		var events []domain.UsageEvent
		for i, n := range []domain.Quantity{3, 0, 7, 11} {
			e := eventAt(suiteStart.Add(time.Duration(i)*time.Minute), fmt.Sprintf("req-%d", i))
			e.MetricID = "cas.api.requests"
			e.Shape = domain.ShapeEvent
			e.Unit = domain.UnitCount
			e.Quantity = n
			events = append(events, e)
		}
		mustAppendCommit(ctx, t, led, events...)

		// Widening the window can only add to an event metric's sum.
		var prev domain.Quantity = -1
		for _, minutes := range []int{1, 2, 3, 4, 60} {
			r := domain.Range{
				TenantID: suiteTenant,
				MetricID: "cas.api.requests",
				From:     suiteStart,
				To:       suiteStart.Add(time.Duration(minutes) * time.Minute),
			}
			sum := sumOf(mustRead(ctx, t, led, r))
			if sum < prev {
				t.Fatalf("event-metric sum fell from %d to %d as the window widened to %d minutes", prev, sum, minutes)
			}
			prev = sum
		}
		if prev != 21 {
			t.Fatalf("event-metric total = %d, want 21", prev)
		}
	})

	t.Run("GaugeDeltaCumulativeSumIsTheLevel", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		// A blob stored in warm, a second stored, then the first deleted:
		// the level is the cumulative sum, and the deletion is expressible
		// only as a negative delta.
		deltas := []domain.Quantity{+4096, +1024, -4096}
		var events []domain.UsageEvent
		for i, q := range deltas {
			e := eventAt(suiteStart.Add(time.Duration(i)*time.Minute), fmt.Sprintf("blob-%d", i))
			e.MetricID = "cas.blob.bytes_stored"
			e.Shape = domain.ShapeGaugeDelta
			e.Unit = domain.UnitByte
			e.Quantity = q
			e.Dimensions = domain.Dimensions{domain.DimensionTier: "warm"}
			events = append(events, e)
		}
		mustAppendCommit(ctx, t, led, events...)

		got := mustRead(ctx, t, led, domain.Range{
			TenantID: suiteTenant,
			MetricID: "cas.blob.bytes_stored",
			From:     suiteStart,
			To:       suiteStart.Add(time.Hour),
		})
		if len(got) != len(deltas) {
			t.Fatalf("read %d gauge deltas, want %d", len(got), len(deltas))
		}
		if sum := sumOf(got); sum != 1024 {
			t.Fatalf("cumulative gauge level = %d, want 1024", sum)
		}

		// The level mid-history is the prefix sum, which is what makes a
		// byte-hours integral computable for a historical window rather
		// than only for now.
		midpoint := mustRead(ctx, t, led, domain.Range{
			TenantID: suiteTenant,
			MetricID: "cas.blob.bytes_stored",
			From:     suiteStart,
			To:       suiteStart.Add(2 * time.Minute),
		})
		if sum := sumOf(midpoint); sum != 5120 {
			t.Fatalf("gauge level after the first two deltas = %d, want 5120", sum)
		}
	})
}

// runQuantitySign pins the asymmetry between the shapes: an event metric
// is monotonic and a negative quantity is always a bug, while a gauge
// metric's negative delta is the normal case. An adapter that accepted a
// negative count would silently make a tenant's request total fall.
func runQuantitySign(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	t.Run("NegativeEventQuantityIsRejected", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		e := eventAt(suiteStart, "neg-1")
		e.MetricID = "cas.api.requests"
		e.Shape = domain.ShapeEvent
		e.Unit = domain.UnitCount
		e.Quantity = -1

		tx := mustBegin(ctx, t, led)
		defer rollback(ctx, t, tx)

		if err := led.Append(ctx, tx, e); !errors.Is(err, domain.ErrNegativeQuantity) {
			t.Fatalf("Append of a negative event quantity returned %v, want domain.ErrNegativeQuantity", err)
		}
	})

	t.Run("ZeroQuantityIsAcceptedForBothShapes", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		var events []domain.UsageEvent
		for i, shape := range []domain.MetricShape{domain.ShapeEvent, domain.ShapeGaugeDelta} {
			e := eventAt(suiteStart.Add(time.Duration(i)*time.Minute), fmt.Sprintf("zero-%d", i))
			e.Shape = shape
			e.Unit = domain.UnitCount
			e.MetricID = domain.MetricID(fmt.Sprintf("cas.zero.%d", i))
			e.Quantity = 0
			events = append(events, e)
		}
		mustAppendCommit(ctx, t, led, events...)

		if got := mustRead(ctx, t, led, fullWindow()); len(got) != 2 {
			t.Fatalf("read %d zero-quantity events, want 2", len(got))
		}
	})

	t.Run("NegativeGaugeDeltaIsAccepted", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		e := eventAt(suiteStart, "neg-delta")
		e.Shape = domain.ShapeGaugeDelta
		e.Unit = domain.UnitByte
		e.Quantity = -4096
		mustAppendCommit(ctx, t, led, e)

		got := mustRead(ctx, t, led, fullWindow())
		if len(got) != 1 || got[0].Quantity != -4096 {
			t.Fatalf("read back %+v, want a single event with quantity -4096", got)
		}
	})
}

// runIdempotentReEmission is the heart of the contract: a unit of work can
// be retried, so the same event emitted twice must count once — within one
// transaction, across sequential transactions, and when two transactions
// race. A duplicate key carrying a *different* measurement is the opposite
// case and must be loud.
func runIdempotentReEmission(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	t.Run("WithinOneTransaction", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		e := eventAt(suiteStart, "retry-same-tx")
		tx := mustBegin(ctx, t, led)

		if err := led.Append(ctx, tx, e); err != nil {
			t.Fatalf("first Append: %v", err)
		}
		if err := led.Append(ctx, tx, e); err != nil {
			t.Fatalf("second Append of an identical event: %v (a retry must be absorbed, not rejected)", err)
		}
		// Also as two copies in one variadic call, which is what a caller
		// that built its event list from a retried loop produces.
		if err := led.Append(ctx, tx, e, e); err != nil {
			t.Fatalf("Append of two identical events in one call: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		assertEvents(t, mustRead(ctx, t, led, fullWindow()), []domain.UsageEvent{mustNormalize(t, e)})
	})

	t.Run("AcrossTransactions", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		e := eventAt(suiteStart, "retry-across-tx")
		mustAppendCommit(ctx, t, led, e)
		mustAppendCommit(ctx, t, led, e)
		mustAppendCommit(ctx, t, led, e)

		assertEvents(t, mustRead(ctx, t, led, fullWindow()), []domain.UsageEvent{mustNormalize(t, e)})
		if sum := sumOf(mustRead(ctx, t, led, fullWindow())); sum != e.Quantity {
			t.Fatalf("sum after three emissions of one event = %d, want %d", sum, e.Quantity)
		}
	})

	// A retry may recompute its timestamp and may carry a
	// differently-populated attribution set; neither changes what is being
	// charged, so neither may defeat deduplication. The first write's
	// values stand.
	t.Run("RetryWithADifferentTimestampStillCountsOnce", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		first := eventAt(suiteStart, "retry-clock-skew")
		retry := first
		retry.OccurredAt = suiteStart.Add(3 * time.Second)
		retry.Dimensions = domain.Dimensions{domain.DimensionTier: "cold"}

		mustAppendCommit(ctx, t, led, first)
		mustAppendCommit(ctx, t, led, retry)

		assertEvents(t, mustRead(ctx, t, led, fullWindow()), []domain.UsageEvent{mustNormalize(t, first)})
	})

	t.Run("ConcurrentTransactionsCountOnce", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		e := eventAt(suiteStart, "retry-concurrent")

		const goroutines = 8
		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			errs  = make([]error, goroutines)
		)
		for i := range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				tx, err := led.Begin(ctx)
				if err != nil {
					errs[i] = fmt.Errorf("begin: %w", err)
					return
				}
				if err := led.Append(ctx, tx, e); err != nil {
					errs[i] = fmt.Errorf("append: %w", errors.Join(err, tx.Rollback(ctx)))
					return
				}
				errs[i] = tx.Commit(ctx)
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("goroutine %d: %v (concurrent emission of one identical event must not conflict)", i, err)
			}
		}
		assertEvents(t, mustRead(ctx, t, led, fullWindow()), []domain.UsageEvent{mustNormalize(t, e)})
	})

	// The loud case. Two different measurements under one key means the
	// emitting code derived its SourceRef from something that does not
	// identify the fact it meters; picking a winner would leave a wrong
	// number nothing downstream could flag.
	t.Run("DifferingQuantityIsAConflict", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		first := eventAt(suiteStart, "conflict")
		mustAppendCommit(ctx, t, led, first)

		for _, tc := range []struct {
			name   string
			mutate func(*domain.UsageEvent)
		}{
			{"quantity", func(e *domain.UsageEvent) { e.Quantity = first.Quantity + 1 }},
			{"unit", func(e *domain.UsageEvent) { e.Unit = domain.UnitCount }},
			{"shape", func(e *domain.UsageEvent) {
				e.Shape = domain.ShapeEvent
				e.Quantity = 1
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				other := first
				tc.mutate(&other)

				tx := mustBegin(ctx, t, led)
				err := led.Append(ctx, tx, other)
				if err == nil {
					// An adapter may defer detection to its unique
					// constraint, so the conflict is allowed to surface
					// from Commit instead of from Append.
					err = tx.Commit(ctx)
				} else {
					rollback(ctx, t, tx)
				}
				if !errors.Is(err, ledger.ErrIdempotencyConflict) {
					t.Fatalf("re-emission with a differing %s returned %v, want ledger.ErrIdempotencyConflict", tc.name, err)
				}

				assertEvents(t, mustRead(ctx, t, led, fullWindow()), []domain.UsageEvent{mustNormalize(t, first)})
			})
		}
	})

	// One state change legitimately emits several metrics — a finished job
	// reports wall time, tokens and pages against the same row — so the
	// metric must be part of the key or the second and third would be
	// swallowed as duplicates of the first.
	t.Run("OneSourceRefMayCarrySeveralMetrics", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		specs := []struct {
			metric domain.MetricID
			unit   domain.Unit
			qty    domain.Quantity
		}{
			{"cas.job.wall_time", domain.UnitMillisecond, 1500},
			{"cas.job.embedding_tokens", domain.UnitToken, 8120},
			{"cas.job.ocr_pages", domain.UnitPage, 14},
		}
		var events []domain.UsageEvent
		for _, s := range specs {
			e := eventAt(suiteStart, "job:01JB000000000000000000000X")
			e.Source = "cas.job.runner"
			e.MetricID = s.metric
			e.Shape = domain.ShapeEvent
			e.Unit = s.unit
			e.Quantity = s.qty
			events = append(events, e)
		}
		mustAppendCommit(ctx, t, led, events...)

		if got := mustRead(ctx, t, led, fullWindow()); len(got) != len(specs) {
			t.Fatalf("read %d events for one source ref, want %d (the metric must be part of the idempotency key)", len(got), len(specs))
		}
	})
}

// runAttributionPreserved pins cas §15.6: the tenant is the billing
// entity, but the dimensions below it are what let an operator answer
// "what did this matter cost" and a tenant charge back internally. An
// adapter that dropped, truncated or re-ordered them would make those
// questions unanswerable after the fact, which is the one failure this
// module cannot recover from later.
func runAttributionPreserved(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	full := domain.Dimensions{
		domain.DimensionTier:             "warm",
		domain.DimensionLane:             "interactive",
		domain.DimensionProcessorID:      "ocr",
		domain.DimensionProcessorVersion: "1.4.2",
		domain.DimensionModelID:          "text-embedding-3-large",
		domain.DimensionRegion:           "eu-west-1",
		domain.DimensionPrincipalID:      "principal:01JB0000000000000000000001",
		domain.DimensionCapabilityChain:  "root>delegated>attenuated",
		domain.DimensionFilesetID:        "fileset:01JB0000000000000000000002",
		domain.DimensionDocumentID:       "document:01JB0000000000000000000003",
		domain.DimensionAgentRunID:       "run:01JB0000000000000000000004",
		domain.DimensionJobID:            "job:01JB0000000000000000000005",
		"cas.matter_ref":                 "matter-2026-114",
	}

	withAll := eventAt(suiteStart, "attrib-full")
	withAll.Dimensions = full

	withNone := eventAt(suiteStart.Add(time.Minute), "attrib-none")
	withNone.Dimensions = nil

	withEmpty := eventAt(suiteStart.Add(2*time.Minute), "attrib-empty")
	withEmpty.Dimensions = domain.Dimensions{}

	mustAppendCommit(ctx, t, led, withAll, withNone, withEmpty)

	got := mustRead(ctx, t, led, fullWindow())
	if len(got) != 3 {
		t.Fatalf("read %d events, want 3", len(got))
	}

	if !got[0].Dimensions.Equal(full) {
		t.Errorf("attribution not preserved:\n got  %v\n want %v", got[0].Dimensions.Keys(), full.Keys())
	}
	for _, k := range full.Keys() {
		if got[0].Dimensions[k] != full[k] {
			t.Errorf("dimension %q was not preserved", k)
		}
	}

	// An absent and an empty dimension set must be indistinguishable after
	// a round trip, so two events that attribute to the same things compare
	// equal regardless of which code path built them.
	for _, e := range got[1:] {
		if e.Dimensions != nil {
			t.Errorf("event %q read back with Dimensions = %#v, want nil", e.SourceRef, e.Dimensions)
		}
	}

	// The ledger must hold a copy: a caller that reuses and mutates its map
	// after appending must not retroactively change a committed row.
	full[domain.DimensionTier] = "cold"
	delete(full, domain.DimensionJobID)
	after := mustRead(ctx, t, led, fullWindow())
	if after[0].Dimensions[domain.DimensionTier] != "warm" {
		t.Error("the ledger aliased the caller's dimension map: mutating it changed a committed event")
	}
	if after[0].Dimensions[domain.DimensionJobID] == "" {
		t.Error("the ledger aliased the caller's dimension map: deleting a key removed it from a committed event")
	}
}

// runRangeStability pins what a rollup depends on: a window read inside
// one transaction does not change under it while other transactions commit
// appends into that same window. Without it an hourly rollup could sum a
// window it never actually observed — reading the first half before a
// concurrent append and the second half after — and the resulting figure
// would be reproducible by nothing.
func runRangeStability(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	const preexisting = 20
	var seed []domain.UsageEvent
	for i := range preexisting {
		seed = append(seed, eventAt(suiteStart.Add(time.Duration(i)*time.Second), fmt.Sprintf("seed-%02d", i)))
	}
	mustAppendCommit(ctx, t, led, seed...)

	reader := mustBegin(ctx, t, led)
	defer rollback(ctx, t, reader)

	first, err := led.ReadRange(ctx, reader, fullWindow())
	if err != nil {
		t.Fatalf("first ReadRange: %v", err)
	}
	if len(first) != preexisting {
		t.Fatalf("first ReadRange returned %d events, want %d", len(first), preexisting)
	}

	// Hammer the same window from other transactions while the reader
	// holds its own.
	const writers = 6
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				e := eventAt(suiteStart.Add(time.Duration(i)*time.Second), fmt.Sprintf("concurrent-%d-%02d", w, i))
				tx, err := led.Begin(ctx)
				if err != nil {
					errs[w] = fmt.Errorf("begin: %w", err)
					return
				}
				if err := led.Append(ctx, tx, e); err != nil {
					errs[w] = fmt.Errorf("append: %w", errors.Join(err, tx.Rollback(ctx)))
					return
				}
				if err := tx.Commit(ctx); err != nil {
					errs[w] = fmt.Errorf("commit: %w", err)
					return
				}
			}
		}()
	}

	// Re-read repeatedly while the writers run.
	for range 20 {
		again, err := led.ReadRange(ctx, reader, fullWindow())
		if err != nil {
			t.Fatalf("repeat ReadRange: %v", err)
		}
		assertEvents(t, again, first)
	}
	wg.Wait()

	for w, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", w, err)
		}
	}

	// A fresh transaction sees everything that committed, exactly once
	// each, still in order.
	final := mustRead(ctx, t, led, fullWindow())
	if want := preexisting + writers*10; len(final) != want {
		t.Fatalf("after concurrent appends a fresh read returned %d events, want %d", len(final), want)
	}
	if !slices.IsSortedFunc(final, domain.CompareUsageEvents) {
		t.Error("a range read is not ordered by domain.CompareUsageEvents")
	}
	seen := map[domain.IdempotencyKey]struct{}{}
	for _, e := range final {
		if _, dup := seen[e.IdempotencyKey()]; dup {
			t.Fatalf("event %s appears more than once after concurrent appends", e.IdempotencyKey())
		}
		seen[e.IdempotencyKey()] = struct{}{}
	}
}

// runRollbackLeavesNoTrace is the other half of cas §15.2: a crashed or
// rolled-back worker cannot produce a charge for work that did not happen.
// It must also not poison the idempotency key, because the work it was
// metering will very likely be retried and succeed.
func runRollbackLeavesNoTrace(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	e := eventAt(suiteStart, "rolled-back")

	tx := mustBegin(ctx, t, led)
	if err := led.Append(ctx, tx, e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Visible inside its own transaction before it commits.
	inTx, err := led.ReadRange(ctx, tx, fullWindow())
	if err != nil {
		t.Fatalf("ReadRange inside the appending tx: %v", err)
	}
	if len(inTx) != 1 {
		t.Fatalf("an append is not visible to a read in its own transaction: got %d events", len(inTx))
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := mustRead(ctx, t, led, fullWindow()); len(got) != 0 {
		t.Fatalf("a rolled-back append left %d events behind", len(got))
	}

	// The key is not reserved: the retry of the metered work must succeed.
	mustAppendCommit(ctx, t, led, e)
	assertEvents(t, mustRead(ctx, t, led, fullWindow()), []domain.UsageEvent{mustNormalize(t, e)})

	t.Run("UseAfterFinishIsErrTxDone", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		committed := mustBegin(ctx, t, led)
		if err := committed.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		assertTxDone(ctx, t, led, committed, "a committed transaction")

		rolledBack := mustBegin(ctx, t, led)
		if err := rolledBack.Rollback(ctx); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		assertTxDone(ctx, t, led, rolledBack, "a rolled-back transaction")
	})
}

// runTransactionRequired pins the port's central constraint. There is no
// implicit auto-commit path and no adapter may invent one: an append that
// is not part of a declared unit of work is the best-effort metering this
// module exists to rule out.
func runTransactionRequired(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	t.Run("NilTxIsErrNoTx", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()
		e := eventAt(suiteStart, "no-tx")

		if err := led.Append(ctx, nil, e); !errors.Is(err, ledger.ErrNoTx) {
			t.Fatalf("Append with a nil tx returned %v, want ledger.ErrNoTx", err)
		}
		if _, err := led.ReadRange(ctx, nil, fullWindow()); !errors.Is(err, ledger.ErrNoTx) {
			t.Fatalf("ReadRange with a nil tx returned %v, want ledger.ErrNoTx", err)
		}
	})

	// An adapter must refuse a handle it does not recognise rather than
	// ignore it and write on its own connection. The silent version of this
	// bug is undetectable downstream: the row is there and looks correct,
	// but it was never atomic with the work.
	t.Run("ForeignTxIsErrForeignTx", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()
		e := eventAt(suiteStart, "foreign-tx")

		if err := led.Append(ctx, foreignTx{}, e); !errors.Is(err, ledger.ErrForeignTx) {
			t.Fatalf("Append with a foreign tx returned %v, want ledger.ErrForeignTx", err)
		}
		if _, err := led.ReadRange(ctx, foreignTx{}, fullWindow()); !errors.Is(err, ledger.ErrForeignTx) {
			t.Fatalf("ReadRange with a foreign tx returned %v, want ledger.ErrForeignTx", err)
		}

		// And nothing was written on the side.
		if got := mustRead(ctx, t, led, fullWindow()); len(got) != 0 {
			t.Fatalf("a rejected foreign-tx append still wrote %d events", len(got))
		}
	})

	// A second instance's transaction is foreign too: in a pool-set
	// deployment one process holds a ledger.Ledger per pool, and crossing them
	// would write a tenant's usage into another pool's database.
	t.Run("AnotherLedgersTxIsForeign", func(t *testing.T) {
		led := newLedger(t)
		other := newLedger(t)
		ctx := context.Background()

		otherTx, err := other.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin on the second ledger: %v", err)
		}
		defer rollback(ctx, t, otherTx)

		err = led.Append(ctx, otherTx, eventAt(suiteStart, "cross-ledger"))
		if !errors.Is(err, ledger.ErrForeignTx) {
			t.Fatalf("Append with another ledger.Ledger's tx returned %v, want ledger.ErrForeignTx", err)
		}
	})
}

// runAppendAtomicity pins that a rejected event takes the whole call with
// it. A caller that emits a job's wall time, tokens and pages in one call
// must never get one of the three: a partial emission is worse than none,
// because it looks complete.
func runAppendAtomicity(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	good := eventAt(suiteStart, "atomic-good")
	bad := eventAt(suiteStart.Add(time.Minute), "atomic-bad")
	bad.Unit = "" // malformed

	tx := mustBegin(ctx, t, led)
	if err := led.Append(ctx, tx, good, bad); !errors.Is(err, domain.ErrInvalidEvent) {
		t.Fatalf("Append with one malformed event returned %v, want domain.ErrInvalidEvent", err)
	}

	stillInTx, err := led.ReadRange(ctx, tx, fullWindow())
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(stillInTx) != 0 {
		t.Fatalf("a rejected Append recorded %d of its events; it must be all-or-nothing", len(stillInTx))
	}

	// The transaction itself stays usable: a rejected append is a
	// validation failure, not a broken connection.
	if err := led.Append(ctx, tx, good); err != nil {
		t.Fatalf("Append after a rejected Append: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertEvents(t, mustRead(ctx, t, led, fullWindow()), []domain.UsageEvent{mustNormalize(t, good)})

	// Appending nothing is a no-op, not an error: a caller that built an
	// empty event list has nothing to meter.
	empty := mustBegin(ctx, t, led)
	if err := led.Append(ctx, empty); err != nil {
		t.Fatalf("Append of no events: %v", err)
	}
	if err := empty.Commit(ctx); err != nil {
		t.Fatalf("Commit of an empty append: %v", err)
	}
}

// runRangeSemantics pins the filter every adapter must implement
// identically — the one place where two adapters disagreeing would be a
// billing discrepancy rather than a test failure.
func runRangeSemantics(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	atFrom := eventAt(suiteStart, "at-from")
	inside := eventAt(suiteStart.Add(30*time.Minute), "inside")
	atTo := eventAt(suiteStart.Add(time.Hour), "at-to")

	otherTenant := eventAt(suiteStart.Add(10*time.Minute), "other-tenant")
	otherTenant.TenantID = "tenant-other"

	otherMetric := eventAt(suiteStart.Add(20*time.Minute), "other-metric")
	otherMetric.MetricID = "cas.api.requests"
	otherMetric.Shape = domain.ShapeEvent
	otherMetric.Unit = domain.UnitCount

	mustAppendCommit(ctx, t, led, atFrom, inside, atTo, otherTenant, otherMetric)

	hour := domain.Range{TenantID: suiteTenant, From: suiteStart, To: suiteStart.Add(time.Hour)}

	t.Run("HalfOpenWindow", func(t *testing.T) {
		got := mustRead(ctx, t, led, hour)
		if len(got) != 3 {
			t.Fatalf("read %d events in [From, To), want 3 (at-from, inside, other-metric)", len(got))
		}
		for _, e := range got {
			if e.SourceRef == "at-to" {
				t.Fatal("an event at To was included; the window must be half-open so consecutive windows do not double-count")
			}
		}
		if !containsRef(got, "at-from") {
			t.Fatal("an event at From was excluded; From is inclusive")
		}
	})

	t.Run("TenantScoped", func(t *testing.T) {
		got := mustRead(ctx, t, led, hour)
		for _, e := range got {
			if e.TenantID != suiteTenant {
				t.Fatalf("a read scoped to %q returned an event for %q", suiteTenant, e.TenantID)
			}
		}

		other := mustRead(ctx, t, led, domain.Range{TenantID: "tenant-other", From: suiteStart, To: suiteStart.Add(time.Hour)})
		if len(other) != 1 || other[0].SourceRef != "other-tenant" {
			t.Fatalf("read for the other tenant returned %+v, want just its own event", other)
		}
	})

	t.Run("MetricFilter", func(t *testing.T) {
		filtered := hour
		filtered.MetricID = "cas.api.requests"
		got := mustRead(ctx, t, led, filtered)
		if len(got) != 1 || got[0].SourceRef != "other-metric" {
			t.Fatalf("metric-filtered read returned %+v, want just the matching metric", got)
		}
	})

	t.Run("LimitTruncatesAfterOrdering", func(t *testing.T) {
		limited := hour
		limited.Limit = 2
		got := mustRead(ctx, t, led, limited)
		if len(got) != 2 {
			t.Fatalf("read with Limit=2 returned %d events", len(got))
		}
		full := mustRead(ctx, t, led, hour)
		assertEvents(t, got, full[:2])
	})
}

func runInvalidRange(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	tx := mustBegin(ctx, t, led)
	defer rollback(ctx, t, tx)

	cases := []struct {
		name string
		r    domain.Range
	}{
		{"no tenant", domain.Range{From: suiteStart, To: suiteStart.Add(time.Hour)}},
		{"zero from", domain.Range{TenantID: suiteTenant, To: suiteStart.Add(time.Hour)}},
		{"zero to", domain.Range{TenantID: suiteTenant, From: suiteStart}},
		{"inverted window", domain.Range{TenantID: suiteTenant, From: suiteStart.Add(time.Hour), To: suiteStart}},
		{"empty window", domain.Range{TenantID: suiteTenant, From: suiteStart, To: suiteStart}},
		{"negative limit", domain.Range{TenantID: suiteTenant, From: suiteStart, To: suiteStart.Add(time.Hour), Limit: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := led.ReadRange(ctx, tx, tc.r); !errors.Is(err, domain.ErrInvalidRange) {
				t.Fatalf("ReadRange error = %v, want domain.ErrInvalidRange", err)
			}
		})
	}
}

// NoDimensionValuesLeakInErrors is this module's required canary, the
// analogue of cas's NoPayloadContentLeaksInErrors. A usage event is not
// payload, but its attribution dimensions are a document id, a principal
// id, an agent run id or a matter reference — the tenant's business, not
// the operator's log file's. Validation is exactly the moment a careless
// error message copies an untrusted value into a string that is logged,
// aggregated and retained, so every adapter is made to prove it does not.
func runNoDimensionValueLeak(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	led := newLedger(t)
	ctx := context.Background()

	const canary = "canary-4b19e7-must-not-appear-in-any-error"

	tx := mustBegin(ctx, t, led)
	defer rollback(ctx, t, tx)

	cases := []struct {
		name string
		dims domain.Dimensions
	}{
		{"malformed key", domain.Dimensions{"Not A Key": canary}},
		{"over-long value", domain.Dimensions{domain.DimensionDocumentID: canary + strings.Repeat("x", domain.MaxDimensionValueLen)}},
		{"control character in value", domain.Dimensions{domain.DimensionDocumentID: canary + "\x00"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := eventAt(suiteStart, "leak-"+tc.name)
			e.Dimensions = tc.dims

			err := led.Append(ctx, tx, e)
			if err == nil {
				t.Fatal("Append of a malformed dimension set returned nil error")
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("Append error leaked a dimension value: %v", err)
			}
		})
	}

	// A conflict error names the idempotency key, which carries no
	// dimension value by construction — but an adapter that rendered the
	// whole event into the message would leak one, so the canary is
	// checked on that path too.
	t.Run("conflict error", func(t *testing.T) {
		led := newLedger(t)
		ctx := context.Background()

		first := eventAt(suiteStart, "leak-conflict")
		first.Dimensions = domain.Dimensions{domain.DimensionDocumentID: canary}
		mustAppendCommit(ctx, t, led, first)

		conflicting := first
		conflicting.Quantity = first.Quantity + 1

		tx := mustBegin(ctx, t, led)
		err := led.Append(ctx, tx, conflicting)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			rollback(ctx, t, tx)
		}
		if err == nil {
			t.Fatal("conflicting re-emission returned nil error")
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("conflict error leaked a dimension value: %v", err)
		}
	})
}

// suiteTenant and suiteStart are the fixed tenant and instant the suite
// builds its events from. The instant is in the past and on an exact hour,
// so a window's boundaries are easy to read in a failure message, and it is
// fixed rather than time.Now so a failure is reproducible.
const suiteTenant domain.TenantID = "tenant-suite"

var suiteStart = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// fullWindow is a window wide enough to hold every event the suite
// appends.
func fullWindow() domain.Range {
	return domain.Range{TenantID: suiteTenant, From: suiteStart.Add(-time.Hour), To: suiteStart.Add(24 * time.Hour)}
}

// eventAt returns a valid gauge-delta event at the given instant with the
// given source ref. Subtests override whichever fields they are about,
// rather than restating nine fields each time.
func eventAt(at time.Time, sourceRef string) domain.UsageEvent {
	return domain.UsageEvent{
		TenantID:   suiteTenant,
		MetricID:   "cas.blob.bytes_stored",
		Shape:      domain.ShapeGaugeDelta,
		Unit:       domain.UnitByte,
		Quantity:   4096,
		OccurredAt: at,
		Source:     "cas.blob.store",
		SourceRef:  sourceRef,
	}
}

// foreignTx is a ledger.Tx no adapter produced: the handle an adapter must refuse
// rather than ignore.
type foreignTx struct{}

func (foreignTx) LedgerTx() {}

func mustNormalize(t *testing.T, e domain.UsageEvent) domain.UsageEvent {
	t.Helper()
	n, err := e.Normalize()
	if err != nil {
		t.Fatalf("Normalize of a suite fixture: %v", err)
	}
	return n
}

func mustBegin(ctx context.Context, t *testing.T, led ledger.Ledger) ledger.OwnedTx {
	t.Helper()
	tx, err := led.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return tx
}

// mustAppendCommit appends events in one transaction and commits it.
func mustAppendCommit(ctx context.Context, t *testing.T, led ledger.Ledger, events ...domain.UsageEvent) {
	t.Helper()
	tx := mustBegin(ctx, t, led)
	if err := led.Append(ctx, tx, events...); err != nil {
		rollback(ctx, t, tx)
		t.Fatalf("Append: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// mustRead reads r in a transaction of its own and rolls it back.
func mustRead(ctx context.Context, t *testing.T, led ledger.Ledger, r domain.Range) []domain.UsageEvent {
	t.Helper()
	tx := mustBegin(ctx, t, led)
	defer rollback(ctx, t, tx)
	got, err := led.ReadRange(ctx, tx, r)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	return got
}

// rollback rolls tx back and fails the test if that errors, so a deferred
// rollback cannot swallow a real adapter fault. ledger.ErrTxDone is tolerated:
// callers defer this after paths that may already have committed.
func rollback(ctx context.Context, t *testing.T, tx ledger.OwnedTx) {
	t.Helper()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, ledger.ErrTxDone) {
		t.Errorf("Rollback: %v", err)
	}
}

// assertTxDone checks that every method rejects a finished transaction.
func assertTxDone(ctx context.Context, t *testing.T, led ledger.Ledger, tx ledger.OwnedTx, what string) {
	t.Helper()

	if err := led.Append(ctx, tx, eventAt(suiteStart, "after-finish")); !errors.Is(err, ledger.ErrTxDone) {
		t.Errorf("Append on %s returned %v, want ledger.ErrTxDone", what, err)
	}
	if _, err := led.ReadRange(ctx, tx, fullWindow()); !errors.Is(err, ledger.ErrTxDone) {
		t.Errorf("ReadRange on %s returned %v, want ledger.ErrTxDone", what, err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, ledger.ErrTxDone) {
		t.Errorf("Commit on %s returned %v, want ledger.ErrTxDone", what, err)
	}
	if err := tx.Rollback(ctx); !errors.Is(err, ledger.ErrTxDone) {
		t.Errorf("Rollback on %s returned %v, want ledger.ErrTxDone", what, err)
	}
}

// assertEvents compares two event sequences field by field, including
// dimensions and the exact instant, and reports the first difference.
func assertEvents(t *testing.T, got, want []domain.UsageEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read %d events, want %d\n got  %s\n want %s", len(got), len(want), refsOf(got), refsOf(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		switch {
		case g.TenantID != w.TenantID,
			g.MetricID != w.MetricID,
			g.Shape != w.Shape,
			g.Unit != w.Unit,
			g.Quantity != w.Quantity,
			g.Source != w.Source,
			g.SourceRef != w.SourceRef:
			t.Fatalf("event %d differs:\n got  %+v\n want %+v", i, g, w)
		case !g.OccurredAt.Equal(w.OccurredAt):
			t.Fatalf("event %d OccurredAt = %v, want %v", i, g.OccurredAt, w.OccurredAt)
		case !g.Dimensions.Equal(w.Dimensions):
			t.Fatalf("event %d dimension keys = %v, want %v", i, g.Dimensions.Keys(), w.Dimensions.Keys())
		}
	}
}

// sumOf totals a result's quantities: an event metric's usage, or a gauge
// metric's level.
func sumOf(events []domain.UsageEvent) domain.Quantity {
	var sum domain.Quantity
	for _, e := range events {
		sum += e.Quantity
	}
	return sum
}

func containsRef(events []domain.UsageEvent, ref string) bool {
	return slices.ContainsFunc(events, func(e domain.UsageEvent) bool { return e.SourceRef == ref })
}

// refsOf renders a result's source refs for a failure message. Source refs
// are code-path identifiers, never dimension values, so they are safe to
// print.
func refsOf(events []domain.UsageEvent) string {
	refs := make([]string, 0, len(events))
	for _, e := range events {
		refs = append(refs, e.SourceRef)
	}
	return "[" + strings.Join(refs, " ") + "]"
}
