// pkg/panes/radartools.go
// Copyright(c) 2022-2024 vice contributors, licensed under the GNU Public License, Version 3.
// SPDX: GPL-3.0-only

package panes

import (
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/mmp/vice/pkg/log"
	"github.com/mmp/vice/pkg/math"
	"github.com/mmp/vice/pkg/renderer"
	"github.com/mmp/vice/pkg/util"
)

///////////////////////////////////////////////////////////////////////////
// WeatherRadar

// WeatherRadar provides functionality for fetching radar images to display
// in radar scopes. Only locations in the USA are currently supported, as
// the only current data source is the US NOAA...
type WeatherRadar struct {
	active bool

	// Radar images are fetched and processed in a separate goroutine;
	// updated radar center locations are sent from the main thread via
	// reqChan and command buffers to draw each of the 6 weather levels are
	// returned by cbChan.
	reqChan chan math.Point2LL
	cbChan  chan [NumWxLevels]renderer.CommandBuffer

	// Texture id for each wx level's image.
	texId [NumWxLevels]uint32
	wxCb  [NumWxLevels]renderer.CommandBuffer
}

const NumWxLevels = 6

// Block size in pixels of the quads in the converted radar image used for
// display.
const WxBlockRes = 4

// Latitude-longitude extent of the fetched image; the requests are +/-
// this much from the current center.
const WxLatLongExtent = 2.5

// Activate must be called for the WeatherRadar to start fetching weather
// radar images; it is called with an initial center position in
// latitude-longitude coordinates.
func (w *WeatherRadar) Activate(center math.Point2LL, r renderer.Renderer, lg *log.Logger) {
	if w.active {
		w.reqChan <- center
		return
	}
	w.active = true

	w.reqChan = make(chan math.Point2LL, 1000) // lots of buffering
	w.reqChan <- center
	w.cbChan = make(chan [NumWxLevels]renderer.CommandBuffer, 8)

	if w.texId[0] == 0 {
		// Create a small texture for each weather level
		img := image.NewRGBA(image.Rectangle{Max: image.Point{X: WxBlockRes, Y: WxBlockRes}})

		for i := 0; i < NumWxLevels; i++ {
			// RGBs from STARS Manual, B-5
			baseColor := util.Select(i < 3, color.RGBA{R: 37, G: 77, B: 77, A: 255},
				color.RGBA{R: 100, G: 100, B: 51, A: 255})

			stipple := i % 3

			for y := 0; y < WxBlockRes; y++ {
				for x := 0; x < WxBlockRes; x++ {
					c := baseColor
					switch stipple {
					case 1: // light stipple: every other line, every 4th pixel
						if y&1 == 1 {
							offset := y & 2 // alternating 0 and 2
							if x%4 == offset {
								c = color.RGBA{R: 250, G: 250, B: 250, A: 255}
							}
						}

					case 2: // dense stipple: every other line, every other pixel
						if x&1 == 1 && y&1 == 1 {
							c = color.RGBA{R: 250, G: 250, B: 250, A: 255}
						}
					}
					img.Set(x, y, c)
				}
			}

			// Nearest filter for magnification
			w.texId[i] = r.CreateTextureFromImage(img, true)
		}
	}

	go fetchWeather(w.reqChan, w.cbChan, lg)
}

// Deactivate causes the WeatherRadar to stop fetching weather updates.
func (w *WeatherRadar) Deactivate() {
	if w.active {
		close(w.reqChan)
		w.active = false
	}
}

// UpdateCenter provides a new center point for the radar image, causing a
// new image to be fetched.
func (w *WeatherRadar) UpdateCenter(center math.Point2LL) {
	select {
	case w.reqChan <- center:
		// success
	default:
		// The channel is full; this may happen if the user is continuously
		// dragging the radar scope around. Worst case, we drop some
		// position update requests, which is generally no big deal.
	}
}

// A single scanline of this color map, converted to RGB bytes:
// https://opengeo.ncep.noaa.gov/geoserver/styles/reflectivity.png
//
//go:embed radar_reflectivity.rgb
var radarReflectivity []byte

type kdNode struct {
	rgb  [3]byte
	refl float32
	c    [2]*kdNode
}

var radarReflectivityKdTree *kdNode

