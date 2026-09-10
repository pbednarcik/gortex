package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServersConfig_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "servers.toml")
	cfg := &ServersConfig{
		Server: []ServerEntry{
			{Slug: "main", URL: "http://127.0.0.1:4747", Default: true, Workspaces: []string{"gortex"}},
			{Slug: "remote", URL: "https://example.com", AuthTokenEnv: "REMOTE_TOK"},
		},
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File created with restrictive perms. NTFS carries no POSIX mode
	// bits — Go reports 0666 for every writable file there and the real
	// protection is the ACL — so the check is Unix-only.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file perm = %o, want 0600", perm)
		}
	}

	got, err := LoadServersConfig(path)
	if err != nil {
		t.Fatalf("LoadServersConfig: %v", err)
	}
	if len(got.Server) != 2 {
		t.Fatalf("got %d servers, want 2", len(got.Server))
	}
	if got.Server[0].Slug != "main" || !got.Server[0].Default {
		t.Fatalf("first entry: %+v", got.Server[0])
	}
	if got.Server[1].AuthTokenEnv != "REMOTE_TOK" {
		t.Fatalf("auth_token_env not preserved: %+v", got.Server[1])
	}
}

func TestServersConfig_AddServer_RejectsDuplicateSlug(t *testing.T) {
	cfg := &ServersConfig{
		Server: []ServerEntry{{Slug: "main", URL: "http://127.0.0.1:4747"}},
	}
	err := cfg.AddServer(ServerEntry{Slug: "main", URL: "http://127.0.0.1:5000"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate-slug error, got %v", err)
	}
	if len(cfg.Server) != 1 {
		t.Fatalf("config mutated on rejection: %+v", cfg.Server)
	}
}

func TestServersConfig_AddServer_RejectsSecondDefault(t *testing.T) {
	cfg := &ServersConfig{
		Server: []ServerEntry{{Slug: "main", URL: "http://127.0.0.1:4747", Default: true}},
	}
	err := cfg.AddServer(ServerEntry{Slug: "remote", URL: "https://example.com", Default: true})
	if err == nil {
		t.Fatal("expected error on second default=true")
	}
	if len(cfg.Server) != 1 {
		t.Fatalf("config not rolled back: %+v", cfg.Server)
	}
}

func TestServersConfig_AddServer_RejectsBadURL(t *testing.T) {
	cfg := &ServersConfig{}
	err := cfg.AddServer(ServerEntry{Slug: "bad", URL: "ftp://nope"})
	if err == nil {
		t.Fatal("expected error on unsupported URL scheme")
	}
	if len(cfg.Server) != 0 {
		t.Fatalf("config not rolled back: %+v", cfg.Server)
	}
}

func TestServersConfig_AddServer_RequiresSlug(t *testing.T) {
	cfg := &ServersConfig{}
	if err := cfg.AddServer(ServerEntry{URL: "http://x"}); err == nil {
		t.Fatal("expected error on empty slug")
	}
}

func TestServersConfig_RemoveServer(t *testing.T) {
	cfg := &ServersConfig{
		Server: []ServerEntry{
			{Slug: "main", URL: "http://127.0.0.1:4747"},
			{Slug: "remote", URL: "https://example.com"},
		},
	}
	ok, err := cfg.RemoveServer("main")
	if err != nil || !ok {
		t.Fatalf("RemoveServer(main): ok=%v err=%v", ok, err)
	}
	if len(cfg.Server) != 1 || cfg.Server[0].Slug != "remote" {
		t.Fatalf("after remove: %+v", cfg.Server)
	}

	ok, err = cfg.RemoveServer("nonexistent")
	if err != nil {
		t.Fatalf("RemoveServer(missing) returned err: %v", err)
	}
	if ok {
		t.Fatal("RemoveServer(missing) should return false")
	}
}

func TestServersConfig_Save_RejectsInvalidConfig(t *testing.T) {
	cfg := &ServersConfig{
		Server: []ServerEntry{
			{Slug: "dup", URL: "http://a"},
			{Slug: "dup", URL: "http://b"},
		},
	}
	path := filepath.Join(t.TempDir(), "servers.toml")
	if err := cfg.Save(path); err == nil {
		t.Fatal("expected validation error on save")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not have been created on validation failure, stat err = %v", err)
	}
}
