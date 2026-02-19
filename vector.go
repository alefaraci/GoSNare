package main

import (
	"bufio"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/dennwc/gotrace"
)

type colorLayer struct {
	r, g, b byte
	alpha   byte // 255 = fully opaque
	paths   []gotrace.Path
}

// canonicalGroup maps an RLE color code to one of 7 groups (0-6), or -1 to skip.
// Groups: 0=black, 1=dark gray, 2=light gray, 3=white(skip), 4-6=markers.
func canonicalGroup(code byte) int {
	switch code {
	case 0x00, 0x61:
		return 0 // black
	case 0x63, 0x9d, 0x9e:
		return 1 // dark gray
	case 0x64, 0xc9, 0xca:
		return 2 // light gray
	case 0x62, 0x65, 0xFE, 0xFF:
		return 3 // white / transparent
	case 0x66:
		return 4 // marker black
	case 0x67:
		return 5 // marker dark gray
	case 0x68:
		return 6 // marker light gray
	default:
		return -1 // interpolated anti-aliasing
	}
}

// decodeRLEToCodeMap decodes RATTA_RLE data into a raw color-code buffer.
// Each pixel gets the original RLE color code. Transparent pixels (0x62) are left as 0xFF.
func decodeRLEToCodeMap(data []byte, codeMap []byte, width, height int) {
	decodeRLE(data, width, height, func(pos, length int, colorCode byte) {
		fillCodes(codeMap, pos, length, colorCode)
	})
}

func renderContentColorLayers(path string, page Page, width, height int, p *Palette) ([]colorLayer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	totalPixels := width * height

	codeMap := make([]byte, totalPixels)
	codeMap[0] = 0xFF
	for filled := 1; filled < len(codeMap); filled *= 2 {
		copy(codeMap[filled:], codeMap[:filled])
	}

	var pngLayers []image.Image

	for _, layer := range page.Layers {
		if layer.BitmapAddress == 0 || layer.Key == "BGLAYER" {
			continue
		}

		switch layer.Protocol {
		case "RATTA_RLE":
			data, err := readLayerData(f, layer.BitmapAddress)
			if err != nil {
				return nil, fmt.Errorf("reading RLE layer %s: %w", layer.Key, err)
			}
			decodeRLEToCodeMap(data, codeMap, width, height)

		case "PNG":
			img, err := decodePNGLayer(f, layer.BitmapAddress)
			if err != nil {
				return nil, fmt.Errorf("decoding PNG layer %s: %w", layer.Key, err)
			}
			pngLayers = append(pngLayers, img)
		}
	}

	var masks [7]*image.Gray
	for i := range totalPixels {
		code := codeMap[i]
		g := canonicalGroup(code)
		if g < 0 || g == 3 {
			continue
		}
		if masks[g] == nil {
			masks[g] = image.NewGray(image.Rect(0, 0, width, height))
			for j := range masks[g].Pix {
				masks[g].Pix[j] = 0xFF
			}
		}
		masks[g].Pix[i] = 0x00
	}
	codeMap = nil

	params := gotrace.Defaults
	params.TurdSize = 2

	var layers []colorLayer
	// Representative palette indices for each group:
	// Black=0, Dark Gray=157, Light Gray=201, White=255, Markers=0x66-0x68
	groupPaletteIdx := [7]byte{0, 157, 201, 255, 0x66, 0x67, 0x68}

	for g := range 7 {
		if g == 3 || masks[g] == nil {
			continue
		}
		bm := gotrace.NewBitmapFromImage(masks[g], func(x, y int, cl color.Color) bool {
			v, _, _, _ := cl.RGBA()
			return v < 0x8000
		})
		paths, err := gotrace.Trace(bm, &params)
		if err != nil {
			return nil, fmt.Errorf("tracing color group %d: %w", g, err)
		}
		if len(paths) == 0 {
			continue
		}
		idx := groupPaletteIdx[g]
		layers = append(layers, colorLayer{
			r:     p.Colors[idx][0],
			g:     p.Colors[idx][1],
			b:     p.Colors[idx][2],
			alpha: p.Alphas[idx],
			paths: paths,
		})
	}

	for _, img := range pngLayers {
		bounds := img.Bounds()
		gray := image.NewGray(image.Rect(0, 0, width, height))
		for j := range gray.Pix {
			gray.Pix[j] = 0xFF
		}
		for y := bounds.Min.Y; y < bounds.Max.Y && y < height; y++ {
			for x := bounds.Min.X; x < bounds.Max.X && x < width; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				if a > 0 {
					luma := (299*r + 587*g + 114*b) / 1000
					if luma < 0x8000 {
						gray.Pix[y*width+x] = 0x00
					}
				}
			}
		}
		bm := gotrace.NewBitmapFromImage(gray, func(x, y int, cl color.Color) bool {
			v, _, _, _ := cl.RGBA()
			return v < 0x8000
		})
		paths, err := gotrace.Trace(bm, &params)
		if err != nil {
			return nil, fmt.Errorf("tracing PNG layer: %w", err)
		}
		if len(paths) > 0 {
			layers = append(layers, colorLayer{
				r: p.Colors[0][0], g: p.Colors[0][1], b: p.Colors[0][2],
				alpha: 255,
				paths: paths,
			})
		}
	}

	// Markers (alpha < 255) first so they're drawn behind opaque strokes
	slices.SortStableFunc(layers, func(a, b colorLayer) int {
		aMarker := a.alpha < 255
		bMarker := b.alpha < 255
		if aMarker && !bMarker {
			return -1
		}
		if !aMarker && bMarker {
			return 1
		}
		return 0
	})

	return layers, nil
}

