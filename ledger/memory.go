package ledger

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/laenenai/meter/internal/domain"
)

// Memory is an in-memory Ledger: the adapter a consumer's unit tests wire
// in so the core can be exercised without a database, and the reference
// implementation the conformance suite is developed against.
//
// It is not a production ledger. It holds every event for the process's
// lifetime with no retention, no partitioning and no durability, so a
// restart loses the ledger — which for a billing record is the one failure
// that cannot be repaired. Production use is the Postgres adapter, where
// the transaction Append joins is the caller's own and the uniqueness
// constraint that makes emission idempotent is an index rather than a map.
//
// What it does take seriously is the *semantics*, because that is what
// metertest.RunLedgerSuite pins and what a consumer's tests will come to rely on:
//
//   - A transaction is a snapshot taken at Begin plus that transaction's
//     own pending appends, so a read inside one transaction is repeatable
//     while others commit — the behaviour Postgres gives a REPEATABLE READ
//     transaction, implemented here by copying rather than by MVCC.
//   - Idempotency is checked twice: against the snapshot when Append runs,
//     and again against live state at Commit, because another transaction
//     may have committed the same key in between. A Postgres adapter gets
//     the second check from its unique index; here it has to be written,
//     and leaving it out would make concurrent retries of one event count
//     twice in exactly the race the suite provokes.
//
// Memory is safe for concurrent use. A single OwnedTx is not: it belongs to
// one unit of work, as a *sql.Tx does.
type Memory struct {
	mu sync.Mutex
	// byKey is the committed ledger, indexed by the uniqueness constraint
	// that makes emission idempotent. A map rather than a slice because
	// that constraint, not insertion order, is what identifies an event;
	// order is imposed on read by domain.CompareUsageEvents.
	byKey map[domain.IdempotencyKey]domain.UsageEvent
}

// NewMemory returns an empty in-memory Ledger.
func NewMemory() *Memory {
	return &Memory{byKey: make(map[domain.IdempotencyKey]domain.UsageEvent)}
}

var _ Ledger = (*Memory)(nil)

// memoryTx is one unit of work against a Memory: the snapshot it reads
// from, and the appends it will contribute on commit.
type memoryTx struct {
	// led is the Ledger that produced this transaction. Append compares
	// against it so another instance's transaction is ErrForeignTx rather
	// than a write into the wrong ledger — in a pool-set deployment one
	// process holds one Ledger per pool.
	led *Memory

	// snapshot is the committed state as of Begin, and pending the appends
	// this transaction has made. A read is snapshot overlaid with pending;
	// pending wins, though it can only ever add, since an append whose key
	// is already in snapshot is either absorbed or rejected.
	snapshot map[domain.IdempotencyKey]domain.UsageEvent
	pending  map[domain.IdempotencyKey]domain.UsageEvent

	done bool
}

// LedgerTx marks memoryTx as a Tx. See Tx.
func (*memoryTx) LedgerTx() {}

var _ OwnedTx = (*memoryTx)(nil)

// Begin starts a unit of work, capturing the committed ledger as this
// transaction's snapshot. See Ledger.Begin — for a consumer with its own
// database this is not the production path.
func (m *Memory) Begin(_ context.Context) (OwnedTx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &memoryTx{
		led:      m,
		snapshot: maps.Clone(m.byKey),
		pending:  make(map[domain.IdempotencyKey]domain.UsageEvent),
	}, nil
}

// Append records events inside tx. See Ledger.Append for the contract.
func (m *Memory) Append(_ context.Context, tx Tx, events ...domain.UsageEvent) error {
	mtx, err := m.unwrap(tx)
	if err != nil {
		return err
	}

	// Validate and resolve every event before recording any of them, so a
	// rejected event takes the whole call with it: a caller that emits a
	// job's wall time, tokens and pages together must never get one of the
	// three. A rejected call leaves tx usable.
	accepted := make(map[domain.IdempotencyKey]domain.UsageEvent, len(events))
	for i, e := range events {
		normalized, err := e.Normalize()
		if err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		key := normalized.IdempotencyKey()

		// Within one call, a repeated key is the same retry case as across
		// calls: absorb an identical measurement, reject a differing one.
		if prior, ok := accepted[key]; ok {
			if !prior.SameBillableContent(normalized) {
				return conflict(key)
			}
			continue
		}
		if prior, ok := mtx.lookup(key); ok {
			if !prior.SameBillableContent(normalized) {
				return conflict(key)
			}
			continue
		}
		accepted[key] = normalized
	}

	maps.Copy(mtx.pending, accepted)
	return nil
}