func init() {
	type rgbRefl struct {
		rgb  [3]byte
		refl float32
	}

	var r []rgbRefl

	for i := 0; i < len(radarReflectivity); i += 3 {
		r = append(r, rgbRefl{
			rgb:  [3]byte{radarReflectivity[i], radarReflectivity[i+1], radarReflectivity[i+2]},
			refl: float32(i) / float32(len(radarReflectivity)),
		})
	}

	// Build a kd-tree over the RGB points in the color map.
	var buildTree func(r []rgbRefl, depth int) *kdNode
	buildTree = func(r []rgbRefl, depth int) *kdNode {
		if len(r) == 0 {
			return nil
		}
		if len(r) == 1 {
			return &kdNode{rgb: r[0].rgb, refl: r[0].refl}
		}

		// The split dimension cycles through RGB with tree depth.
		dim := depth % 3

		// Sort the points in the current dimension
		sort.Slice(r, func(i, j int) bool {
			return r[i].rgb[dim] < r[j].rgb[dim]
		})

		// Split in the middle and recurse
		mid := len(r) / 2
		return &kdNode{
			rgb:  r[mid].rgb,
			refl: r[mid].refl,
			c:    [2]*kdNode{buildTree(r[:mid], depth+1), buildTree(r[mid+1:], depth+1)},
		}
	}

	radarReflectivityKdTree = buildTree(r, 0)
}

func invertRadarReflectivity(rgb [3]byte) float32 {
	// All white -> 0
	if rgb[0] == 255 && rgb[1] == 255 && rgb[2] == 255 {
		return 0
	}

	// Returns the distnace between the specified RGB and the RGB passed to
	// invertRadarReflectivity.
	dist := func(o []byte) float32 {
		d2 := math.Sqr(int(o[0])-int(rgb[0])) + math.Sqr(int(o[1])-int(rgb[1])) + math.Sqr(int(o[2])-int(rgb[2]))
		return math.Sqrt(float32(d2))
	}

	var searchTree func(n *kdNode, closestNode *kdNode, closestDist float32, depth int) (*kdNode, float32)
	searchTree = func(n *kdNode, closestNode *kdNode, closestDist float32, depth int) (*kdNode, float32) {
		if n == nil {
			return closestNode, closestDist
		}

		// Check the current node
		d := dist(n.rgb[:])
		if d < closestDist {
			closestDist = d
			closestNode = n
		}

		// Split dimension as in buildTree above
		dim := depth % 3

		// Initially traverse the tree based on which side of the split
		// plane the lookup point is on.
		var first, second *kdNode
		if rgb[dim] < n.rgb[dim] {
			first, second = n.c[0], n.c[1]
		} else {
			first, second = n.c[1], n.c[0]
		}

		closestNode, closestDist = searchTree(first, closestNode, closestDist, depth+1)

		// If the distance to the split plane is less than the distance to
		// the closest point found so far, we need to check the other side
		// of the split.
		if float32(math.Abs(int(rgb[dim])-int(n.rgb[dim]))) < closestDist {
			closestNode, closestDist = searchTree(second, closestNode, closestDist, depth+1)
		}

		return closestNode, closestDist
	}

	if true {
		n, _ := searchTree(radarReflectivityKdTree, nil, 100000, 0)
		return n.refl
	} else {
		// Debugging: verify the point found is indeed the closest by
		// exhaustively checking the distance to all of points in the color
		// map.
		n, nd := searchTree(radarReflectivityKdTree, nil, 100000, 0)

		closest, closestDist := -1, float32(100000)
		for i := 0; i < len(radarReflectivity); i += 3 {
			d := dist(radarReflectivity[i : i+3])
			if d < closestDist {
				closestDist = d
				closest = i
			}
		}

		// Note that multiple points in the color map may have the same
		// distance to the lookup point; thus we only check the distance
		// here and not the reflectivity (which should be very close but is
		// not necessarily the same.)
		if nd != closestDist {
			fmt.Printf("WAH %d,%d,%d -> %d,%d,%d: dist %f vs %d,%d,%d: dist %f\n",
				int(rgb[0]), int(rgb[1]), int(rgb[2]),
				int(n.rgb[0]), int(n.rgb[1]), int(n.rgb[2]), nd,
				int(radarReflectivity[closest]), int(radarReflectivity[closest+1]), int(radarReflectivity[closest+2]),
				closestDist)
		}

		return n.refl
	}
}