func renderBGLayerRGB(path string, page Page, width, height int, p *Palette) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	totalPixels := width * height
	rgb := make([]byte, totalPixels*3)

	rgb[0] = 0xFF
	for filled := 1; filled < len(rgb); filled *= 2 {
		copy(rgb[filled:], rgb[:filled])
	}

	for _, layer := range page.Layers {
		if layer.Key != "BGLAYER" || layer.BitmapAddress == 0 {
			continue
		}

		switch layer.Protocol {
		case "RATTA_RLE":
			data, err := readLayerData(f, layer.BitmapAddress)
			if err != nil {
				return nil, fmt.Errorf("reading BG RLE layer: %w", err)
			}
			decodeRLEToRGB(data, rgb, width, height, p)

		case "PNG":
			img, err := decodePNGLayer(f, layer.BitmapAddress)
			if err != nil {
				return nil, fmt.Errorf("decoding BG PNG layer: %w", err)
			}
			compositePNGToRGB(img, rgb, width, height)
		}
	}

	return rgb, nil
}

// appendFloat4 appends a float formatted to 4 decimal places (like %.4f).
func appendFloat4(buf []byte, f float64) []byte {
	// Round to 4 decimal places
	rounded := math.Round(f*10000) / 10000
	return strconv.AppendFloat(buf, rounded, 'f', 4, 64)
}

// appendFloat2 appends a float formatted to 2 decimal places (like %.2f).
func appendFloat2(buf []byte, f float64) []byte {
	rounded := math.Round(f*100) / 100
	return strconv.AppendFloat(buf, rounded, 'f', 2, 64)
}

type pdfObject struct {
	id   int
	data []byte
}

type vectorPageChunk struct {
	objects []pdfObject
}

