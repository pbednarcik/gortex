package serverstack

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/testenv"
)

// TestCloseReleasesTheGlobalMemoriesSidecar pins that Close releases the
// user-level memories store too. InitMemories mounts it unconditionally at
// platform.MemoriesDir(), under no directory the SideStores config names, so
// a stack that configured no side stores at all still opened one sidecar —
// and Close only knew about the ones it had been handed.
//
// The consequence is platform-specific: POSIX deletes a file with an open
// handle, Windows refuses. A test whose home lives under t.TempDir() there
// fails its own cleanup with "TempDir RemoveAll cleanup: ... The process
// cannot access the file because it is being used by another process", which
// is how internal/serverstack's lease test failed on the Windows shard.
func TestCloseReleasesTheGlobalMemoriesSidecar(t *testing.T) {
	testenv.Sandbox(t)

	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	conf := config.Default()
	conf.Semantic.Enabled = false

	stack, err := NewSharedServer(SharedServerConfig{
		Lifecycle:   LifecycleOneshot,
		Index:       repo,
		BackendPath: filepath.Join(base, "store.sqlite"),
		Config:      conf,
		Logger:      zap.NewNop(),
		// Pin the ledger to temp paths: with both empty the constructor
		// reaches the machine-global savings sidecar.
		SavingsPath:       filepath.Join(base, "savings.sqlite"),
		SavingsLegacyJSON: filepath.Join(base, "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}

	sidecar := persistence.DefaultSidecarPath(platform.MemoriesDir())
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("the stack opened no global memories sidecar, so this test proves nothing: %v", err)
	}
	// OpenSidecar hands back the cached handle, so this is the instance the
	// stack is holding — not a second one.
	held, err := persistence.OpenSidecar(sidecar)
	if err != nil {
		t.Fatalf("OpenSidecar: %v", err)
	}

	if err := stack.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close must have evicted it from the shared cache, so asking again
	// builds a fresh handle. Same pointer means the old one is still open.
	reopened, err := persistence.OpenSidecar(sidecar)
	if err != nil {
		t.Fatalf("OpenSidecar after Close: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened == held {
		t.Fatal("Close left the global memories sidecar open: on Windows its handle pins the file and the test's own TempDir cleanup fails")
	}
}
