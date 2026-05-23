package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/hajimehoshi/ebiten/v2"
)

const (
	screenWidth   = 720
	screenHeight  = 1280
	bytesPerPixel = 4
	targetFPS     = 60
	exportWorkers = 8
	exportFrames  = 2880
)

var exportFlag = flag.Bool("export", false, "Export perfectly looping cycles to an mp4 and exit")

var pixelPool = sync.Pool{
	New: func() any {
		return make([]byte, screenWidth*screenHeight*bytesPerPixel)
	},
}

var shaderSrc = []byte(`//kage:unit pixels
package main

var Time float
var Resolution vec2

func modFloat(x float, y float) float {
	return x - y*floor(x/y)
}

func springEaseOut(p float, intensity float) float {
	if p <= 0.0 {
		return 0.0
	}
	if p >= 1.0 {
		return 1.0
	}

	t := p * p * (3.0 - 2.0*p)
	envelope := pow(2.0, -10.0*t)
	boost := 1.0 + (intensity * t * 2.0)
	freq := 14.0 - (intensity * 2.0)

	return 1.0 - (envelope * boost * cos(t*freq))
}

func calculateDisplacement(nid vec2, planeType float, phase float, cycle float) float {
	if nid.x < 0.0 || nid.y < 0.0 {
		return 0.0
	}

	nidOffset := fract(cycle*0.33) * 16.0
	center := vec2(1.5, 1.5) + vec2(modFloat(cycle, 13.0)-6.5, modFloat(cycle, 17.0)-8.5)
	d := nid - center

	radius := length(d)
	angle := atan2(d.y, d.x)

	cubeSeed := nid + vec2(nidOffset, nidOffset*1.37)
	rndA := fract(sin(dot(cubeSeed, vec2(12.9898, 78.233))) * 43758.5453)
	rndB := fract(sin(dot(cubeSeed+vec2(42.17, 39.81), vec2(27.91, 112.91))) * 7821.57)

	checker := modFloat(floor(nid.x)+floor(nid.y), 2.0)
	spiralIdx := (radius * (0.6 + rndA*0.4)) - (angle / 6.28318) + (planeType * (0.3333 + rndB*0.1)) + (checker * 0.15)
	slot := fract(spiralIdx)

	popDuration := 0.075
	startTime := slot * 0.85

	if phase > 0.93 {
		return 1.0
	}

	localT := clamp((phase-startTime)/popDuration, 0.0, 1.0)
	seed := nid + vec2(planeType*13.7, planeType*29.3)
	intensity := fract(sin(dot(seed, vec2(12.9898, 78.233))) * 43758.5453) * 1.5

	return springEaseOut(localT, intensity)
}

func calculateBoxSDF(p vec3, b vec3) float {
	q := abs(p) - b
	return length(max(q, 0.0)) + min(max(q.x, max(q.y, q.z)), 0.0)
}

func evaluateCube(p vec3, nid vec2, planeType float, cycle float, phase float) float {
	disp := calculateDisplacement(nid, planeType, phase, cycle)
	halfDisp := disp * 0.5

	bounds := vec3(0.5)
	center := vec3(0.0)

	cx := nid.x + 0.5
	cy := nid.y + 0.5
	cz := halfDisp - 3.0
	cb := halfDisp + 3.0

	if planeType < 0.5 {
		bounds.y = cb
		center = vec3(cx, cz, cy)
	} else if planeType < 1.5 {
		bounds.x = cb
		center = vec3(cz, cx, cy)
	} else {
		bounds.z = cb
		center = vec3(cx, cy, cz)
	}

	center += vec3(cycle)
	return calculateBoxSDF(p-center, bounds)
}

func evaluatePlaneSDF(p vec3, rel2D vec2, planeType float, cycle float, phase float) float {
	idBase := floor(rel2D)
	local := fract(rel2D) - vec2(0.5)
	s := sign(local)

	d := 1000.0
	d = min(d, evaluateCube(p, idBase+vec2(0.0, 0.0), planeType, cycle, phase))
	d = min(d, evaluateCube(p, idBase+vec2(s.x, 0.0), planeType, cycle, phase))
	d = min(d, evaluateCube(p, idBase+vec2(0.0, s.y), planeType, cycle, phase))
	d = min(d, evaluateCube(p, idBase+vec2(s.x, s.y), planeType, cycle, phase))
	return d
}

func mapSceneSDF(p vec3, cycle float, phase float) vec2 {
	relP := p - vec3(cycle)
	dF := evaluatePlaneSDF(p, relP.xz, 0.0, cycle, phase)
	dL := evaluatePlaneSDF(p, relP.yz, 1.0, cycle, phase)
	dR := evaluatePlaneSDF(p, relP.xy, 2.0, cycle, phase)

	res := vec2(dF, 0.0)
	if dL < res.x {
		res = vec2(dL, 1.0)
	}
	if dR < res.x {
		res = vec2(dR, 2.0)
	}
	return res
}

func calculateNormal(p vec3, cycle float, phase float) vec3 {
	e := vec2(0.01, 0.0)
	return normalize(vec3(
		mapSceneSDF(p+e.xyy, cycle, phase).x-mapSceneSDF(p-e.xyy, cycle, phase).x,
		mapSceneSDF(p+e.yxy, cycle, phase).x-mapSceneSDF(p-e.yxy, cycle, phase).x,
		mapSceneSDF(p+e.yyx, cycle, phase).x-mapSceneSDF(p-e.yyx, cycle, phase).x,
	))
}

func hueToRGB(h float) vec3 {
	r := abs(h*6.0-3.0) - 1.0
	g := 2.0 - abs(h*6.0-2.0)
	b := 2.0 - abs(h*6.0-4.0)
	return clamp(vec3(r, g, b), 0.0, 1.0)
}

func hslToRGB(hsl vec3) vec3 {
	rgb := hueToRGB(hsl.x)
	c := (1.0 - abs(2.0*hsl.z-1.0)) * hsl.y
	return (rgb-0.5)*c + hsl.z
}

func calculateStarIntensity(p vec3, t float) float {
	totalIntensity := 0.0
	bandNoise := sin(p.x*0.15+p.y*0.2-p.z*0.1) * cos(p.x*0.1-p.z*0.15)
	band := smoothstep(0.1, 0.8, bandNoise)

	for i := 1.0; i <= 6.0; i += 1.0 {
		scale := i * 3.0
		pos := p*scale + vec3(t*i*0.005, 0.0, t*i*0.002)

		cell := floor(pos)
		local := fract(pos)

		sx := fract(sin(dot(cell, vec3(12.9898, 78.233, 45.164))) * 43758.54)
		sy := fract(sin(dot(cell, vec3(93.989, 67.345, 12.093))) * 23421.23)
		sz := fract(sin(dot(cell, vec3(34.898, 23.233, 89.164))) * 78923.45)
		starPos := vec3(sx, sy, sz)

		dist := length(local - starPos)
		isStatic := fract(sin(dot(cell, vec3(1.11, 2.22, 3.33))) * 111.11)
		twinkle := 1.0

		if isStatic > 0.4 {
			ts := 8.0 + floor(fract(sin(dot(cell, vec3(1.23, 4.56, 7.89)))*123.45)*12.0)
			twinkle = sin((t/48.0)*6.2831853*ts+cell.x*12.0)*0.5 + 0.5
		}

		baseSize := 0.02 + 0.20*fract(sin(dot(cell, vec3(9.87, 6.54, 3.21)))*987.65)
		size := baseSize * (0.8 + band*1.0)

		core := smoothstep(size*0.15, 0.0, dist) * 1.5
		halo := smoothstep(size, 0.0, dist) * 0.5
		brightness := core + halo

		totalIntensity += brightness * twinkle * (1.0 / i) * (2.5 + band*3.0)
	}

	return clamp(totalIntensity, 0.0, 1.0)
}

func calculateShootingStarIntensity(p vec3, t float) float {
	intensity := 0.0
	cycleTime := modFloat(t, 16.0)

	for i := 0.0; i < 2.0; i += 1.0 {
		streakTime := modFloat(cycleTime+i*7.77, 16.0)

		if streakTime < 1.0 {
			startPos := vec3(-15.0+i*15.0, 20.0+i*5.0, 10.0-i*5.0)
			trajectory := vec3(50.0, -30.0, -20.0)
			currentPos := startPos + trajectory*streakTime

			tailDir := normalize(-trajectory)
			proj := dot(p-currentPos, tailDir)
			distToLine := length((p-currentPos) - tailDir*max(proj, 0.0))

			headGlow := smoothstep(0.15, 0.0, length(p-currentPos))
			tailGlow := 0.0

			if proj > 0.0 && proj < 8.0 {
				tailGlow = smoothstep(0.05, 0.0, distToLine) * smoothstep(8.0, 0.0, proj) * 0.4
			}

			lifeFade := smoothstep(0.0, 0.2, streakTime) * smoothstep(1.0, 0.8, streakTime)
			intensity += (headGlow + tailGlow) * lifeFade
		}
	}

	return clamp(intensity, 0.0, 1.0)
}

func Fragment(position vec4, texCoord vec2, color vec4) vec4 {
	uv := (position.xy - Resolution.xy*0.5) / min(Resolution.x, Resolution.y)
	uv.y = -uv.y

	globalTime := Time * 0.0625
	cycle := floor(globalTime)
	phase := fract(globalTime)

	ro := vec3(15.0) + vec3(cycle)
	target := vec3(cycle)

	forward := normalize(target - ro)
	right := normalize(cross(forward, vec3(0.0, 1.0, 0.0)))
	up := normalize(cross(right, forward))

	scale := 8.5
	ro += right*uv.x*scale + up*uv.y*scale
	rd := forward

	tDist := 0.0
	maxDist := 45.0
	hit := false
	p := vec3(0.0)
	matID := 0.0

	for i := 0; i < 180; i++ {
		p = ro + rd*tDist
		res := mapSceneSDF(p, cycle, phase)
		if res.x < 0.001 {
			hit = true
			matID = res.y
			break
		}
		if tDist > maxDist {
			break
		}
		tDist += res.x * 0.45
	}

	cycleProgress := Time * 0.020833333333
	hueBase := 0.70 + 0.15*sin(cycleProgress*6.28318530718-0.26)

	darkPastelBgCol := hslToRGB(vec3(hueBase, 0.55, 0.25))
	lightPastelStarCol := hslToRGB(vec3(fract(hueBase+0.5), 0.85, 0.85))

	fogCol := mix(darkPastelBgCol, hslToRGB(vec3(hueBase+0.05, 0.50, 0.15)), 0.6)
	vignetteColor := mix(darkPastelBgCol, hslToRGB(vec3(hueBase, 0.50, 0.10)), 0.7)

	col := fogCol

	if hit {
		n := calculateNormal(p, cycle, phase)
		baseCol := darkPastelBgCol

		shade := 0.90
		if n.y > 0.5 {
			shade = 1.25
		} else if n.z > 0.5 {
			shade = 1.10
		} else if n.x > 0.5 {
			shade = 0.95
		} else if n.x < -0.5 {
			shade = 0.80
		} else if n.y < -0.5 {
			shade = 0.65
		}

		baseCol *= shade - p.y*0.015

		pointIntensity := calculateStarIntensity(p, Time)
		shootingStarIntensity := calculateShootingStarIntensity(p, Time)

		col = mix(baseCol, lightPastelStarCol, pointIntensity)
		col = mix(col, vec3(0.9, 0.95, 1.0), shootingStarIntensity*0.9)

		rel := p - vec3(cycle)
		fx := fract(rel.x)
		fy := fract(rel.y)
		fz := fract(rel.z)

		distX := abs(fx - 0.5)
		distY := abs(fy - 0.5)
		distZ := abs(fz - 0.5)

		wobble := sin(p.x*15.0+Time*5.2359877) * sin(p.y*15.0+Time*5.2359877) * sin(p.z*15.0) * 0.005
		edgeThresh := 0.495 + wobble
		edgeVal := 0.0
		cD := 0.0

		idF := vec2(floor(rel.x-n.x*0.01), floor(rel.z-n.z*0.01))
		idL := vec2(floor(rel.y-n.y*0.01), floor(rel.z-n.z*0.01))
		idR := vec2(floor(rel.x-n.x*0.01), floor(rel.y-n.y*0.01))

		if matID == 0.0 {
			cD = calculateDisplacement(idF, 0.0, phase, cycle)
			if n.y > 0.5 {
				if fx < 0.5 && abs(cD-calculateDisplacement(idF+vec2(-1.0, 0.0), 0.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distX) }
				if fx > 0.5 && abs(cD-calculateDisplacement(idF+vec2(1.0, 0.0), 0.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distX) }
				if fz < 0.5 && abs(cD-calculateDisplacement(idF+vec2(0.0, -1.0), 0.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distZ) }
				if fz > 0.5 && abs(cD-calculateDisplacement(idF+vec2(0.0, 1.0), 0.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distZ) }
			} else {
				distToYEdges := min(abs(rel.y-cD), abs(rel.y))
				if abs(n.x) > 0.5 { edgeVal = max(distZ, 0.5-distToYEdges) }
				if abs(n.z) > 0.5 { edgeVal = max(distX, 0.5-distToYEdges) }
			}
		} else if matID == 1.0 {
			cD = calculateDisplacement(idL, 1.0, phase, cycle)
			if n.x > 0.5 {
				if fy < 0.5 && abs(cD-calculateDisplacement(idL+vec2(-1.0, 0.0), 1.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distY) }
				if fy > 0.5 && abs(cD-calculateDisplacement(idL+vec2(1.0, 0.0), 1.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distY) }
				if fz < 0.5 && abs(cD-calculateDisplacement(idL+vec2(0.0, -1.0), 1.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distZ) }
				if fz > 0.5 && abs(cD-calculateDisplacement(idL+vec2(0.0, 1.0), 1.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distZ) }
			} else {
				distToXEdges := min(abs(rel.x-cD), abs(rel.x))
				if abs(n.y) > 0.5 { edgeVal = max(distZ, 0.5-distToXEdges) }
				if abs(n.z) > 0.5 { edgeVal = max(distY, 0.5-distToXEdges) }
			}
		} else if matID == 2.0 {
			cD = calculateDisplacement(idR, 2.0, phase, cycle)
			if n.z > 0.5 {
				if fx < 0.5 && abs(cD-calculateDisplacement(idR+vec2(-1.0, 0.0), 2.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distX) }
				if fx > 0.5 && abs(cD-calculateDisplacement(idR+vec2(1.0, 0.0), 2.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distX) }
				if fy < 0.5 && abs(cD-calculateDisplacement(idR+vec2(0.0, -1.0), 2.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distY) }
				if fy > 0.5 && abs(cD-calculateDisplacement(idR+vec2(0.0, 1.0), 2.0, phase, cycle)) > 0.01 { edgeVal = max(edgeVal, distY) }
			} else {
				distToZEdges := min(abs(rel.z-cD), abs(rel.z))
				if abs(n.x) > 0.5 { edgeVal = max(distY, 0.5-distToZEdges) }
				if abs(n.y) > 0.5 { edgeVal = max(distX, 0.5-distToZEdges) }
			}
		}

		glowCol := hslToRGB(vec3(fract(hueBase*13.0), 0.6, 0.85))
		extrusionFactor := smoothstep(0.0, 2.5, cD)

		seamGlow := smoothstep(0.35, 0.50, edgeVal) * (0.05 + extrusionFactor*0.2)
		col += glowCol * seamGlow

		rimLight := smoothstep(edgeThresh-0.015, edgeThresh+0.005, edgeVal) * (0.3 + extrusionFactor*0.4)
		col = mix(col, glowCol, rimLight)

		fogFactor := smoothstep(18.0, 38.0, tDist+phase*1.73205)
		col = mix(col, fogCol, fogFactor)
	}

	return vec4(mix(vignetteColor, col, 1.0-smoothstep(0.5, 1.8, length(uv))), 1.0)
}
`)

