package main

import (
	"bytes"
	"compress/zlib"
	"image"
	"image/png"
	"io"
	"os"
	"sync"
)

type pdfLink struct {
	Rect     [4]float64 // x0, y0, x1, y1 in PDF points (bottom-left origin)
	DestPage int        // 0-indexed destination page
}

// Pooled zlib writers to amortize internal hash table allocation.
var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(&bytes.Buffer{}, zlib.BestSpeed)
		return w
	},
}

func readLayerData(f *os.File, addr uint64) ([]byte, error) {
	if _, err := f.Seek(int64(addr), io.SeekStart); err != nil {
		return nil, err
	}
	blockLen, err := readUint32(f)
	if err != nil {
		return nil, err
	}
	data := make([]byte, blockLen)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	return data, nil
}

func decodePNGLayer(f *os.File, addr uint64) (image.Image, error) {
	if _, err := f.Seek(int64(addr), io.SeekStart); err != nil {
		return nil, err
	}
	blockLen, err := readUint32(f)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, blockLen)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(buf))
}

// compositePNGToRGB composites a decoded PNG image onto an RGB buffer.
// Handles NRGBA fast path and generic image fallback.
func compositePNGToRGB(img image.Image, rgb []byte, width, height int) {
	bounds := img.Bounds()
	maxY := min(bounds.Max.Y, height)
	maxX := min(bounds.Max.X, width)

	if src, ok := img.(*image.NRGBA); ok {
		for y := bounds.Min.Y; y < maxY; y++ {
			for x := bounds.Min.X; x < maxX; x++ {
				pOff := (y-bounds.Min.Y)*src.Stride + (x-bounds.Min.X)*4
				sa := src.Pix[pOff+3]
				if sa == 0 {
					continue
				}
				dOff := (y*width + x) * 3
				if sa == 255 {
					rgb[dOff] = src.Pix[pOff]
					rgb[dOff+1] = src.Pix[pOff+1]
					rgb[dOff+2] = src.Pix[pOff+2]
				} else {
					sa32 := uint32(sa)
					da32 := 255 - sa32
					rgb[dOff] = byte((uint32(src.Pix[pOff])*sa32 + uint32(rgb[dOff])*da32) / 255)
					rgb[dOff+1] = byte((uint32(src.Pix[pOff+1])*sa32 + uint32(rgb[dOff+1])*da32) / 255)
					rgb[dOff+2] = byte((uint32(src.Pix[pOff+2])*sa32 + uint32(rgb[dOff+2])*da32) / 255)
				}
			}
		}
		return
	}

	for y := bounds.Min.Y; y < maxY; y++ {
		for x := bounds.Min.X; x < maxX; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			dOff := (y*width + x) * 3
			if a == 0xFFFF {
				rgb[dOff] = byte(r >> 8)
				rgb[dOff+1] = byte(g >> 8)
				rgb[dOff+2] = byte(b >> 8)
			} else {
				sr := uint32(r >> 8)
				sg := uint32(g >> 8)
				sb := uint32(b >> 8)
				sa := uint32(a >> 8)
				da := 255 - sa
				rgb[dOff] = byte(sr + uint32(rgb[dOff])*da/255)
				rgb[dOff+1] = byte(sg + uint32(rgb[dOff+1])*da/255)
				rgb[dOff+2] = byte(sb + uint32(rgb[dOff+2])*da/255)
			}
		}
	}
}

func compressZlib(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(data) / 4)

	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(&buf)

	if _, err := w.Write(data); err != nil {
		zlibWriterPool.Put(w)
		return nil, err
	}
	if err := w.Close(); err != nil {
		zlibWriterPool.Put(w)
		return nil, err
	}
	zlibWriterPool.Put(w)
	return buf.Bytes(), nil
}
