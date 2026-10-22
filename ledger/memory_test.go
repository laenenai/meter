package ledger_test

import (
	"testing"

	"github.com/laenen-ai/meter/ledger"
)

func newMemoryLedger(t *testing.T) ledger.Ledger {
	t.Helper()
	return ledger.NewMemory()
}

func TestMemory_PassesLedgerSuite(t *testing.T) {
	ledger.RunLedgerSuite(t, newMemoryLedger)
}
