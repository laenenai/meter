# meter

[![ci](https://github.com/laenenai/meter/actions/workflows/ci.yml/badge.svg)](https://github.com/laenenai/meter/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/laenenai/meter.svg)](https://pkg.go.dev/github.com/laenenai/meter)

A usage ledger in stable physical units, written inside the caller's
transaction.

Metering that cannot be reconstructed after the fact has to be built from the
first commit. This records what happened, in units that stay meaningful, so
that rating and billing can be added later without rebuilding history.

> **Status: pre-1.0.** The domain model, the ledger port and an in-memory
> adapter are implemented and tested. There is no Postgres adapter yet. The
> API may change before v1.

## Install

```
go get github.com/laenenai/meter
```

Requires Go 1.25.6 or later. No cgo, and **no third-party dependencies.**

## Example

```go
l := ledger.NewMemory()

tx, err := l.Begin(ctx)
if err != nil {
	return err
}
defer func() { _ = tx.Rollback(ctx) }()

ev, err := meter.UsageEvent{
	TenantID:   meter.TenantID("tenant-42"),
	MetricID:   meter.MetricID("docs.pages.processed"),
	Shape:      meter.ShapeEvent,
	Unit:       meter.UnitPage,
	Quantity:   12,
	OccurredAt: time.Now(),
	Source:     "ocr-worker",
	SourceRef:  "job:9f3a",
	Dimensions: meter.Dimensions{meter.DimensionJobID: "9f3a"},
}.Normalize()
if err != nil {
	return err
}

if err := l.Append(ctx, tx, ev); err != nil {
	return err
}
return tx.Commit(ctx)
```

In production the transaction is **yours** — the one carrying the work being
metered — so the usage row commits with the thing it measures, and the system
cannot produce work that was never billed or bills for work that never
happened. `Begin` exists for callers with no database of their own, for
backfills, and for tests.

## Design notes worth knowing before you use it

**Quantity is an integer, never a float.** A ledger exists to be summed, and
float addition is not associative, so two rollups over the same rows in
different orders would disagree invisibly. Express fractions by choosing a
finer unit: publish wall time in milliseconds, not seconds.

**Two metric shapes.** `ShapeEvent` for things that happened and sum forward;
`ShapeGaugeDelta` for signed changes whose cumulative sum is a level. Which one
a metric is cannot be recovered from the rows later, so it is part of the
event. `ShapeUnspecified` is the zero value and is always invalid — an unset
shape is rejected rather than guessed.

**The idempotency key deliberately excludes the quantity.** A repeat with the
same content is absorbed; a repeat that disagrees returns
`ErrIdempotencyConflict`. If quantity were part of the key, a bug that doubled
a charge would write two rows that both look legitimate — excluding it turns
the same bug into something detectable.

**`Append` requires a transaction**, and an adapter must *refuse* a handle it
does not recognise rather than quietly writing on its own connection. The
silent version of that bug is undetectable downstream, because the row is there
and looks correct.

## What it does not do

Rating, pricing, invoicing, quota enforcement, rollups, retention or export.
All of those are downstream of the ledger, and two of them are explicitly out
of scope.

## Documentation

- [`CHARTER.md`](CHARTER.md) — the boundary and the two mechanisms that enforce it
- `api.txt` — the public surface as a golden file, diffed in CI

## License

Apache-2.0. See [`LICENSE`](LICENSE).