// fetchWeather runs asynchronously in a goroutine, receiving requests from
// reqChan, fetching corresponding radar images from the NOAA, and sending
// the results back on cbChan.  New images are also automatically
// fetched periodically, with a wait time specified by the delay parameter.
func fetchWeather(reqChan chan math.Point2LL, cbChan chan [NumWxLevels]renderer.CommandBuffer,
	lg *log.Logger) {
	// NOAA posts new maps every 2 minutes, so fetch a new map at minimum
	// every 100s to stay current.
	fetchRate := 100 * time.Second

	// center stores the current center position of the radar image
	var center math.Point2LL
	var lastFetch time.Time
	for {
		var ok, timedOut bool
		select {
		case center, ok = <-reqChan:
			if ok {
				// Drain any additional requests so that we get the most
				// recent one.
				for len(reqChan) > 0 {
					center = <-reqChan
				}
			} else {
				// The channel is closed; wrap up.
				close(cbChan)
				return
			}
		case <-time.After(fetchRate):
			// Periodically make a new request even if the center hasn't
			// changed.
			timedOut = true
		}

		// Even if the center has moved, don't fetch more than every 15
		// seconds.
		if !timedOut && !lastFetch.IsZero() && time.Since(lastFetch) < 15*time.Second {
			continue
		}
		lastFetch = time.Now()

		// Lat-long bounds of the region we're going to request weather for.
		rb := math.Extent2D{P0: math.Sub2LL(center, math.Point2LL{WxLatLongExtent, WxLatLongExtent}),
			P1: math.Add2LL(center, math.Point2LL{WxLatLongExtent, WxLatLongExtent})}

		// The weather radar image comes via a WMS GetMap request from the NOAA.
		//
		// Relevant background:
		// https://enterprise.arcgis.com/en/server/10.3/publish-services/windows/communicating-with-a-wms-service-in-a-web-browser.htm
		// http://schemas.opengis.net/wms/1.3.0/capabilities_1_3_0.xsd
		// NOAA weather: https://opengeo.ncep.noaa.gov/geoserver/www/index.html
		// https://opengeo.ncep.noaa.gov/geoserver/conus/conus_bref_qcd/ows?service=wms&version=1.3.0&request=GetCapabilities
		params := url.Values{}
		params.Add("SERVICE", "WMS")
		params.Add("REQUEST", "GetMap")
		params.Add("FORMAT", "image/png")
		params.Add("WIDTH", "2048")
		params.Add("HEIGHT", "2048")
		params.Add("LAYERS", "conus_bref_qcd")
		params.Add("BBOX", fmt.Sprintf("%f,%f,%f,%f", rb.P0[0], rb.P0[1], rb.P1[0], rb.P1[1]))

		url := "https://opengeo.ncep.noaa.gov/geoserver/conus/conus_bref_qcd/ows?" + params.Encode()

		// Request the image
		lg.Info("Fetching weather", slog.String("url", url))
		resp, err := http.Get(url)
		if err != nil {
			lg.Infof("Weather error: %s", err)
			continue
		}
		defer resp.Body.Close()

		img, err := png.Decode(resp.Body)
		if err != nil {
			lg.Infof("Weather error: %s", err)
			continue
		}

		// Send the command buffers back to the main thread.
		cbChan <- makeWeatherCommandBuffers(img, rb, lg)

		lg.Info("finish weather fetch")
	}
}

