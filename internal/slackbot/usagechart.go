package slackbot

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/n-seiji/ebi-x/internal/state"
)

const (
	usageChartWidth   = 720
	usageChartHeight  = 320
	usageChartLeft    = 44
	usageChartRight   = 16
	usageChartTop     = 28
	usageChartBottom  = 28
	usageChartSpan    = 24 * time.Hour
	usageChartTickGap = 6 * time.Hour
	// usageChartMaxGap is the longest gap between two checks still joined
	// by a line; a longer one means ebi-x was not running.
	usageChartMaxGap = 3 * usageCheckInterval
)

var (
	usageChartBackground = color.RGBA{0xff, 0xff, 0xff, 0xff}
	usageChartGrid       = color.RGBA{0xe3, 0xe5, 0xe8, 0xff}
	usageChartAxis       = color.RGBA{0x99, 0x9d, 0xa3, 0xff}
	usageChartText       = color.RGBA{0x3c, 0x40, 0x46, 0xff}
	// usageChartColors and usageChartNames follow usageWindowKeys.
	usageChartColors = map[string]color.RGBA{
		"primary":   {0x1f, 0x6f, 0xd1, 0xff},
		"secondary": {0xe0, 0x7a, 0x10, 0xff},
	}
	usageChartNames = map[string]string{"primary": "5h", "secondary": "weekly"}
)

// renderUsageChart draws each window's remaining percent over the last
// usageChartSpan before now as a PNG line chart.
func renderUsageChart(history []state.UsageSample, now time.Time) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, usageChartWidth, usageChartHeight))
	draw.Draw(img, img.Bounds(), &image.Uniform{usageChartBackground}, image.Point{}, draw.Src)
	left, right := usageChartLeft, usageChartWidth-usageChartRight
	top, bottom := usageChartTop, usageChartHeight-usageChartBottom
	start := now.Add(-usageChartSpan)
	x := func(at time.Time) int {
		return left + int(float64(right-left)*float64(at.Sub(start))/float64(usageChartSpan))
	}
	y := func(remaining float64) int {
		remaining = min(max(remaining, 0), 100)
		return bottom - int(float64(bottom-top)*remaining/100)
	}

	for percent := 0; percent <= 100; percent += 25 {
		line := usageChartGrid
		if percent == 0 {
			line = usageChartAxis
		}
		chartLine(img, left, y(float64(percent)), right, y(float64(percent)), line, 1)
		chartText(img, 4, y(float64(percent))+4, fmt.Sprintf("%3d%%", percent))
	}
	local := now.In(time.Local)
	tick := local.Truncate(time.Hour)
	for tick.Hour()%int(usageChartTickGap/time.Hour) != 0 {
		tick = tick.Add(-time.Hour)
	}
	for ; tick.After(start); tick = tick.Add(-usageChartTickGap) {
		chartLine(img, x(tick), top, x(tick), bottom, usageChartGrid, 1)
		chartText(img, x(tick)-17, bottom+18, tick.Format("15:04"))
	}

	legend := left
	for _, key := range usageWindowKeys {
		chartLine(img, legend, 14, legend+18, 14, usageChartColors[key], 3)
		chartText(img, legend+24, 18, usageChartNames[key])
		legend += 24 + 7*len(usageChartNames[key]) + 20
	}
	chartText(img, right-7*len("remaining %"), 18, "remaining %")

	for _, key := range usageWindowKeys {
		var previous *state.UsageSample
		for i := range history {
			sample := &history[i]
			remaining, ok := sample.Remaining[key]
			if !ok || sample.At.Before(start) {
				previous = nil
				continue
			}
			if previous != nil && sample.At.Sub(previous.At) <= usageChartMaxGap {
				chartLine(img, x(previous.At), y(previous.Remaining[key]), x(sample.At), y(remaining), usageChartColors[key], 2)
			} else {
				chartLine(img, x(sample.At), y(remaining), x(sample.At), y(remaining), usageChartColors[key], 3)
			}
			previous = sample
		}
	}

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("encode usage chart: %w", err)
	}
	return out.Bytes(), nil
}

// chartLine draws a line width pixels thick from (x0, y0) to (x1, y1).
func chartLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, width int) {
	steps := max(abs(x1-x0), abs(y1-y0), 1)
	for i := 0; i <= steps; i++ {
		px := x0 + (x1-x0)*i/steps
		py := y0 + (y1-y0)*i/steps
		for dx := -(width - 1) / 2; dx <= width/2; dx++ {
			for dy := -(width - 1) / 2; dy <= width/2; dy++ {
				img.SetRGBA(px+dx, py+dy, c)
			}
		}
	}
}

// chartText writes ASCII text with its baseline at y.
func chartText(img *image.RGBA, x, y int, text string) {
	drawer := &font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{usageChartText},
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y),
	}
	drawer.DrawString(text)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
