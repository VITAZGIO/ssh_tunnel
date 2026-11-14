package mirror

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Hub — устройство-сервер зеркальных папок. Единственный источник правды:
// у каждого файла номер версии (Ver), клиент присылает изменение с номером,
// от которого правил (base). Номер не совпал — конфликт: ничего не
// перезаписывается, присланное ложится рядом как «файл (конфликт, …)».
//
// Что заменяется или удаляется, сначала уходит в версии (корзину) — там
// лежит Keep (30 дней). Состояние папки — снимок index.json + журнал
// journal.jsonl (строка на изменение, с fsync): оборванная запись теряет
// максимум последнюю строку, а не индекс.
type Hub struct {
	dir  string
	now  func() time.Time
	keep time.Duration
	host string // как сервер подписывает свои изменения
	// OnEvent — заметное для человека на сервере (новый конфликт).
	OnEvent func(kind, text string)
	// Room — квота сервера (quota.go): сколько ещё можно записать в папку,
	// used — сколько она занимает сейчас; nil — без предела.
	Room func(folder string, used int64) (left int64, why string)
	// OnUsage — занятое место папки изменилось на delta байт (сразу).
	OnUsage func(folder string, delta int64)
	// Visible — видна ли папка тому, кто пришёл (ТЗ 18, access.go); nil —
	// всем. Невидимая папка во всём протоколе — как несуществующая.
	Visible func(r *http.Request, folder string) bool

	mu      sync.Mutex
	folders map[string]*hfolder
	wake    chan struct{} // закрывается при любом изменении (долгий опрос)
}

// HubFolder — папка, которую раздаёт этот сервер.
type HubFolder struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Ignore []string `json:"ignore,omitempty"` // свои исключения сверх DefaultIgnore
}

// FileInfo — файл в индексе сервера.
type FileInfo struct {
	Ver     int64  `json:"ver"`
	Hash    string `json:"hash,omitempty"`
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime,omitempty"` // на диске сервера, UnixNano: только «изменился ли»
	Deleted bool   `json:"del,omitempty"`
	By      string `json:"by,omitempty"`   // кто изменил
	Time    int64  `json:"time,omitempty"` // когда, по часам сервера (unix)
}

// Version — старая версия или удалённый файл (корзина).
type Version struct {
	Path    string `json:"path"`
	Ver     int64  `json:"ver"`
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	Saved   int64  `json:"saved"` // когда убрано в версии (unix, часы сервера)
	By      string `json:"by,omitempty"`
	Deleted bool   `json:"deleted,omitempty"` // файл удалили (корзина), а не заменили
	Blob    string `json:"blob"`
}

// Conflict — копия «файл (конфликт, устройство, дата)».
type Conflict struct {
	Path   string `json:"path"`
	Orig   string `json:"orig"`
	Device string `json:"device"`
	Time   int64  `json:"time"`
}

type hindex struct {
	Epoch     string               `json:"epoch"`
	Seq       int64                `json:"seq"`
	Files     map[string]*FileInfo `json:"files"`
	Versions  []Version            `json:"versions"`
	Conflicts map[string]Conflict  `json:"conflicts"`
}

// jrec — строка журнала: одно изменение индекса.
type jrec struct {
	P      string    `json:"p,omitempty"`
	F      *FileInfo `json:"f,omitempty"`
	AddVer *Version  `json:"av,omitempty"`
	DelVer string    `json:"dv,omitempty"`
	Conf   *Conflict `json:"c,omitempty"`
	Unconf string    `json:"uc,omitempty"`
}

type hfolder struct {
	HubFolder
	dir string // данные сервера для папки
	ig  *Ignore

	mu      sync.Mutex // всё изменение папки — под ним
	ix      *hindex
	journal *os.File
	jlines  int
}

// Keep — сколько хранятся версии и корзина.
const Keep = 30 * 24 * time.Hour