func makeWeatherCommandBuffers(img image.Image, rb math.Extent2D, lg *log.Logger) [NumWxLevels]renderer.CommandBuffer {
	// Convert the Image returned by png.Decode to a simple 8-bit RGBA image.
	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, img.Bounds(), img, image.Point{}, draw.Over)

	ny, nx := img.Bounds().Dy(), img.Bounds().Dx()
	if ny%WxBlockRes != 0 || nx%WxBlockRes != 0 {
		lg.Errorf("invalid weather image resolution; must be multiple of WxBlockRes")
		return [NumWxLevels]renderer.CommandBuffer{}
	}
	nby, nbx := ny/WxBlockRes, nx/WxBlockRes

	// First determine the weather level for each WxBlockRes*WxBlockRes
	// block of the image.
	levels := make([]int, nbx*nby)
	for y := 0; y < nby; y++ {
		for x := 0; x < nbx; x++ {
			avg := float32(0)
			for dy := 0; dy < WxBlockRes; dy++ {
				for dx := 0; dx < WxBlockRes; dx++ {
					px := rgba.RGBAAt(x*WxBlockRes+dx, y*WxBlockRes+dy)
					avg += invertRadarReflectivity([3]byte{px.R, px.G, px.B})
				}
			}

			// levels from [0,6].
			level := int(math.Min(avg*7/(WxBlockRes*WxBlockRes), 6))
			levels[x+y*nbx] = level
		}
	}

	// Now generate the command buffer for each weather level.  We don't
	// draw anything for level==0, so the indexing into cb is off by 1
	// below.
	var cb [NumWxLevels]renderer.CommandBuffer
	tb := renderer.GetTexturedTrianglesDrawBuilder()
	defer renderer.ReturnTexturedTrianglesDrawBuilder(tb)

	for level := 1; level <= NumWxLevels; level++ {
		tb.Reset()

		// We'd like to be somewhat efficient and not necessarily draw an
		// individual quad for each block, but on the other hand don't want
		// to make this too complicated... So we'll consider block
		// scanlines and quads across neighbors that are the same level
		// when we find them.
		for y := 0; y < nby; y++ {
			for x := 0; x < nbx; x++ {
				// Skip ahead until we reach a block at the level we currently care about.
				if levels[x+y*nbx] != level {
					continue
				}

				// Now see how long a span of repeats we have.
				// Each quad spans [0,0]->[1,1] in texture coordinates; the
				// texture is created with repeat wrap mode, so we just pad
				// out the u coordinate into u1 accordingly.
				x0 := x
				u1 := float32(0)
				for x < nbx && levels[x+y*nbx] == level {
					x++
					u1++
				}

				// Corner points
				p0 := rb.Lerp([2]float32{float32(x0) / float32(nbx), float32(y) / float32(nby)})
				p1 := rb.Lerp([2]float32{float32(x) / float32(nbx), float32(y+1) / float32(nby)})

				// Draw a single quad
				tb.AddQuad([2]float32{p0[0], p0[1]}, [2]float32{p1[0], p0[1]},
					[2]float32{p1[0], p1[1]}, [2]float32{p0[0], p1[1]},
					[2]float32{0, 1}, [2]float32{u1, 1}, [2]float32{u1, 0}, [2]float32{0, 0})
			}
		}

		// Subtract one so that level==1 is drawn by cb[0], etc, since we
		// don't draw anything for level==0.
		tb.GenerateCommands(&cb[level-1])
	}

	return cb
}

// Draw draws the current weather radar image, if available. (If none is yet
// available, it returns rather than stalling waiting for it).
func (w *WeatherRadar) Draw(ctx *Context, intensity float32, contrast float32,
	active [NumWxLevels]bool, transforms ScopeTransformations, cb *renderer.CommandBuffer) {
	select {
	case w.wxCb = <-w.cbChan:
		// got updated command buffers, yaay.  Note that we always go ahead
		// and drain the cbChan, even if if the WeatherRadar is inactive.

	default:
		// no message
	}

	if w.active {
		transforms.LoadLatLongViewingMatrices(cb)
		cb.SetRGBA(renderer.RGBA{1, 1, 1, intensity})
		cb.Blend()
		for i, wcb := range w.wxCb {
			if active[i] {
				cb.EnableTexture(w.texId[i])
				cb.Call(wcb)
				cb.DisableTexture()
			}
		}
		cb.DisableBlend()
	}
}

///////////////////////////////////////////////////////////////////////////
// Additional useful things we may draw on radar scopes...

