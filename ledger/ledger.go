// Package ledger defines the append-only usage ledger that is the whole of
// what meter does: Ledger.Append records a domain.UsageEvent inside a unit
// of work the caller supplies, and Ledger.ReadRange reads a bounded,
// tenant-scoped window back for a rollup. Every counter, rollup, quota
// figure and invoice line any consumer ever shows is derived from these
// rows; none of that derivation lives here, by charter.
//
// The one structural decision this package exists to enforce is that a
// usage event commits with the work it meters. Append therefore takes a Tx
// and cannot be called without one: a blob that was stored is metered, a
// job that did not run is not, and a worker that crashed mid-transaction
// cannot leave behind a charge for work that was rolled back. Nothing in
// this module meters from a log line, a middleware or a best-effort
// goroutine, because each of those can succeed when the work failed or fail
// when the work succeeded, and both directions are unrecoverable: an
// undercount is lost revenue nobody notices, and an overcount is a customer
// dispute with no evidence to settle it.
//
// Idempotency is the second half of the same guarantee. A unit of work can
// be retried, so the ledger deduplicates on
// domain.UsageEvent.IdempotencyKey — appending an event whose key is
// already recorded counts once. See that type for the key's composition and
// for which fields are deliberately outside it.
//
// This package is public, not internal, for a reason the charter states:
// a consumer counts as having adopted this module when the conformance
// suite runs green in that consumer's CI against its own wiring. A suite
// behind an internal package could not be run by the people it exists to
// hold to account.
//
// That suite is metertest.RunLedgerSuite, in the sibling package
// github.com/laenenai/meter/metertest, and it — not this file — is the
// authoritative description of the contract's edge-case behaviour: the
// exact boundary semantics of a window, what a duplicate key does when its
// quantity disagrees, what a rolled-back transaction must leave behind, and
// what a range read must look like while other transactions are appending.
// It lives beside this package rather than in it so that importing the port
// does not drag testing, and its flag registrations, into a consumer's
// production binary — the relationship httptest has to http.
//
// No error returned by any Ledger method, and no message logged by an
// adapter, may contain a dimension value. A dimension value is a document
// id, a principal id, an agent run id or a matter reference; a usage event
// is not payload, but its attribution is still the tenant's business and
// not the operator's log file's. See metertest's
// NoDimensionValuesLeakInErrors subtest, which every adapter exercises.
package ledger

import (
	"context"
	"errors"

	"github.com/laenenai/meter/internal/domain"
)

// ErrNoTx is returned by Append and ReadRange when tx is nil. There is no
// implicit, auto-committing path into the ledger: an emission that is not
// part of a unit of work is exactly the best-effort metering this module
// exists to rule out, so the absence of a transaction is an error rather
// than a fallback. A caller with no transaction of its own calls Begin.
var ErrNoTx = errors.New("meter: no transaction")

// ErrForeignTx is returned by Append and ReadRange when tx is a Tx this
// Ledger did not produce and does not recognise — another adapter's
// transaction, or another instance's.
//
// Every adapter must return this rather than ignoring the handle and
// writing on its own connection. Silently committing outside the caller's
// transaction would produce exactly the uncorrelated charge the port's
// shape is designed to make impossible, and it would do so invisibly:
// the append succeeds, the row is there, and nothing downstream can tell
// that it was not atomic with the work.
var ErrForeignTx = errors.New("meter: foreign transaction")

// ErrTxDone is returned when a Tx has already been committed or rolled
// back: by Append or ReadRange given such a handle, and by Commit or
// Rollback called a second time. It mirrors database/sql's ErrTxDone and
// exists so a double-commit in a retry loop is an error rather than a
// second set of rows.
var ErrTxDone = errors.New("meter: transaction already committed or rolled back")

// ErrIdempotencyConflict is returned when an event's IdempotencyKey is
// already recorded but the stored event measures something different — a
// different quantity, unit or metric shape under the same key (see
// domain.UsageEvent.SameBillableContent).
//
// A repeat of an identical event is a retry and is silently absorbed; this
// error is the other case, and it is deliberately loud. Two different
// measurements emitted under one key means the emitting code derived its
// SourceRef from something that does not identify the fact it is metering —
// a loop counter, a clock, a reused request id. Picking a winner would
// leave a wrong number in a billing row that no rollup or reconciliation
// could ever flag, so the append fails and the caller's transaction rolls
// back with the work it was metering.
var ErrIdempotencyConflict = errors.New("meter: idempotency conflict")

