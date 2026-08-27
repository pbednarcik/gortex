package graph

// EvictFiles removes all nodes owned by the supplied file paths and every
// edge touching those nodes under one graph-wide write section. It preserves
// EvictFile's exact indexes/counters/receipt semantics while amortising the
// lock and edge cleanup across a partial-reconcile chunk.
func (g *Graph) EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int) {
	pathSet := make(map[string]struct{}, len(filePaths))
	for _, filePath := range filePaths {
		if filePath != "" {
			pathSet[filePath] = struct{}{}
		}
	}
	if len(pathSet) == 0 {
		return 0, 0
	}

	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.lockAllWrite()
	defer g.unlockAllWrite()

	var nodes []*Node
	for filePath := range pathSet {
		for _, shard := range g.shards {
			nodes = append(nodes, shard.byFile[filePath]...)
		}
	}
	if len(nodes) == 0 {
		return 0, 0
	}
	evictedIDs := make(map[string]string, len(nodes))
	for _, node := range nodes {
		if node != nil {
			evictedIDs[node.ID] = node.RepoPrefix
		}
	}
	g.recordFileEvictionForReceipts(nodes, evictedIDs)
	for _, node := range nodes {
		if node == nil {
			continue
		}
		shard := g.shardFor(node.ID)
		shard.repoNodeRemove(node)
		delete(shard.nodes, node.ID)
		if node.QualName != "" {
			if current, ok := shard.byQual[node.QualName]; ok && current.ID == node.ID {
				delete(shard.byQual, node.QualName)
			}
		}
		removeNodeFromBucket(shard.byName, shard.byNameIdx, node.Name, node.ID)
		removeNodeFromBucket(shard.byFile, shard.byFileIdx, node.FilePath, node.ID)
		removeNodeFromBucket(shard.byRepo, shard.byRepoIdx, node.RepoPrefix, node.ID)
	}
	nodesRemoved = len(evictedIDs)
	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	return nodesRemoved, edgesRemoved
}

var _ FileBatchEvicter = (*Graph)(nil)
