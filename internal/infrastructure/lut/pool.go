package lut

import (
	"image"
	"sync"
)

// bufferPool recycles image.NRGBA pixel buffers to avoid per-frame allocations
// in the preview path.
var bufferPool = sync.Pool{
	New: func() interface{} {
		return &image.NRGBA{}
	},
}

// acquireBuffer retrieves a buffer from the pool and resizes it to fit bounds.
func acquireBuffer(r image.Rectangle) *image.NRGBA {
	buf := bufferPool.Get().(*image.NRGBA)
	need := 4 * r.Dx() * r.Dy()
	if cap(buf.Pix) >= need {
		buf.Pix = buf.Pix[:need]
	} else {
		buf.Pix = make([]uint8, need)
	}
	buf.Stride = 4 * r.Dx()
	buf.Rect = r
	return buf
}

// releaseBuffer returns buf to the pool. The caller must not use buf after this call.
func releaseBuffer(buf *image.NRGBA) {
	bufferPool.Put(buf)
}
