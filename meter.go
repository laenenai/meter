// Package meter is the public facade for the usage ledger: an append-only
// record of physical usage in stable units, written inside the transaction
// that performed the work it meters.
//
// Rating, pricing, invoicing and quota enforcement are not here and must
// never be, per CHARTER.md. What is here is the measurement, because a
// measurement that was never recorded cannot be reconstructed afterwards
// while a price can always be applied to a recorded measurement later.
//
// Every domain entity and value object lives in internal/domain and nowhere
// else; this file re-exports the ones a consumer needs to name as type
// aliases, so an importer of github.com/laenen-ai/meter never reaches into
// an internal package (Go's internal rule would refuse that anyway).
// Aliasing also means a value internal/domain produces and a value a
// consumer holds are the identical type, not a copy that needs converting
// at the boundary.
//
// This file declares aliases, re-exported constants and re-exported
// sentinel errors only — no behaviour and no constructors. The port itself,
// its conformance suite and its adapters live in
// github.com/laenen-ai/meter/ledger, which is a separate, non-internal
// package because the charter makes adoption mean "this module's suite runs
// green in the consumer's CI": a suite behind an internal package could not
// be run by the people it exists to hold to account.
package meter

import (
	"github.com/laenen-ai/meter/internal/domain"
	"github.com/laenen-ai/meter/ledger"
)

type (
	// TenantID identifies the billing entity. See domain.TenantID.
	TenantID = domain.TenantID
	// MetricID names an entry in the consumer's metric catalog. See domain.MetricID.
	MetricID = domain.MetricID
	// Unit is the physical unit a Quantity is expressed in. See domain.Unit.
	Unit = domain.Unit
	// Quantity is an exact integer measurement. See domain.Quantity.
	Quantity = domain.Quantity
	// MetricShape is one of the two shapes a metric can have. See domain.MetricShape.
	MetricShape = domain.MetricShape

	// Dimensions are the attribution dimensions below the tenant. See domain.Dimensions.
	Dimensions = domain.Dimensions

	// UsageEvent is one physical measurement. See domain.UsageEvent.
	UsageEvent = domain.UsageEvent
	// IdempotencyKey is the tuple that makes emission idempotent. See domain.IdempotencyKey.
	IdempotencyKey = domain.IdempotencyKey

	// Range selects the events a rollup reads. See domain.Range.
	Range = domain.Range

	// Ledger is the port every usage-ledger adapter implements. See
	// ledger.Ledger. Constructing one, and running the conformance suite
	// against it, is done through the ledger package itself; this alias
	// exists so a consumer can declare a dependency on the port without a
	// second import.
	Ledger = ledger.Ledger
	// Tx is a unit of work an Append joins. See ledger.Tx.
	Tx = ledger.Tx
	// OwnedTx is a Tx this module began itself. See ledger.OwnedTx.
	OwnedTx = ledger.OwnedTx
)

// The two metric shapes. See domain.MetricShape.
const (
	// ShapeUnspecified is the zero value and never valid. See domain.ShapeUnspecified.
	ShapeUnspecified = domain.ShapeUnspecified
	// ShapeEvent is a metric counted as it happens. See domain.ShapeEvent.
	ShapeEvent = domain.ShapeEvent
	// ShapeGaugeDelta is a signed delta to a level. See domain.ShapeGaugeDelta.
	ShapeGaugeDelta = domain.ShapeGaugeDelta
)

// The canonical physical units. See domain.Unit.
const (
	// UnitByte counts bytes. See domain.UnitByte.
	UnitByte = domain.UnitByte
	// UnitCount counts occurrences. See domain.UnitCount.
	UnitCount = domain.UnitCount
	// UnitMillisecond measures elapsed time. See domain.UnitMillisecond.
	UnitMillisecond = domain.UnitMillisecond
	// UnitToken counts model tokens. See domain.UnitToken.
	UnitToken = domain.UnitToken
	// UnitPage counts pages. See domain.UnitPage.
	UnitPage = domain.UnitPage
)

// The well-known dimension keys. The set is open; these spellings are not.
// See domain's dimensions.go.
const (
	// DimensionTier is the storage tier. See domain.DimensionTier.
	DimensionTier = domain.DimensionTier
	// DimensionLane is the scheduling lane. See domain.DimensionLane.
	DimensionLane = domain.DimensionLane
	// DimensionProcessorID names the processor. See domain.DimensionProcessorID.
	DimensionProcessorID = domain.DimensionProcessorID
	// DimensionProcessorVersion is the processor's version. See domain.DimensionProcessorVersion.
	DimensionProcessorVersion = domain.DimensionProcessorVersion
	// DimensionModelID names the model. See domain.DimensionModelID.
	DimensionModelID = domain.DimensionModelID
	// DimensionRegion is the region. See domain.DimensionRegion.
	DimensionRegion = domain.DimensionRegion
	// DimensionPrincipalID is the principal that caused the usage. See domain.DimensionPrincipalID.
	DimensionPrincipalID = domain.DimensionPrincipalID
	// DimensionCapabilityChain identifies the credential chain. See domain.DimensionCapabilityChain.
	DimensionCapabilityChain = domain.DimensionCapabilityChain
	// DimensionFilesetID attributes usage to a fileset. See domain.DimensionFilesetID.
	DimensionFilesetID = domain.DimensionFilesetID
	// DimensionDocumentID attributes usage to a document. See domain.DimensionDocumentID.
	DimensionDocumentID = domain.DimensionDocumentID
	// DimensionAgentRunID attributes usage to one agent run. See domain.DimensionAgentRunID.
	DimensionAgentRunID = domain.DimensionAgentRunID
	// DimensionJobID attributes usage to one job. See domain.DimensionJobID.
	DimensionJobID = domain.DimensionJobID
)

// The bounds a dimension set must satisfy. See domain's dimensions.go.
const (
	// MaxDimensions bounds how many dimensions one event may carry. See domain.MaxDimensions.
	MaxDimensions = domain.MaxDimensions
	// MaxDimensionKeyLen bounds a single dimension key. See domain.MaxDimensionKeyLen.
	MaxDimensionKeyLen = domain.MaxDimensionKeyLen
	// MaxDimensionValueLen bounds a single dimension value. See domain.MaxDimensionValueLen.
	MaxDimensionValueLen = domain.MaxDimensionValueLen
)

var (
	// ErrInvalidEvent is domain.ErrInvalidEvent.
	ErrInvalidEvent = domain.ErrInvalidEvent
	// ErrNegativeQuantity is domain.ErrNegativeQuantity.
	ErrNegativeQuantity = domain.ErrNegativeQuantity
	// ErrInvalidRange is domain.ErrInvalidRange.
	ErrInvalidRange = domain.ErrInvalidRange

	// ErrNoTx is ledger.ErrNoTx.
	ErrNoTx = ledger.ErrNoTx
	// ErrForeignTx is ledger.ErrForeignTx.
	ErrForeignTx = ledger.ErrForeignTx
	// ErrTxDone is ledger.ErrTxDone.
	ErrTxDone = ledger.ErrTxDone
	// ErrIdempotencyConflict is ledger.ErrIdempotencyConflict.
	ErrIdempotencyConflict = ledger.ErrIdempotencyConflict
)
