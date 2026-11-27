//go:build windows

package main

import (
	"math"
	"unsafe"
)

const (
	blendFuncAlpha = 0x01FF0000 // AC_SRC_OVER (0) | Flags (0) | Alpha (255) | AC_SRC_ALPHA (1)
)

// sdSegmentDist returns distance from (px, py) to segment (x1, y1)-(x2, y2).
func sdSegmentDist(px, py, x1, y1, x2, y2 float64) float64 {
	vx, vy := x2-x1, y2-y1
	wx, wy := px-x1, py-y1
	c1 := vx*wx + vy*wy
	if c1 <= 0 {
		return math.Hypot(px-x1, py-y1)
	}
	c2 := vx*vx + vy*vy
	if c2 <= c1 {
		return math.Hypot(px-x2, py-y2)
	}
	t := c1 / c2
	return math.Hypot(px-(x1+t*vx), py-(y1+t*vy))
}

// sdRoundedBoxDist returns signed distance to a rounded box centered at (cx, cy).
func sdRoundedBoxDist(px, py, cx, cy, hx, hy, r float64) float64 {
	dx := math.Abs(px-cx) - (hx - r)
	dy := math.Abs(py-cy) - (hy - r)
	return math.Hypot(math.Max(dx, 0), math.Max(dy, 0)) + math.Min(math.Max(dx, dy), 0) - r
}

func toolbarIconAccentRGB(cmdID uintptr, dark bool) (uint8, uint8, uint8) {
	if dark {
		switch cmdID {
		case cmdAddURL:
			return 74, 222, 128 // Emerald
		case cmdResume:
			return 96, 165, 250 // Sky blue
		case cmdPause:
			return 251, 191, 36 // Amber
		case cmdCancel:
			return 248, 113, 113 // Coral red
		case cmdDelete:
			return 251, 113, 133 // Rose
		case cmdOpenFolder:
			return 245, 158, 11 // Warm gold
		case cmdClearDone:
			return 232, 121, 249 // Orchid
		case cmdToolsMenu:
			return 167, 139, 250 // Soft violet
		default: // Search magnifying glass
			return 156, 163, 175 // Muted cool gray
		}
	}
	switch cmdID {
	case cmdAddURL:
		return 22, 163, 74
	case cmdResume:
		return 37, 99, 235
	case cmdPause:
		return 217, 119, 6
	case cmdCancel:
		return 220, 38, 38
	case cmdDelete:
		return 225, 29, 72
	case cmdOpenFolder:
		return 202, 138, 4
	case cmdClearDone:
		return 162, 28, 175
	case cmdToolsMenu:
		return 109, 40, 217
	default:
		return 100, 116, 139
	}
}

// toolbarIconSDF evaluates the signed distance (<=0 inside stroke) in a 32x32 virtual grid.
func toolbarIconSDF(cmdID uintptr, px, py float64) float64 {
	switch cmdID {
	case cmdAddURL:
		// Crisp bold plus (+)
		d1 := sdSegmentDist(px, py, 16.0, 5.5, 16.0, 26.5) - 1.55
		d2 := sdSegmentDist(px, py, 5.5, 16.0, 26.5, 16.0) - 1.55
		return math.Min(d1, d2)

	case cmdResume:
		// Smooth circle + downward download arrow
		dRing := math.Abs(math.Hypot(px-16.0, py-16.0)-12.4) - 1.15
		dStem := sdSegmentDist(px, py, 16.0, 9.2, 16.0, 21.8) - 1.2
		dLeft := sdSegmentDist(px, py, 11.4, 17.4, 16.0, 22.0) - 1.2
		dRight := sdSegmentDist(px, py, 16.0, 22.0, 20.6, 17.4) - 1.2
		return math.Min(math.Min(dRing, dStem), math.Min(dLeft, dRight))

	case cmdPause:
		// Smooth circle + two balanced pause bars
		dRing := math.Abs(math.Hypot(px-16.0, py-16.0)-12.4) - 1.15
		dBarL := sdSegmentDist(px, py, 12.6, 10.6, 12.6, 21.4) - 1.4
		dBarR := sdSegmentDist(px, py, 19.4, 10.6, 19.4, 21.4) - 1.4
		return math.Min(dRing, math.Min(dBarL, dBarR))

	case cmdCancel:
		// Smooth circle + crisp rounded stop square
		dRing := math.Abs(math.Hypot(px-16.0, py-16.0)-12.4) - 1.15
		dBox := math.Abs(sdRoundedBoxDist(px, py, 16.0, 16.0, 4.8, 4.8, 1.1)) - 1.15
		return math.Min(dRing, dBox)

	case cmdDelete:
		// Symmetric modern trash bin with top handle, lid, rounded body & two ribs
		dHandleTop := sdSegmentDist(px, py, 12.6, 6.2, 19.4, 6.2) - 1.1
		dHandleL := sdSegmentDist(px, py, 12.6, 6.2, 12.6, 9.4) - 1.1
		dHandleR := sdSegmentDist(px, py, 19.4, 6.2, 19.4, 9.4) - 1.1
		dLid := sdSegmentDist(px, py, 6.6, 9.6, 25.4, 9.6) - 1.15
		dBody := math.Abs(sdRoundedBoxDist(px, py, 16.0, 18.2, 6.9, 8.3, 1.8)) - 1.1
		// Keep top of body cleanly joined to lid line
		if py < 9.6 {
			dBody = 100
		}
		dRibL := sdSegmentDist(px, py, 13.3, 13.8, 13.3, 22.8) - 1.05
		dRibR := sdSegmentDist(px, py, 18.7, 13.8, 18.7, 22.8) - 1.05
		d := math.Min(math.Min(dHandleTop, math.Min(dHandleL, dHandleR)), dLid)
		d = math.Min(d, dBody)
		return math.Min(d, math.Min(dRibL, dRibR))

	case cmdOpenFolder:
		// Sleek folder outline with smooth angled top tab
		dL := sdSegmentDist(px, py, 5.2, 8.6, 5.2, 24.4) - 1.18
		dB := sdSegmentDist(px, py, 5.2, 24.4, 26.8, 24.4) - 1.18
		dR := sdSegmentDist(px, py, 26.8, 24.4, 26.8, 12.0) - 1.18
		dTR := sdSegmentDist(px, py, 26.8, 12.0, 15.6, 12.0) - 1.18
		dSlant := sdSegmentDist(px, py, 15.6, 12.0, 13.0, 8.6) - 1.18
		dTL := sdSegmentDist(px, py, 13.0, 8.6, 5.2, 8.6) - 1.18
		return math.Min(math.Min(math.Min(dL, dB), math.Min(dR, dTR)), math.Min(dSlant, dTL))

	case cmdClearDone:
		// Rounded check box with smooth checkmark
		dBox := math.Abs(sdRoundedBoxDist(px, py, 16.0, 16.0, 10.6, 10.6, 3.2)) - 1.15
		dCk1 := sdSegmentDist(px, py, 10.4, 16.4, 14.3, 20.4) - 1.3
		dCk2 := sdSegmentDist(px, py, 14.3, 20.4, 21.8, 11.8) - 1.3
		return math.Min(dBox, math.Min(dCk1, dCk2))

	case cmdToolsMenu:
		// True 8-tooth gear / cog icon
		dist := math.Hypot(px-16.0, py-16.0)
		dOuter := math.Abs(dist-8.4) - 1.35
		dInner := math.Abs(dist-3.8) - 1.15
		d := math.Min(dOuter, dInner)
		for i := 0; i < 8; i++ {
			ang := float64(i) * (math.Pi / 4.0)
			cosA, sinA := math.Cos(ang), math.Sin(ang)
			x1, y1 := 16.0+cosA*9.2, 16.0+sinA*9.2
			x2, y2 := 16.0+cosA*12.4, 16.0+sinA*12.4
			dTooth := sdSegmentDist(px, py, x1, y1, x2, y2) - 1.6
			if dTooth < d {
				d = dTooth
			}
		}
		return d

	default:
		// Crisp magnifying glass for search box
		dLens := math.Abs(math.Hypot(px-13.2, py-13.2)-7.6) - 1.65
		dHandle := sdSegmentDist(px, py, 18.8, 18.8, 26.6, 26.6) - 2.0
		return math.Min(dLens, dHandle)
	}
}

