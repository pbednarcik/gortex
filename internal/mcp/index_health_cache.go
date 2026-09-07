package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const indexHealthSnapshotTTL = 30 * time.Second

type indexHealthCache struct {
	mu         sync.RWMutex
	payload    map[string]any
	updatedAt  time.Time
	refreshing bool
}

func (s *Server) indexHealthSnapshot() (map[string]any, time.Time, bool) {
	s.indexHealth.mu.RLock()
	defer s.indexHealth.mu.RUnlock()
	if s.indexHealth.payload == nil {
		return nil, time.Time{}, s.indexHealth.refreshing
	}
	return s.indexHealth.payload, s.indexHealth.updatedAt, s.indexHealth.refreshing
}

func (s *Server) refreshIndexHealthInBackground() {
	s.indexHealth.mu.Lock()
	if s.indexHealth.refreshing {
		s.indexHealth.mu.Unlock()
		return
	}
	s.indexHealth.refreshing = true
	s.indexHealth.mu.Unlock()

	go func() {
		payload, err := s.buildIndexHealthBasePayloadCtx(context.Background())
		s.indexHealth.mu.Lock()
		defer s.indexHealth.mu.Unlock()
		s.indexHealth.refreshing = false
		if err == nil && payload != nil {
			s.indexHealth.payload = payload
			s.indexHealth.updatedAt = time.Now()
		}
	}()
}

func (s *Server) indexHealthNeedsRefresh(updatedAt time.Time) bool {
	return updatedAt.IsZero() || time.Since(updatedAt) >= indexHealthSnapshotTTL
}

func compactIndexHealth(payload map[string]any, updatedAt time.Time, refreshing bool) string {
	stale, _ := payload["stale_files"].([]string)
	parseFailureCount := indexHealthParseFailureCount(payload["parse_failures"])
	failedFiles, _ := payload["failed_file_count"].(int)
	unreadableFiles, _ := payload["unreadable_file_count"].(int)
	status := "ready"
	if refreshing {
		status = "refreshing"
	}
	if failedFiles > 0 || payload["status"] == "degraded" {
		status = "degraded"
	}
	return fmt.Sprintf("health=%v nodes=%v stale=%d failures=%d status=%s age=%ds failed_files=%d unreadable_files=%d\n",
		payload["health_score"], payload["node_count"], len(stale), parseFailureCount, status, int(time.Since(updatedAt).Seconds()), failedFiles, unreadableFiles)
}
