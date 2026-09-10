package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
)

// installedSkillPath recognizes only the curated files that Gortex installs.
// This is a read-only exception; repository path resolution must not use it.
func installedSkillPath(rawPath string) bool {
	if !filepath.IsAbs(rawPath) {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	copilotHome := strings.TrimSpace(os.Getenv("COPILOT_HOME"))
	if copilotHome == "" {
		copilotHome = filepath.Join(home, ".copilot")
	}
	roots := []string{
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(home, ".config", "opencode", "skills"),
		filepath.Join(copilotHome, "skills"),
		filepath.Join(filepath.Dir(claudecode.UserClaudeMdPath(home)), "skills"),
	}
	path := filepath.Clean(rawPath)
	for _, root := range roots {
		if skillFileInRoot(path, root) {
			return true
		}
		// Home/config directories can themselves be symlinks (for example
		// /var on macOS). Trust the install root, not symlinks beneath it.
		if realRoot, err := filepath.EvalSymlinks(root); err == nil && skillFileInRoot(path, realRoot) {
			return true
		}
	}
	return false
}

func skillFileInRoot(path, root string) bool {
	if !filepath.IsAbs(root) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[1] != "SKILL.md" {
		return false
	}
	_, known := agents.GlobalSkills[parts[0]]
	return known
}

func guardInstalledSkillPath(requestedPath, resolvedPath string) error {
	if !installedSkillPath(requestedPath) || !installedSkillPath(resolvedPath) {
		return fmt.Errorf("%w: installed skill %q resolves to %q, outside Gortex's installed skill files", errPathEscape, requestedPath, resolvedPath)
	}
	return nil
}

func (s *Server) readInstalledSkillFile(absPath string) ([]byte, physicalReadEvidence, error) {
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not resolve installed skill: %w", err)
	}
	if err := guardInstalledSkillPath(absPath, resolved); err != nil {
		return nil, physicalReadEvidence{}, err
	}
	// Even ordinary skill reads verify the physical target: an allowlisted
	// filename must not authorize a symlink retargeted outside the allowlist.
	read := readPhysicalFileEvidence
	if s.physicalEvidenceOverride != nil {
		read = s.physicalEvidenceOverride
	}
	content, evidence, err := read(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, err
	}
	if err := guardInstalledSkillPath(absPath, evidence.resolvedPath); err != nil {
		return nil, physicalReadEvidence{}, err
	}
	resolved, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not verify installed skill: %w", err)
	}
	if err := guardInstalledSkillPath(absPath, resolved); err != nil {
		return nil, physicalReadEvidence{}, err
	}
	return content, evidence, nil
}
