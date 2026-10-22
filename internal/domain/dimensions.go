package domain

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Dimensions are the attribution and descriptive dimensions carried by
// every usage event. The tenant is the billing entity; these are everything
// below and beside it, and they are the reason a ledger is worth more than
// a set of per-tenant counters: with them an operator can answer "what did
// this legal matter cost" and "what did this agent run cost", and a tenant
// can charge back internally. Without them that question is unanswerable
// after the fact, which is why they are recorded on every event rather than
// added when someone asks.
//
// Two distinct kinds of key live in one map, deliberately:
//
//   - Attribution refs — fileset, document, principal, agent run, job,
//     capability chain — answer "who or what incurred this".
//   - Descriptive dimensions — tier, lane, processor id and version, model
//     id, region — answer "under what conditions", and are what a rollup
//     groups by.
//
// They are not separated into two fields because no adapter, rollup or
// export treats them differently, and the boundary between them moves per
// consumer: cas's "document" is an attribution ref, kgs's "graph" is too,
// and neither module should have to know the other's noun. Keeping one open
// map is also what lets meter serve three consumers without closing the set
// to whichever one was written first; the well-known keys below exist so
// the three agree on spelling without this module owning the vocabulary.
//
// High cardinality is fine here, and that is worth stating because the
// opposite rule holds one layer away: these are ledger columns, where a
// document id or a principal id is the entire point, not metric labels on
// an observability instrument, where the same id would multiply time series
// until the metrics backend fell over. A consumer must not mirror these
// dimensions into OpenTelemetry attributes.
type Dimensions map[string]string

// The well-known dimension keys. The set is open — a consumer may use any
// key that satisfies the spelling rule in Validate — but a key that means
// one of these things must be spelled this way, because a rollup, a
// chargeback report and an export all group by the literal key.
const (
	// DimensionTier is the storage tier a byte figure applies to, e.g.
	// "hot", "warm", "cold", "inline". Required in practice on every
	// gauge-delta byte metric: a level is per tier, so a delta without a
	// tier cannot be summed into anything meaningful.
	DimensionTier = "tier"
	// DimensionLane is the scheduling lane work ran in, e.g.
	// "interactive", "bulk".
	DimensionLane = "lane"
	// DimensionProcessorID names the processor that performed the work.
	DimensionProcessorID = "processor_id"
	// DimensionProcessorVersion is that processor's version, recorded
	// separately so a cost change can be attributed to a version bump.
	DimensionProcessorVersion = "processor_version"
	// DimensionModelID names the model a token or embedding figure is for.
	DimensionModelID = "model_id"
	// DimensionRegion is the region the work or the bytes live in.
	DimensionRegion = "region"
	// DimensionPrincipalID is the principal that caused the usage.
	DimensionPrincipalID = "principal_id"
	// DimensionCapabilityChain identifies the credential chain the request
	// arrived with, so usage can be traced to a delegation.
	DimensionCapabilityChain = "capability_chain"
	// DimensionFilesetID attributes usage to a fileset.
	DimensionFilesetID = "fileset_id"
	// DimensionDocumentID attributes usage to a document.
	DimensionDocumentID = "document_id"
	// DimensionAgentRunID attributes usage to one agent run — the
	// "what did this agent run cost" question.
	DimensionAgentRunID = "agent_run_id"
	// DimensionJobID attributes usage to one asynchronous job.
	DimensionJobID = "job_id"
)

// The bounds Validate enforces. They are generous for any real attribution
// set and exist so a malformed or hostile caller cannot push an unbounded
// blob into a billing row that is retained for ninety days and exported to
// a third party.
const (
	// MaxDimensions bounds how many dimensions one event may carry.
	MaxDimensions = 32
	// MaxDimensionKeyLen bounds a single dimension key.
	MaxDimensionKeyLen = 64
	// MaxDimensionValueLen bounds a single dimension value.
	MaxDimensionValueLen = 256
)

// canonicalSeparator separates the key=value pairs in Canonical. It is the
// ASCII unit separator, which Validate rejects in both keys and values, so
// it cannot occur inside a pair.
const canonicalSeparator = "\x1f"

