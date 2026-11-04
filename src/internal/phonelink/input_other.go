//go:build !windows

package phonelink

// Linux (рабочий стол X11/Wayland с xdotool или ydotool) и телефон. На
// телефоне мышью и текстом с других устройств управлять нельзя (Android не
// даёт приложениям вводить за человека); медиа и звонок делает приложение
// (SetPhone).

import (
	"errors"
	"os/exec"
	"runtime"
	"strconv"
)

func tool() string {
	for _, t := range []string{"xdotool", "ydotool"} {
		if p, err := exec.LookPath(t); err == nil {
			return p
		}
	}
	return ""
}

var mediaKeys = map[string]string{
	"playpause": "XF86AudioPlay", "next": "XF86AudioNext", "prev": "XF86AudioPrev", "stop": "XF86AudioStop",
	"volup": "XF86AudioRaiseVolume", "voldown": "XF86AudioLowerVolume", "mute": "XF86AudioMute",
}

func inject(e Ev) error {
	if runtime.GOOS == "android" {
		return errors.New("телефоном с другого устройства управлять нельзя — Android этого не даёт")
	}
	t := tool()
	if t == "" {
		return errors.New("на этом Linux нет xdotool — поставь: sudo apt install xdotool")
	}
	var args [][]string
	switch e.Kind {
	case "media":
		k, ok := mediaKeys[e.Key]
		if !ok {
			return errors.New("нет такой кнопки")
		}
		args = append(args, []string{"key", k})
	case "mouse":
		if e.DX != 0 || e.DY != 0 {
			args = append(args, []string{"mousemove_relative", "--", strconv.Itoa(e.DX), strconv.Itoa(e.DY)})
		}
		switch e.Btn {
		case "left":
			args = append(args, []string{"click", "1"})
		case "right":
			args = append(args, []string{"click", "3"})
		case "middle":
			args = append(args, []string{"click", "2"})
		case "down":
			args = append(args, []string{"mousedown", "1"})
		case "up":
			args = append(args, []string{"mouseup", "1"})
		}
		for i := 0; i < e.Scroll; i++ {
			args = append(args, []string{"click", "4"})
		}
		for i := 0; i > e.Scroll; i-- {
			args = append(args, []string{"click", "5"})
		}
	case "text":
		args = append(args, []string{"type", "--", e.Text})
	}
	for _, a := range args {
		if err := exec.Command(t, a...).Run(); err != nil {
			return err
		}
	}
	return nil
}

func showNotif(e Ev) {
	if runtime.GOOS == "android" {
		return
	}
	if p, err := exec.LookPath("notify-send"); err == nil {
		_ = exec.Command(p, e.From+" · "+e.App+": "+e.Title, e.Text).Start()
	}
}

func ring(from string) {
	if p, err := exec.LookPath("notify-send"); err == nil && runtime.GOOS != "android" {
		_ = exec.Command(p, "-u", "critical", "Ищут этот компьютер", "С устройства «"+from+"» нажали «Найти».").Start()
	}
}
