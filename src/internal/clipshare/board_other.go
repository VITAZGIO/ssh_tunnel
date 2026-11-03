//go:build !windows

package clipshare

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Linux с рабочим столом: wl-paste/wl-copy (Wayland) или xclip (X11).
// Ни того ни другого (сервер без экрана) — буфера нет, запись приходит
// только в историю. На телефоне (GOOS=android) буфер — у Kotlin (SetSink).

func localBoard() Board {
	if runtime.GOOS != "linux" {
		return nil
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("wl-paste"); err == nil {
			if _, err := exec.LookPath("wl-copy"); err == nil {
				return &cmdBoard{wayland: true}
			}
		}
	}
	if os.Getenv("DISPLAY") != "" {
		if _, err := exec.LookPath("xclip"); err == nil {
			return &cmdBoard{}
		}
	}
	return nil
}

type cmdBoard struct {
	wayland bool

	mu   sync.Mutex
	last Clip
	ok   bool
}

func (b *cmdBoard) out(args ...string) []byte {
	var cmd *exec.Cmd
	if b.wayland {
		cmd = exec.Command("wl-paste", append([]string{"--no-newline"}, args...)...)
	} else {
		cmd = exec.Command("xclip", append([]string{"-selection", "clipboard", "-o"}, args...)...)
	}
	o, err := cmd.Output()
	if err != nil {
		return nil
	}
	return o
}

func (b *cmdBoard) types() []string {
	var o []byte
	if b.wayland {
		o = b.out("--list-types")
	} else {
		o = b.out("-t", "TARGETS")
	}
	return strings.Fields(string(o))
}

func (b *cmdBoard) typed(t string) []byte { return b.out("-t", t) }

// read — содержимое сейчас.
func (b *cmdBoard) read() (Clip, bool) {
	ts := b.types()
	has := func(t string) bool {
		for _, x := range ts {
			if x == t {
				return true
			}
		}
		return false
	}
	switch {
	case has("text/uri-list") || has("x-special/gnome-copied-files"):
		list := string(b.typed("text/uri-list"))
		var files []string
		for _, l := range strings.Split(list, "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "file://") {
				if u, err := url.Parse(l); err == nil {
					files = append(files, u.Path)
				}
			}
		}
		if len(files) != 1 {
			return Clip{}, false
		}
		st, err := os.Stat(files[0])
		if err != nil || !st.Mode().IsRegular() {
			return Clip{}, false
		}
		c := Clip{Kind: KindFile, Name: filepath.Base(files[0])}
		if st.Size() > MaxBytes {
			return c, true
		}
		if c.Data, err = os.ReadFile(files[0]); err != nil {
			return Clip{}, false
		}
		return c, true
	case has("text/plain;charset=utf-8") || has("UTF8_STRING") || has("text/plain"):
		t := "text/plain;charset=utf-8"
		if !has(t) {
			t = map[bool]string{true: "UTF8_STRING", false: "text/plain"}[has("UTF8_STRING")]
		}
		if d := b.typed(t); len(d) > 0 {
			return Clip{Kind: KindText, Data: d}, true
		}
	case has("image/png"):
		if d := b.typed("image/png"); len(d) > 0 {
			return Clip{Kind: KindImage, Mime: "image/png", Data: d}, true
		}
	}
	return Clip{}, false
}

// Token — отпечаток содержимого (опрос; событий смены у этих утилит нет).
func (b *cmdBoard) Token() string {
	c, ok := b.read()
	b.mu.Lock()
	b.last, b.ok = c, ok
	b.mu.Unlock()
	h := sha256.Sum256(append([]byte(c.Kind+c.Name), c.Data...))
	return hex.EncodeToString(h[:8])
}

func (b *cmdBoard) Read() (Clip, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last, b.ok
}

func (b *cmdBoard) Write(c Clip) error {
	mime, data := "text/plain;charset=utf-8", c.Data
	switch c.Kind {
	case KindImage:
		mime = "image/png"
	case KindFile:
		mime, data = "text/uri-list", []byte((&url.URL{Scheme: "file", Path: c.Path}).String()+"\n")
	}
	var cmd *exec.Cmd
	if b.wayland {
		cmd = exec.Command("wl-copy", "-t", mime)
	} else {
		cmd = exec.Command("xclip", "-selection", "clipboard", "-t", mime, "-i")
	}
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}
