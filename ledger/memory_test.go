package ledger_test

import (
	"testing"

	"github.com/laenenai/meter/ledger"
	"github.com/laenenai/meter/metertest"
)

func newMemoryLedger(t *testing.T) ledger.Ledger {
	t.Helper()
	return ledger.NewMemory()
}

func TestMemory_PassesLedgerSuite(t *testing.T) {
	metertest.RunLedgerSuite(t, newMemoryLedger)
}
