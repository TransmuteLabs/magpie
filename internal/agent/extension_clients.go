package agent

import (
	"os"
	"slices"
	"sync"

	"github.com/yetone/magpie/internal/agentplug"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/usage"
)

var pluginClientCache struct {
	sync.Mutex
	path   string
	file   os.FileInfo
	legacy []*agentplug.Agent
	names  []usage.Known
}

func pluginClients() []usage.Known {
	legacy, _ := agentplug.List()
	path := extension.CatalogPath()
	file := clientCatalogFile(path)
	if file == nil && len(legacy) == 0 {
		return nil
	}
	pluginClientCache.Lock()
	if pluginClientCache.path == path && sameClientCatalog(file, pluginClientCache.file) &&
		slices.Equal(legacy, pluginClientCache.legacy) {
		names := pluginClientCache.names
		pluginClientCache.Unlock()
		return names
	}
	pluginClientCache.Unlock()

	home, cfg := homes()
	own := onMachine(home, cfg)
	agents := pluggedAgents(home, own, nil)
	if file != nil {
		errs := map[string]string{}
		sdkAgents := installedExtensionAgents(extension.Default(), home, append(own, agents...), errs)
		if errs["sdk"] != "" || !sameClientCatalog(file, clientCatalogFile(path)) {
			return clientNames(agents)
		}
		agents = append(agents, sdkAgents...)
	}
	names := clientNames(agents)
	pluginClientCache.Lock()
	pluginClientCache.path, pluginClientCache.file = path, file
	pluginClientCache.legacy, pluginClientCache.names = legacy, names
	pluginClientCache.Unlock()
	return names
}

func clientNames(agents []*Agent) []usage.Known {
	var names []usage.Known
	for _, agent := range agents {
		names = append(names, usage.Known{ID: agent.ID, Name: agent.Name, Names: append([]string{agent.ID}, agent.Aliases...), UA: agent.UA})
	}
	return names
}

func clientCatalogFile(path string) os.FileInfo {
	file, err := os.Lstat(path)
	if err != nil || !file.Mode().IsRegular() {
		return nil
	}
	return file
}

func sameClientCatalog(first, second os.FileInfo) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return first.Mode() == second.Mode() && first.Size() == second.Size() &&
		first.ModTime().Equal(second.ModTime()) && os.SameFile(first, second)
}
