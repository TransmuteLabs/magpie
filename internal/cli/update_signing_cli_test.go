package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/update"
)

func mockUpdateSigning(t *testing.T) {
	t.Helper()
	status, identities, use, off, bundle := updateSigning, updateSigningIdentities, updateUseSigning, updateDisableSigning, updateSigningBundle
	t.Cleanup(func() {
		updateSigning, updateSigningIdentities, updateUseSigning, updateDisableSigning, updateSigningBundle = status, identities, use, off, bundle
	})
	updateSigning = func() (*update.SigningConfig, error) { t.Fatal("unexpected signing status read"); return nil, nil }
	updateSigningIdentities = func(context.Context) ([]update.SigningIdentity, error) {
		t.Fatal("unexpected identity listing")
		return nil, nil
	}
	updateUseSigning = func(context.Context, string, string) error { t.Fatal("unexpected signing selection"); return nil }
	updateDisableSigning = func() error { t.Fatal("unexpected signing disable"); return nil }
	updateSigningBundle = func() string { t.Fatal("unexpected bundle lookup"); return "" }
	t.Setenv("MAGPIE_UPDATE_FEED", "://invalid-no-network")
}

func TestUpdateSigningDispatch(t *testing.T) {
	mockUpdateSigning(t)
	const fp = "0123456789ABCDEF0123456789ABCDEF01234567"
	var called string
	checkContext := func(ctx context.Context, limit time.Duration) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > limit {
			t.Fatalf("operation has no deadline within %v", limit)
		}
	}
	updateSigning = func() (*update.SigningConfig, error) { called = "status"; return nil, nil }
	updateSigningIdentities = func(ctx context.Context) ([]update.SigningIdentity, error) {
		checkContext(ctx, 45*time.Second)
		called = "list"
		return []update.SigningIdentity{{Fingerprint: fp, Name: "same name", Team: "TEAM", Expires: time.Now().Add(time.Hour)}, {Fingerprint: strings.Repeat("A", 40), Name: "same name", Team: "TEAM", Expires: time.Now().Add(time.Hour)}}, nil
	}
	updateSigningBundle = func() string { return "/fixture/magpie.app" }
	updateUseSigning = func(ctx context.Context, fingerprint, bundle string) error {
		checkContext(ctx, 120*time.Second)
		if fingerprint != fp || bundle != "/fixture/magpie.app" {
			t.Fatalf("selection = %q in %q", fingerprint, bundle)
		}
		called = "use"
		return nil
	}
	updateDisableSigning = func() error { called = "off"; return nil }
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "status"},
		{[]string{"list"}, "list"},
		{[]string{"use", strings.ToLower(fp)}, "use"},
		{[]string{"off"}, "off"},
	} {
		called = ""
		if err := updateCmd(append([]string{"update", "signing"}, tc.args...)); err != nil || called != tc.want {
			t.Fatalf("%v: called %q, error %v", tc.args, called, err)
		}
	}
}

func TestUpdateSigningRefusesInvalidArguments(t *testing.T) {
	mockUpdateSigning(t)
	for _, args := range [][]string{
		{"unknown"}, {"status"}, {"list", "extra"}, {"off", "extra"}, {"off", "--force"},
		{"use"}, {"use", "short"}, {"use", strings.Repeat("Z", 40)}, {"use", strings.Repeat("A", 40), "extra"},
	} {
		if err := updateCmd(append([]string{"update", "signing"}, args...)); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestUpdateSigningNeedsAppBundle(t *testing.T) {
	mockUpdateSigning(t)
	updateSigningBundle = func() string { return "" }
	err := updateCmd([]string{"update", "signing", "use", strings.Repeat("A", 40)})
	if err == nil || !strings.Contains(err.Error(), "Contents/MacOS/magpie") {
		t.Fatalf("outside a bundle: %v", err)
	}
}

func TestUpdateSigningPropagatesErrors(t *testing.T) {
	mockUpdateSigning(t)
	want := errors.New("fixture signing operation failed")
	updateSigning = func() (*update.SigningConfig, error) { return nil, want }
	updateSigningIdentities = func(context.Context) ([]update.SigningIdentity, error) { return nil, want }
	updateSigningBundle = func() string { return "/fixture/magpie.app" }
	updateUseSigning = func(context.Context, string, string) error { return want }
	updateDisableSigning = func() error { return want }
	for _, args := range [][]string{nil, {"list"}, {"use", strings.Repeat("A", 40)}, {"off"}} {
		if err := updateCmd(append([]string{"update", "signing"}, args...)); !errors.Is(err, want) {
			t.Errorf("%v: error = %v", args, err)
		}
	}
}