// Tx is a unit of work that an Append or a ReadRange joins. Its only method
// is a marker, never called by this module.
//
// The marker is what makes Tx a nominal type rather than `any`, and that is
// the point: a caller cannot pass a *sql.Tx, a pgx.Tx or a stray nil
// straight into Append and have it compile. An adapter instead wraps the
// caller's transaction in its own type — which is also where the adapter
// keeps the concrete handle it needs to issue statements — and that wrapper
// is what the caller passes.
//
// The honest limitation, stated here rather than left to be discovered:
// this interface cannot prove, at compile time, that the transaction handed
// to Append is the same transaction the caller's own state change is using.
// Go has no way to express that; a module that does not own the caller's
// database cannot. What the port can and does guarantee is that an append
// is impossible without *some* declared unit of work (ErrNoTx), and that an
// adapter must refuse a handle it does not recognise instead of quietly
// writing outside it (ErrForeignTx, pinned by the conformance suite). The
// remaining step — that the handle the caller wraps is the transaction the
// caller is committing its work in — is one line at the call site, and it
// is the caller's to get right.
type Tx interface {
	// LedgerTx marks a deliberate Tx implementation. It is never called.
	LedgerTx()
}

// OwnedTx is a Tx whose lifetime this Ledger controls, as returned by
// Begin. It is a separate interface from Tx because in the arrangement this
// module is built for, the caller owns the transaction and commits it
// itself — an adapter's wrapper around a caller-owned *sql.Tx implements Tx
// and nothing more, so Commit and Rollback are not on the interface Append
// accepts and cannot be called on a transaction that is not this module's
// to finish.
type OwnedTx interface {
	Tx

	// Commit makes this transaction's appends durable. Committing an
	// already-finished transaction returns ErrTxDone.
	//
	// Commit may return ErrIdempotencyConflict: a duplicate key that was
	// not visible when Append ran — because another transaction committed
	// it in the meantime — is detected here instead. An adapter backed by a
	// unique constraint gets this behaviour from the database rather than
	// from a check it has to remember to write.
	Commit(ctx context.Context) error

	// Rollback discards this transaction's appends. Rolling back an
	// already-finished transaction returns ErrTxDone.
	//
	// A rolled-back append leaves nothing behind, including no reservation
	// of its idempotency key: the same event must still be appendable
	// afterwards, because the work it meters may well be retried and
	// succeed. Rollback is safe to defer unconditionally after a Commit.
	Rollback(ctx context.Context) error
}

// Ledger is the port every usage-ledger adapter implements: the in-memory
// adapter in this package, and the Postgres adapter a later increment adds.
// See the package doc comment and metertest.RunLedgerSuite for the full
// contract.
type Ledger interface {
	// Begin starts a unit of work this Ledger owns.
	//
	// It is not the production path for a consumer that has its own
	// database: such a consumer passes its own transaction, wrapped by the
	// adapter, to Append, so that the usage event and the state change
	// commit together. Begin exists for the two cases where that is not
	// possible — a consumer whose metered work is not a database write at
	// all, and a backfill or import that has no state change to join — and
	// so that the conformance suite can exercise transactional behaviour
	// without knowing any adapter's concrete handle.
	Begin(ctx context.Context) (OwnedTx, error)

	// Append records events inside tx. It is all-or-nothing within the
	// call: if any event is rejected, none of the events in that call are
	// recorded, so a caller that emits a job's wall time, tokens and pages
	// together never gets one of the three. Appending no events at all is a
	// no-op and returns nil.
	//
	// Every event is validated and canonicalised with
	// domain.UsageEvent.Normalize on the way in, so what ReadRange returns
	// is the normalized form: OccurredAt in UTC at microsecond precision,
	// and an empty dimension set as nil.
	//
	// An event whose domain.IdempotencyKey is already recorded — in this
	// transaction or in a committed one — is absorbed silently if it
	// measures the same thing, and returns ErrIdempotencyConflict if it
	// does not. Appends within tx are visible to a later ReadRange in the
	// same tx before it commits.
	//
	// Errors: ErrNoTx if tx is nil, ErrForeignTx if tx came from another
	// Ledger, ErrTxDone if tx has finished, domain.ErrInvalidEvent or
	// domain.ErrNegativeQuantity for a malformed event, and
	// ErrIdempotencyConflict as described above.
	Append(ctx context.Context, tx Tx, events ...domain.UsageEvent) error

	// ReadRange returns the events matching r, ordered by
	// domain.CompareUsageEvents and truncated to r.Limit if one is set.
	// A window with no events returns an empty slice and a nil error: a
	// tenant that used nothing used nothing. The returned slice is the
	// caller's to retain and mutate.
	//
	// The read is scoped to tx, which makes it repeatable: two ReadRange
	// calls with the same r inside one tx return the same events even while
	// other transactions are committing appends into that window. A rollup
	// needs that — it reads a window, aggregates, and must not have the
	// window change underneath it — and it is what lets a reconciliation
	// compare a ledger-derived figure against authoritative state without
	// racing. Events appended within tx itself are visible.
	//
	// Errors: ErrNoTx, ErrForeignTx and ErrTxDone as for Append, and
	// domain.ErrInvalidRange for a malformed r.
	ReadRange(ctx context.Context, tx Tx, r domain.Range) ([]domain.UsageEvent, error)
}
