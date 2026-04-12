package lut

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"photoboot-picstop/internal/application/filter"
)

// Engine implements filter.IFilterPort using 3-D LUT (.cube) files.
type Engine struct {
	reg          *registry
	dir          string
	activeFilter atomic.Value // stores string
}

// NewEngine loads all *.cube files from dir and returns a ready Engine.
// If dir does not exist or is empty, the engine starts with no filters loaded.
func NewEngine(dir string) (*Engine, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lut: create filters dir %s: %w", dir, err)
	}
	reg := newRegistry()
	if err := reg.load(dir); err != nil {
		return nil, fmt.Errorf("lut: load from %s: %w", dir, err)
	}
	e := &Engine{reg: reg, dir: dir}
	e.activeFilter.Store("")
	return e, nil
}

// StartWatcher begins hot-reload polling (every 5 s). Call once with a long-lived context.
func (e *Engine) StartWatcher(ctx context.Context) {
	e.reg.watchDir(ctx, e.dir)
}

// ListFilters implements filter.IFilterPort.
func (e *Engine) ListFilters() []string {
	return e.reg.names()
}

// ActiveFilter implements filter.IFilterPort.
func (e *Engine) ActiveFilter() string {
	return e.activeFilter.Load().(string)
}

// SetActiveFilter implements filter.IFilterPort.
func (e *Engine) SetActiveFilter(name string) error {
	if name == "" {
		e.activeFilter.Store("")
		return nil
	}
	if !e.reg.has(name) {
		return fmt.Errorf("filter %q not found", name)
	}
	e.activeFilter.Store(name)
	return nil
}

// ApplyToFile implements filter.IFilterPort.
// Reads the JPEG at originalPath, applies filterName, and writes
// <base>_filtered<ext> alongside it. Returns the filtered path.
func (e *Engine) ApplyToFile(ctx context.Context, originalPath, filterName string) (string, error) {
	lut, ok := e.reg.get(filterName)
	if !ok {
		return "", fmt.Errorf("filter %q not found", filterName)
	}

	f, err := os.Open(originalPath)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	src, decErr := jpeg.Decode(f)
	f.Close()
	if decErr != nil {
		return "", fmt.Errorf("decode jpeg: %w", decErr)
	}

	dst := applyLUTParallel(src, lut)
	defer releaseBuffer(dst)

	ext := filepath.Ext(originalPath)
	outPath := originalPath[:len(originalPath)-len(ext)] + "_filtered" + ext

	out, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("create output: %w", err)
	}
	defer out.Close()

	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: 95}); err != nil {
		return "", fmt.Errorf("encode jpeg: %w", err)
	}
	return outPath, nil
}

// WrapPreviewChannel implements filter.IFilterPort.
// If filterName is "", the active filter is read atomically on each frame,
// allowing filter changes to take effect on the next frame without restarting.
func (e *Engine) WrapPreviewChannel(ctx context.Context, raw <-chan []byte, filterName string) <-chan []byte {
	out := make(chan []byte, 1)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-raw:
				if !ok {
					return
				}
				name := filterName
				if name == "" {
					name = e.activeFilter.Load().(string)
				}
				result := frame
				if name != "" {
					filtered, err := e.applyToJPEGBytes(frame, name)
					if err != nil {
						log.Printf("lut preview: %v (passing raw frame)", err)
					} else {
						result = filtered
					}
				}
				select {
				case out <- result:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// applyToJPEGBytes decodes a JPEG, applies the named LUT, and re-encodes as JPEG.
// If the named filter has been removed since the call, it returns data unchanged.
func (e *Engine) applyToJPEGBytes(data []byte, filterName string) ([]byte, error) {
	lut, ok := e.reg.get(filterName)
	if !ok {
		return data, nil
	}
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	dst := applyLUTParallel(src, lut)
	defer releaseBuffer(dst)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), nil
}

// applyLUTParallel applies lut to src using row-striping across GOMAXPROCS workers.
// The caller is responsible for calling releaseBuffer on the returned buffer.
func applyLUTParallel(src image.Image, lut *LUT3D) *image.NRGBA {
	bounds := src.Bounds()
	dst := acquireBuffer(bounds)

	workers := runtime.GOMAXPROCS(0)
	if workers > bounds.Dy() {
		workers = bounds.Dy()
	}
	rowsPerWorker := (bounds.Dy() + workers - 1) / workers

	var wg sync.WaitGroup
	for w := range workers {
		startY := bounds.Min.Y + w*rowsPerWorker
		endY := startY + rowsPerWorker
		if endY > bounds.Max.Y {
			endY = bounds.Max.Y
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			for y := y0; y < y1; y++ {
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					r32, g32, b32, a32 := src.At(x, y).RGBA()
					// RGBA() returns pre-multiplied 16-bit values.
					// For JPEG (alpha always 0xffff) this equals the straight value.
					ro, go_, bo := lut.Apply(
						float32(r32)/65535.0,
						float32(g32)/65535.0,
						float32(b32)/65535.0,
					)
					dst.SetNRGBA(x, y, color.NRGBA{
						R: f32ToByte(ro),
						G: f32ToByte(go_),
						B: f32ToByte(bo),
						A: uint8(a32 >> 8),
					})
				}
			}
		}(startY, endY)
	}
	wg.Wait()
	return dst
}

func f32ToByte(v float32) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return uint8(v*255 + 0.5)
}

// Compile-time interface check.
var _ filter.IFilterPort = (*Engine)(nil)
