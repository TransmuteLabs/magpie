package access

import (
	"os"
	"reflect"
	"testing"
)

func TestCurrentIdentityRechecksLimitsModelsAccountsAndRevocation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	secret, err := Update("add-key", Change{Name: "paid-task"})
	if err != nil {
		t.Fatal(err)
	}
	original, err := Check(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update("models-key", Change{Key: original.KeyID, Models: []string{"vendor/model"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Update("limit-key", Change{Key: original.KeyID, Limit: &Limit{Period: "day", Tokens: 7}}); err != nil {
		t.Fatal(err)
	}
	current, err := CurrentIdentity(original)
	if err != nil || !reflect.DeepEqual(current.Models, []string{"vendor/model"}) || len(current.Limits()) != 1 ||
		current.Limits()[0].Tokens != 7 {
		t.Fatal("scheduled caller retained the old unlimited policy", current, err)
	}
	if _, err := CurrentIdentity(current); err != nil {
		t.Fatal("current identity lost the original credential proof", err)
	}
	if _, err := Update("rotate-key", Change{Key: original.KeyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := CurrentIdentity(original); err == nil {
		t.Fatal("rotated caller remained a paid task authority")
	}
}

func TestCurrentIdentityFailsClosedOnUnreadableStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	secret, err := Update("add-key", Change{Name: "paid-task"})
	if err != nil {
		t.Fatal(err)
	}
	original, err := Check(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CurrentIdentity(original); err == nil {
		t.Fatal("unreadable caller policy granted paid work")
	}
}