func buildVectorPageChunk(
	colorLayers []colorLayer,
	bgRGB []byte,
	width, height int,
	pageWidthPt, pageHeightPt float64,
	links []pdfLink,
	objStart int,
	ocrFallback bool,
) (vectorPageChunk, int) {
	hasBG := bgRGB != nil
	bgWidth, bgHeight := width, height
	if !hasBG && ocrFallback {
		// 1x1 white pixel triggers macOS Preview.app Live Text OCR on vector-only pages
		bgRGB = []byte{0xFF, 0xFF, 0xFF}
		bgWidth, bgHeight = 1, 1
		hasBG = true
	}

	type gsEntry struct {
		name  string
		alpha byte
	}
	var gsEntries []gsEntry
	gsMap := make(map[byte]string)
	for _, cl := range colorLayers {
		if cl.alpha < 255 {
			if _, ok := gsMap[cl.alpha]; !ok {
				name := fmt.Sprintf("/GS%d", len(gsEntries)+1)
				gsMap[cl.alpha] = name
				gsEntries = append(gsEntries, gsEntry{name: name, alpha: cl.alpha})
			}
		}
	}

	// Build content stream using byte buffer for performance
	content := make([]byte, 0, 16*1024)

	if hasBG {
		content = append(content, "q\n"...)
		content = appendFloat4(content, pageWidthPt)
		content = append(content, " 0 0 "...)
		content = appendFloat4(content, pageHeightPt)
		content = append(content, " 0 0 cm\n/Im1 Do\nQ\n"...)
	}

	sx := pageWidthPt / float64(width)
	sy := pageHeightPt / float64(height)

	for _, cl := range colorLayers {
		if len(cl.paths) == 0 {
			continue
		}

		content = append(content, "q\n"...)

		if cl.alpha < 255 {
			content = append(content, gsMap[cl.alpha]...)
			content = append(content, " gs\n"...)
		}

		content = appendFloat4(content, float64(cl.r)/255.0)
		content = append(content, ' ')
		content = appendFloat4(content, float64(cl.g)/255.0)
		content = append(content, ' ')
		content = appendFloat4(content, float64(cl.b)/255.0)
		content = append(content, " rg\n"...)

		for _, p := range cl.paths {
			content = appendPDFSubpathTree(content, p, sx, sy, pageHeightPt)
		}

		content = append(content, "f*\nQ\n"...)
	}

	pageObjID := objStart
	contentsObjID := objStart + 1
	numObjects := 2

	gsObjIDs := make(map[byte]int)
	for _, gs := range gsEntries {
		gsObjIDs[gs.alpha] = objStart + numObjects
		numObjects++
	}

	var imageObjID int
	if hasBG {
		imageObjID = objStart + numObjects
		numObjects++
	}

	var annots string
	if len(links) > 0 {
		var buf bytes.Buffer
		buf.WriteString("\n   /Annots [\n")
		for _, l := range links {
			fmt.Fprintf(&buf, "     << /Type /Annot /Subtype /Link /Rect [%.2f %.2f %.2f %.2f] /Border [0 0 0] /A << /S /GoTo /D [PAGEOBJ_%d /Fit] >> >>\n",
				l.Rect[0], l.Rect[1], l.Rect[2], l.Rect[3], l.DestPage)
		}
		buf.WriteString("   ]")
		annots = buf.String()
	}

	var resBuf strings.Builder
	resBuf.WriteString("<< ")
	if hasBG {
		fmt.Fprintf(&resBuf, "/XObject << /Im1 %d 0 R >> ", imageObjID)
	}
	if len(gsEntries) > 0 {
		resBuf.WriteString("/ExtGState << ")
		for _, gs := range gsEntries {
			fmt.Fprintf(&resBuf, "%s %d 0 R ", gs.name, gsObjIDs[gs.alpha])
		}
		resBuf.WriteString(">> ")
	}
	resBuf.WriteString(">>")
	resources := resBuf.String()

	pageObj := fmt.Sprintf(
		"%d 0 obj\n<< /Type /Page\n   /Parent 2 0 R\n   /MediaBox [0 0 %.2f %.2f]\n   /Contents %d 0 R\n   /Resources %s%s\n>>\nendobj\n",
		pageObjID, pageWidthPt, pageHeightPt, contentsObjID, resources, annots,
	)

	contentsObj := fmt.Sprintf(
		"%d 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n",
		contentsObjID, len(content), content,
	)

	var objects []pdfObject
	objects = append(objects,
		pdfObject{id: pageObjID, data: []byte(pageObj)},
		pdfObject{id: contentsObjID, data: []byte(contentsObj)},
	)

	for _, gs := range gsEntries {
		objID := gsObjIDs[gs.alpha]
		gsObj := fmt.Sprintf(
			"%d 0 obj\n<< /Type /ExtGState /ca %.4f >>\nendobj\n",
			objID, float64(gs.alpha)/255.0,
		)
		objects = append(objects, pdfObject{id: objID, data: []byte(gsObj)})
	}

	if hasBG {
		compressed, err := compressZlib(bgRGB)
		if err != nil {
			compressed = bgRGB
		}

		imageHeader := fmt.Sprintf(
			"%d 0 obj\n<< /Type /XObject\n   /Subtype /Image\n   /Width %d\n   /Height %d\n   /ColorSpace /DeviceRGB\n   /BitsPerComponent 8\n   /Filter /FlateDecode\n   /Length %d >>\nstream\n",
			imageObjID, bgWidth, bgHeight, len(compressed),
		)

		var imageObj bytes.Buffer
		imageObj.Grow(len(imageHeader) + len(compressed) + 30)
		imageObj.WriteString(imageHeader)
		imageObj.Write(compressed)
		imageObj.WriteString("\nendstream\nendobj\n")

		objects = append(objects, pdfObject{id: imageObjID, data: imageObj.Bytes()})
	}

	return vectorPageChunk{objects: objects}, numObjects
}

