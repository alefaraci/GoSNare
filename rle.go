package main

type Palette struct {
	Colors [256][3]byte
	Alphas [256]byte
}

// BuildPalette constructs a palette by interpolating between four anchor colors.
// Anchors: Black (0), Dark Gray (157), Light Gray (201), White (255).
func BuildPalette(cfg ColorConfig, markerOpacity float64) *Palette {
	p := &Palette{}

	bR, bG, bB, _ := parseHexColor(cfg.Black)
	dgR, dgG, dgB, _ := parseHexColor(cfg.DarkGray)
	lgR, lgG, lgB, _ := parseHexColor(cfg.LightGray)
	wR, wG, wB, _ := parseHexColor(cfg.White)

	anchors := []struct {
		pos     int
		r, g, b byte
	}{
		{0, bR, bG, bB},
		{157, dgR, dgG, dgB},
		{201, lgR, lgG, lgB},
		{255, wR, wG, wB},
	}

	for i := 0; i < len(anchors)-1; i++ {
		start := anchors[i]
		end := anchors[i+1]
		dist := end.pos - start.pos
		for j := start.pos; j <= end.pos; j++ {
			f := float64(j-start.pos) / float64(dist)
			p.Colors[j][0] = byte(float64(start.r) + f*float64(int(end.r)-int(start.r)))
			p.Colors[j][1] = byte(float64(start.g) + f*float64(int(end.g)-int(start.g)))
			p.Colors[j][2] = byte(float64(start.b) + f*float64(int(end.b)-int(start.b)))
		}
	}

	for i := range p.Alphas {
		p.Alphas[i] = 0xFF
	}

	// Specialized pen codes map to their anchor colors
	p.Colors[0x61] = p.Colors[0]   // Black
	p.Colors[0x63] = p.Colors[157] // Dark Gray
	p.Colors[0x64] = p.Colors[201] // Light Gray
	p.Colors[0x65] = p.Colors[255] // White

	mOpacity := byte(markerOpacity * 255)
	if mOpacity == 0 {
		mOpacity = 0x26 // ~15% default
	}

	p.Colors[0x66] = p.Colors[0]   // Marker Black
	p.Colors[0x67] = p.Colors[157] // Marker Dark Gray
	p.Colors[0x68] = p.Colors[201] // Marker Light Gray

	p.Alphas[0x66] = mOpacity
	p.Alphas[0x67] = mOpacity
	p.Alphas[0x68] = mOpacity

	// Compatibility entries for alternate dark/light gray codes
	p.Colors[0x9d] = p.Colors[157]
	p.Colors[0x9e] = p.Colors[157]
	p.Colors[0xc9] = p.Colors[201]
	p.Colors[0xca] = p.Colors[201]

	return p
}

// identityPalette is a grayscale palette where each byte value maps to itself.
// Cached at package level since it never changes.
var identityPalette = buildIdentityPalette()

func buildIdentityPalette() *Palette {
	p := &Palette{}
	for i := range 256 {
		p.Colors[i] = [3]byte{byte(i), byte(i), byte(i)}
		p.Alphas[i] = 0xFF
	}
	p.Colors[0x61] = [3]byte{0, 0, 0}
	p.Colors[0x63] = [3]byte{157, 157, 157}
	p.Colors[0x64] = [3]byte{201, 201, 201}
	p.Colors[0x65] = [3]byte{255, 255, 255}
	p.Colors[0x66] = [3]byte{0, 0, 0}
	p.Colors[0x67] = [3]byte{157, 157, 157}
	p.Colors[0x68] = [3]byte{201, 201, 201}
	return p
}

// IdentityPalette returns the cached identity (grayscale) palette.
func IdentityPalette() *Palette {
	return identityPalette
}

// decodeRLE runs the RATTA_RLE state machine and calls emit for each non-transparent run.
// emit receives the pixel position, run length, and raw color code.
func decodeRLE(data []byte, width, height int, emit func(pos, length int, colorCode byte)) {
	expected := width * height
	pos := 0

	var heldColor, heldLength byte
	var hasHolder bool

	i := 0
	for i+1 < len(data) && pos < expected {
		colorCode := data[i]
		lengthCode := data[i+1]
		i += 2

		var length int

		if hasHolder {
			prevColor, prevLength := heldColor, heldLength
			hasHolder = false

			if colorCode == prevColor {
				length = 1 + int(lengthCode) + ((int(prevLength&0x7f) + 1) << 7)
			} else {
				heldLen := (int(prevLength&0x7f) + 1) << 7
				if pos+heldLen > expected {
					heldLen = expected - pos
				}
				if prevColor != 0x62 {
					emit(pos, heldLen, prevColor)
				}
				pos += heldLen
				length = int(lengthCode) + 1
			}
		} else if lengthCode == 0xff {
			length = 0x4000
		} else if lengthCode&0x80 != 0 {
			heldColor, heldLength = colorCode, lengthCode
			hasHolder = true
			continue
		} else {
			length = int(lengthCode) + 1
		}

		if pos+length > expected {
			length = expected - pos
		}

		if colorCode != 0x62 {
			emit(pos, length, colorCode)
		}
		pos += length
	}

	if hasHolder && pos < expected {
		tailLen := (int(heldLength&0x7f) + 1) << 7
		if remaining := expected - pos; tailLen > remaining {
			tailLen = remaining
		}
		if tailLen > 0 && heldColor != 0x62 {
			emit(pos, tailLen, heldColor)
		}
	}
}

func decodeRLEToRGB(data []byte, rgb []byte, width, height int, p *Palette) {
	decodeRLE(data, width, height, func(pos, length int, colorCode byte) {
		c := p.Colors[colorCode]
		fillRGB(rgb, pos, length, c[0], c[1], c[2])
	})
}

func decodeRLEToRGBA(data []byte, rgba []byte, width, height int, p *Palette) {
	decodeRLE(data, width, height, func(pos, length int, colorCode byte) {
		c := p.Colors[colorCode]
		fillRGBA(rgba, pos, length, c[0], c[1], c[2], p.Alphas[colorCode])
	})
}

func fillRGBA(rgba []byte, pos, count int, r, g, b byte, alpha byte) {
	start := pos * 4
	end := min(start+count*4, len(rgba))
	if start >= end {
		return
	}
	rgba[start] = r
	rgba[start+1] = g
	rgba[start+2] = b
	rgba[start+3] = alpha
	for filled := 4; filled < end-start; filled *= 2 {
		copy(rgba[start+filled:end], rgba[start:start+filled])
	}
}

func fillRGB(rgb []byte, pos, count int, r, g, b byte) {
	start := pos * 3
	end := min(start+count*3, len(rgb))
	n := end - start
	if n <= 0 {
		return
	}
	rgb[start] = r
	rgb[start+1] = g
	rgb[start+2] = b
	for filled := 1; filled < count; filled *= 2 {
		copy(rgb[start+filled*3:end], rgb[start:start+filled*3])
	}
}

func fillCodes(buf []byte, pos, count int, code byte) {
	end := min(pos+count, len(buf))
	if pos >= end {
		return
	}
	buf[pos] = code
	for filled := 1; filled < end-pos; filled *= 2 {
		copy(buf[pos+filled:end], buf[pos:pos+filled])
	}
}
