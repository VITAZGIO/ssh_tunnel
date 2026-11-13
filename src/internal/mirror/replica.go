package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Replica — папка на устройстве-клиенте. Сверка трёх состояний каждого
// файла: что на диске (L), с какой версией сервера он совпадал при прошлой
// сверке (S, Synced) и что сейчас на сервере (R, Remote).
//
// Правила, на которых держится «ни один байт не потерян»:
//   - отправка — всегда с номером версии, от которой правили; сервер при
//     несовпадении кладёт присланное рядом копией, а не поверх;
//   - локальный файл заменяется или удаляется, только если его содержимое
//     есть на сервере (хеш = S.Hash, а S.Hash всегда подтверждён сервером);
//     замена идёт через отодвинутую копию, и если файл успели изменить в
//     этот миг — он становится «конфликт»-копией, а не пропадает;
//   - время нигде не сравнивается на «раньше/позже», только на равенство:
//     сбитые часы клиента ни на что не влияют;
//   - очередь изменений — это разница диска и S: она переживает выключение,
//     перезапуск и сутки без сервера.
type Replica struct {
	Folder string // id папки на сервере
	Path   string // где лежит на этом устройстве
	Mode   string // ModeBoth, ModeRecv, ModeSend
	Device string // имя этого устройства (для локальных конфликт-копий)
	State  string // файл состояния
	R      *Remote
	// OnEvent — заметное для человека: новый конфликт и т. п.
	OnEvent func(kind, text string)

	now func() time.Time

	mu     sync.Mutex
	st     repState
	loaded bool
	gone   bool // папку отключили
	cache  map[string]cached
	// status — под своим замком smu: Sync держит mu весь проход (с долгим
	// ожиданием сервера), и Status ждал его — страница папок висела пустой.
	smu    sync.Mutex
	status Status
}

// Режимы папки.
const (
	ModeBoth = "both" // отправлять и получать
	ModeRecv = "recv" // только получать
	ModeSend = "send" // только отправлять (бэкап: удаления не отправляются)
)

type repFile struct {
	Ver   int64  `json:"v"`
	Hash  string `json:"h,omitempty"` // "" — файла нет
	Size  int64  `json:"s,omitempty"`
	MTime int64  `json:"m,omitempty"`
}

type remFile struct {
	Ver  int64  `json:"v"`
	Hash string `json:"h,omitempty"`
	Size int64  `json:"s,omitempty"`
	Del  bool   `json:"d,omitempty"`
}

type repState struct {
	Epoch     string             `json:"epoch"`
	Cursor    int64              `json:"cursor"`
	Synced    map[string]repFile `json:"synced"`
	Remote    map[string]remFile `json:"remote"`
	Conflicts []Conflict         `json:"conflicts"`
	LastOK    int64              `json:"lastOk,omitempty"`
}

type cached struct {
	st   stat
	hash string
}

// Status — что показывать у папки.
type Status struct {
	State     string     `json:"state"` // ok, pending, conflict, offline, error
	Pending   int        `json:"pending"`
	Conflicts []Conflict `json:"conflicts"`
	Files     int        `json:"files"`
	Error     string     `json:"error,omitempty"`
	LastOK    int64      `json:"lastOk,omitempty"`
}

func (r *Replica) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Replica) event(kind, text string) {
	if r.OnEvent != nil {
		r.OnEvent(kind, text)
	}
}

// Status — состояние после последней сверки.
func (r *Replica) Status() Status {
	r.smu.Lock()
	defer r.smu.Unlock()
	s := r.status
	if s.State == "" {
		s.State = "pending"
	}
	if s.Conflicts == nil {
		s.Conflicts = []Conflict{}
	}
	return s
}

func (r *Replica) setStatus(s Status) {
	r.smu.Lock()
	r.status = s
	r.smu.Unlock()
}

