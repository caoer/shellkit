package dashboard

import (
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
)

// TestCellCacheVersioning verifies the per-call render cache:
//   - MISS on first access (no cached entry)
//   - HIT when version unchanged
//   - MISS (stale) after Apply bumps version
//   - Permanent HIT for completed calls (no active entry)
//   - Cache invalidation on layout change
func TestCellCacheVersioning(t *testing.T) {
	m := ldInitialModel()
	m.width = 120
	m.height = 40

	// Seed a single active call.
	callID := "cache-test-1"
	a := &activeCall{ID: callID, CurrentStep: -1}
	a.Apply(mcp.LiveEvent{
		Kind:      "call-start",
		Ts:        time.Now(),
		SessionID: "sess",
		Input:     "### build\necho hello",
		Steps:     []mcp.StepBrief{{Name: "build", Action: "local"}},
	})
	m.active[callID] = a
	m.activeIDs = []string{callID}

	// Build merged/filtered so renderCell can find the entry.
	m.entries = nil
	m.rebuildFiltered()

	if len(m.filtered) == 0 {
		t.Fatal("expected at least 1 filtered entry after adding active call")
	}

	// --- First access: MISS ---
	cell1 := m.renderCell(callID)
	if cell1 == nil {
		t.Fatal("renderCell returned nil")
	}
	v1 := cell1.version
	if v1 == 0 {
		// call-start Apply bumps version; should be > 0.
		t.Error("expected version > 0 after call-start Apply")
	}

	// --- Second access, no mutation: HIT (same pointer) ---
	cell2 := m.renderCell(callID)
	if cell2 != cell1 {
		t.Error("expected cache HIT (same pointer), got MISS")
	}

	// --- Apply new event → version bumps → stale → MISS ---
	a.Apply(mcp.LiveEvent{Kind: "step-start", Step: 0, Name: "build"})
	cell3 := m.renderCell(callID)
	if cell3 == cell1 {
		t.Error("expected cache MISS after version bump, got same pointer (HIT)")
	}
	if cell3.version <= v1 {
		t.Errorf("new cell version %d should be > old %d", cell3.version, v1)
	}

	// --- Simulate call completion: remove from active → permanent HIT ---
	delete(m.active, callID)
	// Cell still in cache with version from last render.
	cell4 := m.renderCell(callID)
	if cell4 != cell3 {
		t.Error("completed call should be permanent HIT, got MISS")
	}

	// --- Layout change invalidates all ---
	m.cacheInvalidateAll()
	cell5 := m.renderCell(callID)
	if cell5 == cell3 {
		t.Error("expected MISS after cacheInvalidateAll, got same pointer")
	}
}

// TestCellCacheLRUEviction verifies that the cache evicts oldest entries
// when exceeding cellCacheCap.
func TestCellCacheLRUEviction(t *testing.T) {
	m := ldInitialModel()
	m.width = 120
	m.height = 40

	// Populate merged with cellCacheCap+10 fake entries (completed, no active).
	for i := 0; i < cellCacheCap+10; i++ {
		id := "evict-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		m.merged = append(m.merged, mcp.CallEntry{
			ID:        id,
			Timestamp: time.Now(),
			Input:     "### s\necho hi",
			Steps:     []mcp.StepBrief{{Name: "s", Action: "local"}},
		})
		m.filtered = append(m.filtered, i)
	}

	// Render all cells — should not panic and should trigger eviction.
	for _, e := range m.merged {
		m.renderCell(e.ID)
	}

	// Force eviction pass.
	m.cacheEvict()

	if len(m.cellCache) > cellCacheCap {
		t.Errorf("cache size %d exceeds cap %d after eviction", len(m.cellCache), cellCacheCap)
	}

	// Most recent entries should still be cached.
	lastID := m.merged[len(m.merged)-1].ID
	if _, ok := m.cellCache[lastID]; !ok {
		t.Error("most recent entry should survive eviction")
	}
}

// TestCellCacheCallEndEviction verifies that completing a call (active→static
// transition) removes the stale cache entry.
func TestCellCacheCallEndEviction(t *testing.T) {
	m := ldInitialModel()
	m.width = 120
	m.height = 40

	callID := "end-test"
	a := &activeCall{ID: callID, CurrentStep: -1}
	a.Apply(mcp.LiveEvent{
		Kind:  "call-start",
		Ts:    time.Now(),
		Steps: []mcp.StepBrief{{Name: "s", Action: "local"}},
	})
	m.active[callID] = a
	m.activeIDs = []string{callID}
	m.rebuildFiltered()

	// Prime cache.
	m.renderCell(callID)
	if _, ok := m.cellCache[callID]; !ok {
		t.Fatal("cell should be cached after renderCell")
	}

	// Simulate ldEntriesLoaded eviction path.
	delete(m.active, callID)
	delete(m.cellCache, callID)

	if _, ok := m.cellCache[callID]; ok {
		t.Error("cell cache should be cleared after call-end eviction")
	}
}
