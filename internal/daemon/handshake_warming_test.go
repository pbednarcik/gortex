package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/testenv"
)

// startWarmingTestServer brings up a daemon on a temp socket with the given
// optional Ready probe and returns the live socket path.
func startWarmingTestServer(t *testing.T, ready func() (bool, string)) string {
	t.Helper()
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))

	srv := New(socket, "test", zap.NewNop())
	srv.Controller = &fakeController{}
	srv.Ready = ready
	require.NoError(t, srv.Listen())
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	require.Eventually(t, func() bool { return IsRunningAt(socket) },
		2*time.Second, 10*time.Millisecond)
	return socket
}

// TestHandshake_StampsWarmingState proves the daemon reports its warmup state
// on the handshake ack so a connecting proxy / CLI can tell a still-filling
// graph from a ready one instead of guessing (and dead-ending on empty).
func TestHandshake_StampsWarmingState(t *testing.T) {
	t.Run("warming", func(t *testing.T) {
		socket := startWarmingTestServer(t, func() (bool, string) {
			return false, "global_resolve"
		})
		client, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
		require.NoError(t, err)
		defer client.Close()
		assert.True(t, client.Ack.OK)
		assert.True(t, client.Ack.Warming, "ack should flag a still-warming daemon")
		assert.Equal(t, "global_resolve", client.Ack.WarmupPhase)
	})

	t.Run("ready", func(t *testing.T) {
		socket := startWarmingTestServer(t, func() (bool, string) {
			return true, "ready"
		})
		client, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
		require.NoError(t, err)
		defer client.Close()
		assert.True(t, client.Ack.OK)
		assert.False(t, client.Ack.Warming, "ack should not flag a ready daemon as warming")
		assert.Equal(t, "ready", client.Ack.WarmupPhase)
	})

	t.Run("no probe assumes ready", func(t *testing.T) {
		socket := startWarmingTestServer(t, nil)
		client, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
		require.NoError(t, err)
		defer client.Close()
		assert.True(t, client.Ack.OK)
		assert.False(t, client.Ack.Warming, "a nil Ready probe means assume ready")
		assert.Empty(t, client.Ack.WarmupPhase)
	})
}