func (r *Replica) load() {
	if r.loaded {
		return
	}
	if err := readJSON(r.State, &r.st); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Битое состояние — начинаем с нуля: сверка по хешам ничего не
		// потеряет, в худшем случае появятся конфликт-копии.
		r.st = repState{}
	}
	if r.st.Synced == nil {
		r.st.Synced = map[string]repFile{}
	}
	if r.st.Remote == nil {
		r.st.Remote = map[string]remFile{}
	}
	r.cache = map[string]cached{}
	r.loaded = true
}

func (r *Replica) save() error { return writeJSONAtomic(r.State, &r.st) }

// Sync — один проход сверки. wait > 0 — сначала подождать изменений на
// сервере (долгий опрос). full — перечитать хеши всех файлов, не веря
// размеру и времени. Возвращает, было ли что делать.
func (r *Replica) Sync(ctx context.Context, wait time.Duration, full bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone {
		return false, context.Canceled
	}
	r.load()
	if st, err := os.Stat(r.Path); err != nil || !st.IsDir() {
		st := Status{State: "error", Error: "папки " + r.Path + " нет", LastOK: r.st.LastOK}
		r.setStatus(st)
		return false, errors.New(st.Error)
	}
	r.recover()

	ictx, cancel := context.WithTimeout(ctx, wait+30*time.Second)
	rep, err := r.R.Index(ictx, r.Folder, r.st.Cursor, wait)
	cancel()
	if err == nil && rep.Epoch != r.st.Epoch {
		if r.st.Epoch != "" {
			// Сервер начал индекс заново (переустановка, потеря данных):
			// всё, что знали о его номерах, недействительно — сверяем по
			// хешам с нуля.
			for p, s := range r.st.Synced {
				s.Ver = -1
				r.st.Synced[p] = s
			}
		}
		r.st.Remote = map[string]remFile{}
		r.st.Cursor = 0
		r.st.Epoch = rep.Epoch
		ictx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		rep, err = r.R.Index(ictx, r.Folder, 0, 0)
		cancel()
	}
	ig := NewIgnore(nil)
	if err == nil {
		ig = NewIgnore(rep.Ignore)
		for p, f := range rep.Files {
			r.st.Remote[p] = remFile{Ver: f.Ver, Hash: f.Hash, Size: f.Size, Del: f.Deleted}
		}
		if rep.Seq > r.st.Cursor {
			r.st.Cursor = rep.Seq
		}
		r.noteConflicts(rep.Conflicts)
	}

	disk, serr := scanDir(r.Path, ig)
	if serr != nil {
		r.setStatus(Status{State: "error", Error: serr.Error(), LastOK: r.st.LastOK, Conflicts: r.st.Conflicts})
		return false, serr
	}
	if !rep.Dirs {
		for p := range disk {
			if isDir(p) {
				delete(disk, p)
			}
		}
	}
	local := map[string]string{}
	for p, st := range disk {
		h, herr := r.localHash(p, st, full)
		if herr != nil {
			continue // нечитаемый сейчас файл — в следующий раз
		}
		local[p] = h
	}

	if err != nil {
		// Сервера нет: считаем, сколько ждёт, и ничего не трогаем.
		n := 0
		for p, h := range local {
			if r.st.Synced[p].Hash != h && r.Mode != ModeRecv {
				n++
			}
		}
		if r.Mode == ModeBoth {
			for p, s := range r.st.Synced {
				if _, ok := local[p]; !ok && s.Hash != "" && !ig.Match(p, false) {
					n++
				}
			}
		}
		_ = r.save()
		r.setStatus(Status{State: "offline", Pending: n, Error: err.Error(), LastOK: r.st.LastOK,
			Conflicts: r.st.Conflicts, Files: countFiles(local)})
		return false, err
	}

	paths := map[string]bool{}
	for p := range local {
		paths[p] = true
	}
	for p := range r.st.Synced {
		paths[p] = true
	}
	for p := range r.st.Remote {
		paths[p] = true
	}
	list := make([]string, 0, len(paths))
	for p := range paths {
		if !ig.Match(p, false) && (rep.Dirs || !isDir(p)) {
			list = append(list, p)
		}
	}
	// Сначала файлы, потом папки — глубокие первыми: удалённая папка к
	// своей очереди уже пустая.
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if isDir(a) != isDir(b) {
			return !isDir(a)
		}
		if isDir(a) {
			return a > b
		}
		return a < b
	})

	did, pending, ops := false, 0, 0
	var firstErr error
	lastSave := time.Now()
	for _, p := range list {
		if ctx.Err() != nil {
			firstErr = ctx.Err()
			break
		}
		act, aerr := r.reconcile(ctx, p, local[p], disk[p])
		if act {
			did = true
			ops++
		}
		if aerr != nil {
			pending++
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p, aerr)
			}
		}
		if ops >= 500 || time.Since(lastSave) > 3*time.Second {
			_ = r.save()
			ops, lastSave = 0, time.Now()
		}
	}
	if firstErr == nil {
		r.st.LastOK = r.clock().Unix()
	}
	if err := r.save(); err != nil && firstErr == nil {
		firstErr = err
	}
	st := Status{State: "ok", Pending: pending, LastOK: r.st.LastOK, Conflicts: r.st.Conflicts, Files: countFiles(local)}
	if firstErr != nil {
		st.Error = firstErr.Error()
	}
	switch {
	case len(r.st.Conflicts) > 0:
		st.State = "conflict"
	case pending > 0:
		st.State = "pending"
	}
	r.setStatus(st)
	return did, firstErr
}