// DrawCompass emits drawing commands to draw compass heading directions at
// the edges of the current window. It takes a center point p in lat-long
// coordinates, transformation functions and the radar scope's current
// rotation angle, if any.  Drawing commands are added to the provided
// command buffer, which is assumed to have projection matrices set up for
// drawing using window coordinates.
func DrawCompass(p math.Point2LL, ctx *Context, rotationAngle float32, font *renderer.Font, color renderer.RGB,
	paneBounds math.Extent2D, transforms ScopeTransformations, cb *renderer.CommandBuffer) {
	// Window coordinates of the center point.
	// TODO: should we explicitly handle the case of this being outside the window?
	pw := transforms.WindowFromLatLongP(p)
	bounds := math.Extent2D{P1: [2]float32{paneBounds.Width(), paneBounds.Height()}}

	td := renderer.GetTextDrawBuilder()
	defer renderer.ReturnTextDrawBuilder(td)
	ld := renderer.GetColoredLinesDrawBuilder()
	defer renderer.ReturnColoredLinesDrawBuilder(ld)

	// Draw lines at a 5 degree spacing.
	for h := float32(5); h <= 360; h += 5 {
		hr := h + rotationAngle
		dir := [2]float32{math.Sin(math.Radians(hr)), math.Cos(math.Radians(hr))}
		// Find the intersection of the line from the center point to the edge of the window.
		isect, _, t := bounds.IntersectRay(pw, dir)
		if !isect {
			// Happens on initial launch w/o a sector file...
			//lg.Infof("no isect?! p %+v dir %+v bounds %+v", pw, dir, ctx.paneExtent)
			continue
		}

		// Draw a short line from the intersection point at the edge to the
		// point ten pixels back inside the window toward the center.
		pEdge := math.Add2f(pw, math.Scale2f(dir, t))
		pInset := math.Add2f(pw, math.Scale2f(dir, t-10))
		ld.AddLine(pEdge, pInset, color)

		// Every 10 degrees draw a heading label.
		if int(h)%10 == 0 {
			// Generate the label ourselves rather than via fmt.Sprintf,
			// out of some probably irrelevant attempt at efficiency.
			label := []byte{'0', '0', '0'}
			hi := int(h)
			for i := 2; i >= 0 && hi != 0; i-- {
				label[i] = byte('0' + hi%10)
				hi /= 10
			}

			bx, by := font.BoundText(string(label), 0)

			// Initial inset to place the text--a little past the end of
			// the line.
			pText := math.Add2f(pw, math.Scale2f(dir, t-14))

			// Finer text positioning depends on which edge of the window
			// pane we're on; this is made more grungy because text drawing
			// is specified w.r.t. the position of the upper-left corner...
			if math.Abs(pEdge[0]) < .125 {
				// left edge
				pText[1] += float32(by) / 2
			} else if math.Abs(pEdge[0]-bounds.P1[0]) < .125 {
				// right edge
				pText[0] -= float32(bx)
				pText[1] += float32(by) / 2
			} else if math.Abs(pEdge[1]) < .125 {
				// bottom edge
				pText[0] -= float32(bx) / 2
				pText[1] += float32(by)
			} else if math.Abs(pEdge[1]-bounds.P1[1]) < .125 {
				// top edge
				pText[0] -= float32(bx) / 2
			} else {
				ctx.Lg.Infof("Edge borkage! pEdge %+v, bounds %+v", pEdge, bounds)
			}

			td.AddText(string(label), pText, renderer.TextStyle{Font: font, Color: color})
		}
	}

	transforms.LoadWindowViewingMatrices(cb)
	ld.GenerateCommands(cb)
	td.GenerateCommands(cb)
}

// DrawRangeRings draws ten circles around the specified lat-long point in
// steps of the specified radius (in nm).
func DrawRangeRings(ctx *Context, center math.Point2LL, radius float32, color renderer.RGB, transforms ScopeTransformations,
	cb *renderer.CommandBuffer) {
	pixelDistanceNm := transforms.PixelDistanceNM(ctx.ControlClient.NmPerLongitude)
	centerWindow := transforms.WindowFromLatLongP(center)

	ld := renderer.GetColoredLinesDrawBuilder()
	defer renderer.ReturnColoredLinesDrawBuilder(ld)

	for i := 1; i < 40; i++ {
		// Radius of this ring in pixels
		r := float32(i) * radius / pixelDistanceNm
		ld.AddCircle(centerWindow, r, 360, color)
	}

	transforms.LoadWindowViewingMatrices(cb)
	ld.GenerateCommands(cb)
}

///////////////////////////////////////////////////////////////////////////
// ScopeTransformations

// ScopeTransformations manages various transformation matrices that are
// useful when drawing radar scopes and provides a number of useful methods
// to transform among related coordinate spaces.
type ScopeTransformations struct {
	ndcFromLatLong                       math.Matrix3
	ndcFromWindow                        math.Matrix3
	latLongFromWindow, windowFromLatLong math.Matrix3
}

