package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

const (
	NomadWidth  = 1404
	NomadHeight = 1872
	NomadPPI    = 300.0

	MantaWidth  = 1920
	MantaHeight = 2560
	MantaPPI    = 300.0
)

type NoteLink struct {
	SourcePage int
	X, Y, W, H int
	DestPage   int
	SameFile   bool
}

type Notebook struct {
	Signature string
	Pages     []Page
	Links     []NoteLink
	FileID    string
	Width     int
	Height    int
	PPI       float64
}

type Page struct {
	Addr   uint64
	Layers []Layer
	Number int
}

type Layer struct {
	Key           string
	Protocol      string
	LayerType     string
	BitmapAddress uint64
}

func readUint32(r io.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

func getSignature(f *os.File) (string, error) {
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		return "", err
	}
	var buf [20]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return "", err
	}
	return string(buf[:]), nil
}

// parseMetadataBlock reads a metadata block at the given address.
// The binary format is: 4-byte length, then <KEY1:VALUE1><KEY2:VALUE2>...
func parseMetadataBlock(f *os.File, addr uint64) (map[string]string, error) {
	if addr == 0 {
		return map[string]string{}, nil
	}
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

	result := make(map[string]string)
	i := 0
	for i < len(buf) {
		// Find opening '<'
		if buf[i] != '<' {
			i++
			continue
		}
		i++ // skip '<'

		// Find ':' separator
		colonIdx := -1
		for j := i; j < len(buf); j++ {
			if buf[j] == ':' {
				colonIdx = j
				break
			}
			if buf[j] == '>' || buf[j] == '<' {
				break
			}
		}
		if colonIdx < 0 {
			continue
		}

		key := string(buf[i:colonIdx])

		// Find closing '>'
		closeIdx := -1
		for j := colonIdx + 1; j < len(buf); j++ {
			if buf[j] == '>' {
				closeIdx = j
				break
			}
		}
		if closeIdx < 0 {
			break
		}

		value := string(buf[colonIdx+1 : closeIdx])
		result[key] = value
		i = closeIdx + 1
	}
	return result, nil
}

// detectDeviceDimensions checks the header metadata for the Supernote model.
// "N5" in APPLY_EQUIPMENT = Manta, otherwise Nomad.
func detectDeviceDimensions(f *os.File, footerMap map[string]string) (int, int, float64, map[string]string) {
	if addrStr, ok := footerMap["FILE_FEATURE"]; ok {
		if addr, err := strconv.ParseUint(addrStr, 10, 64); err == nil {
			if headerMap, err := parseMetadataBlock(f, addr); err == nil {
				if equip, ok := headerMap["APPLY_EQUIPMENT"]; ok && equip == "N5" {
					return MantaWidth, MantaHeight, MantaPPI, headerMap
				}
				return NomadWidth, NomadHeight, NomadPPI, headerMap
			}
		}
	}
	return NomadWidth, NomadHeight, NomadPPI, nil
}

var defaultLayerOrder = []string{"BGLAYER", "MAINLAYER", "LAYER1", "LAYER2", "LAYER3"}

func ParseNotebook(path string) (*Notebook, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sig, err := getSignature(f)
	if err != nil {
		return nil, fmt.Errorf("reading signature: %w", err)
	}

	// Footer address is stored in the last 4 bytes of the file
	if _, err := f.Seek(-4, io.SeekEnd); err != nil {
		return nil, err
	}
	footerAddr, err := readUint32(f)
	if err != nil {
		return nil, err
	}

	footerMap, err := parseMetadataBlock(f, uint64(footerAddr))
	if err != nil {
		return nil, fmt.Errorf("reading footer: %w", err)
	}

	width, height, ppi, headerMap := detectDeviceDimensions(f, footerMap)
	var fileID string
	if headerMap != nil {
		fileID = headerMap["FILE_ID"]
	}

	type pageEntry struct {
		index int
		addr  uint64
	}
	var pageEntries []pageEntry
	for k, v := range footerMap {
		if !strings.HasPrefix(k, "PAGE") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(k, "PAGE"))
		if err != nil {
			continue
		}
		addr, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			continue
		}
		pageEntries = append(pageEntries, pageEntry{idx, addr})
	}
	slices.SortFunc(pageEntries, func(a, b pageEntry) int {
		return a.index - b.index
	})

	var pages []Page
	for _, pe := range pageEntries {
		pageMap, err := parseMetadataBlock(f, pe.addr)
		if err != nil {
			return nil, fmt.Errorf("reading page at %d: %w", pe.addr, err)
		}

		layerOrder := defaultLayerOrder
		if seq, ok := pageMap["LAYERSEQ"]; ok {
			layerOrder = strings.Split(seq, ",")
		}

		var layers []Layer
		for _, key := range layerOrder {
			addrStr, ok := pageMap[key]
			if !ok {
				continue
			}
			layerAddr, err := strconv.ParseUint(addrStr, 10, 64)
			if err != nil {
				continue
			}
			data, err := parseMetadataBlock(f, layerAddr)
			if err != nil {
				continue
			}

			var bitmapAddr uint64
			if s, ok := data["LAYERBITMAP"]; ok {
				bitmapAddr, _ = strconv.ParseUint(s, 10, 64)
			}

			layers = append(layers, Layer{
				Key:           key,
				Protocol:      data["LAYERPROTOCOL"],
				LayerType:     data["LAYERTYPE"],
				BitmapAddress: bitmapAddr,
			})
		}

		pages = append(pages, Page{Addr: pe.addr, Layers: layers, Number: pe.index})
	}

	links := parseLinks(f, footerMap, fileID)

	return &Notebook{
		Signature: sig,
		Pages:     pages,
		Links:     links,
		FileID:    fileID,
		Width:     width,
		Height:    height,
		PPI:       ppi,
	}, nil
}

func parseLinks(f *os.File, footerMap map[string]string, fileID string) []NoteLink {
	var links []NoteLink
outer:
	for k, v := range footerMap {
		if !strings.HasPrefix(k, "LINKO_") || len(k) < 10 {
			continue
		}
		srcPage, err := strconv.Atoi(k[6:10])
		if err != nil {
			continue
		}
		addr, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			continue
		}
		linkMap, err := parseMetadataBlock(f, addr)
		if err != nil {
			continue
		}

		rectStr, ok := linkMap["LINKRECT"]
		if !ok {
			continue
		}
		parts := strings.Split(rectStr, ",")
		if len(parts) != 4 {
			continue
		}
		var nums [4]int
		for i, p := range parts {
			nums[i], err = strconv.Atoi(p)
			if err != nil {
				continue outer
			}
		}
		x, y, w, h := nums[0], nums[1], nums[2], nums[3]

		// Destination page is 1-indexed in the file format
		destPageStr, ok := linkMap["OBJPAGE"]
		if !ok {
			continue
		}
		destPage, err := strconv.Atoi(destPageStr)
		if err != nil {
			continue
		}

		sameFile := fileID != "" && linkMap["LINKFILEID"] == fileID

		links = append(links, NoteLink{
			SourcePage: srcPage - 1,
			X:          x,
			Y:          y,
			W:          w,
			H:          h,
			DestPage:   destPage - 1,
			SameFile:   sameFile,
		})
	}
	return links
}