// ReadRange returns the events matching r as of tx's snapshot, plus tx's
// own pending appends. See Ledger.ReadRange for the contract.
func (m *Memory) ReadRange(_ context.Context, tx Tx, r domain.Range) ([]domain.UsageEvent, error) {
	mtx, err := m.unwrap(tx)
	if err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}

	// The two sources are disjoint by construction: Append never adds a key
	// to pending that is already in snapshot.
	out := make([]domain.UsageEvent, 0, len(mtx.pending))
	for _, src := range []map[domain.IdempotencyKey]domain.UsageEvent{mtx.snapshot, mtx.pending} {
		for _, e := range src {
			if r.Matches(e) {
				// Clone the dimension map on the way out as well as on the
				// way in: a caller that mutates what it read must not be
				// able to change the stored event either.
				e.Dimensions = e.Dimensions.Clone()
				out = append(out, e)
			}
		}
	}

	slices.SortFunc(out, domain.CompareUsageEvents)
	if r.Limit > 0 && len(out) > r.Limit {
		out = out[:r.Limit]
	}
	return out, nil
}

// unwrap resolves tx to this Memory's own transaction type, rejecting a
// nil handle, a handle from another Ledger, and a finished one. Every
// Memory method starts here, which is what makes "an adapter must never
// quietly write outside the caller's transaction" a property of the code
// rather than of remembering to check.
func (m *Memory) unwrap(tx Tx) (*memoryTx, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	mtx, ok := tx.(*memoryTx)
	if !ok || mtx == nil || mtx.led != m {
		return nil, ErrForeignTx
	}
	if mtx.done {
		return nil, ErrTxDone
	}
	return mtx, nil
}

// lookup returns the event already recorded under key, from this
// transaction's own pending appends or from its snapshot.
func (tx *memoryTx) lookup(key domain.IdempotencyKey) (domain.UsageEvent, bool) {
	if e, ok := tx.pending[key]; ok {
		return e, true
	}
	e, ok := tx.snapshot[key]
	return e, ok
}

// Commit makes this transaction's appends durable. See OwnedTx.Commit.
//
// The idempotency check runs again here against live state, not against
// the snapshot: between Begin and Commit another transaction may have
// committed the same key. An identical measurement is absorbed — concurrent
// retries of one event count once, which is the whole point — and a
// differing one fails the commit, leaving the ledger untouched.
func (tx *memoryTx) Commit(_ context.Context) error {
	tx.led.mu.Lock()
	defer tx.led.mu.Unlock()

	if tx.done {
		return ErrTxDone
	}

	for key, e := range tx.pending {
		if prior, ok := tx.led.byKey[key]; ok && !prior.SameBillableContent(e) {
			return conflict(key)
		}
	}
	for key, e := range tx.pending {
		if _, ok := tx.led.byKey[key]; ok {
			// First write wins: an identical re-emission changes nothing,
			// including the first write's occurred-at and dimensions.
			continue
		}
		tx.led.byKey[key] = e
	}

	tx.done = true
	tx.pending = nil
	tx.snapshot = nil
	return nil
}

// Rollback discards this transaction's appends. See OwnedTx.Rollback — in
// particular, it reserves nothing, so the same event is still appendable
// afterwards when the work it meters is retried.
func (tx *memoryTx) Rollback(_ context.Context) error {
	tx.led.mu.Lock()
	defer tx.led.mu.Unlock()

	if tx.done {
		return ErrTxDone
	}
	tx.done = true
	tx.pending = nil
	tx.snapshot = nil
	return nil
}

// conflict builds the ErrIdempotencyConflict error. It names the
// idempotency key and nothing else: a key is a tenant, a metric, a source
// and a source ref, all of them code-path identifiers, so it carries no
// dimension value and no quantity and is always safe to log.
func conflict(key domain.IdempotencyKey) error {
	return fmt.Errorf("%w: %s is already recorded with a different measurement", ErrIdempotencyConflict, key)
}
