package sessions

import (
	"context"
	"slices"
	"sync"
)

type nativeHistoryReader struct {
	agent string
	read  func() []file
	wsl   []string
}

func nativeHistoryReaders(zcode func() []file) []nativeHistoryReader {
	return []nativeHistoryReader{
		{agent: "claude", read: claudeFiles},
		{agent: "claude-desktop", read: claudeDesktopFiles},
		{agent: "codex", read: codexFiles},
		{wsl: []string{"claude", "codex"}},
		{agent: "opencode", read: openCodeFiles},
		{agent: "pi", read: piFiles},
		{agent: "raft", read: raftFiles},
		{agent: "cindy", read: cindyFiles},
		{wsl: []string{"pi", "raft", "omp", "kimi", "antigravity", "agy"}},
		{agent: "zcode", read: zcode},
		{agent: "dsh", read: dshFiles},
		{agent: "cline", read: clineFiles},
		{agent: "qoder", read: func() []file { return ccFiles("qoder", QoderDir("qoder")) }},
		{agent: "qoder-cn", read: func() []file { return ccFiles("qoder-cn", QoderDir("qoder-cn")) }},
		{agent: "grok", read: grokFiles},
		{agent: "workbuddy", read: workbuddyFiles},
		{agent: "workbuddy-ai", read: workbuddyAIFiles},
		{agent: "codebuddy", read: codebuddyFiles},
		{agent: "droid", read: droidFiles},
		{agent: "omp", read: ompFiles},
		{agent: "cursor", read: cursorFiles},
		{agent: "hermes", read: hermesFiles},
		{agent: "alma", read: almaFiles},
		{agent: "openmausbot", read: openmausbotFiles},
		{agent: "liveagent", read: liveagentFiles},
		{agent: "kimi", read: kimiFiles},
		{agent: "agy", read: agyFiles},
		{agent: "antigravity", read: antigravityFiles},
	}
}

var nativeHistoryOwners = sync.OnceValue(func() []string {
	readers := nativeHistoryReaders(nil)
	owners := make([]string, 0, len(readers))
	for _, reader := range readers {
		if reader.agent != "" && !slices.Contains(owners, reader.agent) {
			owners = append(owners, reader.agent)
		}
		for _, agent := range reader.wsl {
			if !slices.Contains(owners, agent) {
				owners = append(owners, agent)
			}
		}
	}
	return owners
})

func nativeHistoryLists(view *sessionSourceView, readers []nativeHistoryReader, readWSL func(...string) []file) [][]file {
	authority, err := inspectHistoryPublication(context.Background())
	if err != nil {
		return nil
	}
	authority.view = view
	lists := make([][]file, 0, len(readers))
	for _, reader := range readers {
		if len(reader.wsl) != 0 {
			agents := slices.DeleteFunc(slices.Clone(reader.wsl), func(agent string) bool { return !authority.native(agent) })
			if len(agents) != 0 {
				lists = append(lists, readWSL(agents...))
			}
		} else if authority.native(reader.agent) && reader.read != nil {
			lists = append(lists, reader.read())
		}
	}
	return lists
}

func nativeHistoryCanRead(view *sessionSourceView, agent string) bool {
	authority, err := inspectHistoryPublication(context.Background())
	authority.view = view
	return err == nil && authority.native(agent)
}
