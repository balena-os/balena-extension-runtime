package manager

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below pin the error identities a front end classifies on.

func TestCleanup_MissingSocketCarriesEngineUnavailable(t *testing.T) {
	testEngineEnv(t, filepath.Join(t.TempDir(), "absent.sock"))
	err := Cleanup(context.Background(), quietLogger(), CleanupOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEngineUnavailable,
		"an unreachable engine must not read as there being nothing to sweep")
}

// TestDo_UnreachableSocketCarriesEngineUnavailable covers the engine going
// away after CheckSocket passed: the socket file is still on disk, so only the
// dial fails. The listener is closed with the file left behind, which is what
// a stopped balena-engine leaves under /var/run.
func TestDo_UnreachableSocketCarriesEngineUnavailable(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "engine.sock")
	addr, err := net.ResolveUnixAddr("unix", sock)
	require.NoError(t, err)
	ln, err := net.ListenUnix("unix", addr)
	require.NoError(t, err)
	ln.SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())

	info, err := os.Stat(sock)
	require.NoError(t, err, "the socket file must outlive the listener for this to be the case under test")
	require.NotZero(t, info.Mode()&os.ModeSocket)

	eng := testEngine(sock)
	require.NoError(t, eng.CheckSocket(), "a stale socket file passes the pre-flight check")

	_, err = eng.ListContainers(context.Background(), "io.balena.image.class=overlay")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEngineUnavailable)
}

// TestDo_EngineErrorIsNotUnavailable keeps an engine that answered from being
// classified as an engine that could not be reached. The two call for opposite
// responses: one is worth retrying against a daemon that may come back, the
// other is a request the daemon has already refused.
func TestDo_EngineErrorIsNotUnavailable(t *testing.T) {
	sock := testServer(t, func(_, _ string, _ []byte) (int, []byte) {
		return 500, []byte(`{"message":"boom"}`)
	})
	_, err := testEngine(sock).ListContainers(context.Background(), "io.balena.image.class=overlay")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrEngineUnavailable)
}
