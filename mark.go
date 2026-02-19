package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dennwc/gotrace"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcolor "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/color"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

type MarkAnnotation struct {
	AnnotationType int         `json:"annotationType"` // 0=Highlight, 1=Underline
	ColorType      int         `json:"colorType"`      // 0=Yellow, 4=Red
	Page           int         `json:"page"`
	MupdfRects     []MupdfRect `json:"mupdfRectList"`
}

// MupdfRect is a rectangle in mupdf coordinate space (origin top-left, y downward).
type MupdfRect struct {
	X0 float64 `json:"x0"`
	X1 float64 `json:"x1"`
	Y0 float64 `json:"y0"`
	Y1 float64 `json:"y1"`
}

func renderMarkPageRGBA(path string, page Page, width, height int, p *Palette) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	totalPixels := width * height
	rgba := make([]byte, totalPixels*4)

	for _, layer := range page.Layers {
		if layer.BitmapAddress == 0 || layer.LayerType != "MARK" {
			continue
		}

		switch layer.Protocol {
		case "RATTA_RLE":
			data, err := readLayerData(f, layer.BitmapAddress)
			if err != nil {
				return nil, fmt.Errorf("reading RLE layer %s: %w", layer.Key, err)
			}
			decodeRLEToRGBA(data, rgba, width, height, p)

		case "PNG":
			img, err := decodePNGLayer(f, layer.BitmapAddress)
			if err != nil {
				return nil, fmt.Errorf("decoding PNG layer %s: %w", layer.Key, err)
			}
			compositePNGToRGBA(img, rgba, width, height)
		}
	}

	return rgba, nil
}

// compositePNGToRGBA composites a decoded PNG image onto an RGBA buffer using source-over blending.
func compositePNGToRGBA(img image.Image, rgba []byte, width, height int) {
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
				dOff := (y*width + x) * 4
				if sa == 255 {
					rgba[dOff] = src.Pix[pOff]
					rgba[dOff+1] = src.Pix[pOff+1]
					rgba[dOff+2] = src.Pix[pOff+2]
					rgba[dOff+3] = 0xFF
				} else {
					sa32 := uint32(sa)
					da32 := uint32(rgba[dOff+3])
					invSa := 255 - sa32
					outA := sa32 + da32*invSa/255
					if outA == 0 {
						continue
					}
					rgba[dOff] = byte((uint32(src.Pix[pOff])*sa32 + uint32(rgba[dOff])*da32*invSa/255) / outA)
					rgba[dOff+1] = byte((uint32(src.Pix[pOff+1])*sa32 + uint32(rgba[dOff+1])*da32*invSa/255) / outA)
					rgba[dOff+2] = byte((uint32(src.Pix[pOff+2])*sa32 + uint32(rgba[dOff+2])*da32*invSa/255) / outA)
					rgba[dOff+3] = byte(outA)
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
			dOff := (y*width + x) * 4
			if a == 0xFFFF {
				rgba[dOff] = byte(r >> 8)
				rgba[dOff+1] = byte(g >> 8)
				rgba[dOff+2] = byte(b >> 8)
				rgba[dOff+3] = 0xFF
			} else {
				sa := uint32(a >> 8)
				invSa := 255 - sa
				da := uint32(rgba[dOff+3])
				rgba[dOff] = byte(uint32(r>>8) + uint32(rgba[dOff])*invSa/255)
				rgba[dOff+1] = byte(uint32(g>>8) + uint32(rgba[dOff+1])*invSa/255)
				rgba[dOff+2] = byte(uint32(b>>8) + uint32(rgba[dOff+2])*invSa/255)
				rgba[dOff+3] = byte(sa + da*invSa/255)
			}
		}
	}
}

func hasVisiblePixels(rgba []byte) bool {
	for i := 3; i < len(rgba); i += 4 {
		if rgba[i] != 0 {
			return true
		}
	}
	return false
}

func annotationColor(colorType int) pdfcolor.SimpleColor {
	switch colorType {
	case 4:
		return pdfcolor.SimpleColor{R: 1, G: 0, B: 0}
	default:
		return pdfcolor.SimpleColor{R: 1, G: 1, B: 0}
	}
}

// parseMarkAnnotations reads highlight/underline annotations from a .mark file's
// HIGHLIGHTINFO metadata (base64-encoded JSON with quad points).
func parseMarkAnnotations(path string) (map[int][]MarkAnnotation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(-4, io.SeekEnd); err != nil {
		return nil, err
	}
	footerAddr, err := readUint32(f)
	if err != nil {
		return nil, err
	}

	footerMap, err := parseMetadataBlock(f, uint64(footerAddr))
	if err != nil {
		return nil, err
	}

	featureStr, ok := footerMap["FILE_FEATURE"]
	if !ok {
		return nil, nil
	}
	featureAddr, err := strconv.ParseUint(featureStr, 10, 64)
	if err != nil {
		return nil, nil
	}
	featureMap, err := parseMetadataBlock(f, featureAddr)
	if err != nil {
		return nil, err
	}

	highlightStr, ok := featureMap["HIGHLIGHTINFO"]
	if !ok {
		return nil, nil
	}

	highlightAddr, err := strconv.ParseUint(highlightStr, 10, 64)
	if err != nil {
		return nil, nil
	}

	raw, err := readLayerData(f, highlightAddr)
	if err != nil {
		return nil, nil // highlight data corrupt/truncated; skip gracefully
	}

	jsonBytes, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		return nil, fmt.Errorf("decoding highlight base64: %w", err)
	}

	var rawMap map[string][]MarkAnnotation
	if err := json.Unmarshal(jsonBytes, &rawMap); err != nil {
		return nil, fmt.Errorf("parsing highlight JSON: %w", err)
	}

	result := make(map[int][]MarkAnnotation, len(rawMap))
	for k, v := range rawMap {
		idx, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		result[idx] = v
	}
	return result, nil
}