// OpenHub открывает данные сервера в dir (hub.json и папка на каждую
// раздаваемую папку).
func OpenHub(dir, host string) (*Hub, error) {
	h := &Hub{dir: dir, now: time.Now, keep: Keep, host: host,
		folders: map[string]*hfolder{}, wake: make(chan struct{})}
	var cfg []HubFolder
	if err := readJSON(filepath.Join(dir, "hub.json"), &cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, c := range cfg {
		f, err := h.openFolder(c)
		if err != nil {
			return nil, fmt.Errorf("папка %s: %w", c.Name, err)
		}
		h.folders[c.ID] = f
	}
	return h, nil
}

// Close сбрасывает журналы в снимки.
func (h *Hub) Close() {
	h.mu.Lock()
	fs := h.list()
	h.mu.Unlock()
	for _, f := range fs {
		f.mu.Lock()
		_ = f.compact()
		if f.journal != nil {
			f.journal.Close()
			f.journal = nil
		}
		f.mu.Unlock()
	}
}

func (h *Hub) list() []*hfolder {
	out := make([]*hfolder, 0, len(h.folders))
	for _, f := range h.folders {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h *Hub) saveCfg() error {
	cfg := []HubFolder{}
	for _, f := range h.list() {
		cfg = append(cfg, f.HubFolder)
	}
	return writeJSONAtomic(filepath.Join(h.dir, "hub.json"), cfg)
}

// Folders — раздаваемые папки.
func (h *Hub) Folders() []HubFolder {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []HubFolder{}
	for _, f := range h.list() {
		out = append(out, f.HubFolder)
	}
	return out
}

func (h *Hub) folder(id string) (*hfolder, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.folders[id]
	if !ok {
		return nil, errors.New("сервер больше не раздаёт эту папку")
	}
	return f, nil
}

// Changed будит долгие опросы: изменился доступ (Visible) — пусть
// ожидающие запросы перепроверят его сразу.
func (h *Hub) Changed() { h.changed() }

// changed будит долгие опросы.
func (h *Hub) changed() {
	h.mu.Lock()
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

func (h *Hub) waitCh() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wake
}

// AddFolder — раздавать папку path. Вложенные друг в друга папки нельзя:
// один файл — один сервер.
func (h *Hub) AddFolder(p, name string) (HubFolder, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return HubFolder{}, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return HubFolder{}, errors.New("папки нет")
	}
	h.mu.Lock()
	for _, f := range h.folders {
		if nested(f.Path, abs) {
			h.mu.Unlock()
			return HubFolder{}, fmt.Errorf("пересекается с уже раздаваемой папкой «%s»", f.Name)
		}
	}
	h.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(abs)
	}
	c := HubFolder{ID: randHex(5), Name: strings.TrimSpace(name), Path: abs}
	f, err := h.openFolder(c)
	if err != nil {
		return HubFolder{}, err
	}
	h.mu.Lock()
	h.folders[c.ID] = f
	err = h.saveCfg()
	h.mu.Unlock()
	if err != nil {
		return HubFolder{}, err
	}
	if err := h.Scan(c.ID); err != nil {
		return c, err
	}
	return c, nil
}

