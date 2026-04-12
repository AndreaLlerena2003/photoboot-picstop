package lut

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// registry stores named LUT3D entries and is safe for concurrent reads.
type registry struct {
	mu   sync.RWMutex
	luts map[string]*LUT3D
}

func newRegistry() *registry {
	return &registry{luts: make(map[string]*LUT3D)}
}

// load scans dir for *.cube files and loads each one into the registry.
func (reg *registry) load(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "*.cube"))
	if err != nil {
		return err
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, path := range matches {
		name := cubeNameFromPath(path)
		lut, err := ParseCubeFile(path)
		if err != nil {
			log.Printf("lut: skipping %s: %v", path, err)
			continue
		}
		reg.luts[name] = lut
		log.Printf("lut: loaded %q (%dx%dx%d)", name, lut.Size, lut.Size, lut.Size)
	}
	return nil
}

// get returns a LUT by name (nil, false if not found).
func (reg *registry) get(name string) (*LUT3D, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	l, ok := reg.luts[name]
	return l, ok
}

// names returns a sorted list of all loaded LUT names.
func (reg *registry) names() []string {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	out := make([]string, 0, len(reg.luts))
	for k := range reg.luts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// has reports whether the registry contains the named LUT.
func (reg *registry) has(name string) bool {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	_, ok := reg.luts[name]
	return ok
}

// reload re-scans dir: removes deleted files and adds new ones.
func (reg *registry) reload(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.cube"))
	if err != nil {
		log.Printf("lut: reload glob error: %v", err)
		return
	}
	present := make(map[string]struct{}, len(matches))
	for _, path := range matches {
		present[cubeNameFromPath(path)] = struct{}{}
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()

	for name := range reg.luts {
		if _, ok := present[name]; !ok {
			delete(reg.luts, name)
			log.Printf("lut: unloaded %q (file removed)", name)
		}
	}
	for _, path := range matches {
		name := cubeNameFromPath(path)
		if _, alreadyLoaded := reg.luts[name]; alreadyLoaded {
			continue
		}
		lut, err := ParseCubeFile(path)
		if err != nil {
			log.Printf("lut: skipping %s: %v", path, err)
			continue
		}
		reg.luts[name] = lut
		log.Printf("lut: hot-loaded %q", name)
	}
}

// watchDir polls dir every 5 seconds for changes. Stops when ctx is cancelled.
func (reg *registry) watchDir(ctx context.Context, dir string) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reg.reload(dir)
			}
		}
	}()
}

func cubeNameFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
