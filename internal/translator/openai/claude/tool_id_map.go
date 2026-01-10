package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

type toolIDMappingEntry struct {
	upstreamID string
	expiresAt  time.Time
}

var (
	toolIDMappingTTL = 30 * time.Minute

	toolIDMappingMu sync.Mutex
	toolIDMapping   = make(map[string]toolIDMappingEntry)
)

func stableToolUseID(seed string, toolIndex int) string {
	sum := sha256.Sum256([]byte(seed + ":" + strconv.Itoa(toolIndex)))
	// 24 hex chars keeps IDs short while staying collision-resistant for our usage.
	return "toolu_" + hex.EncodeToString(sum[:])[:24]
}

func requestSeedFromPayload(payload []byte) string {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return "empty"
	}
	sum := sha256.Sum256(trimmed)
	return hex.EncodeToString(sum[:])[:16]
}

func registerToolUseIDMapping(toolUseID, upstreamID string) {
	toolUseID = strings.TrimSpace(toolUseID)
	upstreamID = strings.TrimSpace(upstreamID)
	if toolUseID == "" || upstreamID == "" {
		return
	}

	now := time.Now()
	expiresAt := now.Add(toolIDMappingTTL)

	toolIDMappingMu.Lock()
	defer toolIDMappingMu.Unlock()

	for k, v := range toolIDMapping {
		if now.After(v.expiresAt) {
			delete(toolIDMapping, k)
		}
	}

	toolIDMapping[toolUseID] = toolIDMappingEntry{upstreamID: upstreamID, expiresAt: expiresAt}
}

func resolveToolUseIDMapping(toolUseID string) (string, bool) {
	toolUseID = strings.TrimSpace(toolUseID)
	if toolUseID == "" {
		return "", false
	}

	now := time.Now()

	toolIDMappingMu.Lock()
	defer toolIDMappingMu.Unlock()

	entry, ok := toolIDMapping[toolUseID]
	if !ok {
		return "", false
	}
	if now.After(entry.expiresAt) {
		delete(toolIDMapping, toolUseID)
		return "", false
	}
	return entry.upstreamID, true
}
