package plugin

import "github.com/yetone/magpie/internal/extension"

func init() { extension.SetBunResolver(Bun) }
