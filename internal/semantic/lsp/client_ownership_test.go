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
	_, accepted := p.registerPendingClient(c)
	require.True(t, accepted)

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
	gen, accepted := p.registerPendingClient(c)
	require.True(t, accepted)

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

// A sealed provider accepts no new clients, ever: the lane's readiness
// teardown can abandon a prober wedged BEFORE the spawn (package restore,
// process start) — when that leg finally returns, its registration and
// publication must be refused and the fresh server reaped, or it leaks
// for the daemon's lifetime with nobody left to Close it. Sealing is for
// single-use lane instances; a foreground Close stays reversible.
func TestLSPProvider_SealRefusesLateSpawn(t *testing.T) {
	c, serverIn, serverOut, cleanup := newPipedClient(t)
	defer cleanup()
	go newFakeLSPServer().run(serverIn, serverOut)

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 0, zap.NewNop())
	p.sealClients() // the teardown ran while the prober was wedged pre-spawn

	gen, accepted := p.registerPendingClient(c)
	assert.False(t, accepted, "a sealed provider must refuse a late registration")
	assert.False(t, p.publishClient(c, gen), "and a late publication")
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the refused registration did not reap the client")
	}
	assert.Nil(t, p.client)
}

// The undisturbed path publishes normally.
func TestLSPProvider_PublishClientHappyPath(t *testing.T) {
	c, serverIn, serverOut, cleanup := newPipedClient(t)
	defer cleanup()
	go newFakeLSPServer().run(serverIn, serverOut)

	p := NewProvider("fake-lsp", nil, []string{"go"}, false, 0, zap.NewNop())
	gen, accepted := p.registerPendingClient(c)
	require.True(t, accepted)
	require.True(t, p.publishClient(c, gen))
	assert.Same(t, c, p.client)
	require.NoError(t, p.Close())
}
