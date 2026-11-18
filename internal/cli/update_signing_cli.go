package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/update"
)

var (
	updateSigning           = update.Signing
	updateSigningIdentities = update.SigningIdentities
	updateUseSigning        = update.UseSigning
	updateDisableSigning    = update.DisableSigning
	updateSigningBundle     = update.Bundle
)

func updateSigningCmd(args []string) error {
	const usage = "usage: magpie update signing [list|use <fullfingerprint>|off]"
	if len(args) == 0 {
		c, err := updateSigning()
		if err != nil {
			return err
		}
		if c == nil {
			fmt.Println("local update signing: off")
			fmt.Println("macOS: magpie update signing list, then magpie update signing use <fullfingerprint>")
			fmt.Println("select once for the exact custom MAGPIE_UPDATE_FEED; the private key stays in Keychain")
			return nil
		}
		fmt.Println("local update signing: on")
		fmt.Println("certificate:", c.Fingerprint)
		fmt.Println("team:", c.Team)
		fmt.Println("bundle:", c.BundleID)
		fmt.Println("feed:", c.Feed)
		fmt.Println("revision:", c.Revision)
		return nil
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		ids, err := updateSigningIdentities(ctx)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Println("no usable code-signing identities found in Keychain")
			return nil
		}
		for _, id := range ids {
			fmt.Printf("%s  %s  team %s  expires %s\n", id.Fingerprint, id.Name, id.Team, id.Expires.UTC().Format(time.RFC3339))
		}
		fmt.Println("select one: magpie update signing use <fullfingerprint>")
		return nil
	case "use":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		fp := strings.ToUpper(strings.TrimSpace(args[1]))
		cert, err := hex.DecodeString(fp)
		if err != nil || len(cert) != 20 {
			return fmt.Errorf("use the complete 40-hex code-signing certificate fingerprint from magpie update signing list")
		}
		bundle := updateSigningBundle()
		if bundle == "" {
			return fmt.Errorf("local update signing needs the macOS app; run its Contents/MacOS/magpie update signing use <fullfingerprint>")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if err := updateUseSigning(ctx, fp, bundle); err != nil {
			return err
		}
		fmt.Println("local update signing is on for", fp)
		fmt.Println("this pins the exact custom MAGPIE_UPDATE_FEED for future updates; the private key stays in Keychain")
		fmt.Println("the installed app has not changed; the first signed update may ask for macOS permissions again")
		return nil
	case "off":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		if err := updateDisableSigning(); err != nil {
			return err
		}
		fmt.Println("local update signing is off; the installed app has not changed")
		return nil
	default:
		return fmt.Errorf("%s", usage)
	}
}
