package inventory

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// InventoryStore holds a live-reloading server inventory.
// Safe for concurrent reads from MCP tool handlers.
type InventoryStore struct {
	mu      sync.RWMutex
	servers []Server
	path    string
}

// NewInventoryStore loads inventory from path and returns a store.
func NewInventoryStore(path string) (*InventoryStore, error) {
	servers, err := LoadInventory(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return &InventoryStore{servers: servers, path: abs}, nil
}

// Get returns current server list. Callers must not mutate the slice.
func (s *InventoryStore) Get() []Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.servers
}

// Reload re-reads inventory from disk.
func (s *InventoryStore) Reload() error {
	servers, err := LoadInventory(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.servers = servers
	s.mu.Unlock()
	return nil
}

// StartWatcher watches the inventory for changes and reloads automatically.
// For nix inventories, watches both the host directory and groups/ subdirectory.
// Debounces rapid events with 150ms window.
func (s *InventoryStore) StartWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return fmt.Errorf("watch %s: %w", dir, err)
	}

	if strings.HasSuffix(s.path, ".nix") {
		groupsDir := filepath.Join(dir, "groups")
		if info, err := os.Stat(groupsDir); err == nil && info.IsDir() {
			if err := watcher.Add(groupsDir); err != nil {
				log.Printf("warning: cannot watch %s: %v", groupsDir, err)
			}
		}
	}

	go func() {
		var debounce *time.Timer

		for {
			select {
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if !strings.HasSuffix(ev.Name, ".nix") {
					continue
				}

				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(150*time.Millisecond, func() {
					if _, err := os.Stat(s.path); err != nil {
						return
					}
					if err := s.Reload(); err != nil {
						log.Printf("inventory reload failed: %v", err)
					} else {
						s.mu.RLock()
						n := len(s.servers)
						s.mu.RUnlock()
						log.Printf("inventory reloaded: %d servers from %s", n, s.path)
					}
				})

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("fsnotify error: %v", err)
			}
		}
	}()

	log.Printf("watching %s for changes", s.path)
	return nil
}
