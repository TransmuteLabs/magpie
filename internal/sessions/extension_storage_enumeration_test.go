package sessions

import "testing"

func TestStorageDelegationPreventsNativeZCodeEnumeration(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		delegated bool
		stale     bool
	}{
		{name: "native"},
		{name: "delegated", delegated: true},
		{name: "delegated-stale", delegated: true, stale: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			setup(t)
			previous := sourceView.Load()
			t.Cleanup(func() { sourceView.Store(previous) })
			for _, kept := range []*filesKept{&allKept[0], &allKept[1]} {
				sourceView.Store(nil)
				if scenario.delegated {
					epoch := sourceEpoch.Load()
					if scenario.stale {
						epoch--
					}
					sourceView.Store(&sessionSourceView{epoch: epoch, owners: map[string]bool{"zcode": true}})
				}
				calls := 0
				allFilesWith(kept, func() []file {
					calls++
					return nil
				})
				want := 1
				if scenario.delegated {
					want = 0
				}
				if calls != want {
					t.Fatalf("native ZCode enumerator called %d times, want %d", calls, want)
				}
			}
		})
	}
}