// createAntiAliasedIconBitmap renders a 4x4 supersampled premultiplied BGRA DIB bitmap
// for crisp, jagged-free rendering on Windows via AlphaBlend.
func createAntiAliasedIconBitmap(cmdID uintptr, size int, dark bool) uintptr {
	if size <= 0 {
		return 0
	}
	var bi bitmapInfoHeader
	bi.BiSize = uint32(unsafe.Sizeof(bi))
	bi.BiWidth = int32(size)
	bi.BiHeight = -int32(size) // top-down DIB
	bi.BiPlanes = 1
	bi.BiBitCount = 32
	bi.BiCompression = 0 // BI_RGB

	var bits unsafe.Pointer
	hdc, _, _ := procGetDC.Call(0)
	hBmp, _, _ := procCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hdc != 0 {
		procReleaseDC.Call(0, hdc)
	}
	if hBmp == 0 || bits == nil {
		return 0
	}

	pixels := unsafe.Slice((*byte)(bits), size*size*4)
	rCol, gCol, bCol := toolbarIconAccentRGB(cmdID, dark)
	const ss = 4
	invSS := 1.0 / float64(ss)
	invSamples := 1.0 / float64(ss*ss)
	aaWidth := 32.0 / float64(size) * 0.85

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var cov float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := (float64(x) + (float64(sx)+0.5)*invSS) * 32.0 / float64(size)
					py := (float64(y) + (float64(sy)+0.5)*invSS) * 32.0 / float64(size)
					d := toolbarIconSDF(cmdID, px, py)
					if d < aaWidth {
						a := 0.5 - d/aaWidth
						if a < 0 {
							a = 0
						} else if a > 1 {
							a = 1
						}
						cov += a
					}
				}
			}
			alpha := cov * invSamples
			if alpha <= 0.002 {
				continue
			}
			idx := (y*size + x) * 4
			aByte := uint8(math.Round(alpha * 255.0))
			// Premultiplied BGRA required by Win32 AlphaBlend with AC_SRC_ALPHA
			pixels[idx+0] = uint8(math.Round(float64(bCol) * alpha))
			pixels[idx+1] = uint8(math.Round(float64(gCol) * alpha))
			pixels[idx+2] = uint8(math.Round(float64(rCol) * alpha))
			pixels[idx+3] = aByte
		}
	}
	return hBmp
}

func blitAlphaBitmap(dstHDC, hBmp uintptr, dstX, dstY, size int32) {
	if dstHDC == 0 || hBmp == 0 || size <= 0 {
		return
	}
	memDC, _, _ := procCreateCompatibleDC.Call(dstHDC)
	if memDC == 0 {
		return
	}
	oldBmp, _, _ := procSelectObject.Call(memDC, hBmp)
	if procAlphaBlend.Find() == nil {
		procAlphaBlend.Call(
			dstHDC,
			uintptr(dstX), uintptr(dstY), uintptr(size), uintptr(size),
			memDC,
			0, 0, uintptr(size), uintptr(size),
			uintptr(blendFuncAlpha),
		)
	}
	if oldBmp != 0 {
		procSelectObject.Call(memDC, oldBmp)
	}
	procDeleteDC.Call(memDC)
}