// GetScopeTransformations returns a ScopeTransformations object
// corresponding to the specified radar scope center, range, and rotation
// angle.
func GetScopeTransformations(paneExtent math.Extent2D, magneticVariation float32, nmPerLongitude float32,
	center math.Point2LL, rangenm float32, rotationAngle float32) ScopeTransformations {
	width, height := paneExtent.Width(), paneExtent.Height()
	aspect := width / height
	ndcFromLatLong := math.Identity3x3().
		// Final orthographic projection including the effect of the
		// window's aspect ratio.
		Ortho(-aspect, aspect, -1, 1).
		// Account for magnetic variation and any user-specified rotation
		Rotate(-math.Radians(rotationAngle+magneticVariation)).
		// Scale based on range and nm per latitude / longitude
		Scale(nmPerLongitude/rangenm, math.NMPerLatitude/rangenm).
		// Translate to center point
		Translate(-center[0], -center[1])

	ndcFromWindow := math.Identity3x3().
		Translate(-1, -1).
		Scale(2/width, 2/height)

	latLongFromNDC := ndcFromLatLong.Inverse()
	latLongFromWindow := latLongFromNDC.PostMultiply(ndcFromWindow)
	windowFromLatLong := latLongFromWindow.Inverse()

	return ScopeTransformations{
		ndcFromLatLong:    ndcFromLatLong,
		ndcFromWindow:     ndcFromWindow,
		latLongFromWindow: latLongFromWindow,
		windowFromLatLong: windowFromLatLong,
	}
}

// LoadLatLongViewingMatrices adds commands to the provided command buffer
// to load viewing matrices so that latitude-longiture positions can be
// provided for subsequent vertices.
func (st *ScopeTransformations) LoadLatLongViewingMatrices(cb *renderer.CommandBuffer) {
	cb.LoadProjectionMatrix(st.ndcFromLatLong)
	cb.LoadModelViewMatrix(math.Identity3x3())
}

// LoadWindowViewingMatrices adds commands to the provided command buffer
// to load viewing matrices so that window-coordinate positions can be
// provided for subsequent vertices.
func (st *ScopeTransformations) LoadWindowViewingMatrices(cb *renderer.CommandBuffer) {
	cb.LoadProjectionMatrix(st.ndcFromWindow)
	cb.LoadModelViewMatrix(math.Identity3x3())
}

// WindowFromLatLongP transforms a point given in latitude-longitude
// coordinates to window coordinates.
func (st *ScopeTransformations) WindowFromLatLongP(p math.Point2LL) [2]float32 {
	return st.windowFromLatLong.TransformPoint(p)
}

// LatLongFromWindowP transforms a point p in window coordinates to
// latitude-longitude.
func (st *ScopeTransformations) LatLongFromWindowP(p [2]float32) math.Point2LL {
	return st.latLongFromWindow.TransformPoint(p)
}

// NormalizedFromWindowP transforms a point p in window coordinates to
// normalized [0,1]^2 coordinates.
func (st *ScopeTransformations) NormalizedFromWindowP(p [2]float32) [2]float32 {
	pn := st.ndcFromWindow.TransformPoint(p) // [-1,1]
	return [2]float32{(pn[0] + 1) / 2, (pn[1] + 1) / 2}
}

// LatLongFromWindowV transforms a vector in window coordinates to a vector
// in latitude-longitude coordinates.
func (st *ScopeTransformations) LatLongFromWindowV(v [2]float32) math.Point2LL {
	return st.latLongFromWindow.TransformVector(v)
}

// PixelDistanceNM returns the space between adjacent pixels expressed in
// nautical miles.
func (st *ScopeTransformations) PixelDistanceNM(nmPerLongitude float32) float32 {
	ll := st.LatLongFromWindowV([2]float32{1, 0})
	return math.NMLength2LL(ll, nmPerLongitude)
}

///////////////////////////////////////////////////////////////////////////
// Other utilities

var (
	highlightedLocation        math.Point2LL
	highlightedLocationEndTime time.Time
)

// If the user has run the "find" command to highlight a point in the
// world, draw a red circle around that point for a few seconds.
func DrawHighlighted(ctx *Context, transforms ScopeTransformations, cb *renderer.CommandBuffer) {
	remaining := time.Until(highlightedLocationEndTime)
	if remaining < 0 {
		return
	}

	color := UIErrorColor
	fade := 1.5
	if sec := remaining.Seconds(); sec < fade {
		x := float32(sec / fade)
		color = renderer.LerpRGB(x, renderer.RGB{}, color)
	}

	p := transforms.WindowFromLatLongP(highlightedLocation)
	radius := float32(10) // 10 pixel radius
	ld := renderer.GetColoredLinesDrawBuilder()
	defer renderer.ReturnColoredLinesDrawBuilder(ld)
	ld.AddCircle(p, radius, 360, color)

	transforms.LoadWindowViewingMatrices(cb)
	cb.LineWidth(3, ctx.Platform.DPIScale())
	ld.GenerateCommands(cb)
}
