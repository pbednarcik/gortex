package indexer

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Candidates have already passed the ordinary walk, exclusion, global-size,
// and mtime checks. Keep only asset classes, including currently admitted
// assets whose old content-policy stub may need to become parsed graph.
type contentPolicyCensusCandidate struct {
	relPath string
	lang    string
	size    int64
}

func contentPolicyCensusAsset(gate *contentAdmissionGate, lang string) bool {
	if gate == nil {
		return false
	}
	switch gate.classes[lang] {
	case parser.AssetDocument, parser.AssetData, parser.AssetImage:
		return true
	default:
		return false
	}
}

// Read only deterministic asset file-node IDs from the selected store. This
// adds neither a whole-repository scan nor hydration of ordinary source files.
func (idx *Indexer) staleContentPolicyFiles(gate *contentAdmissionGate, candidates []contentPolicyCensusCandidate) []string {
	const batchSize = 128
	var stale []string
	for start := 0; start < len(candidates); start += batchSize {
		end := min(start+batchSize, len(candidates))
		batch := candidates[start:end]
		ids := make([]string, len(batch))
		for i, candidate := range batch {
			ids[i] = idx.prefixPath(candidate.relPath)
		}
		nodes := idx.graph.GetNodesByIDs(ids)
		for i, candidate := range batch {
			node := nodes[ids[i]]
			var reason string
			var validStub bool
			if node != nil {
				rel, scoped := idx.graphPathRelKey(node.FilePath)
				size, sizeOK := sizeSkipMetaInt64(node.Meta["file_size_bytes"])
				reason, _ = node.Meta["skip_reason"].(string)
				validStub = node.Kind == graph.KindFile && node.ID == ids[i] &&
					node.RepoPrefix == idx.repoPrefix && scoped && rel == candidate.relPath &&
					node.Meta["skipped_due_to_content"] == true && sizeOK && size == candidate.size
			}
			// The cold untracked gate has precedence over content admission.
			// Leave its valid stub alone even when content policy also rejects
			// the asset; this change does not certify untracked-file receipts.
			if validStub && reason == skipReasonUntrackedAsset {
				continue
			}
			wantedReason, skip := gate.skip(candidate.lang, candidate.size)
			if !skip {
				if node != nil {
					if _, marked := node.Meta["skipped_due_to_content"]; marked {
						stale = append(stale, candidate.relPath)
					}
				}
				continue
			}
			// Matching reason and size are the complete persisted policy
			// evidence: changing a cap while the same rejection still holds
			// does not require a refresh. There is no stored cap field.
			if !validStub || reason != wantedReason {
				stale = append(stale, candidate.relPath)
			}
		}
	}
	return stale
}
