package diag

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/events"
)

// Journal — журнал на диске за последние дни (для отчёта поддержке): записи
// журнала программы, смены состояния и неудачные соединения. Удачные
// соединения не пишутся — они никому не нужны и раздувают файл. Два файла
// по journalMax: journal.log и journal.1.log.
type Journal struct {
	path string
	mu   sync.Mutex
}

const journalMax = 1 << 20

// StartJournal пишет события bus в journal.log в папке dir.
func StartJournal(bus *events.Bus, dir string) *Journal {
	j := &Journal{path: filepath.Join(dir, "journal.log")}
	ch, _ := bus.Subscribe()
	go func() {
		for e := range ch {
			if line := journalLine(e); line != "" {
				j.write(line)
			}
		}
	}()
	return j
}

func journalLine(e events.Event) string {
	ts := e.Time.Format("2006-01-02 15:04:05")
	switch e.Kind {
	case events.KindLog:
		return fmt.Sprintf("%s %-5s %s", ts, e.Level, e.Text)
	case events.KindState:
		s := fmt.Sprintf("%s state %s", ts, e.State)
		if e.Detail != "" {
			s += " — " + e.Detail
		}
		if e.ErrorKind != "" {
			s += " [" + e.ErrorKind + "]"
		}
		return s
	case events.KindConn:
		if e.Failed {
			return fmt.Sprintf("%s conn  %s → %s: %s", ts, e.Process, e.Target, e.Error)
		}
	}
	return ""
}

func (j *Journal) write(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if st, err := os.Stat(j.path); err == nil && st.Size() > journalMax {
		_ = os.Rename(j.path, strings.TrimSuffix(j.path, ".log")+".1.log")
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strings.ReplaceAll(line, "\n", " ") + "\n")
}

// recentJournal — строки журнала за последние days дней (старый файл, потом
// новый).
func recentJournal(dir string, days int) string {
	since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	var b strings.Builder
	for _, name := range []string{"journal.1.log", "journal.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			if len(l) >= 10 && l[:10] >= since {
				b.WriteString(l)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
