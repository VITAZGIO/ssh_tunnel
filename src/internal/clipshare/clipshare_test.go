package clipshare

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
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