var shader *ebiten.Shader

func init() {
	var err error
	shader, err = ebiten.NewShader(shaderSrc)
	if err != nil {
		log.Fatal(err)
	}
}

type renderJob struct {
	frame int
	img   *image.RGBA
}

type App struct {
	currentTime   float32
	currentFrame  int
	totalFrames   int
	isExporting   bool
	tempDirectory string
	offscreenBuf  *ebiten.Image
	exportJobs    chan renderJob
	waitGroup     sync.WaitGroup
}

func (a *App) Update() error {
	if !a.isExporting {
		a.currentTime += 1.0 / float32(targetFPS)
		a.currentFrame++
		return nil
	}

	if a.currentFrame >= a.totalFrames {
		return ebiten.Termination
	}

	renderTime := float32(a.currentFrame) / float32(targetFPS)

	if a.offscreenBuf == nil {
		a.offscreenBuf = ebiten.NewImage(screenWidth, screenHeight)
	}

	opts := &ebiten.DrawRectShaderOptions{
		Uniforms: map[string]any{
			"Time":       renderTime,
			"Resolution": []float32{float32(screenWidth), float32(screenHeight)},
		},
	}

	a.offscreenBuf.DrawRectShader(screenWidth, screenHeight, shader, opts)

	pixelData := pixelPool.Get().([]byte)
	a.offscreenBuf.ReadPixels(pixelData)

	imgBuffer := &image.RGBA{
		Pix:    pixelData,
		Stride: screenWidth * bytesPerPixel,
		Rect:   image.Rect(0, 0, screenWidth, screenHeight),
	}

	a.exportJobs <- renderJob{
		frame: a.currentFrame,
		img:   imgBuffer,
	}

	if a.currentFrame%targetFPS == 0 {
		percentComplete := float64(a.currentFrame) / float64(a.totalFrames) * 100.0
		log.Printf("Rendered logical frame %d / %d (%.1f%%)", a.currentFrame, a.totalFrames, percentComplete)
	}

	a.currentFrame++
	return nil
}

