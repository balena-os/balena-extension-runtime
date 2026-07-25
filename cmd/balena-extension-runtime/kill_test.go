package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression guard for the force-delete path: containerd passes --all, and
// rejecting it made the engine return 500 on teardown. The e2e suite covers
// the full invocation; this pins the flag registration itself.
func TestKillCmdAcceptsAllFlag(t *testing.T) {
	// killCmd is package-level state, so parsing here would otherwise leave
	// --all set for every test that runs after this one.
	t.Cleanup(func() {
		flag := killCmd.Flags().Lookup("all")
		require.NoError(t, flag.Value.Set("false"))
		flag.Changed = false
	})

	require.NoError(t, killCmd.ParseFlags([]string{"--all", "container-id", "9"}))
	require.NoError(t, killCmd.ParseFlags([]string{"-a", "container-id", "9"}))
	assert.Equal(t, []string{"container-id", "9"}, killCmd.Flags().Args())
}
