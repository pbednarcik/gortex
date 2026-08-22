package lsp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Between dialOrSpawn and the p.client publication the live server process
// is held only on the initializing goroutine's stack. Close() must reap
// that unpublished client too — a readiness-budget teardown that lands in
// this window otherwise no-ops on the nil p.client, and the server
// outlives the daemon's interest in it with zero references (the lane
// instance is never router-registered, so nothing idle-reaps it).
func TestLSPProvider_CloseReapsPendingClient(t *testing.T) {
	c, serverIn, serverOut, cleanup := newPipedClient(t)
	defer cleanup()
	go newFakeLSPServer().run(serverIn, serverOut)

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 0, zap.NewNop())
	p.registerPendingClient(c)

	require.NoError(t, p.Close())
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close left the unpublished client running")
	}
}

// A server that answers initialize only AFTER the teardown already ran
// must not be published into a Provider nobody will Close again: the
// publication must observe the intervening Close, refuse, and reap.
func TestLSPProvider_LatePublicationAfterCloseIsReaped(t *testing.T) {
	c, serverIn, serverOut, cleanup := newPipedClient(t)
	defer cleanup()
	go newFakeLSPServer().run(serverIn, serverOut)

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 0, zap.NewNop())
	gen := p.registerPendingClient(c)

	require.NoError(t, p.Close())
	assert.False(t, p.publishClient(c, gen),
		"a Close between spawn and publish must refuse the publication")
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the refused publication did not reap the client")
	}
	assert.Nil(t, p.client, "no client may be published over a closed window")
}

// The undisturbed path publishes normally.
func TestLSPProvider_PublishClientHappyPath(t *testing.T) {
	c, serverIn, serverOut, cleanup := newPipedClient(t)
	defer cleanup()
	go newFakeLSPServer().run(serverIn, serverOut)

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 0, zap.NewNop())
	gen := p.registerPendingClient(c)
	require.True(t, p.publishClient(c, gen))
	assert.Same(t, c, p.client)
	require.NoError(t, p.Close())
}