func (r *Replica) noteConflicts(cs []Conflict) {
	old := map[string]bool{}
	for _, c := range r.st.Conflicts {
		old[c.Path] = true
	}
	for _, c := range cs {
		if !old[c.Path] {
			r.event("conflict", fmt.Sprintf("Конфликт: %s (изменён на %s)", c.Orig, c.Device))
		}
	}
	r.st.Conflicts = cs
}

// localHash — хеш файла; без rehash верим размеру и времени прошлой
// сверки — кроме только что изменённых файлов (две правки за один тик часов
// файловой системы дают одинаковое время, как «racy git»).
func (r *Replica) localHash(p string, st stat, rehash bool) (string, error) {
	if isDir(p) {
		return DirHash, nil
	}
	if !rehash && !fresh(st) {
		if s, ok := r.st.Synced[p]; ok && s.Hash != "" && s.Size == st.Size && s.MTime == st.MTime {
			return s.Hash, nil
		}
		if c, ok := r.cache[p]; ok && c.st == st {
			return c.hash, nil
		}
	}
	h, _, err := hashFile(full(r.Path, p))
	if err != nil {
		return "", err
	}
	r.cache[p] = cached{st, h}
	return h, nil
}

// reconcile — одно решение по файлу. lh — хеш на диске ("" — нет файла).
func (r *Replica) reconcile(ctx context.Context, p, lh string, lst stat) (bool, error) {
	s := r.st.Synced[p]
	rm := r.st.Remote[p]
	rh := rm.Hash
	if rm.Del {
		rh = ""
	}
	if lh == rh {
		if lh == "" {
			delete(r.st.Synced, p)
		} else {
			r.st.Synced[p] = repFile{Ver: rm.Ver, Hash: lh, Size: lst.Size, MTime: lst.MTime}
		}
		return false, nil
	}
	localChanged := lh != s.Hash
	remoteChanged := rm.Ver != s.Ver
	canSend, canRecv := r.Mode != ModeRecv, r.Mode != ModeSend
	switch {
	case localChanged && canSend && lh != "":
		return true, r.push(ctx, p, lh, lst, s)
	case localChanged && lh == "" && r.Mode == ModeBoth && !remoteChanged:
		return true, r.pushDelete(ctx, p, s)
	case remoteChanged && canRecv, r.Mode == ModeRecv && lh == "" && rh != "":
		return true, r.pull(ctx, p, lh, s, rm)
	}
	return false, nil
}