// RemoveFolder — перестать раздавать. Сама папка не трогается; версии и
// корзина остаются ещё Keep в removed-<id>.
func (h *Hub) RemoveFolder(id string) error {
	h.mu.Lock()
	f, ok := h.folders[id]
	if !ok {
		h.mu.Unlock()
		return nil
	}
	delete(h.folders, id)
	err := h.saveCfg()
	h.mu.Unlock()
	f.mu.Lock()
	_ = f.compact()
	if f.journal != nil {
		f.journal.Close()
		f.journal = nil
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	h.changed()
	return os.Rename(f.dir, filepath.Join(h.dir, "removed-"+id))
}

// SetIgnore — свои исключения папки.
func (h *Hub) SetIgnore(id string, pats []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.folders[id]
	if !ok {
		return errors.New("нет такой папки")
	}
	f.mu.Lock()
	f.Ignore = pats
	f.ig = NewIgnore(pats)
	f.mu.Unlock()
	return h.saveCfg()
}

func nested(a, b string) bool {
	a, b = strings.ToLower(filepath.Clean(a)), strings.ToLower(filepath.Clean(b))
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

func (h *Hub) openFolder(c HubFolder) (*hfolder, error) {
	f := &hfolder{HubFolder: c, dir: filepath.Join(h.dir, c.ID), ig: NewIgnore(c.Ignore)}
	if err := os.MkdirAll(filepath.Join(f.dir, "versions"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "incoming"), 0o700); err != nil {
		return nil, err
	}
	ix := &hindex{}
	if err := readJSON(filepath.Join(f.dir, "index.json"), ix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if ix.Epoch == "" {
		ix.Epoch = randHex(8)
	}
	if ix.Files == nil {
		ix.Files = map[string]*FileInfo{}
	}
	if ix.Conflicts == nil {
		ix.Conflicts = map[string]Conflict{}
	}
	f.ix = ix
	// Журнал поверх снимка; недописанную последнюю строку пропускаем.
	if jf, err := os.Open(filepath.Join(f.dir, "journal.jsonl")); err == nil {
		sc := bufio.NewScanner(jf)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var r jrec
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				f.apply(r)
			}
		}
		jf.Close()
	}
	if err := f.compact(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *hfolder) apply(r jrec) {
	ix := f.ix
	if r.F != nil {
		ix.Files[r.P] = r.F
		if r.F.Ver > ix.Seq {
			ix.Seq = r.F.Ver
		}
	}
	if r.AddVer != nil {
		ix.Versions = append(ix.Versions, *r.AddVer)
	}
	if r.DelVer != "" {
		for i, v := range ix.Versions {
			if v.Blob == r.DelVer {
				ix.Versions = append(ix.Versions[:i], ix.Versions[i+1:]...)
				break
			}
		}
	}
	if r.Conf != nil {
		ix.Conflicts[r.Conf.Path] = *r.Conf
	}
	if r.Unconf != "" {
		delete(ix.Conflicts, r.Unconf)
	}
}

// log — изменение в индекс и в журнал (на диск до ответа клиенту).
func (f *hfolder) log(r jrec) error {
	f.apply(r)
	if f.journal == nil {
		j, err := os.OpenFile(filepath.Join(f.dir, "journal.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f.journal = j
	}
	b, _ := json.Marshal(r)
	if _, err := f.journal.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.journal.Sync(); err != nil {
		return err
	}
	f.jlines++
	if f.jlines >= 2000 {
		return f.compact()
	}
	return nil
}

// compact — снимок индекса, журнал с нуля.
func (f *hfolder) compact() error {
	if err := writeJSONAtomic(filepath.Join(f.dir, "index.json"), f.ix); err != nil {
		return err
	}
	if f.journal != nil {
		f.journal.Close()
		f.journal = nil
	}
	f.jlines = 0
	err := os.Remove(filepath.Join(f.dir, "journal.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return err
}

func (f *hfolder) next() int64 { return f.ix.Seq + 1 }

// setFile — новое состояние файла с новым номером.
func (f *hfolder) setFile(h *Hub, rel string, fi FileInfo) error {
	fi.Ver = f.next()
	fi.Time = h.now().Unix()
	return f.log(jrec{P: rel, F: &fi})
}

// refresh сверяет один файл с диском: правку прямо на сервере (мимо
// программы) регистрирует как новую версию до того, как что-то решать.
func (h *Hub) refresh(f *hfolder, rel string) error {
	p := full(f.Path, rel)
	rec := f.ix.Files[rel]
	if isDir(rel) {
		fi, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		has := err == nil && fi.IsDir()
		switch {
		case has && (rec == nil || rec.Deleted):
			return f.setFile(h, rel, FileInfo{Hash: DirHash, By: h.host})
		case !has && rec != nil && !rec.Deleted:
			return f.setFile(h, rel, FileInfo{Deleted: true, By: h.host})
		}
		return nil
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if rec != nil && !rec.Deleted {
			return f.setFile(h, rel, FileInfo{Deleted: true, By: h.host})
		}
		return nil
	}
	st := statOf(fi)
	if rec != nil && !rec.Deleted && rec.Size == st.Size && rec.MTime == st.MTime && !fresh(st) {
		return nil
	}
	hash, size, err := hashFile(p)
	if err != nil {
		return err
	}
	if rec != nil && !rec.Deleted && rec.Hash == hash {
		if rec.Size == size && rec.MTime == st.MTime {
			return nil
		}
		// Тот же файл, только время другое — номер не меняем.
		n := *rec
		n.Size, n.MTime = size, st.MTime
		return f.log(jrec{P: rel, F: &n})
	}
	return f.setFile(h, rel, FileInfo{Hash: hash, Size: size, MTime: st.MTime, By: h.host})
}

// Scan — полная сверка папки с диском (новые, изменённые и удалённые прямо
// на сервере файлы).
func (h *Hub) Scan(id string) error {
	f, err := h.folder(id)
	if err != nil {
		return err
	}
	f.mu.Lock()
	seq := f.ix.Seq
	var disk map[string]stat
	err = h.trackLocked(f, func() error {
		var e error
		disk, e = scanDir(f.Path, f.ig)
		if e != nil {
			return e
		}
		return h.scanLocked(f, disk)
	})
	grew := f.ix.Seq != seq
	f.mu.Unlock()
	if grew {
		h.changed()
	}
	return err
}

func (h *Hub) scanLocked(f *hfolder, disk map[string]stat) error {
	var err error
	{
		paths := map[string]bool{}
		for p := range disk {
			paths[p] = true
		}
		for p, r := range f.ix.Files {
			if !r.Deleted && !f.ig.Match(p, false) {
				paths[p] = true
			}
		}
		for p := range paths {
			if e := h.refresh(f, p); e != nil && err == nil {
				err = e
			}
		}
	}
	return err
}

// IndexReply — изменения папки после since.
type IndexReply struct {
	Epoch     string              `json:"epoch"`
	Seq       int64               `json:"seq"`
	Files     map[string]FileInfo `json:"files"`
	Conflicts []Conflict          `json:"conflicts"`
	Ignore    []string            `json:"ignore"`
	Dirs      bool                `json:"dirs,omitempty"` // сервер понимает записи пустых папок
}

// Index — файлы с Ver > since. wait > 0 — ждать изменений до wait (долгий
// опрос), done — отмена ожидания.
func (h *Hub) Index(id string, since int64, wait time.Duration, done <-chan struct{}) (IndexReply, error) {
	deadline := time.Now().Add(wait)
	for {
		ch := h.waitCh()
		f, err := h.folder(id)
		if err != nil {
			return IndexReply{}, err
		}
		f.mu.Lock()
		if f.ix.Seq > since || wait <= 0 || !time.Now().Before(deadline) {
			r := IndexReply{Epoch: f.ix.Epoch, Seq: f.ix.Seq, Files: map[string]FileInfo{},
				Conflicts: []Conflict{}, Ignore: append([]string{}, f.Ignore...), Dirs: true}
			for p, fi := range f.ix.Files {
				if fi.Ver > since {
					r.Files[p] = *fi
				}
			}
			for _, c := range f.ix.Conflicts {
				r.Conflicts = append(r.Conflicts, c)
			}
			sort.Slice(r.Conflicts, func(i, j int) bool { return r.Conflicts[i].Path < r.Conflicts[j].Path })
			f.mu.Unlock()
			return r, nil
		}
		f.mu.Unlock()
		t := time.NewTimer(time.Until(deadline))
		select {
		case <-ch:
		case <-t.C:
		case <-done:
			t.Stop()
			return IndexReply{}, errors.New("отменено")
		}
		t.Stop()
	}
}

// Open — текущее содержимое rel, если его хеш = hash (иначе файл уже
// другой: клиент перечитает индекс).
func (h *Hub) Open(id, rel, hash string) (*os.File, error) {
	f, err := h.folder(id)
	if err != nil {
		return nil, err
	}
	rel, ok := cleanRel(rel)
	if !ok {
		return nil, errors.New("плохой путь")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := h.trackLocked(f, func() error { return h.refresh(f, rel) }); err != nil {
		return nil, err
	}
	cur := f.ix.Files[rel]
	if cur == nil || cur.Deleted || (hash != "" && cur.Hash != hash) || isDir(rel) {
		return nil, errChanged
	}
	return os.Open(full(f.Path, rel))
}

// OpenVersion — содержимое старой версии (blob) для просмотра.
func (h *Hub) OpenVersion(id, blob string) (*os.File, error) {
	f, err := h.folder(id)
	if err != nil {
		return nil, err
	}
	if blob == "" || strings.ContainsAny(blob, `/\.`) {
		return nil, errors.New("плохая версия")
	}
	return os.Open(filepath.Join(f.dir, "versions", blob))
}

var errChanged = errors.New("файл на сервере уже другой")

// partPath — куда копится загрузка (dev, rel, hash) — у каждой своё место,
// докачка продолжает с конца.
func (f *hfolder) partPath(dev, rel, hash string) string {
	return filepath.Join(f.dir, "incoming", hashString(dev + "\x00" + rel + "\x00" + hash)[:24]+".part")
}

// PartSize — сколько уже загружено.
func (h *Hub) PartSize(id, dev, rel, hash string) (int64, error) {
	f, err := h.folder(id)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(f.partPath(dev, rel, hash))
	if err != nil {
		return 0, nil
	}
	return st.Size(), nil
}

// WritePart дописывает кусок загрузки с места off (докачка). Место не
// совпало — ошибка и сколько есть.
func (h *Hub) WritePart(id, dev, rel, hash string, off int64, r io.Reader) (int64, error) {
	f, err := h.folder(id)
	if err != nil {
		return 0, err
	}
	if _, ok := cleanRel(rel); !ok || len(hash) != 64 {
		return 0, errors.New("плохой путь или хеш")
	}
	f.mu.Lock()
	left, why := h.roomLocked(f)
	f.mu.Unlock()
	if left >= 0 {
		r = &quotaReader{r: r, left: left, why: why}
	}
	p := f.partPath(dev, rel, hash)
	pf, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer pf.Close()
	st, err := pf.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() != off {
		return st.Size(), errOffset
	}
	if _, err := pf.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	n, err := io.Copy(pf, r)
	if serr := pf.Sync(); err == nil {
		err = serr
	}
	if n > 0 && h.OnUsage != nil {
		h.OnUsage(f.ID, n)
	}
	return off + n, err
}

// CheckRoom — влезет ли загрузка n байт в папку id (n <= 0 — неизвестно).
func (h *Hub) CheckRoom(id string, n int64) error {
	f, err := h.folder(id)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return h.needLocked(f, n)
}

var errOffset = errors.New("не то место докачки")

// CommitReply — итог записи или удаления.
type CommitReply struct {
	Ver      int64  `json:"ver"`
	Conflict bool   `json:"conflict,omitempty"`
	Path     string `json:"path,omitempty"` // куда легла копия при конфликте
	Need     bool   `json:"need,omitempty"` // содержимого у сервера нет — загрузи
}

// Commit — сделать загруженное (или уже имеющееся у сервера с тем же
// хешем) содержимым rel. base — версия, от которой клиент правил (0 —
// файла не было).
func (h *Hub) Commit(id, dev, rel, hash string, base int64) (CommitReply, error) {
	return h.CommitSized(id, dev, rel, hash, base, -1)
}

// CommitSized — Commit, когда устройство назвало размер содержимого (size
// >= 0): недогруженный кусок (короче size) тогда не считается испорченным —
// ответ Need, и устройство докачивает с места, где оборвалось.
func (h *Hub) CommitSized(id, dev, rel, hash string, base, size int64) (CommitReply, error) {
	f, err := h.folder(id)
	if err != nil {
		return CommitReply{}, err
	}
	rel, ok := cleanRel(rel)
	if !ok || len(hash) != 64 {
		return CommitReply{}, errors.New("плохой путь или хеш")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var reply CommitReply
	err = h.trackLocked(f, func() error {
		var e error
		reply, e = h.commitLocked(f, dev, rel, hash, base, size)
		return e
	})
	return reply, err
}

func (h *Hub) commitLocked(f *hfolder, dev, rel, hash string, base, size int64) (CommitReply, error) {
	if f.ig.Match(rel, false) {
		return CommitReply{}, errors.New("файл исключён из синхронизации")
	}
	if err := h.refresh(f, rel); err != nil {
		return CommitReply{}, err
	}
	cur := f.ix.Files[rel]
	if cur != nil && !cur.Deleted && cur.Hash == hash {
		return CommitReply{Ver: cur.Ver}, nil // уже так (повтор после обрыва ответа)
	}
	if isDir(rel) != (hash == DirHash) {
		return CommitReply{}, errors.New("плохой путь или хеш")
	}
	if isDir(rel) {
		// Папка: содержимого нет — просто создать.
		if cur != nil && !cur.Deleted {
			return CommitReply{}, errors.New("на месте папки файл")
		}
		if err := os.MkdirAll(full(f.Path, rel), 0o755); err != nil {
			return CommitReply{}, err
		}
		if err := f.setFile(h, rel, FileInfo{Hash: DirHash, By: dev}); err != nil {
			return CommitReply{}, err
		}
		go h.changed()
		return CommitReply{Ver: f.ix.Files[rel].Ver}, nil
	}
	conflict := cur != nil && !cur.Deleted && cur.Ver != base
	if conflict {
		// Та же правка того же устройства уже лежит копией — вторую не плодим.
		for _, c := range f.ix.Conflicts {
			if c.Orig == rel && c.Device == dev {
				if cf := f.ix.Files[c.Path]; cf != nil && !cf.Deleted && cf.Hash == hash {
					return CommitReply{Ver: cur.Ver, Conflict: true, Path: c.Path}, nil
				}
			}
		}
	}

	// Откуда взять содержимое: загрузка или такой же файл на сервере
	// (переименование и копия не передаются заново).
	part := f.partPath(dev, rel, hash)
	src, fromPart := "", false
	var cost int64 // сколько прибавится места: копия, а не перенос загрузки
	if st, err := os.Stat(part); err == nil && size >= 0 && st.Size() < size {
		return CommitReply{Need: true}, nil // загрузка не дошла до конца — докачать
	} else if err == nil {
		got, _, err := hashFile(part)
		if err != nil {
			return CommitReply{}, err
		}
		if got != hash {
			os.Remove(part)
			return CommitReply{}, errors.New("загруженное не совпало с хешем, загрузка заново")
		}
		src, fromPart = part, true
	} else {
		for p, fi := range f.ix.Files {
			if !fi.Deleted && fi.Hash == hash {
				src, cost = full(f.Path, p), fi.Size
				break
			}
		}
		if src == "" {
			for _, v := range f.ix.Versions {
				if v.Hash == hash {
					src, cost = filepath.Join(f.dir, "versions", v.Blob), v.Size
					break
				}
			}
		}
		if src == "" {
			return CommitReply{Need: true}, nil
		}
		if err := h.needLocked(f, cost); err != nil {
			return CommitReply{}, err
		}
	}

	target := rel
	if conflict {
		target = h.conflictName(f, rel, dev)
	}
	tmp, err := h.stage(f, target, src, fromPart, hash)
	if err != nil {
		return CommitReply{}, err
	}
	if !conflict && cur != nil && !cur.Deleted {
		// Старое — в версии до замены. При конфликте имя копии свободно.
		if err := h.archive(f, rel, cur, dev, false); err != nil {
			os.Remove(tmp)
			return CommitReply{}, err
		}
	}
	dst := full(f.Path, target)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return CommitReply{}, err
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		return CommitReply{}, err
	}
	st := statOf(fi)
	if err := f.setFile(h, target, FileInfo{Hash: hash, Size: st.Size, MTime: st.MTime, By: dev}); err != nil {
		return CommitReply{}, err
	}
	var reply CommitReply
	if conflict {
		c := Conflict{Path: target, Orig: rel, Device: dev, Time: h.now().Unix()}
		if err := f.log(jrec{Conf: &c}); err != nil {
			return CommitReply{}, err
		}
		reply = CommitReply{Ver: cur.Ver, Conflict: true, Path: target}
		if h.OnEvent != nil {
			go h.OnEvent("conflict", fmt.Sprintf("Конфликт в «%s»: %s изменён на %s и ещё где-то — обе версии сохранены", f.Name, rel, dev))
		}
	} else {
		reply = CommitReply{Ver: f.ix.Files[rel].Ver}
	}
	go h.changed()
	return reply, nil
}

// stage кладёт содержимое во временный файл рядом с целью и проверяет хеш.
func (h *Hub) stage(f *hfolder, target, src string, move bool, hash string) (string, error) {
	dst := full(f.Path, target)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(filepath.Dir(dst), tmpPrefix+randHex(6)+".tmp")
	if move && os.Rename(src, tmp) == nil {
		return tmp, nil // загрузка уже проверена по хешу
	}
	got, err := copyFile(src, tmp)
	if err != nil {
		return "", err
	}
	if got != hash {
		os.Remove(tmp)
		return "", errBusy
	}
	if move {
		os.Remove(src)
	}
	return tmp, nil
}

// archive убирает текущее содержимое rel в версии (жёсткая ссылка, иначе
// копия) — до замены или удаления.
func (h *Hub) archive(f *hfolder, rel string, cur *FileInfo, dev string, deleted bool) error {
	blob := fmt.Sprintf("%d-%s", cur.Ver, cur.Hash[:16])
	dst := filepath.Join(f.dir, "versions", blob)
	src := full(f.Path, rel)
	if _, err := os.Stat(dst); err != nil {
		if err := os.Link(src, dst); err != nil {
			got, err := copyFile(src, dst)
			if err != nil {
				return err
			}
			if got != cur.Hash {
				// Файл поменяли прямо сейчас мимо программы — отложить.
				os.Remove(dst)
				return errBusy
			}
		}
	}
	v := Version{Path: rel, Ver: cur.Ver, Hash: cur.Hash, Size: cur.Size, Saved: h.now().Unix(),
		By: dev, Deleted: deleted, Blob: blob}
	return f.log(jrec{AddVer: &v})
}

// conflictName — «имя (конфликт, устройство, 03.11.2026).ext», свободное.
func (h *Hub) conflictName(f *hfolder, rel, dev string) string {
	return freeConflictName(rel, dev, h.now(), func(p string) bool {
		if fi := f.ix.Files[p]; fi != nil && !fi.Deleted {
			return true
		}
		_, err := os.Lstat(full(f.Path, p))
		return err == nil
	})
}

func freeConflictName(rel, dev string, now time.Time, taken func(string) bool) string {
	dir, base := path.Split(rel)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" { // «.gitignore»
		stem, ext = base, ""
	}
	dev = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, dev)
	if dev == "" {
		dev = "?"
	}
	day := now.Format("02.01.2006")
	for i := 1; ; i++ {
		n := fmt.Sprintf("%s (конфликт, %s, %s)%s", stem, dev, day, ext)
		if i > 1 {
			n = fmt.Sprintf("%s (конфликт, %s, %s, %d)%s", stem, dev, day, i, ext)
		}
		if !taken(dir + n) {
			return dir + n
		}
	}
}

// Delete — удалить rel, если клиент удалял именно текущую версию base.
// Иначе файл уже изменили — конфликт, ничего не удаляется.
func (h *Hub) Delete(id, dev, rel string, base int64) (CommitReply, error) {
	f, err := h.folder(id)
	if err != nil {
		return CommitReply{}, err
	}
	rel, ok := cleanRel(rel)
	if !ok {
		return CommitReply{}, errors.New("плохой путь")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var reply CommitReply
	err = h.trackLocked(f, func() error {
		var e error
		reply, e = h.deleteLocked(f, dev, rel, base)
		return e
	})
	return reply, err
}

func (h *Hub) deleteLocked(f *hfolder, dev, rel string, base int64) (CommitReply, error) {
	if err := h.refresh(f, rel); err != nil {
		return CommitReply{}, err
	}
	cur := f.ix.Files[rel]
	if cur == nil {
		return CommitReply{}, nil
	}
	if cur.Deleted {
		return CommitReply{Ver: cur.Ver}, nil
	}
	if cur.Ver != base {
		return CommitReply{Ver: cur.Ver, Conflict: true}, nil
	}
	if err := h.remove(f, rel, cur, dev); err != nil {
		if errors.Is(err, errDirBusy) {
			return CommitReply{Ver: cur.Ver, Conflict: true}, nil
		}
		return CommitReply{}, err
	}
	go h.changed()
	return CommitReply{Ver: f.ix.Files[rel].Ver}, nil
}

// errDirBusy — в папке ещё что-то есть, удалять её нельзя.
var errDirBusy = errors.New("папка не пустая")

// remove — в корзину, потом с диска, потом в индекс.
func (h *Hub) remove(f *hfolder, rel string, cur *FileInfo, dev string) error {
	if isDir(rel) {
		// Папку — только пустую: остальное в ней ещё живо.
		if err := os.Remove(full(f.Path, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errDirBusy
		}
		return f.setFile(h, rel, FileInfo{Deleted: true, By: dev})
	}
	if err := h.archive(f, rel, cur, dev, true); err != nil {
		return err
	}
	if err := os.Remove(full(f.Path, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := f.setFile(h, rel, FileInfo{Deleted: true, By: dev}); err != nil {
		return err
	}
	if _, ok := f.ix.Conflicts[rel]; ok {
		return f.log(jrec{Unconf: rel})
	}
	return nil
}

// Conflicts — текущие конфликты папки.
func (h *Hub) Conflicts(id string) ([]Conflict, error) {
	r, err := h.Index(id, 1<<62, 0, nil)
	return r.Conflicts, err
}

// Resolve — решить конфликт cpath: keep = "orig" (оставить основной файл,
// копию в корзину), "copy" (копию сделать основным, прежний — в версии),
// "both" (оставить оба, убрать из списка).
func (h *Hub) Resolve(id, dev, cpath, keep string) error {
	f, err := h.folder(id)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return h.trackLocked(f, func() error { return h.resolveLocked(f, dev, cpath, keep) })
}

func (h *Hub) resolveLocked(f *hfolder, dev, cpath, keep string) error {
	c, ok := f.ix.Conflicts[cpath]
	if !ok {
		return errors.New("конфликт уже решён")
	}
	if err := h.refresh(f, cpath); err != nil {
		return err
	}
	if err := h.refresh(f, c.Orig); err != nil {
		return err
	}
	cf := f.ix.Files[cpath]
	switch keep {
	case "both":
	case "orig":
		if cf != nil && !cf.Deleted {
			if err := h.remove(f, cpath, cf, dev); err != nil {
				return err
			}
		}
	case "copy":
		if cf == nil || cf.Deleted {
			return errors.New("копии конфликта уже нет")
		}
		if err := h.needLocked(f, cf.Size); err != nil {
			return err
		}
		tmp, err := h.stage(f, c.Orig, full(f.Path, cpath), false, cf.Hash)
		if err != nil {
			return err
		}
		if cur := f.ix.Files[c.Orig]; cur != nil && !cur.Deleted {
			if err := h.archive(f, c.Orig, cur, dev, false); err != nil {
				os.Remove(tmp)
				return err
			}
		}
		dst := full(f.Path, c.Orig)
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return err
		}
		fi, err := os.Lstat(dst)
		if err != nil {
			return err
		}
		st := statOf(fi)
		if err := f.setFile(h, c.Orig, FileInfo{Hash: cf.Hash, Size: st.Size, MTime: st.MTime, By: dev}); err != nil {
			return err
		}
		if err := h.remove(f, cpath, cf, dev); err != nil {
			return err
		}
	default:
		return errors.New("keep: orig, copy или both")
	}
	if _, ok := f.ix.Conflicts[cpath]; ok {
		if err := f.log(jrec{Unconf: cpath}); err != nil {
			return err
		}
	}
	go h.changed()
	return nil
}

// Versions — старые версии и корзина (rel == "" — всей папки), новые сверху.
func (h *Hub) Versions(id, rel string) ([]Version, error) {
	f, err := h.folder(id)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Version{}
	for _, v := range f.ix.Versions {
		if rel == "" || v.Path == rel {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Saved > out[j].Saved || (out[i].Saved == out[j].Saved && out[i].Ver > out[j].Ver)
	})
	return out, nil
}

// Restore — вернуть версию blob на её место; текущее содержимое при этом
// само уходит в версии, так что восстановление тоже можно отменить.
func (h *Hub) Restore(id, dev, blob string) (string, error) {
	f, err := h.folder(id)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var rel string
	err = h.trackLocked(f, func() error {
		var e error
		rel, e = h.restoreLocked(f, dev, blob)
		return e
	})
	return rel, err
}

func (h *Hub) restoreLocked(f *hfolder, dev, blob string) (string, error) {
	var v *Version
	for i := range f.ix.Versions {
		if f.ix.Versions[i].Blob == blob {
			v = &f.ix.Versions[i]
			break
		}
	}
	if v == nil {
		return "", errors.New("такой версии уже нет")
	}
	rel := v.Path
	if err := h.refresh(f, rel); err != nil {
		return "", err
	}
	if err := h.needLocked(f, v.Size); err != nil {
		return "", err
	}
	tmp, err := h.stage(f, rel, filepath.Join(f.dir, "versions", v.Blob), false, v.Hash)
	if err != nil {
		return "", err
	}
	if cur := f.ix.Files[rel]; cur != nil && !cur.Deleted {
		if cur.Hash == v.Hash {
			os.Remove(tmp)
			return rel, nil
		}
		if err := h.archive(f, rel, cur, dev, false); err != nil {
			os.Remove(tmp)
			return "", err
		}
	}
	dst := full(f.Path, rel)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		return "", err
	}
	st := statOf(fi)
	if err := f.setFile(h, rel, FileInfo{Hash: v.Hash, Size: st.Size, MTime: st.MTime, By: dev}); err != nil {
		return "", err
	}
	go h.changed()
	return rel, nil
}

// Prune — версии и корзина старше Keep, брошенные загрузки (2 дня),
// данные снятых с раздачи папок.
func (h *Hub) Prune() {
	now := h.now()
	h.mu.Lock()
	fs := h.list()
	h.mu.Unlock()
	for _, f := range fs {
		f.mu.Lock()
		h.trackLocked(f, func() error { h.pruneLocked(f, now); return nil })
		f.mu.Unlock()
	}
	h.pruneRemoved(now)
}

func (h *Hub) pruneLocked(f *hfolder, now time.Time) {
	{
		var drop []Version
		for _, v := range f.ix.Versions {
			if now.Sub(time.Unix(v.Saved, 0)) > h.keep {
				drop = append(drop, v)
			}
		}
		for _, v := range drop {
			if f.log(jrec{DelVer: v.Blob}) == nil {
				still := false
				for _, o := range f.ix.Versions {
					if o.Blob == v.Blob {
						still = true
					}
				}
				if !still {
					os.Remove(filepath.Join(f.dir, "versions", v.Blob))
				}
			}
		}
		if es, err := os.ReadDir(filepath.Join(f.dir, "incoming")); err == nil {
			for _, e := range es {
				if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > 48*time.Hour {
					os.Remove(filepath.Join(f.dir, "incoming", e.Name()))
				}
			}
		}
	}
}

func (h *Hub) pruneRemoved(now time.Time) {
	if es, err := os.ReadDir(h.dir); err == nil {
		for _, e := range es {
			if strings.HasPrefix(e.Name(), "removed-") {
				if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > h.keep {
					os.RemoveAll(filepath.Join(h.dir, e.Name()))
				}
			}
		}
	}
}
