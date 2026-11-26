package convert

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/tileforge/octoconv-worker-tectonic/internal/sandbox"
)

const (
	tectonicRenderDefaultEdge = 1280
	tectonicRenderMinEdge     = 256
	tectonicRenderMaxEdge     = 2048
	tectonicBlankBelow        = 250
	tectonicSpeckFloor        = 240
	tectonicSpeckFew          = 16
)

var tectonicRenderReadOnly = []string{
	"/lib",
	"/lib64",
	"/usr/lib",
	"/etc/ld.so.cache",
	"/usr/share/poppler",
	"/etc/fonts",
	"/usr/share/fonts",
	"/usr/share/fontconfig",
	"/dev/urandom",
	"/dev/random",
	"/dev/zero",
}

// TectonicRenderConverter draws one printed page of a tectonic PDF as a square PNG.
type TectonicRenderConverter struct {
	TectonicConverter
	run     func(ctx context.Context, name string, args ...string) error
	compile func(ctx context.Context, inPath, pdfPath string) error
}

func init() {
	Default.Register(TectonicRenderConverter{})
}

func (TectonicRenderConverter) Operation() string { return OperationRender }

func (TectonicRenderConverter) Pairs() []Pair {
	return []Pair{{From: "latex", To: "png"}}
}

func (c TectonicRenderConverter) Convert(ctx context.Context, inPath, outPath string, opts map[string]any) error {
	if err := c.render(ctx, inPath, outPath, opts); err != nil {
		_ = os.Remove(outPath)
		return err
	}
	return nil
}

func (c TectonicRenderConverter) render(ctx context.Context, inPath, outPath string, opts map[string]any) error {
	edge, err := tectonicRenderEdge(opts)
	if err != nil {
		return err
	}
	src := NormalizeFormat(filepath.Ext(inPath))
	tgt := NormalizeFormat(filepath.Ext(outPath))
	if src != "latex" || tgt != "png" {
		return fmt.Errorf("unsupported conversion %s -> %s", src, tgt)
	}

	jobDir := filepath.Dir(outPath)
	var pdfPath, prefix string
	defer func() {
		tectonicRenderCleanup(pdfPath, prefix)
	}()

	pdfPath, err = tectonicRenderTemp(jobDir, ".tectonic-render-*.pdf", inPath, outPath)
	if err != nil {
		return err
	}
	prefix, err = tectonicRenderTemp(jobDir, ".tectonic-render-*", inPath, outPath)
	if err != nil {
		return err
	}
	if err := os.Remove(prefix); err != nil {
		return err
	}

	if err := c.typeset(ctx, inPath, pdfPath); err != nil {
		return err
	}
	if err := c.raster(ctx, jobDir, pdfPath, prefix, edge); err != nil {
		return err
	}
	pages, err := tectonicRenderPages(prefix)
	if err != nil {
		return err
	}
	for _, page := range pages {
		img, err := tectonicDecodePNG(page)
		if err != nil {
			return err
		}
		if err := tectonicPageEdge(img, edge); err != nil {
			return err
		}
		if tectonicPageClean(img) {
			continue
		}
		boxed, err := tectonicLetterbox(img, edge)
		if err != nil {
			return err
		}
		return tectonicWritePNG(outPath, boxed)
	}
	return tectonicWriteWhite(outPath, edge)
}

func (c TectonicRenderConverter) typeset(ctx context.Context, inPath, pdfPath string) error {
	if c.compile != nil {
		return c.compile(ctx, inPath, pdfPath)
	}
	if c.run != nil {
		return c.run(ctx, "tectonic", tectonicArgs(inPath, filepath.Dir(pdfPath), c.BundlePath)...)
	}
	return c.TectonicConverter.Convert(ctx, inPath, pdfPath, nil)
}

