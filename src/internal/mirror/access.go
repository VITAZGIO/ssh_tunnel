package mirror

import (
	"errors"
	"net/http"
	"os"
	"sort"
	"sync"
)

// Доступ к раздаваемым папкам по устройствам (ТЗ 18), как у Syncthing:
// новое устройство сети не видит ни одной папки, пока владелец не отметит
// её для него. Список — по постоянному id устройства (mesh.PeerID), не по
// адресу и не по имени: оба могут смениться.
//
// Файл access.json рядом с индексом сервера папок:
//
//	{"v":1,"folders":{"<папка>":["<id>",…]},"old":["<папка>",…],"names":{"<id>":"имя"}}
//
// Обновление (папки, которые раздавались до этой версии), чтобы никого не
// отрезать: при первом открытии все уже раздаваемые папки попадают в old —
// они видны всем, как раньше. Как только владелец узнаёт id устройств сети
// (новый meshd), Migrate кладёт каждое известное в этот момент устройство в
// список каждой старой папки, и old пустеет. Дальше — только по спискам.
// Новая папка начинается с пустого списка.

// Access — кому видна каждая раздаваемая папка.
type Access struct {
	path string

	mu sync.Mutex
	d  accessFile
}

type accessFile struct {
	V       int                 `json:"v"`
	Folders map[string][]string `json:"folders"`
	Old     []string            `json:"old,omitempty"`
	// Names — последнее известное имя устройства (для списка владельца,
	// когда устройства уже нет в сети устройств).
	Names map[string]string `json:"names,omitempty"`
}

// maxGrants — сколько устройств в списке одной папки.
const maxGrants = 1024

// OpenAccess открывает список доступа path. existing — папки, которые уже
// раздаются: если файла ещё нет (первый запуск этой версии), они
// считаются старыми и видны всем до Migrate.
func OpenAccess(path string, existing []string) (*Access, error) {
	a := &Access{path: path}
	err := readJSON(path, &a.d)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		a.d = accessFile{V: 1, Old: append([]string(nil), existing...)}
		sort.Strings(a.d.Old)
		a.fix()
		return a, writeJSONAtomic(path, &a.d)
	default:
		return nil, err
	}
	a.fix()
	return a, nil
}

func (a *Access) fix() {
	a.d.V = 1
	if a.d.Folders == nil {
		a.d.Folders = map[string][]string{}
	}
	if a.d.Names == nil {
		a.d.Names = map[string]string{}
	}
}

func (a *Access) saveLocked() error { return writeJSONAtomic(a.path, &a.d) }

func (a *Access) oldLocked(folder string) bool {
	for _, f := range a.d.Old {
		if f == folder {
			return true
		}
	}
	return false
}

// Allowed — видна ли папка устройству id. Пустой id (не узнать, кто
// пришёл) получает только старые папки до Migrate — как было до этой
// версии.
func (a *Access) Allowed(folder, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oldLocked(folder) {
		return true
	}
	if id == "" {
		return false
	}
	for _, d := range a.d.Folders[folder] {
		if d == id {
			return true
		}
	}
	return false
}

// Pending — есть старые папки, ждущие Migrate.
func (a *Access) Pending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.d.Old) > 0
}

// IsOld — папка из старых (видна всем до Migrate).
func (a *Access) IsOld(folder string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.oldLocked(folder)
}

// Known — устройство сети на момент Migrate.
type Known struct {
	ID   string
	Name string
}

// Migrate — правило обновления: каждое устройство из known попадает в
// список каждой старой папки, старых больше нет. Пустой known — ничего не
// делать (id устройств ещё не известны). Возвращает, было ли что делать.
func (a *Access) Migrate(known []Known) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.d.Old) == 0 || len(known) == 0 {
		return false, nil
	}
	for _, f := range a.d.Old {
		for _, k := range known {
			if k.ID != "" {
				a.addLocked(f, k.ID)
				if k.Name != "" {
					a.d.Names[k.ID] = k.Name
				}
			}
		}
	}
	a.d.Old = nil
	return true, a.saveLocked()
}

func (a *Access) addLocked(folder, id string) bool {
	for _, d := range a.d.Folders[folder] {
		if d == id {
			return false
		}
	}
	a.d.Folders[folder] = append(a.d.Folders[folder], id)
	sort.Strings(a.d.Folders[folder])
	return true
}

// ErrTooMany — список папки полон.
var ErrTooMany = errors.New("у папки уже слишком много устройств")

// Set даёт (on) или забирает доступ к папке у устройства id. name —
// как его показать, когда устройства не будет в сети (необязательно).
// Старая папка при первой правке перестаёт быть старой: дальше — только по
// списку (иначе снятая галочка ничего бы не значила).
func (a *Access) Set(folder, id, name string, on bool) error {
	if folder == "" || id == "" || len(id) > 64 {
		return errors.New("нужны папка и id устройства")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropOldLocked(folder)
	if on {
		if len(a.d.Folders[folder]) >= maxGrants {
			return ErrTooMany
		}
		a.addLocked(folder, id)
		if name != "" {
			a.d.Names[id] = name
		}
	} else {
		list := a.d.Folders[folder][:0]
		for _, d := range a.d.Folders[folder] {
			if d != id {
				list = append(list, d)
			}
		}
		if len(list) == 0 {
			delete(a.d.Folders, folder)
		} else {
			a.d.Folders[folder] = list
		}
	}
	return a.saveLocked()
}

func (a *Access) dropOldLocked(folder string) {
	out := a.d.Old[:0]
	for _, f := range a.d.Old {
		if f != folder {
			out = append(out, f)
		}
	}
	a.d.Old = out
	if len(a.d.Old) == 0 {
		a.d.Old = nil
	}
}

// Devices — у кого есть папка (id по порядку).
func (a *Access) Devices(folder string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.d.Folders[folder]...)
}

// Granted — все id, у которых есть хоть одна папка, с их числом папок.
func (a *Access) Granted() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]int{}
	for _, list := range a.d.Folders {
		for _, d := range list {
			out[d]++
		}
	}
	return out
}

// Name — последнее известное имя устройства.
func (a *Access) Name(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.d.Names[id]
}

// Remove — папку больше не раздают: её список не нужен.
func (a *Access) Remove(folder string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.d.Folders, folder)
	a.dropOldLocked(folder)
	return a.saveLocked()
}

// Guard — проверка для Hub.Visible: id устройства запроса — по id(r).
func (a *Access) Guard(id func(*http.Request) string) func(*http.Request, string) bool {
	return func(r *http.Request, folder string) bool { return a.Allowed(folder, id(r)) }
}