// push отправляет файл: сначала «сделай файлом» (у сервера может уже быть
// такое содержимое — переименование), иначе загрузка с докачкой.
func (r *Replica) push(ctx context.Context, p, lh string, lst stat, s repFile) error {
	req := map[string]any{"f": r.Folder, "p": p, "hash": lh, "base": s.Ver, "size": lst.Size}
	var rep CommitReply
	if err := r.R.post(ctx, "/commit", req, &rep); err != nil {
		return err
	}
	if rep.Need {
		if err := r.upload(ctx, p, lh, lst.Size); err != nil {
			return err
		}
		rep = CommitReply{}
		if err := r.R.post(ctx, "/commit", req, &rep); err != nil {
			return err
		}
		if rep.Need {
			return errors.New("сервер не принял загрузку")
		}
	}
	if rep.Conflict {
		// Наше содержимое у сервера рядом копией — значит, оно сохранено,
		// и основной файл можно заменить серверной версией.
		r.st.Synced[p] = repFile{Ver: s.Ver, Hash: lh, Size: lst.Size, MTime: lst.MTime}
		r.event("conflict", fmt.Sprintf("Конфликт: %s изменён и здесь, и на другом устройстве — обе версии сохранены (%s)", p, rep.Path))
		return nil
	}
	r.st.Synced[p] = repFile{Ver: rep.Ver, Hash: lh, Size: lst.Size, MTime: lst.MTime}
	if rep.Ver >= r.st.Remote[p].Ver {
		r.st.Remote[p] = remFile{Ver: rep.Ver, Hash: lh, Size: lst.Size}
	}
	return nil
}

const chunk = 4 << 20