// expandPDFMediaBox expands the PDF MediaBox/CropBox to match the notebook aspect ratio.
func expandPDFMediaBox(pdfPath, outputPath string, dims []types.Dim, width, height int) error {
	d := dims[0]
	targetAspect := float64(width) / float64(height)
	currentAspect := d.Width / d.Height

	var llx, lly, urx, ury float64
	if math.Abs(currentAspect-targetAspect) < 0.001 {
		llx, lly = 0.0, 0.0
		urx, ury = d.Width, d.Height
	} else if currentAspect > targetAspect {
		newH := d.Width / targetAspect
		dy := (newH - d.Height) / 2
		llx, lly = 0.0, -dy
		urx, ury = d.Width, d.Height+dy
	} else {
		newW := d.Height * targetAspect
		dx := (newW - d.Width) / 2
		llx, lly = -dx, 0.0
		urx, ury = d.Width+dx, d.Height
	}

	pb := &model.PageBoundaries{
		Media: &model.Box{Rect: types.NewRectangle(llx, lly, urx, ury)},
		Crop:  &model.Box{Rect: types.NewRectangle(llx, lly, urx, ury)},
	}

	if err := api.AddBoxesFile(pdfPath, outputPath, nil, pb, nil); err != nil {
		return fmt.Errorf("expanding PDF boundaries: %w", err)
	}
	return nil
}

// traceAndOverlayMask traces a grayscale mask via potrace and stamps the resulting
// vector overlay onto outputPath at the given page.
func traceAndOverlayMask(
	mask *image.Gray, p *Palette,
	width, height int,
	pageWidthPt, pageHeightPt float64,
	tmpDir string, pageIndex, pageNumber int,
	outputPath string, pageStr []string,
	label, wmDesc string,
	traceParams *gotrace.Params,
) error {
	bm := gotrace.NewBitmapFromImage(mask, func(x, y int, cl color.Color) bool {
		v, _, _, _ := cl.RGBA()
		return v < 0x8000
	})
	paths, err := gotrace.Trace(bm, traceParams)
	if err != nil {
		return fmt.Errorf("tracing %s mask page %d: %w", label, pageNumber, err)
	}
	if len(paths) == 0 {
		return nil
	}

	cl := colorLayer{
		r: p.Colors[0][0], g: p.Colors[0][1], b: p.Colors[0][2],
		alpha: 255, paths: paths,
	}
	chunk, _ := buildVectorPageChunk(
		[]colorLayer{cl},
		nil, width, height,
		pageWidthPt, pageHeightPt,
		nil, 3,
		false,
	)
	overlayPath := filepath.Join(tmpDir, fmt.Sprintf("vector_%s_%d.pdf", label, pageIndex))
	if err := writeOnePageVectorPDF(overlayPath, chunk, pageWidthPt, pageHeightPt); err != nil {
		return fmt.Errorf("writing %s vector overlay for page %d: %w", label, pageNumber, err)
	}
	if err := api.AddPDFWatermarksFile(
		outputPath, "", pageStr, true,
		overlayPath, wmDesc, nil,
	); err != nil {
		return fmt.Errorf("stamping %s vector page %d: %w", label, pageNumber, err)
	}
	return nil
}

