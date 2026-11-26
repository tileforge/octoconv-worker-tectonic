package convert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTectonicRenderManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-tectonic-render.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Capabilities []struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
			Legacy  struct {
				Operation string `json:"operation"`
				Engine    string `json:"engine"`
			} `json:"legacy"`
			ClientOperation string `json:"client_operation"`
			Inputs          []struct {
				Role string `json:"role"`
				Min  int    `json:"min"`
				Max  int    `json:"max"`
			} `json:"inputs"`
			Outputs []struct {
				Role string `json:"role"`
				Min  int    `json:"min"`
				Max  int    `json:"max"`
			} `json:"outputs"`
			ParameterSchema json.RawMessage `json:"parameter_schema"`
			Formats         []Pair          `json:"formats"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Capabilities) != 1 {
		t.Fatalf("capabilities %d", len(doc.Capabilities))
	}
	cap := doc.Capabilities[0]
	if cap.ID != "octoconv.render.tectonic" || cap.Version != 1 {
		t.Fatalf("id %s version %d", cap.ID, cap.Version)
	}
	if cap.Legacy.Operation != OperationRender || cap.Legacy.Engine != EngineTectonic || cap.ClientOperation != OperationRender {
		t.Fatalf("legacy %+v client %s", cap.Legacy, cap.ClientOperation)
	}
	if len(cap.Inputs) != 1 || cap.Inputs[0].Role != "source" || cap.Inputs[0].Min != 1 || cap.Inputs[0].Max != 32 {
		t.Fatalf("inputs %+v", cap.Inputs)
	}
	if len(cap.Outputs) != 1 || cap.Outputs[0].Role != "primary" || cap.Outputs[0].Min != 1 || cap.Outputs[0].Max != 1 {
		t.Fatalf("outputs %+v", cap.Outputs)
	}
	var schema map[string]any
	if err := json.Unmarshal(cap.ParameterSchema, &schema); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(schema)
	if err != nil || string(encoded) != "{}" || len(schema) != 0 {
		t.Fatalf("parameter_schema %s", cap.ParameterSchema)
	}
	if len(cap.Formats) != 1 || cap.Formats[0] != (Pair{From: "latex", To: "png"}) {
		t.Fatalf("formats %+v", cap.Formats)
	}
	pairs := (TectonicRenderConverter{}).Pairs()
	if len(pairs) != 1 || pairs[0] != cap.Formats[0] {
		t.Fatalf("pairs %+v", pairs)
	}
	got, ok := Default.LookupOp(OperationRender, "latex", "png")
	if !ok {
		t.Fatal("missing render pair")
	}
	if _, is := got.(TectonicRenderConverter); !is {
		t.Fatalf("type %T", got)
	}
	if _, ok := Default.LookupOp(OperationRender, "pdf", "png"); ok {
		t.Fatal("pdf render pair")
	}
	if MIMEType("png") != "image/png" {
		t.Fatal(MIMEType("png"))
	}
	convertRaw, err := os.ReadFile(filepath.Join("..", "..", "schema", "manifest-tectonic-convert.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(convertRaw), "octoconv.render.tectonic") || strings.Contains(string(convertRaw), `"to": "png"`) {
		t.Fatalf("convert manifest changed: %s", convertRaw)
	}
	docker, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(docker)
	for _, name := range []string{"ca-certificates", "poppler-utils", "--no-install-recommends", "command -v biber"} {
		if !strings.Contains(text, name) {
			t.Fatalf("dockerfile missing %s", name)
		}
	}
	if strings.Contains(text, "weasyprint") {
		t.Fatal("dockerfile names weasyprint")
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(readme)
	for _, name := range []string{
		"schema/manifest-tectonic-render.json",
		"octoconv.render.tectonic",
		"latex",
		"png",
		"parameter_schema",
		"not approved",
		"1280",
		"256",
		"2048",
		"pdftoppm",
		"tectonic",
	} {
		if !strings.Contains(page, name) {
			t.Fatalf("readme missing %s", name)
		}
	}
}

type renderCall struct {
	name string
	args []string
}

func scriptedRender(bundle string, fn func(context.Context, string, []string) error) (TectonicRenderConverter, *[]renderCall) {
	calls := []renderCall{}
	c := TectonicRenderConverter{TectonicConverter: TectonicConverter{BundlePath: bundle}}
	c.run = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, renderCall{name: name, args: append([]string{}, args...)})
		if fn == nil {
			return nil
		}
		return fn(ctx, name, args)
	}
	return c, &calls
}

func writeSolid(path string, w, h int, fill color.NRGBA, paint func(*image.NRGBA)) error {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, fill)
		}
	}
	if paint != nil {
		paint(img)
	}
	return tectonicWritePNG(path, img)
}

func decodePNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func isWhite(c color.Color) bool {
	r, g, b, a := c.RGBA()
	return uint8(r>>8) == 255 && uint8(g>>8) == 255 && uint8(b>>8) == 255 && uint8(a>>8) == 255
}

func assertWhiteSquare(t *testing.T, path string, edge int) {
	t.Helper()
	img := decodePNG(t, path)
	if img.Bounds().Dx() != edge || img.Bounds().Dy() != edge {
		t.Fatalf("bounds %v", img.Bounds())
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if !isWhite(img.At(x, y)) {
				t.Fatalf("pixel %d,%d", x, y)
			}
		}
	}
}

func absentOutput(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("output stat %v", err)
	}
}

func precreate(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("precreated"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func paintPages(edge int, pages ...func(prefix string) error) func(context.Context, string, []string) error {
	return func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		prefix := args[len(args)-1]
		for _, page := range pages {
			if err := page(prefix); err != nil {
				return err
			}
		}
		return nil
	}
}

func whitePage(n, w, h int) func(string) error {
	return func(prefix string) error {
		return writeSolid(prefix+"-"+itoa(n)+".png", w, h, color.NRGBA{255, 255, 255, 255}, nil)
	}
}

func inkPage(n, w, h, x, y int) func(string) error {
	return func(prefix string) error {
		return writeSolid(prefix+"-"+itoa(n)+".png", w, h, color.NRGBA{255, 255, 255, 255}, func(img *image.NRGBA) {
			img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 255})
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestTectonicRenderFirstContent(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("\\documentclass{article}\\begin{document}x\\end{document}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := "/opt/tectonic/octoconv-bundle.ttb"
	c, calls := scriptedRender(bundle, paintPages(256,
		whitePage(1, 256, 256),
		inkPage(2, 256, 128, 128, 64),
	))
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[0].name != "tectonic" || (*calls)[1].name != "pdftoppm" {
		t.Fatalf("calls %+v", *calls)
	}
	if strings.Join((*calls)[0].args, "\x00") != strings.Join(tectonicArgs(in, dir, bundle), "\x00") {
		t.Fatalf("tectonic %v", (*calls)[0].args)
	}
	for _, bad := range []string{"-Z", "--keep-intermediates", "-k", "--synctex"} {
		for _, arg := range (*calls)[0].args {
			if arg == bad {
				t.Fatalf("tectonic has %s", bad)
			}
		}
	}
	ppm := (*calls)[1].args
	if len(ppm) != 5 || ppm[0] != "-png" || ppm[1] != "-scale-to" || ppm[2] != "256" {
		t.Fatalf("pdftoppm %v", ppm)
	}
	for _, bad := range []string{"-f", "-l", "-singlefile", "-r", "-cropbox"} {
		for _, arg := range ppm {
			if arg == bad {
				t.Fatalf("pdftoppm has %s", bad)
			}
		}
	}
	img := decodePNG(t, out)
	if img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("bounds %v", img.Bounds())
	}
	if !isWhite(img.At(0, 0)) {
		t.Fatal("corner is not white")
	}
	if isWhite(img.At(128, 128)) {
		t.Fatal("content pixel is white")
	}
}

func TestTectonicRenderAllCleanIsWhiteSquare(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := scriptedRender("/bundle", func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		prefix := args[len(args)-1]
		return writeSolid(prefix+"-1.png", 256, 128, color.NRGBA{255, 255, 255, 255}, func(img *image.NRGBA) {
			img.SetNRGBA(3, 3, color.NRGBA{245, 245, 245, 255})
		})
	})
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err != nil {
		t.Fatal(err)
	}
	assertWhiteSquare(t, out, 256)
}

func TestTectonicRenderConnectedSpeckIsContent(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := scriptedRender("/bundle", func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		prefix := args[len(args)-1]
		return writeSolid(prefix+"-1.png", 256, 128, color.NRGBA{255, 255, 255, 255}, func(img *image.NRGBA) {
			img.SetNRGBA(4, 5, color.NRGBA{245, 245, 245, 255})
			img.SetNRGBA(5, 5, color.NRGBA{245, 245, 245, 255})
		})
	})
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, out)
	if isWhite(img.At(4, 69)) || isWhite(img.At(5, 69)) {
		t.Fatal("connected speck was dropped")
	}
}

func TestTectonicRenderUndrawableStops(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	precreate(t, out)
	c, _ := scriptedRender("/bundle", func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		prefix := args[len(args)-1]
		if err := writeSolid(prefix+"-1.png", 256, 256, color.NRGBA{255, 255, 255, 255}, nil); err != nil {
			return err
		}
		if err := os.WriteFile(prefix+"-2.png", []byte("not a png"), 0o600); err != nil {
			return err
		}
		return writeSolid(prefix+"-3.png", 256, 128, color.NRGBA{255, 255, 255, 255}, func(img *image.NRGBA) {
			img.SetNRGBA(1, 1, color.NRGBA{0, 0, 0, 255})
		})
	})
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err == nil {
		t.Fatal("undrawable page stored")
	}
	absentOutput(t, out)
}

func TestTectonicRenderBadBoundsDoNotSkip(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	precreate(t, out)
	c, _ := scriptedRender("/bundle", paintPages(256,
		whitePage(1, 10, 10),
		inkPage(2, 256, 128, 1, 1),
	))
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err == nil {
		t.Fatal("short page stored a later page")
	}
	absentOutput(t, out)
}

func TestTectonicRenderPaddingGapsAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.png")
	c, _ := scriptedRender("/bundle", func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		prefix := args[len(args)-1]
		if err := writeSolid(prefix+"-01.png", 256, 256, color.NRGBA{255, 255, 255, 255}, nil); err != nil {
			return err
		}
		return writeSolid(prefix+"-02.png", 256, 128, color.NRGBA{255, 255, 255, 255}, func(img *image.NRGBA) {
			img.SetNRGBA(8, 8, color.NRGBA{0, 0, 0, 255})
		})
	})
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, out)
	if isWhite(img.At(8, 72)) {
		t.Fatal("padded content page was not drawn")
	}

	gap := t.TempDir()
	gapIn := filepath.Join(gap, "in.latex")
	gapOut := filepath.Join(gap, "out.png")
	if err := os.WriteFile(gapIn, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	precreate(t, gapOut)
	gapConv, _ := scriptedRender("/bundle", func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		prefix := args[len(args)-1]
		if err := writeSolid(prefix+"-01.png", 256, 256, color.NRGBA{255, 255, 255, 255}, nil); err != nil {
			return err
		}
		return writeSolid(prefix+"-03.png", 256, 128, color.NRGBA{255, 255, 255, 255}, func(img *image.NRGBA) {
			img.SetNRGBA(1, 1, color.NRGBA{0, 0, 0, 255})
		})
	})
	if err := gapConv.Convert(context.Background(), gapIn, gapOut, map[string]any{"size": 256}); err == nil {
		t.Fatal("gap stored")
	}
	absentOutput(t, gapOut)

	dup := t.TempDir()
	dupIn := filepath.Join(dup, "in.latex")
	dupOut := filepath.Join(dup, "out.png")
	if err := os.WriteFile(dupIn, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	precreate(t, dupOut)
	dupConv, _ := scriptedRender("/bundle", func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		prefix := args[len(args)-1]
		if err := writeSolid(prefix+"-1.png", 8, 8, color.NRGBA{255, 255, 255, 255}, nil); err != nil {
			return err
		}
		return writeSolid(prefix+"-01.png", 8, 8, color.NRGBA{255, 255, 255, 255}, nil)
	})
	if err := dupConv.Convert(context.Background(), dupIn, dupOut, map[string]any{"size": 256}); err == nil {
		t.Fatal("duplicate stored")
	}
	absentOutput(t, dupOut)
}

func TestTectonicRenderSize(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.tex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var saw string
	c, calls := scriptedRender("/bundle", func(_ context.Context, name string, args []string) error {
		if name != "pdftoppm" {
			return nil
		}
		saw = args[2]
		edge := 1280
		if saw == "256" {
			edge = 256
		}
		if saw == "2048" {
			edge = 2048
		}
		return inkPage(1, edge, edge/2, 1, 1)(args[len(args)-1])
	})
	if err := c.Convert(context.Background(), in, out, nil); err != nil {
		t.Fatal(err)
	}
	if saw != "1280" {
		t.Fatalf("default %s", saw)
	}
	for _, size := range []any{256, 2048, float64(256), int64(2048)} {
		if err := c.Convert(context.Background(), in, out, map[string]any{"size": size}); err != nil {
			t.Fatalf("%v %v", size, err)
		}
	}
	before := len(*calls)
	precreate(t, out)
	for _, opts := range []map[string]any{
		{"size": 255},
		{"size": 2049},
		{"size": 256.5},
		{"size": float64(256.5)},
		{"quality": 1},
		{"size": 256, "quality": 1},
	} {
		if err := c.Convert(context.Background(), in, out, opts); err == nil {
			t.Fatalf("accepted %#v", opts)
		}
		absentOutput(t, out)
	}
	if len(*calls) != before {
		t.Fatalf("engine ran for a bad size: %d", len(*calls))
	}
	precreate(t, out)
	other := filepath.Join(dir, "out.webp")
	if err := os.WriteFile(other, []byte("precreated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Convert(context.Background(), filepath.Join(dir, "in.md"), other, nil); err == nil {
		t.Fatal("md accepted")
	}
	absentOutput(t, other)
	if len(*calls) != before {
		t.Fatal("engine ran for an unsupported pair")
	}
}

func TestTectonicRenderFailureRemovesOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	precreate(t, out)
	c, calls := scriptedRender("/bundle", func(_ context.Context, name string, _ []string) error {
		if name == "tectonic" {
			return errors.New("typeset")
		}
		return nil
	})
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err == nil {
		t.Fatal("typeset error stored")
	}
	absentOutput(t, out)
	if len(*calls) != 1 {
		t.Fatalf("calls %d", len(*calls))
	}

	precreate(t, out)
	bare := TectonicRenderConverter{}
	if err := bare.Convert(context.Background(), in, out, nil); err == nil || !errors.Is(err, ErrTeXBundleUnusable) {
		t.Fatal(err)
	}
	absentOutput(t, out)
}

func TestTectonicRenderNoPages(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	precreate(t, out)
	c, _ := scriptedRender("/bundle", func(context.Context, string, []string) error { return nil })
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err == nil {
		t.Fatal("empty raster stored")
	}
	absentOutput(t, out)
}

func TestTectonicRenderSpeckRule(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 4))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	if !tectonicPageClean(img) {
		t.Fatal("blank page")
	}
	img.SetNRGBA(0, 0, color.NRGBA{0, 0, 0, 0})
	if !tectonicPageClean(img) {
		t.Fatal("transparent black")
	}
	img.SetNRGBA(0, 0, color.NRGBA{0, 0, 0, 255})
	if tectonicPageClean(img) {
		t.Fatal("opaque black")
	}
	img.SetNRGBA(0, 0, color.NRGBA{255, 255, 255, 255})
	for i := 0; i < 16; i++ {
		img.SetNRGBA(i*2, 0, color.NRGBA{245, 245, 245, 255})
	}
	if !tectonicPageClean(img) {
		t.Fatal("sixteen isolated specks")
	}
	img.SetNRGBA(16*2, 0, color.NRGBA{245, 245, 245, 255})
	if tectonicPageClean(img) {
		t.Fatal("seventeen specks")
	}
	diag := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := range diag.Pix {
		diag.Pix[i] = 255
	}
	diag.SetNRGBA(0, 0, color.NRGBA{245, 245, 245, 255})
	diag.SetNRGBA(1, 1, color.NRGBA{245, 245, 245, 255})
	if !tectonicPageClean(diag) {
		t.Fatal("diagonal specks")
	}
	diag.SetNRGBA(1, 0, color.NRGBA{245, 245, 245, 255})
	if tectonicPageClean(diag) {
		t.Fatal("adjacent specks")
	}
	ink := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for i := range ink.Pix {
		ink.Pix[i] = 255
	}
	ink.SetNRGBA(0, 0, color.NRGBA{239, 255, 255, 255})
	if tectonicPageClean(ink) {
		t.Fatal("channel below 240")
	}
}

func TestTectonicRenderSandbox(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TECTONIC_CACHE_DIR", cache)
	bundle := "/opt/tectonic/octoconv-bundle.ttb"
	pdf := filepath.Join(dir, "in.pdf")
	prefix := filepath.Join(dir, "pages")
	ppm := []string{"-png", "-scale-to", "1280", pdf, prefix}
	c := TectonicRenderConverter{TectonicConverter: TectonicConverter{
		BundlePath:        bundle,
		ConfineFilesystem: true,
		SandboxHelper:     []string{"/usr/local/bin/tectonic-worker", "__sandbox-exec"},
		CPUSeconds:        271,
	}}
	name, args, err := c.rasterCommand(dir, ppm)
	if err != nil {
		t.Fatal(err)
	}
	if name != c.SandboxHelper[0] {
		t.Fatalf("name %s", name)
	}
	dash := -1
	for i, arg := range args {
		if arg == "--" {
			dash = i
			break
		}
	}
	if dash < 0 {
		t.Fatalf("args %v", args)
	}
	engine, err := exec.LookPath("pdftoppm")
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "__sandbox-exec" || args[dash+1] != engine {
		t.Fatalf("args %v", args)
	}
	if strings.Join(args[dash+2:], "\x00") != strings.Join(ppm, "\x00") {
		t.Fatalf("command %v", args[dash+2:])
	}
	job, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	var cpu, jobRW, devNull bool
	for _, arg := range args[:dash] {
		if strings.HasPrefix(arg, "sock:") {
			t.Fatalf("socket grant %s", arg)
		}
		if arg == "cpu:271" {
			cpu = true
		}
		if arg == "rw:"+job {
			jobRW = true
		}
		if arg == "rw:/dev/null" {
			devNull = true
		}
		path := arg
		if i := strings.IndexByte(arg, ':'); i >= 0 {
			path = arg[i+1:]
		}
		if path == bundle || path == cache || path == "/tmp" || path == "/private/tmp" || path == "/usr" {
			t.Fatalf("grant %s", arg)
		}
	}
	if !cpu || !jobRW || !devNull {
		t.Fatalf("cpu %v job %v devnull %v args %v", cpu, jobRW, devNull, args[:dash])
	}

	c.ConfineFilesystem = false
	c.SandboxHelper = []string{"helper", "mode"}
	c.CPUSeconds = 9
	name, args, err = c.rasterCommand(dir, ppm)
	if err != nil {
		t.Fatal(err)
	}
	if name != "helper" || len(args) < 3 || args[0] != "mode" || args[1] != "cpu:9" || args[2] != "--" {
		t.Fatalf("unconfined %s %v", name, args)
	}
	for _, arg := range args[:2] {
		if strings.HasPrefix(arg, "rw:") || strings.HasPrefix(arg, "ro:") {
			t.Fatalf("path grant without confinement %s", arg)
		}
	}

	c.SandboxHelper = nil
	name, args, err = c.rasterCommand(dir, ppm)
	if err != nil {
		t.Fatal(err)
	}
	if name != "pdftoppm" || strings.Join(args, "\x00") != strings.Join(ppm, "\x00") {
		t.Fatalf("direct %s %v", name, args)
	}
}

func TestTectonicRenderRasterDeadline(t *testing.T) {
	c := TectonicRenderConverter{TectonicConverter: TectonicConverter{CPUSeconds: 271}}
	raw := fmt.Errorf("pdftoppm killed: %w", context.DeadlineExceeded)
	busy := engineCPU{User: time.Second, Elapsed: time.Second, Known: true}
	err := c.classifyRaster(context.Background(), raw, busy)
	if !errors.Is(err, ErrTeXEngineTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTeXEngineStarved) {
		t.Fatal(err)
	}
	idle := engineCPU{User: 10 * time.Millisecond, Elapsed: time.Second, Known: true}
	err = c.classifyRaster(context.Background(), raw, idle)
	if !errors.Is(err, ErrTeXEngineStarved) || errors.Is(err, ErrTeXEngineTimeout) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	unknown := engineCPU{}
	err = c.classifyRaster(context.Background(), raw, unknown)
	if !errors.Is(err, ErrTeXEngineTimeout) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	half := engineCPU{User: 500 * time.Millisecond, Elapsed: time.Second, Known: true}
	err = c.classifyRaster(context.Background(), raw, half)
	if !errors.Is(err, ErrTeXEngineTimeout) || errors.Is(err, ErrTeXEngineStarved) {
		t.Fatal(err)
	}
	plain := errors.New("pdftoppm failed")
	if got := c.classifyRaster(context.Background(), plain, busy); got != plain {
		t.Fatal(got)
	}
}

func TestTectonicRenderRealPage(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.latex")
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := TectonicRenderConverter{}
	c.compile = func(_ context.Context, _, pdfPath string) error {
		return os.WriteFile(pdfPath, tinyPDF, 0o600)
	}
	if err := c.Convert(context.Background(), in, out, map[string]any{"size": 256}); err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, out)
	if img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("bounds %v", img.Bounds())
	}
	if !isWhite(img.At(0, 0)) {
		t.Fatal("padding is not white")
	}
	found := false
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y && !found; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if !isWhite(img.At(x, y)) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("printed page is blank")
	}
}

var tinyPDF = []byte(`%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Count 1/Kids[3 0 R]>>endobj
3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 300 100]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj
4 0 obj<</Length 44>>stream
BT /F1 24 Tf 40 40 Td (Hello) Tj ET
endstream
endobj
5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj
xref
0 6
0000000000 65535 f 
0000000009 00000 n 
0000000052 00000 n 
0000000101 00000 n 
0000000206 00000 n 
0000000300 00000 n 
trailer<</Size 6/Root 1 0 R>>
startxref
373
%%EOF
`)
