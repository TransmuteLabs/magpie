package access

import (
	"crypto/sha256"
	"errors"
	"slices"
)

func CurrentIdentity(who Identity) (Identity, error) {
	mu.Lock()
	defer mu.Unlock()
	keys, err := load()
	if err != nil {
		return Identity{}, err
	}
	index := slices.IndexFunc(keys, func(key Key) bool { return key.ID == who.KeyID && who.KeyID != "" })
	if index < 0 {
		return Identity{}, ErrNoKey
	}
	key := keys[index]
	if key.Off || key.Secret == "" || key.expired(Now()) || sha256.Sum256([]byte(key.Secret)) != who.sum {
		return Identity{}, errors.New("scheduled caller was revoked, expired or replaced")
	}
	current, err := identityOf(key, keys, Now())
	current.sum = who.sum
	return current, err
}
