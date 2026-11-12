package clipshare

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistoryLimits(t *testing.T) {
	h := openHistory(t.TempDir())
	for i := 0; i < keepItems+5; i++ {
		if _, err := h.add(Item{Kind: KindText}, []byte{byte('a' + i%26)}); err != nil {
			t.Fatal(err)
		}
	}
	items, seq, _ := h.list(0)
	if len(items) != keepItems || seq != keepItems+5 || items[0].Seq != seq {
		t.Fatalf("история: %d записей, seq %d", len(items), seq)
	}
	newer, _, _ := h.list(seq - 2)
	if len(newer) != 2 {
		t.Fatalf("новее: %d", len(newer))
	}
	if !h.has(items[0].ID) {
		t.Fatal("повтор не узнан")
	}
	h.remove("")
	if all, _, _ := h.list(0); len(all) != 0 {
		t.Fatal("не очистилось")
	}
}

func TestDIBRoundTrip(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	img.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	img.SetNRGBA(2, 1, color.NRGBA{0, 0, 255, 128})
	var p bytes.Buffer
	png.Encode(&p, img)
	dib, err := pngToDIB(p.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	back, err := dibToPNG(dib)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := png.Decode(bytes.NewReader(back))
	if c := color.NRGBAModel.Convert(got.At(0, 0)).(color.NRGBA); c != (color.NRGBA{255, 0, 0, 255}) {
		t.Fatalf("(0,0) = %v", c)
	}
	if c := color.NRGBAModel.Convert(got.At(2, 1)).(color.NRGBA); c != (color.NRGBA{0, 0, 255, 128}) {
		t.Fatalf("(2,1) = %v", c)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", `C:\x\y.txt`: "y.txt", "": "file", "..": "file"} {
		if got := safeName(in); got != want {
			t.Errorf("%q → %q, ждали %q", in, got, want)
		}
	}
}

// fakeBoard — буфер в памяти.
type fakeBoard struct {
	mu  sync.Mutex
	c   Clip
	seq int
}

func (f *fakeBoard) Token() string { f.mu.Lock(); defer f.mu.Unlock(); return fmt.Sprint(f.seq) }
func (f *fakeBoard) Read() (Clip, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c, len(f.c.Data) > 0
}
func (f *fakeBoard) Write(c Clip) error { f.mu.Lock(); f.c = c; f.seq++; f.mu.Unlock(); return nil }

// countLink — ядро в процессе, но отправки только считаются.
type countLink struct {
	localLink
	mu     sync.Mutex
	pushed []string
}

func (l *countLink) active() bool { return true }
func (l *countLink) push(c Clip) error {
	l.mu.Lock()
	l.pushed = append(l.pushed, string(c.Data))
	l.mu.Unlock()
	return nil
}

func TestNoLoop(t *testing.T) {
	n := startNode(t.TempDir())
	defer n.close()
	nodeMu.Lock()
	running = n
	nodeMu.Unlock()
	defer func() { nodeMu.Lock(); running = nil; nodeMu.Unlock() }()

	b := &fakeBoard{}
	l := &countLink{}
	stop := watch(b, l)
	defer stop()
	time.Sleep(300 * time.Millisecond) // наблюдатель узнал номер истории

	// Пришло с другого устройства → в буфер, но обратно не уходит.
	req := httptest.NewRequest(http.MethodPost, "/clip?id=abc&kind=text", strings.NewReader("с телефона"))
	w := httptest.NewRecorder()
	n.receive(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, _ := b.Read(); string(c.Data) == "с телефона" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("полученное не попало в буфер")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	// Повтор той же записи не кладётся второй раз.
	n.receive(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/clip?id=abc&kind=text", strings.NewReader("с телефона")))
	if items, _, _ := n.h.list(0); len(items) != 1 {
		t.Fatalf("повтор попал в историю: %d", len(items))
	}
	// Своё — уходит.
	b.Write(Clip{Kind: KindText, Data: []byte("с ПК")})
	time.Sleep(1500 * time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pushed) != 1 || l.pushed[0] != "с ПК" {
		t.Fatalf("отправлено: %q", l.pushed)
	}
}

// jpegOf — JPEG w×h одного цвета.
func jpegOf(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func near(a, b uint8) bool { return int(a)-int(b) < 12 && int(b)-int(a) < 12 }

func TestImageBitmapsJPEG(t *testing.T) {
	// Нечётные размеры: строка DIB выравнивается не нужно (32 бита), но
	// цветовые блоки JPEG (подвыборка 2×2) на краю неполные.
	for _, sz := range [][2]int{{1, 1}, {3, 5}, {17, 9}, {33, 1}} {
		src := jpegOf(t, sz[0], sz[1], color.RGBA{R: 200, G: 40, B: 40, A: 255})
		dib, p, err := imageBitmaps(src)
		if err != nil {
			t.Fatalf("%v: %v", sz, err)
		}
		if len(dib) != 40+sz[0]*sz[1]*4 {
			t.Fatalf("%v: DIB %d байт", sz, len(dib))
		}
		if w, h := int32(binary.LittleEndian.Uint32(dib[4:])), int32(binary.LittleEndian.Uint32(dib[8:])); int(w) != sz[0] || int(h) != sz[1] {
			t.Fatalf("%v: в заголовке %d×%d", sz, w, h)
		}
		// Первая точка в DIB — B, G, R, A.
		if !near(dib[40], 40) || !near(dib[41], 40) || !near(dib[42], 200) || dib[43] != 255 {
			t.Fatalf("%v: цвет %v", sz, dib[40:44])
		}
		if cfg, err := png.DecodeConfig(bytes.NewReader(p)); err != nil || cfg.Width != sz[0] || cfg.Height != sz[1] {
			t.Fatalf("%v: PNG %v %v", sz, cfg, err)
		}
	}
}

func TestImageBitmapsOrderAndFormats(t *testing.T) {
	// Верх красный, низ синий: в DIB строки идут снизу вверх.
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for x := 0; x < 2; x++ {
		img.SetNRGBA(x, 0, color.NRGBA{255, 0, 0, 255})
		img.SetNRGBA(x, 1, color.NRGBA{0, 0, 255, 255})
	}
	var pb, gb bytes.Buffer
	png.Encode(&pb, img)
	gif.Encode(&gb, img, nil)
	for name, src := range map[string][]byte{"png": pb.Bytes(), "gif": gb.Bytes()} {
		dib, p, err := imageBitmaps(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if dib[40] != 255 || dib[42] != 0 { // первая строка DIB — нижняя, синяя
			t.Fatalf("%s: нижняя строка %v", name, dib[40:44])
		}
		if dib[40+8+2] != 255 { // вторая строка DIB — верхняя, красная
			t.Fatalf("%s: верхняя строка %v", name, dib[48:52])
		}
		if _, err := png.Decode(bytes.NewReader(p)); err != nil {
			t.Fatalf("%s: PNG: %v", name, err)
		}
	}
	if _, _, err := imageBitmaps([]byte("просто текст")); err != errNotImage {
		t.Fatalf("текст: %v", err)
	}
	if _, _, err := imageBitmaps(nil); err != errNotImage {
		t.Fatalf("пусто: %v", err)
	}
}

func TestImageBitmapsPixelLimit(t *testing.T) {
	old := maxPixels
	maxPixels = 100
	defer func() { maxPixels = old }()
	if _, _, err := imageBitmaps(jpegOf(t, 10, 10, color.RGBA{A: 255})); err != nil {
		t.Fatalf("ровно на пределе: %v", err)
	}
	if _, _, err := imageBitmaps(jpegOf(t, 11, 10, color.RGBA{A: 255})); err != errTooLarge {
		t.Fatalf("сверх предела: %v", err)
	}
	var p bytes.Buffer
	png.Encode(&p, image.NewNRGBA(image.Rect(0, 0, 11, 10)))
	if _, err := pngToDIB(p.Bytes()); err != errTooLarge {
		t.Fatalf("pngToDIB сверх предела: %v", err)
	}
}

// Полученный файл-картинка (на Windows он лежит в буфере несколькими
// форматами, а Read отдаёт тот же файл) обратно в сеть не уходит.
func TestNoLoopFileImage(t *testing.T) {
	n := startNode(t.TempDir())
	defer n.close()
	nodeMu.Lock()
	running = n
	nodeMu.Unlock()
	defer func() { nodeMu.Lock(); running = nil; nodeMu.Unlock() }()

	b := &fakeBoard{}
	l := &countLink{}
	stop := watch(b, l)
	defer stop()
	time.Sleep(300 * time.Millisecond)

	photo := jpegOf(t, 5, 3, color.RGBA{R: 9, A: 255})
	req := httptest.NewRequest(http.MethodPost, "/clip?id=img1&kind=file&name=p.jpg&mime=image/jpeg", bytes.NewReader(photo))
	w := httptest.NewRecorder()
	n.receive(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, _ := b.Read(); len(c.Data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("фото не попало в буфер")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pushed) != 0 {
		t.Fatalf("полученное фото ушло обратно: %d", len(l.pushed))
	}
}