func (r *Replica) upload(ctx context.Context, p, hash string, size int64) error {
	var part struct {
		Have int64 `json:"have"`
	}
	q := url.Values{"f": {r.Folder}, "p": {p}, "hash": {hash}}
	if err := r.R.json(ctx, "GET", "/part", q, nil, &part); err != nil {
		return err
	}
	fh, err := os.Open(full(r.Path, p))
	if err != nil {
		return err
	}
	defer fh.Close()
	have := part.Have
	if have > size {
		have = 0 // чужой хвост: сервер скажет правильное место
	}
	for tries := 0; have < size || (size == 0 && tries == 0); tries++ {
		if tries > 50 {
			return errors.New("загрузка не идёт")
		}
		if _, err := fh.Seek(have, io.SeekStart); err != nil {
			return err
		}
		n := size - have
		if n > chunk {
			n = chunk
		}
		q.Set("off", strconv.FormatInt(have, 10))
		resp, err := r.R.do(ctx, "PUT", "/put", q, io.LimitReader(fh, n), nil)
		if err != nil {
			return err
		}
		var out struct {
			Have  int64  `json:"have"`
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		switch resp.StatusCode {
		case 200, http.StatusConflict:
			have = out.Have
		default:
			return &RemoteError{resp.StatusCode, out.Error}
		}
		if size == 0 {
			break
		}
	}
	return nil
}

func (r *Replica) pushDelete(ctx context.Context, p string, s repFile) error {
	var rep CommitReply
	if err := r.R.post(ctx, "/delete", map[string]any{"f": r.Folder, "p": p, "base": s.Ver}, &rep); err != nil {
		return err
	}
	if rep.Conflict {
		// Файл на сервере уже изменили — правка важнее удаления: вернётся
		// при следующем проходе.
		return nil
	}
	r.st.Synced[p] = repFile{Ver: rep.Ver}
	if rep.Ver >= r.st.Remote[p].Ver {
		r.st.Remote[p] = remFile{Ver: rep.Ver, Del: true}
	}
	return nil
}

// pull приводит файл к серверному. safe — содержимое, которое можно
// заменить (оно есть на сервере).
func (r *Replica) pull(ctx context.Context, p, lh string, s repFile, rm remFile) error {
	dst := full(r.Path, p)
	if isDir(p) {
		if rm.Del {
			if lh != "" {
				if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
					// Тут в папке есть своё — папка нужна, вернуть её на сервер.
					return r.push(ctx, p, lh, stat{}, repFile{Ver: rm.Ver})
				}
			}
			r.st.Synced[p] = repFile{Ver: rm.Ver}
			return nil
		}
		if fi, err := os.Lstat(dst); err == nil && !fi.IsDir() {
			return errors.New("на месте папки файл")
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		r.st.Synced[p] = repFile{Ver: rm.Ver, Hash: DirHash}
		return nil
	}
	safe := s.Hash
	if lh == "" {
		// По сверке файла нет. Появился сейчас или это то же имя в другом
		// регистре (Windows, Android) — не трогаем.
		if _, err := os.Lstat(dst); err == nil {
			h, _, err := hashFile(dst)
			if err == nil && !rm.Del && h == rm.Hash {
				return nil
			}
			return errors.New("на месте файла другой (возможно, имя отличается только регистром)")
		}
	}
	if rm.Del {
		if lh != "" {
			if err := r.replace(p, "", safe, ""); err != nil {
				return err
			}
		}
		r.st.Synced[p] = repFile{Ver: rm.Ver}
		return nil
	}
	tmp, err := r.download(ctx, p, rm)
	if err != nil {
		return err
	}
	if err := r.replace(p, tmp, safe, rm.Hash); err != nil {
		os.Remove(tmp)
		return err
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		return err
	}
	st := statOf(fi)
	r.st.Synced[p] = repFile{Ver: rm.Ver, Hash: rm.Hash, Size: st.Size, MTime: st.MTime}
	r.cache[p] = cached{st, rm.Hash}
	return nil
}

// download качает версию в «.~mirror-<хеш>.part» рядом с файлом
// (докачка — с конца) и проверяет хеш.
func (r *Replica) download(ctx context.Context, p string, rm remFile) (string, error) {
	dst := full(r.Path, p)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	part := filepath.Join(filepath.Dir(dst), tmpPrefix+rm.Hash[:16]+".part")
	fh, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	st, _ := fh.Stat()
	have := st.Size()
	if have > rm.Size {
		have = 0
		_ = fh.Truncate(0)
	}
	hdr := map[string]string{}
	if have > 0 {
		hdr["Range"] = fmt.Sprintf("bytes=%d-", have)
	}
	if have < rm.Size || rm.Size == 0 {
		resp, err := r.R.do(ctx, "GET", "/get", url.Values{"f": {r.Folder}, "p": {p}, "hash": {rm.Hash}}, nil, hdr)
		if err != nil {
			fh.Close()
			return "", err
		}
		switch resp.StatusCode {
		case http.StatusPartialContent:
		case http.StatusOK:
			have = 0
			_ = fh.Truncate(0)
		case http.StatusRequestedRangeNotSatisfiable:
			have = 0
			_ = fh.Truncate(0)
		default:
			resp.Body.Close()
			fh.Close()
			if resp.StatusCode == http.StatusConflict {
				os.Remove(part)
			}
			return "", &RemoteError{resp.StatusCode, "файл на сервере изменился, повтор"}
		}
		if _, err := fh.Seek(have, io.SeekStart); err != nil {
			resp.Body.Close()
			fh.Close()
			return "", err
		}
		_, cerr := io.Copy(fh, resp.Body)
		resp.Body.Close()
		if serr := fh.Sync(); cerr == nil {
			cerr = serr
		}
		if cerr != nil {
			fh.Close()
			return "", cerr // недокачанное остаётся — продолжим с места обрыва
		}
	}
	fh.Close()
	got, _, err := hashFile(part)
	if err != nil {
		return "", err
	}
	if got != rm.Hash {
		os.Remove(part)
		return "", errors.New("скачанное не совпало с хешем, повтор")
	}
	return part, nil
}

// oldName — куда отодвигается локальный файл на время замены.
func oldName(dst string) string {
	return filepath.Join(filepath.Dir(dst), tmpPrefix+"old."+filepath.Base(dst))
}

// replace ставит newFile на место p ("" — удалить p). Старое содержимое
// отодвигается и удаляется, только если его хеш = safe (оно есть на
// сервере) или = want (то же, что ставим). Иначе — конфликт-копия рядом.
func (r *Replica) replace(p, newFile, safe, want string) error {
	dst := full(r.Path, p)
	old := oldName(dst)
	moved := false
	if _, err := os.Lstat(dst); err == nil {
		if _, err := os.Lstat(old); err == nil {
			return errBusy // прошлая замена не разобрана (recover)
		}
		if err := os.Rename(dst, old); err != nil {
			return err // файл открыт без права на замену — позже
		}
		moved = true
	}
	if newFile != "" {
		if err := os.Rename(newFile, dst); err != nil {
			if moved {
				_ = os.Rename(old, dst)
			}
			return err
		}
	}
	if moved {
		r.settle(p, old, safe, want, false)
	}
	return nil
}

// settle разбирает отодвинутый файл old. Сохранённое на сервере (хеш =
// safe или want) удаляется — если только это не разбор оборванного прохода
// (recovering) и место файла не пустое: тогда возвращается на место.
// Несохранённое возвращается на место, а если место занято — остаётся
// конфликт-копией.
func (r *Replica) settle(p, old, safe, want string, recovering bool) {
	dst := full(r.Path, p)
	_, derr := os.Lstat(dst)
	free := derr != nil
	if recovering && free {
		_ = os.Rename(old, dst)
		return
	}
	h, _, err := hashFile(old)
	if err == nil && h != "" && (h == safe || h == want) {
		os.Remove(old)
		return
	}
	if free {
		_ = os.Rename(old, dst)
		return
	}
	name := freeConflictName(p, r.Device, r.clock(), func(c string) bool {
		_, err := os.Lstat(full(r.Path, c))
		return err == nil
	})
	if os.Rename(old, full(r.Path, name)) == nil {
		r.event("conflict", fmt.Sprintf("Конфликт: %s изменён здесь во время обновления — эта версия сохранена как %s", p, name))
	}
}

// recover разбирает то, что оставил оборванный проход: отодвинутые файлы
// и временные файлы.
func (r *Replica) recover() {
	_ = filepath.WalkDir(r.Path, func(fp string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, tmpPrefix+"old."):
			base := strings.TrimPrefix(name, tmpPrefix+"old.")
			rel, rerr := filepath.Rel(r.Path, filepath.Join(filepath.Dir(fp), base))
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			want := ""
			if rm, ok := r.st.Remote[rel]; ok && !rm.Del {
				want = rm.Hash
			}
			r.settle(rel, fp, r.st.Synced[rel].Hash, want, true)
		case strings.HasSuffix(name, ".part"):
			if fi, err := d.Info(); err == nil && r.clock().Sub(fi.ModTime()) > 48*time.Hour && time.Since(fi.ModTime()) > 48*time.Hour {
				os.Remove(fp)
			}
		case strings.HasSuffix(name, ".tmp"):
			os.Remove(fp)
		}
		return nil
	})
}

// Resolve — решить конфликт на сервере (keep: orig, copy, both).
func (r *Replica) Resolve(ctx context.Context, cpath, keep string) error {
	var out map[string]bool
	return r.R.post(ctx, "/resolve", map[string]any{"f": r.Folder, "path": cpath, "keep": keep}, &out)
}

// Forget — удалить файл состояния (папку отключили; сами файлы остаются).
func (r *Replica) Forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	os.Remove(r.State)
	r.gone = true
}

// countFiles — сколько файлов (без записей папок).
func countFiles(m map[string]string) int {
	n := 0
	for p := range m {
		if !isDir(p) {
			n++
		}
	}
	return n
}
