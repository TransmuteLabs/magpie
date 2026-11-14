package usage

import "testing"

func TestSDKLedgerObserverImportDoesNotRegister(t *testing.T) {
	for _, observer := range Observers() {
		if _, registered := observer.(*extensionLedgerObserver); registered {
			t.Fatal("importing usage registered an SDK ledger observer without a serving owner")
		}
	}
}
