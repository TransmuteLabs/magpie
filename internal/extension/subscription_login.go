package extension

import (
	"crypto/sha256"
	"encoding/hex"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type PublicSubscriptionLogin struct {
	Provider, User, Revision string
	Visible                  bool
}

func ResolveSubscriptionLogin(owners []SubscriptionOwner, scope sdk.Scope) PublicSubscriptionLogin {
	sum := sha256.Sum256([]byte(scope.Plugin + "\x00" + scope.Provider))
	login := PublicSubscriptionLogin{
		Provider: "extension-" + hex.EncodeToString(sum[:12]), User: scope.Account, Visible: true,
	}
	for _, owner := range owners {
		if owner.Module != scope.Plugin || owner.Provider != scope.Provider {
			continue
		}
		login.Revision, login.Visible = owner.Revision, owner.Active
		if owner.Active {
			login.Provider = owner.PublicID
			for _, account := range owner.Accounts {
				if account.ID == scope.Account {
					login.User = account.User
					break
				}
			}
		}
		break
	}
	return login
}