// Validate reports whether d is well formed, returning an error wrapping
// ErrInvalidEvent otherwise. A nil or empty set is valid: attribution is
// optional, only the tenant is not.
//
// The rules, and why each one is a rejection rather than a repair:
//
//   - A key is lowercase ASCII letters, digits, '_', '.' and '-', at most
//     MaxDimensionKeyLen bytes, and not empty. Rejecting rather than
//     lower-casing keeps "JobID" from quietly becoming a second spelling of
//     "job_id" in a rollup.
//   - A value is at most MaxDimensionValueLen bytes, not empty, and free of
//     control characters. An absent dimension is an absent key, not a key
//     with an empty value, so that two events that attribute to the same
//     things compare equal regardless of which code path built them.
//   - At most MaxDimensions keys.
//
// The returned error names the offending key and never the offending
// value. A dimension value is a document id, a principal id, a matter
// reference or an agent run id; CONVENTIONS forbids sensitive payload in an
// error or a log line, and a validation failure is exactly the moment a
// careless message would copy an untrusted value into one. The conformance
// suite asserts this for every adapter.
func (d Dimensions) Validate() error {
	if len(d) > MaxDimensions {
		return fmt.Errorf("%w: %d dimensions exceeds the maximum of %d", ErrInvalidEvent, len(d), MaxDimensions)
	}
	for k, v := range d {
		if err := validateDimensionKey(k); err != nil {
			return err
		}
		if v == "" {
			return fmt.Errorf("%w: dimension %q has an empty value; omit the key instead", ErrInvalidEvent, k)
		}
		if len(v) > MaxDimensionValueLen {
			return fmt.Errorf("%w: dimension %q value length %d exceeds %d",
				ErrInvalidEvent, k, len(v), MaxDimensionValueLen)
		}
		if i := strings.IndexFunc(v, isControl); i >= 0 {
			return fmt.Errorf("%w: dimension %q value contains a control character at byte %d",
				ErrInvalidEvent, k, i)
		}
	}
	return nil
}

// Clone returns an independent copy, or nil for a nil receiver so an absent
// dimension set never becomes an empty one. Normalize clones on the way
// into the ledger: without it a caller that reused and then mutated its map
// would retroactively change a committed billing row.
func (d Dimensions) Clone() Dimensions {
	if d == nil {
		return nil
	}
	return maps.Clone(d)
}

// Equal reports whether d and other hold the same key/value pairs, treating
// nil and empty as equal.
func (d Dimensions) Equal(other Dimensions) bool {
	return maps.Equal(d, other)
}

// Keys returns d's keys in ascending order.
func (d Dimensions) Keys() []string {
	return slices.Sorted(maps.Keys(d))
}

// Canonical returns a deterministic, injective encoding of d: the pairs
// sorted by key, rendered "key=value", joined by the ASCII unit separator.
// nil and empty both encode to the empty string.
//
// Two different dimension sets never produce the same string, because
// Validate rejects '=' in a key and the unit separator in both keys and
// values — so neither the pair boundary nor the key/value boundary can be
// forged by content. That injectivity is load-bearing for an adapter that
// compacts a counter metric into a per-(tenant, metric, dimensions, minute)
// bucket: the bucket is keyed on this string, so a collision would merge
// two tenants' or two matters' usage into one row, and no later
// reconciliation could separate them again.
func (d Dimensions) Canonical() string {
	if len(d) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(d))
	for _, k := range d.Keys() {
		pairs = append(pairs, k+"="+d[k])
	}
	return strings.Join(pairs, canonicalSeparator)
}

// validateDimensionKey enforces the key spelling rule. See Validate.
func validateDimensionKey(k string) error {
	if k == "" {
		return fmt.Errorf("%w: dimension key is empty", ErrInvalidEvent)
	}
	if len(k) > MaxDimensionKeyLen {
		return fmt.Errorf("%w: dimension key %q length %d exceeds %d",
			ErrInvalidEvent, k, len(k), MaxDimensionKeyLen)
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '_', r == '.', r == '-':
		default:
			return fmt.Errorf("%w: dimension key %q contains %q, want lowercase ASCII, digits, '_', '.' or '-'",
				ErrInvalidEvent, k, r)
		}
	}
	return nil
}
