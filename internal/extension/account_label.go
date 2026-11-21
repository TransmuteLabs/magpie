package extension

import "os"

// Credential storage stays readable by older hosts. The sibling file is
// presentation metadata; a missing or unusable label never blocks a sign-in.
type accountLabel struct {
	Generation uint64 `json:"generation"`
	Name       string `json:"name"`
}

func (account Account) labelFile() string {
	return "label-" + storeHash(account.Plugin, account.Provider, account.ID) + ".json"
}

func (record storeAccount) namedAccount(root *os.Root) Account {
	account := record.Account
	if refresh := record.Refresh; refresh != nil && refresh.State == "completed" && refresh.ToGeneration == account.Generation && refresh.Receipt.Name != "" {
		account.Name = refresh.Receipt.Name
		return account
	}
	var label accountLabel
	found, err := storeRead(root, account.labelFile(), 4096, &label)
	if err == nil && found && label.Generation == account.Generation && storeToken(label.Name) {
		account.Name = label.Name
	}
	return account
}

// Called with the account store locked after its credential generation commits.
func (account Account) writeLabel(root *os.Root) {
	if account.Name != "" && storeToken(account.Name) {
		_ = storeWrite(root, account.labelFile(), accountLabel{Generation: account.Generation, Name: account.Name}, 4096)
	}
}
