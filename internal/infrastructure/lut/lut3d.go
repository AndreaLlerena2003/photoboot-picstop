package lut

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// LUT3D is a 3-D colour look-up table loaded from an Adobe .cube file.
// Entries are stored in .cube file order: R varies fastest, B slowest.
// All channel values are normalised to [0, 1].
type LUT3D struct {
	Size int
	data [][3]float32 // flat index: b*Size*Size + g*Size + r
}

// Apply maps a normalised sRGB triplet through the LUT using trilinear interpolation.
// Input values outside [0, 1] are clamped.
func (l *LUT3D) Apply(r, g, b float32) (float32, float32, float32) {
	sz := float32(l.Size - 1)

	ri := clamp01(r) * sz
	gi := clamp01(g) * sz
	bi := clamp01(b) * sz

	r0, g0, b0 := int(ri), int(gi), int(bi)
	if r0 >= l.Size-1 {
		r0 = l.Size - 2
	}
	if g0 >= l.Size-1 {
		g0 = l.Size - 2
	}
	if b0 >= l.Size-1 {
		b0 = l.Size - 2
	}

	dr := ri - float32(r0)
	dg := gi - float32(g0)
	db := bi - float32(b0)

	c000 := l.at(r0, g0, b0)
	c001 := l.at(r0, g0, b0+1)
	c010 := l.at(r0, g0+1, b0)
	c011 := l.at(r0, g0+1, b0+1)
	c100 := l.at(r0+1, g0, b0)
	c101 := l.at(r0+1, g0, b0+1)
	c110 := l.at(r0+1, g0+1, b0)
	c111 := l.at(r0+1, g0+1, b0+1)

	var out [3]float32
	for i := 0; i < 3; i++ {
		out[i] = trilinear(
			c000[i], c001[i], c010[i], c011[i],
			c100[i], c101[i], c110[i], c111[i],
			dr, dg, db,
		)
	}
	return out[0], out[1], out[2]
}

func (l *LUT3D) at(r, g, b int) [3]float32 {
	return l.data[b*l.Size*l.Size+g*l.Size+r]
}

func lerp(a, b, t float32) float32 { return a + t*(b-a) }

// trilinear interpolates across 8 cube corners (standard cube-linear formula).
func trilinear(c000, c001, c010, c011, c100, c101, c110, c111, dr, dg, db float32) float32 {
	return lerp(
		lerp(lerp(c000, c100, dr), lerp(c010, c110, dr), dg),
		lerp(lerp(c001, c101, dr), lerp(c011, c111, dr), dg),
		db,
	)
}

func clamp01(v float32) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ParseCube parses an Adobe .cube 3-D LUT from r.
func ParseCube(r io.Reader) (*LUT3D, error) {
	scanner := bufio.NewScanner(r)
	lut := &LUT3D{}
	var entries [][3]float32

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		upper := strings.ToUpper(line)

		if strings.HasPrefix(upper, "LUT_3D_SIZE") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return nil, fmt.Errorf("malformed LUT_3D_SIZE line: %q", line)
			}
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 2 || n > 256 {
				return nil, fmt.Errorf("invalid LUT_3D_SIZE %q (want 2–256)", parts[1])
			}
			lut.Size = n
			entries = make([][3]float32, 0, n*n*n)
			continue
		}

		if isKeywordLine(upper) {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) != 3 {
			continue
		}
		var rgb [3]float32
		for i := 0; i < 3; i++ {
			v, err := strconv.ParseFloat(parts[i], 32)
			if err != nil {
				return nil, fmt.Errorf("invalid float %q in LUT data: %w", parts[i], err)
			}
			rgb[i] = float32(math.Max(0, math.Min(1, v)))
		}
		entries = append(entries, rgb)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if lut.Size == 0 {
		return nil, fmt.Errorf("LUT_3D_SIZE directive not found")
	}
	expected := lut.Size * lut.Size * lut.Size
	if len(entries) != expected {
		return nil, fmt.Errorf("expected %d LUT entries, got %d", expected, len(entries))
	}
	lut.data = entries
	return lut, nil
}

// ParseCubeFile opens path and parses it as an Adobe .cube LUT.
func ParseCubeFile(path string) (*LUT3D, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseCube(f)
}

func isKeywordLine(upper string) bool {
	for _, kw := range []string{"TITLE", "DOMAIN_MIN", "DOMAIN_MAX", "LUT_1D_SIZE", "LUT_3D_INPUT_RANGE"} {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}
	return false
}