func (a *App) Draw(screen *ebiten.Image) {
	if a.isExporting {
		if a.offscreenBuf != nil {
			screen.DrawImage(a.offscreenBuf, nil)
		}
	} else {
		opts := &ebiten.DrawRectShaderOptions{
			Uniforms: map[string]any{
				"Time":       a.currentTime,
				"Resolution": []float32{float32(screenWidth), float32(screenHeight)},
			},
		}
		screen.DrawRectShader(screenWidth, screenHeight, shader, opts)
	}
}

func (a *App) Layout(outsideWidth, outsideHeight int) (int, int) {
	return screenWidth, screenHeight
}

func main() {
	flag.Parse()

	app := &App{
		isExporting: *exportFlag,
		totalFrames: exportFrames,
	}

	if app.isExporting {
		tempDir, err := os.MkdirTemp("", "voxel_export_*")
		if err != nil {
			log.Fatal(err)
		}
		app.tempDirectory = tempDir
		defer os.RemoveAll(app.tempDirectory)

		log.Printf("Export mode active. Directory: %s", app.tempDirectory)
		ebiten.SetVsyncEnabled(false)

		app.exportJobs = make(chan renderJob, 200)

		for i := 0; i < exportWorkers; i++ {
			app.waitGroup.Add(1)
			go func() {
				defer app.waitGroup.Done()
				for job := range app.exportJobs {
					func() {
						filename := filepath.Join(app.tempDirectory, fmt.Sprintf("frame_%04d.png", job.frame))
						file, err := os.Create(filename)
						if err != nil {
							return
						}
						defer file.Close()
						png.Encode(file, job.img)
					}()
					pixelPool.Put(job.img.Pix)
				}
			}()
		}
	}

	ebiten.SetWindowSize(screenWidth, screenHeight)
	ebiten.SetWindowTitle("Concave Corner")

	if err := ebiten.RunGame(app); err != nil && err != ebiten.Termination {
		log.Fatal(err)
	}

	if app.isExporting {
		log.Println("Renderer complete. Awaiting PNG IO...")
		close(app.exportJobs)
		app.waitGroup.Wait()

		log.Println("Assembling mp4 via FFmpeg...")

		cmd := exec.Command("ffmpeg",
			"-y",
			"-framerate", fmt.Sprintf("%d", targetFPS),
			"-i", filepath.Join(app.tempDirectory, "frame_%04d.png"),
			"-c:v", "libx264",
			"-preset", "slower",
			"-crf", "12",
			"-pix_fmt", "yuv420p",
			"output.mp4",
		)

		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			log.Fatalf("FFmpeg assembly failed: %v", err)
		}

		log.Println("Export complete: output.mp4")
	}
}
