//go:build !windows

package phonelink

// Linux: картинка в буфер через xclip (X11) или wl-copy (Wayland), затем
// Ctrl+V через xdotool/ydotool. На телефоне — нельзя.

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg"
	"image/png"
	"os/exec"
	"runtime"
	"time"
)

func pasteImage(data []byte) error {
	if runtime.GOOS == "android" {
		return errors.New("на телефон картинку так не вставить")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return errors.New("не картинка (нужен JPEG или PNG)")
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	var cmd *exec.Cmd
	if p, err := exec.LookPath("wl-copy"); err == nil {
		cmd = exec.Command(p, "--type", "image/png")
	} else if p, err := exec.LookPath("xclip"); err == nil {
		cmd = exec.Command(p, "-selection", "clipboard", "-t", "image/png", "-i")
	} else {
		return errors.New("нужен xclip или wl-clipboard: sudo apt install xclip")
	}
	cmd.Stdin = &buf
	if err := cmd.Start(); err != nil { // xclip остаётся держать буфер — не ждём
		return err
	}
	go cmd.Wait()
	time.Sleep(150 * time.Millisecond)
	t := tool()
	if t == "" {
		return nil // в буфере — вставишь сам
	}
	return exec.Command(t, "key", "ctrl+v").Run()
}
