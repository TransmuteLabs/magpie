package sessions

import "maps"

func snapshotCacheState(value *state) *state {
	if value == nil {
		return nil
	}
	persistent := *value
	persistent.Codex = nil
	snapshot := persistent.clone()
	snapshot.CodexIdentity = cacheValue(value.CodexIdentity)
	if extra := snapshot.StateMore; extra != nil {
		extra.Grok = cacheValue(extra.Grok)
		if extra.Grok != nil {
			extra.Grok.Tail = cacheValue(extra.Grok.Tail)
		}
		extra.Droid = cacheValue(extra.Droid)
		if extra.Droid != nil {
			extra.Droid.Tail = cacheValue(extra.Droid.Tail)
		}
		extra.Kimi = cacheValue(extra.Kimi)
		if extra.Kimi != nil {
			extra.Kimi.Tail = cacheValue(extra.Kimi.Tail)
		}
		extra.Agy = cacheValue(extra.Agy)
		if extra.Agy != nil {
			extra.Agy.Tail = cacheValue(extra.Agy.Tail)
			extra.Agy.Steps = maps.Clone(extra.Agy.Steps)
		}
		extra.Dsh = cacheValue(extra.Dsh)
		if extra.Dsh != nil {
			extra.Dsh.Tail = cacheValue(extra.Dsh.Tail)
		}
		extra.Cline = cacheValue(extra.Cline)
	}
	return snapshot
}

func cacheValue[Value any](value *Value) *Value {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
