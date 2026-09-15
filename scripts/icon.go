//go:build ignore

// icon 是图标生成工具：把项目根目录的 github++.png 转换为
// fpk 打包所需的 ICON.PNG（512x512）、ICON_256.PNG（256x256）
// 以及 Web 控制台的 favicon.png（32x32）。
//
// 用法：go run scripts/icon.go
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	src, err := loadPNG("github++.png")
	if err != nil {
		fmt.Println("读取源图标失败:", err)
		os.Exit(1)
	}

	jobs := []struct {
		out  string
		size int
	}{
		{"ICON.PNG", 512},
		{"ICON_256.PNG", 256},
		// fpk 桌面入口所需的图标：app/ui/images/icon-64.png 与 icon-256.png。
		//
		// 注意这里用的是连字符而不是下划线：app/ui/config 里写的是
		// "icon": "images/icon-{0}.png"，{0} 会被系统替换成 64 / 256，
		// 所以磁盘上的文件名必须是 icon-64.png / icon-256.png。
		// 命名成 icon_64.png 会让桌面图标 404。
		{filepath.Join("fpk", "app", "ui", "images", "icon-64.png"), 64},
		{filepath.Join("fpk", "app", "ui", "images", "icon-256.png"), 256},
		{filepath.Join("web", "static", "favicon.png"), 32},
	}
	for _, j := range jobs {
		if err := savePNG(j.out, fitSquare(src, j.size)); err != nil {
			fmt.Println("生成 "+j.out+" 失败:", err)
			os.Exit(1)
		}
		fmt.Printf("已生成 %s (%dx%d)\n", j.out, j.size, j.size)
	}
}

// loadPNG 读取 PNG 文件。
func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// savePNG 保存 RGBA PNG。
func savePNG(path string, img *image.RGBA) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// fitSquare 把图像等比缩放到 size x size 的正方形画布，
// 背景色取源图像左上角像素，保证视觉上无缝。
func fitSquare(src image.Image, size int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()

	// 取左上角作为画布底色。
	bg := color.RGBAModel.Convert(src.At(b.Min.X, b.Min.Y)).(color.RGBA)

	out := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			out.SetRGBA(x, y, bg)
		}
	}

	// 等比缩放：长边贴合画布。
	scale := float64(size) / float64(max(sw, sh))
	dw := int(float64(sw) * scale)
	dh := int(float64(sh) * scale)
	ox := (size - dw) / 2
	oy := (size - dh) / 2

	for y := 0; y < dh; y++ {
		// 双线性插值的源坐标。
		sy := float64(y) / scale
		sy0 := int(sy)
		fy := sy - float64(sy0)
		sy1 := minInt(sy0+1, sh-1)
		if sy0 >= sh {
			sy0 = sh - 1
		}
		for x := 0; x < dw; x++ {
			sx := float64(x) / scale
			sx0 := int(sx)
			fx := sx - float64(sx0)
			sx1 := minInt(sx0+1, sw-1)
			if sx0 >= sw {
				sx0 = sw - 1
			}

			// 四个邻点双线性混合。
			c00 := rgbaAt(src, b.Min.X+sx0, b.Min.Y+sy0)
			c10 := rgbaAt(src, b.Min.X+sx1, b.Min.Y+sy0)
			c01 := rgbaAt(src, b.Min.X+sx0, b.Min.Y+sy1)
			c11 := rgbaAt(src, b.Min.X+sx1, b.Min.Y+sy1)
			out.SetRGBA(ox+x, oy+y, bilerp(c00, c10, c01, c11, fx, fy))
		}
	}
	return out
}

func rgbaAt(img image.Image, x, y int) color.RGBA {
	return color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
}

func bilerp(c00, c10, c01, c11 color.RGBA, fx, fy float64) color.RGBA {
	top := lerpRGBA(c00, c10, fx)
	bottom := lerpRGBA(c01, c11, fx)
	return lerpRGBA(top, bottom, fy)
}

func lerpRGBA(a, b color.RGBA, t float64) color.RGBA {
	return color.RGBA{
		R: lerp8(a.R, b.R, t),
		G: lerp8(a.G, b.G, t),
		B: lerp8(a.B, b.B, t),
		A: lerp8(a.A, b.A, t),
	}
}

func lerp8(a, b uint8, t float64) uint8 {
	return uint8(float64(a)*(1-t) + float64(b)*t + 0.5)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