// applyHighlightAnnotations parses HIGHLIGHTINFO metadata from the mark file
// and stamps highlight/underline annotations onto the output PDF.
func applyHighlightAnnotations(markPath, outputPath string, dims []types.Dim) error {
	markAnnotations, err := parseMarkAnnotations(markPath)
	if err != nil {
		return fmt.Errorf("parsing mark annotations: %w", err)
	}

	if len(markAnnotations) == 0 {
		return nil
	}

	annotMap := make(map[int][]model.AnnotationRenderer)
	annID := 0

	for pageIdx, anns := range markAnnotations {
		pageNum := pageIdx + 1

		var pageHeight float64
		if pageIdx < len(dims) {
			pageHeight = dims[pageIdx].Height
		} else {
			pageHeight = dims[0].Height
		}

		for _, ann := range anns {
			if len(ann.MupdfRects) == 0 {
				continue
			}

			col := annotationColor(ann.ColorType)

			var quadPoints types.QuadPoints
			minX, minY := math.MaxFloat64, math.MaxFloat64
			maxX, maxY := -math.MaxFloat64, -math.MaxFloat64

			for _, mr := range ann.MupdfRects {
				x0 := mr.X0
				x1 := mr.X1
				y0 := pageHeight - mr.Y1
				y1 := pageHeight - mr.Y0

				rect := types.NewRectangle(x0, y0, x1, y1)
				ql := types.NewQuadLiteralForRect(rect)
				quadPoints = append(quadPoints, *ql)

				minX = min(minX, x0)
				maxX = max(maxX, x1)
				minY = min(minY, y0)
				maxY = max(maxY, y1)
			}

			boundingRect := types.NewRectangle(minX, minY, maxX, maxY)
			annID++
			id := fmt.Sprintf("sn_%d", annID)

			var ar model.AnnotationRenderer
			switch ann.AnnotationType {
			case 0:
				ar = model.NewHighlightAnnotation(
					*boundingRect, 0, "", id, "",
					0, &col, 0, 0, 0, "", nil, nil, "", "",
					quadPoints,
				)
			case 1:
				ar = model.NewUnderlineAnnotation(
					*boundingRect, 0, "", id, "",
					0, &col, 0, 0, 0, "", nil, nil, "", "",
					quadPoints,
				)
			default:
				continue
			}

			annotMap[pageNum] = append(annotMap[pageNum], ar)
		}
	}

	if len(annotMap) > 0 {
		conf := model.NewDefaultConfiguration()
		if err := api.AddAnnotationsMapFile(outputPath, "", annotMap, conf, true); err != nil {
			return fmt.Errorf("adding annotations: %w", err)
		}
	}

	return nil
}

// ConvertMarkToPDFVector traces mark annotations as vector paths and stamps them onto the companion PDF.
func ConvertMarkToPDFVector(markPath, pdfPath, outputPath string, parallel bool, cfg *Config) error {
	notebook, err := ParseNotebook(markPath)
	if err != nil {
		return fmt.Errorf("parsing mark file: %w", err)
	}

	width := notebook.Width
	height := notebook.Height
	pageWidthPt := float64(width) / notebook.PPI * 72.0
	pageHeightPt := float64(height) / notebook.PPI * 72.0

	dims, err := api.PageDimsFile(pdfPath)
	if err != nil {
		return fmt.Errorf("reading PDF page dims: %w", err)
	}
	if len(dims) == 0 {
		return fmt.Errorf("no pages found in PDF")
	}

	tmpDir, err := os.MkdirTemp("", "supernote-mark-vector-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := expandPDFMediaBox(pdfPath, outputPath, dims, width, height); err != nil {
		return err
	}

	p := BuildPalette(cfg.Mark.ColorConfig, cfg.Mark.MarkerOpacity)

	// .mark files encode marker strokes as regular light gray values (>= 196),
	// not as special marker codes 0x66-0x68. Use identity palette + grayscale
	// threshold for pen/marker separation, then apply config colors.
	const markerThreshold = 196
	traceParams := gotrace.Defaults
	traceParams.TurdSize = 2

	for i, page := range notebook.Pages {
		rgba, err := renderMarkPageRGBA(markPath, page, width, height, IdentityPalette())
		if err != nil {
			return fmt.Errorf("rendering mark page %d: %w", page.Number, err)
		}
		if !hasVisiblePixels(rgba) {
			continue
		}

		penMask := image.NewGray(image.Rect(0, 0, width, height))
		markerMask := image.NewGray(image.Rect(0, 0, width, height))
		for j := range penMask.Pix {
			penMask.Pix[j] = 0xFF
			markerMask.Pix[j] = 0xFF
		}
		hasPen, hasMarker := false, false
		for pix := 0; pix < len(rgba); pix += 4 {
			if rgba[pix+3] == 0 {
				continue
			}
			gray := rgba[pix]
			idx := pix / 4
			if gray >= markerThreshold {
				markerMask.Pix[idx] = 0x00
				hasMarker = true
			} else {
				penMask.Pix[idx] = 0x00
				hasPen = true
			}
		}

		pageStr := []string{strconv.Itoa(page.Number)}

		if hasPen {
			if err := traceAndOverlayMask(
				penMask, p, width, height,
				pageWidthPt, pageHeightPt,
				tmpDir, i, page.Number,
				outputPath, pageStr,
				"pen", "pos:c, scale:1 rel, rotation:0",
				&traceParams,
			); err != nil {
				return err
			}
		}

		if hasMarker {
			desc := fmt.Sprintf("pos:c, scale:1 rel, rotation:0, opacity:%.2f", cfg.Mark.MarkerOpacity)
			if err := traceAndOverlayMask(
				markerMask, p, width, height,
				pageWidthPt, pageHeightPt,
				tmpDir, i, page.Number,
				outputPath, pageStr,
				"marker", desc,
				&traceParams,
			); err != nil {
				return err
			}
		}
	}

	return applyHighlightAnnotations(markPath, outputPath, dims)
}