func (c TectonicRenderConverter) raster(ctx context.Context, jobDir, pdfPath, prefix string, edge int) error {
	ppm := []string{"-png", "-scale-to", strconv.Itoa(edge), pdfPath, prefix}
	if c.run != nil {
		return c.run(ctx, "pdftoppm", ppm...)
	}
	name, args, err := c.rasterCommand(jobDir, ppm)
	if err != nil {
		return err
	}
	_, cpu, err := runCommandLimitedEnvCPU(ctx, tectonicLogLimit, tectonicEnv(jobDir), name, args...)
	if err != nil {
		return c.classifyRaster(ctx, err, cpu)
	}
	return nil
}

func (c TectonicRenderConverter) classifyRaster(ctx context.Context, err error, cpu engineCPU) error {
	if c.CPUSeconds > 0 && ctx.Err() == nil {
		var xerr *exec.ExitError
		if errors.As(err, &xerr) && cpu.Known {
			if st, ok := xerr.Sys().(syscall.WaitStatus); ok && st.Signaled() &&
				cpuExhausted(st.Signal(), cpu.Total(), c.CPUSeconds) {
				return fmt.Errorf("pdftoppm: %w after %.0fs of CPU", ErrTeXCPUExhausted, cpu.Total().Seconds())
			}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) && classifyDeadline(cpu) {
		return fmt.Errorf("pdftoppm: %w: the deadline expired after %.1fs of CPU in %.1fs of wall clock (%.2f of one CPU)",
			ErrTeXEngineStarved, cpu.Total().Seconds(), cpu.Elapsed.Seconds(), cpu.Share())
	}
	var setupErr *exec.ExitError
	if len(c.SandboxHelper) > 0 && errors.As(err, &setupErr) && setupErr.ExitCode() == TectonicSandboxSetupExit {
		return fmt.Errorf("pdftoppm: %w: %s", ErrTeXSandboxUnavailable, tectonicTail([]byte(err.Error())))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("pdftoppm: %w: %v", ErrTeXEngineTimeout, err)
	}
	return err
}

func tectonicRenderCleanup(pdfPath, prefix string) {
	if pdfPath != "" {
		_ = os.Remove(pdfPath)
	}
	if prefix == "" {
		return
	}
	dir, base := filepath.Dir(prefix), filepath.Base(prefix)
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, base+"-") && strings.HasSuffix(name, ".png") {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	_ = os.Remove(prefix)
}

func (c TectonicRenderConverter) rasterCommand(jobDir string, ppm []string) (string, []string, error) {
	if len(c.SandboxHelper) == 0 {
		return "pdftoppm", ppm, nil
	}
	enginePath, err := exec.LookPath("pdftoppm")
	if err != nil {
		return "", nil, fmt.Errorf("pdftoppm: %w", err)
	}
	spec := sandbox.Paths{}.SpecWith(sandbox.Limits{CPUSeconds: c.CPUSeconds})
	if c.ConfineFilesystem {
		spec = tectonicRenderSandboxSpec(jobDir, enginePath, c.CPUSeconds)
	}
	args := append(append(append(
		append([]string{}, c.SandboxHelper[1:]...),
		spec...),
		"--", enginePath), ppm...)
	return c.SandboxHelper[0], args, nil
}

func tectonicRenderSandboxSpec(jobDir, enginePath string, cpuSeconds int) []string {
	rw := dedupeExisting([]string{jobDir, "/dev/null"})
	ro := dedupeExisting(append([]string{enginePath}, tectonicRenderReadOnly...))
	ro = dropAncestorsOf(ro, rw)
	return sandbox.Paths{ReadWrite: rw, ReadOnly: ro}.SpecWith(sandbox.Limits{CPUSeconds: cpuSeconds})
}

func tectonicRenderEdge(opts map[string]any) (int, error) {
	if len(opts) == 0 {
		return tectonicRenderDefaultEdge, nil
	}
	if len(opts) != 1 {
		return 0, fmt.Errorf("size")
	}
	raw, ok := opts["size"]
	if !ok {
		return 0, fmt.Errorf("size")
	}
	n, ok := tectonicRenderSize(raw)
	if !ok || n < tectonicRenderMinEdge || n > tectonicRenderMaxEdge {
		return 0, fmt.Errorf("size")
	}
	return n, nil
}

func tectonicRenderSize(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		if v > int64(math.MaxInt) || v < int64(math.MinInt) {
			return 0, false
		}
		return int(v), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
			return 0, false
		}
		if v > float64(math.MaxInt) || v < float64(math.MinInt) {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

func tectonicRenderTemp(dir, pattern, inPath, outPath string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	closeErr := f.Close()
	if name == inPath || name == outPath {
		_ = os.Remove(name)
		return "", fmt.Errorf("temp path")
	}
	if closeErr != nil {
		_ = os.Remove(name)
		return "", closeErr
	}
	return name, nil
}

func tectonicPageNumber(mid string) (int, error) {
	if mid == "" {
		return 0, fmt.Errorf("page number")
	}
	for i := 0; i < len(mid); i++ {
		if mid[i] < '0' || mid[i] > '9' {
			return 0, fmt.Errorf("page number")
		}
	}
	n, err := strconv.Atoi(mid)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("page number")
	}
	return n, nil
}

func tectonicRenderPages(prefix string) ([]string, error) {
	dir := filepath.Dir(prefix)
	base := filepath.Base(prefix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	found := map[int]string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base+"-") || !strings.HasSuffix(name, ".png") {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(name, base+"-"), ".png")
		n, convErr := tectonicPageNumber(mid)
		if convErr != nil {
			return nil, fmt.Errorf("page name %s", name)
		}
		if _, ok := found[n]; ok {
			return nil, fmt.Errorf("duplicate page %d", n)
		}
		found[n] = filepath.Join(dir, name)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no pages")
	}
	paths := make([]string, len(found))
	for n := 1; n <= len(found); n++ {
		path, ok := found[n]
		if !ok {
			return nil, fmt.Errorf("missing page %d", n)
		}
		paths[n-1] = path
	}
	return paths, nil
}

func tectonicDecodePNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

func tectonicPageEdge(img image.Image, edge int) error {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	longer := w
	if h > longer {
		longer = h
	}
	if longer != edge || w > edge || h > edge {
		return fmt.Errorf("page bounds %d by %d", w, h)
	}
	return nil
}

func tectonicFlatten(c color.Color) (uint8, uint8, uint8) {
	r, g, b, a := c.RGBA()
	return uint8((r + (0xffff - a)) >> 8), uint8((g + (0xffff - a)) >> 8), uint8((b + (0xffff - a)) >> 8)
}

func tectonicPageClean(img image.Image) bool {
	b := img.Bounds()
	specks := map[[2]int]struct{}{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl := tectonicFlatten(img.At(x, y))
			if r < tectonicSpeckFloor || g < tectonicSpeckFloor || bl < tectonicSpeckFloor {
				return false
			}
			if r < tectonicBlankBelow || g < tectonicBlankBelow || bl < tectonicBlankBelow {
				specks[[2]int{x, y}] = struct{}{}
				if len(specks) > tectonicSpeckFew {
					return false
				}
			}
		}
	}
	for pix := range specks {
		if _, ok := specks[[2]int{pix[0] + 1, pix[1]}]; ok {
			return false
		}
		if _, ok := specks[[2]int{pix[0], pix[1] + 1}]; ok {
			return false
		}
	}
	return true
}

func tectonicLetterbox(src image.Image, edge int) (*image.NRGBA, error) {
	if err := tectonicPageEdge(src, edge); err != nil {
		return nil, err
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, edge, edge))
	for i := range dst.Pix {
		dst.Pix[i] = 255
	}
	ox := (edge - b.Dx()) / 2
	oy := (edge - b.Dy()) / 2
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(ox+x, oy+y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst, nil
}

func tectonicWriteWhite(path string, edge int) error {
	img := image.NewNRGBA(image.Rect(0, 0, edge, edge))
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	for y := 0; y < edge; y++ {
		for x := 0; x < edge; x++ {
			img.SetNRGBA(x, y, white)
		}
	}
	return tectonicWritePNG(path, img)
}

func tectonicWritePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = png.Encode(f, img)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
