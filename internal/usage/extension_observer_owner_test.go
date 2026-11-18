package usage

import "testing"

func TestSDKObserversImportDoesNotRegister(t *testing.T) {
	for _, observer := range Observers() {
		switch observer.(type) {
		case *extensionLedgerObserver, *extensionRequestObserver:
			t.Fatal("importing usage registered an SDK observer without a serving owner")
		}
	}
}