// appendPDFSubpath appends a single traced path as PDF subpath operators to buf.
func appendPDFSubpath(buf []byte, p gotrace.Path, sx, sy, pageHeightPt float64) []byte {
	c := p.Curve
	if len(c) == 0 {
		return buf
	}

	last := c[len(c)-1]
	buf = appendFloat4(buf, last.Pnt[2].X*sx)
	buf = append(buf, ' ')
	buf = appendFloat4(buf, pageHeightPt-last.Pnt[2].Y*sy)
	buf = append(buf, " m\n"...)

	for _, seg := range c {
		switch seg.Type {
		case gotrace.TypeBezier:
			buf = appendFloat4(buf, seg.Pnt[0].X*sx)
			buf = append(buf, ' ')
			buf = appendFloat4(buf, pageHeightPt-seg.Pnt[0].Y*sy)
			buf = append(buf, ' ')
			buf = appendFloat4(buf, seg.Pnt[1].X*sx)
			buf = append(buf, ' ')
			buf = appendFloat4(buf, pageHeightPt-seg.Pnt[1].Y*sy)
			buf = append(buf, ' ')
			buf = appendFloat4(buf, seg.Pnt[2].X*sx)
			buf = append(buf, ' ')
			buf = appendFloat4(buf, pageHeightPt-seg.Pnt[2].Y*sy)
			buf = append(buf, " c\n"...)
		case gotrace.TypeCorner:
			buf = appendFloat4(buf, seg.Pnt[1].X*sx)
			buf = append(buf, ' ')
			buf = appendFloat4(buf, pageHeightPt-seg.Pnt[1].Y*sy)
			buf = append(buf, " l\n"...)
			buf = appendFloat4(buf, seg.Pnt[2].X*sx)
			buf = append(buf, ' ')
			buf = appendFloat4(buf, pageHeightPt-seg.Pnt[2].Y*sy)
			buf = append(buf, " l\n"...)
		}
	}

	buf = append(buf, "h\n"...)
	return buf
}

// appendPDFSubpathTree recursively appends a path and all its children (holes, islands)
// so the even-odd fill rule (f*) correctly cuts out enclosed counters.
func appendPDFSubpathTree(buf []byte, p gotrace.Path, sx, sy, pageHeightPt float64) []byte {
	buf = appendPDFSubpath(buf, p, sx, sy, pageHeightPt)
	for _, child := range p.Childs {
		buf = appendPDFSubpathTree(buf, child, sx, sy, pageHeightPt)
	}
	return buf
}

// pdfWriter wraps a buffered writer with offset tracking for PDF generation.
type pdfWriter struct {
	w      *bufio.Writer
	offset uint64
}

func (pw *pdfWriter) write(data []byte) {
	pw.w.Write(data)
	pw.offset += uint64(len(data))
}

func (pw *pdfWriter) writeStr(s string) {
	pw.w.WriteString(s)
	pw.offset += uint64(len(s))
}

