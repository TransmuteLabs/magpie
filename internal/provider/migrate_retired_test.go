package provider

import (
	"strings"
	"testing"
)

// A retired built-in is its plugin's to serve before any move: signing in
// to the built-in is turned away, and only an id with a plugin can be
// retired. An id not retired is the plugin's only once moved.
func TestRetiredBuiltinIsThePlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	id := ""
	for m := range movers {
		if !retiredBuiltins[m] {
			id = m
			break
		}
	}
	if id == "" {
		t.Skip("every built-in with a plugin is retired")
	}
	if PluginServes(id) {
		t.Fatalf("%s is the plugin's with nothing moved or retired", id)
	}
	retiredBuiltins[id], retiredBuiltins["claude"] = true, true
	t.Cleanup(func() { delete(retiredBuiltins, id); delete(retiredBuiltins, "claude") })
	if !PluginServes(id) || !BuiltinRetired(id) {
		t.Fatalf("retired %s isn't the plugin's", id)
	}
	if BuiltinRetired("claude") || PluginServes("claude") {
		t.Fatal("claude, which has no plugin, is retired")
	}
	if _, err := StartSignInWith(id, SignInOptions{}); err == nil || !strings.Contains(err.Error(), "plugin") {
		t.Fatalf("signing in to the retired built-in: %v", err)
	}
}
