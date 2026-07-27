// Package localizationauth carries one-shot terminal receipts between the
// Gortex MCP server and short-lived Claude Code hook processes.
package localizationauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

const (
	// ArgumentKey is injected by the authenticated PreToolUse hook. Facade
	// dispatch must remove it before validating or forwarding user arguments.
	ArgumentKey = "_gortex_terminal_auth"

	receiptVersion       = 1
	contractVersionV2    = 2
	tokenBytes           = 32
	tokenHexLength       = tokenBytes * 2
	receiptTTL           = 24 * time.Hour
	maxFinalResponseSize = 64 << 10
	maxReceiptFiles      = 256
	hookSessionDirEnvVar = "GORTEX_HOOK_SESSION_DIR"
)

// Receipt is written only by the MCP response path after it has constructed an
// answer_ready contract. PostToolUse uses this authenticated record, rather than
// visible tool_response text, to attach advisory localization context. A receipt
// never grants permission to deny or replace a later tool call.
type Receipt struct {
	FinalResponse   string `json:"final_response"`
	ContractVersion int    `json:"contract_version"`
	Enforceable     bool   `json:"enforceable"`
}

type receiptEnvelope struct {
	Version         int     `json:"version"`
	TokenDigest     string  `json:"token_digest"`
	Receipt         Receipt `json:"receipt"`
	CreatedUnixNano int64   `json:"created_unix_nano"`
}

// NewToken returns a cryptographically random, single-call correlation token.
func NewToken() (string, bool) {
	var token [tokenBytes]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(token[:]), true
}

// TakeArgument removes and returns the hook-owned token from a facade argument
// map. Callers must strip the reserved field before schema validation or
// forwarding; a model-supplied malformed value becomes an empty token.
func TakeArgument(arguments map[string]any) string {
	if arguments == nil {
		return ""
	}
	raw, exists := arguments[ArgumentKey]
	delete(arguments, ArgumentKey)
	if !exists {
		return ""
	}
	token, ok := raw.(string)
	if !ok || !validToken(token) {
		return ""
	}
	return strings.TrimSpace(token)
}

// Publish atomically records an answer_ready result immediately before the MCP
// handler returns it. Invalid or oversized records fail open and are not stored.
func Publish(token string, receipt Receipt) bool {
	path, digest, ok := receiptPath(token)
	if !ok || !validReceipt(receipt) {
		return false
	}
	envelope := receiptEnvelope{
		Version:         receiptVersion,
		TokenDigest:     digest,
		Receipt:         receipt,
		CreatedUnixNano: time.Now().UnixNano(),
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return false
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	tmp, err := os.CreateTemp(dir, ".terminal-receipt-*")
	if err != nil {
		return false
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return false
	}
	if _, err := tmp.Write(data); err != nil {
		return false
	}
	if err := tmp.Sync(); err != nil {
		return false
	}
	if err := tmp.Close(); err != nil {
		return false
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false
	}
	committed = true
	trimReceipts(dir, path)
	return true
}

// Consume atomically claims and removes one receipt. Concurrent or repeated
// consumers cannot observe the same terminal result.
func Consume(token string) (Receipt, bool) {
	path, digest, ok := receiptPath(token)
	if !ok {
		return Receipt{}, false
	}
	claimToken, ok := NewToken()
	if !ok {
		return Receipt{}, false
	}
	claimPath := path + ".consume-" + claimToken + ".json"
	if err := os.Rename(path, claimPath); err != nil {
		return Receipt{}, false
	}
	defer os.Remove(claimPath) //nolint:errcheck // one-shot cleanup is best effort

	data, err := os.ReadFile(claimPath)
	if err != nil {
		return Receipt{}, false
	}
	var envelope receiptEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil ||
		envelope.Version != receiptVersion || envelope.TokenDigest != digest ||
		!fresh(envelope.CreatedUnixNano) || !validReceipt(envelope.Receipt) {
		return Receipt{}, false
	}
	return envelope.Receipt, true
}

// Discard removes a pending receipt without exposing its contents.
func Discard(token string) {
	path, _, ok := receiptPath(token)
	if ok {
		_ = os.Remove(path)
	}
}

func validReceipt(receipt Receipt) bool {
	return receipt.FinalResponse != "" && len(receipt.FinalResponse) <= maxFinalResponseSize &&
		receipt.ContractVersion >= contractVersionV2
}

func validToken(token string) bool {
	token = strings.TrimSpace(token)
	if len(token) != tokenHexLength || token != strings.ToLower(token) {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func receiptPath(token string) (string, string, bool) {
	token = strings.TrimSpace(token)
	if !validToken(token) {
		return "", "", false
	}
	root := receiptRoot()
	if root == "" {
		return "", "", false
	}
	digestBytes := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(digestBytes[:])
	return filepath.Join(root, digest+".json"), digest, true
}

func receiptRoot() string {
	base := strings.TrimSpace(os.Getenv(hookSessionDirEnvVar))
	if base == "" {
		base = filepath.Join(platform.OSCacheDir(), "sessions")
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "localization-terminal-v3", "receipts")
}

func fresh(createdUnixNano int64) bool {
	if createdUnixNano <= 0 {
		return false
	}
	age := time.Since(time.Unix(0, createdUnixNano))
	return age >= 0 && age <= receiptTTL
}

func trimReceipts(dir, preserve string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type receiptFile struct {
		path    string
		modTime time.Time
	}
	files := make([]receiptFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > receiptTTL && path != preserve {
			_ = os.Remove(path)
			continue
		}
		files = append(files, receiptFile{path: path, modTime: info.ModTime()})
	}
	if len(files) <= maxReceiptFiles {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	remaining := len(files) - maxReceiptFiles
	for _, file := range files {
		if remaining == 0 {
			break
		}
		if file.path == preserve {
			continue
		}
		if os.Remove(file.path) == nil {
			remaining--
		}
	}
}