func (pw *pdfWriter) writeHeader() {
	pw.write([]byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"))
}

func (pw *pdfWriter) writeXrefTrailer(xrefOffsets []uint64, totalObjects int) {
	xrefStart := pw.offset
	pw.writeStr("xref\n")
	pw.writeStr(fmt.Sprintf("0 %d\n", totalObjects+1))
	pw.writeStr("0000000000 65535 f \n")
	for _, off := range xrefOffsets {
		fmt.Fprintf(pw.w, "%010d 00000 n \n", off)
		pw.offset += 20
	}
	pw.writeStr("trailer\n")
	pw.writeStr(fmt.Sprintf("<< /Size %d /Root 1 0 R >>\n", totalObjects+1))
	pw.writeStr("startxref\n")
	pw.writeStr(fmt.Sprintf("%d\n", xrefStart))
	pw.writeStr("%%EOF\n")
}

func ConvertNoteToPDFVector(inputPath, outputPath string, noBg, parallel bool, cfg *Config) error {
	notebook, err := ParseNotebook(inputPath)
	if err != nil {
		return fmt.Errorf("parsing notebook: %w", err)
	}

	palette := BuildPalette(cfg.Note.ColorConfig, 0.2)

	width := notebook.Width
	height := notebook.Height
	pageWidthPt := float64(width) / notebook.PPI * 72.0
	pageHeightPt := float64(height) / notebook.PPI * 72.0
	totalPages := len(notebook.Pages)

	scale := 72.0 / notebook.PPI
	pageLinks := make(map[int][]pdfLink)
	for _, nl := range notebook.Links {
		if !nl.SameFile || nl.DestPage < 0 || nl.DestPage >= totalPages {
			continue
		}
		pageLinks[nl.SourcePage] = append(pageLinks[nl.SourcePage], pdfLink{
			Rect: [4]float64{
				float64(nl.X) * scale,
				pageHeightPt - float64(nl.Y+nl.H)*scale,
				float64(nl.X+nl.W) * scale,
				pageHeightPt - float64(nl.Y)*scale,
			},
			DestPage: nl.DestPage,
		})
	}

	type pageResult struct {
		colorLayers []colorLayer
		bgRGB       []byte
		err         error
	}

	results := make([]pageResult, totalPages)

	renderPage := func(i int) {
		page := notebook.Pages[i]

		layers, err := renderContentColorLayers(inputPath, page, width, height, palette)
		if err != nil {
			results[i].err = err
			return
		}
		results[i].colorLayers = layers

		if !noBg {
			bgRGB, err := renderBGLayerRGB(inputPath, page, width, height, palette)
			if err != nil {
				results[i].err = err
				return
			}
			allWhite := true
			for _, b := range bgRGB {
				if b != 0xFF {
					allWhite = false
					break
				}
			}
			if !allWhite {
				results[i].bgRGB = bgRGB
			}
		}
	}

	if parallel {
		var wg sync.WaitGroup
		sem := make(chan struct{}, runtime.GOMAXPROCS(0))
		for i := range notebook.Pages {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				renderPage(i)
			}()
		}
		wg.Wait()
	} else {
		for i := range notebook.Pages {
			renderPage(i)
		}
	}

	for i, r := range results {
		if r.err != nil {
			return fmt.Errorf("rendering page %d: %w", i+1, r.err)
		}
	}

	nextObjID := 3
	pageObjIDs := make([]int, totalPages)
	chunks := make([]vectorPageChunk, totalPages)

	for i := range results {
		pageObjIDs[i] = nextObjID
		chunk, numObjs := buildVectorPageChunk(
			results[i].colorLayers,
			results[i].bgRGB,
			width, height,
			pageWidthPt, pageHeightPt,
			pageLinks[i],
			nextObjID,
			true,
		)
		chunks[i] = chunk
		nextObjID += numObjs
	}

	// Replace PAGEOBJ_N placeholders with actual object IDs for link annotations
	for i := range chunks {
		data := chunks[i].objects[0].data
		for destPage, destObjID := range pageObjIDs {
			placeholder := fmt.Appendf(nil, "PAGEOBJ_%d", destPage)
			replacement := fmt.Appendf(nil, "%d 0 R", destObjID)
			data = bytes.ReplaceAll(data, placeholder, replacement)
		}
		chunks[i].objects[0].data = data
	}

	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	pw := &pdfWriter{w: bufio.NewWriter(outFile)}
	totalObjects := nextObjID - 1
	xrefOffsets := make([]uint64, totalObjects)

	pw.writeHeader()

	xrefOffsets[0] = pw.offset
	pw.write([]byte("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n"))

	xrefOffsets[1] = pw.offset
	var pageRefs strings.Builder
	for i := range totalPages {
		if i > 0 {
			pageRefs.WriteByte(' ')
		}
		fmt.Fprintf(&pageRefs, "%d 0 R", pageObjIDs[i])
	}
	pw.writeStr(fmt.Sprintf("2 0 obj\n<< /Type /Pages /Kids [ %s ] /Count %d >>\nendobj\n", pageRefs.String(), totalPages))

	for _, chunk := range chunks {
		for _, obj := range chunk.objects {
			xrefOffsets[obj.id-1] = pw.offset
			pw.write(obj.data)
		}
	}

	pw.writeXrefTrailer(xrefOffsets, totalObjects)
	return pw.w.Flush()
}

// writeOnePageVectorPDF writes a single-page vector PDF.
// Used for mark overlay pages that get stamped onto the companion PDF via pdfcpu.
func writeOnePageVectorPDF(outPath string, chunk vectorPageChunk, pageWidthPt, pageHeightPt float64) error {
	outFile, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	pageObjID := 3
	numChunkObjs := len(chunk.objects)
	totalObjects := 2 + numChunkObjs
	xrefOffsets := make([]uint64, totalObjects)

	pw := &pdfWriter{w: bufio.NewWriter(outFile)}
	pw.writeHeader()

	xrefOffsets[0] = pw.offset
	pw.write([]byte("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n"))

	xrefOffsets[1] = pw.offset
	pw.writeStr(fmt.Sprintf("2 0 obj\n<< /Type /Pages /Kids [ %d 0 R ] /Count 1 >>\nendobj\n", pageObjID))

	for _, obj := range chunk.objects {
		xrefOffsets[obj.id-1] = pw.offset
		pw.write(obj.data)
	}

	pw.writeXrefTrailer(xrefOffsets, totalObjects)
	return pw.w.Flush()
}
