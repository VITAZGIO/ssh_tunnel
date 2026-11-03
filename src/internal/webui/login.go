package webui

// Вход по паролю (замечание владельца 16, 03.11.2026). Панель, открытая
// для локальной сети (-web-lan, Linux), пускала любого из сети без ключа.
// С галкой «Вход по паролю» и заданным паролем такой гость сначала вводит
// пароль и получает куку сеанса. Окно самой программы ходит с ключом (он
// есть только у неё) — ему пароль не нужен. Забыл пароль — задать новый в
// окне программы (оно уже своё) или `-reset-password` на Linux от root.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"sshtunnel/internal/updater"
)

const (
	sessCookie = "sshtunnel_sess"
	sessTTL    = 30 * 24 * time.Hour
	minUIPass  = 4
)

type sessions struct {
	mu    sync.Mutex
	m     map[string]time.Time
	fails int
	until time.Time // после пяти ошибок — минута ожидания
}

func (s *sessions) valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, exp := range s.m {
		if subtle.ConstantTimeCompare([]byte(k), []byte(id)) == 1 {
			return time.Now().Before(exp)
		}
	}
	return false
}

func (s *sessions) add() string {
	b := make([]byte, 24)
	rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]time.Time{}
	}
	now := time.Now()
	for k, exp := range s.m {
		if now.After(exp) {
			delete(s.m, k)
		}
	}
	s.m[id] = now.Add(sessTTL)
	s.mu.Unlock()
	return id
}

func (s *sessions) reset() {
	s.mu.Lock()
	s.m = nil
	s.mu.Unlock()
}

func setSession(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{Name: sessCookie, Value: id, Path: "/",
		MaxAge: int(sessTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// lockOn — пароль действует: галка стоит и пароль задан.
func (s *Server) lockOn() bool {
	c := s.app.Config()
	return c.UILock && c.UIPassHash != ""
}

// sessionOK — у гостя из сети есть кука сеанса после пароля.
func (s *Server) sessionOK(r *http.Request) bool {
	c, err := r.Cookie(sessCookie)
	return err == nil && s.sess.valid(c.Value)
}

// tokenOK — запрос пришёл с ключом программы (окно, а не гость из сети).
func (s *Server) tokenOK(r *http.Request) bool {
	tok := r.Header.Get("X-Token")
	if tok == "" {
		tok = r.URL.Query().Get("t")
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

// handleLogin: GET — нужен ли пароль этому гостю; POST {password} — войти.
// Без ключа и без guard: это и есть дверь.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "запрещено", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		// Для «Забыли пароль?»: чем сбросить на этой машине.
		exe, _ := os.Executable()
		writeJSON(w, map[string]any{"need": s.lockOn() && !s.allowed(r),
			"exe": filepath.Base(exe), "unit": updater.OwnUnit()})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
	s.sess.mu.Lock()
	wait := time.Until(s.sess.until)
	s.sess.mu.Unlock()
	if wait > 0 {
		writeJSON(w, map[string]string{"error": "слишком много неверных паролей, подожди минуту"})
		return
	}
	hash := s.app.Config().UIPassHash
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		s.sess.mu.Lock()
		s.sess.fails++
		if s.sess.fails >= 5 {
			s.sess.fails, s.sess.until = 0, time.Now().Add(time.Minute)
		}
		s.sess.mu.Unlock()
		writeJSON(w, map[string]string{"error": "неверный пароль"})
		return
	}
	s.sess.mu.Lock()
	s.sess.fails = 0
	s.sess.mu.Unlock()
	setSession(w, s.sess.add())
	writeJSON(w, map[string]bool{"ok": true})
}

// handleUIPass: GET — {lock, set}; POST {password} — задать пароль входа
// ("" — убрать). Старый пароль не спрашиваем: сюда пускают только своих
// (окно программы с ключом или уже вошедший гость) — так и работает «забыл
// пароль».
func (s *Server) handleUIPass(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		c := s.app.Config()
		writeJSON(w, map[string]bool{"lock": c.UILock, "set": c.UIPassHash != ""})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "не разобрал запрос"})
		return
	}
	cfg := s.app.Config()
	if req.Password == "" {
		cfg.UIPassHash = ""
	} else {
		if len([]rune(req.Password)) < minUIPass {
			writeJSON(w, map[string]string{"error": "пароль — хотя бы 4 знака"})
			return
		}
		h, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		cfg.UIPassHash = string(h)
	}
	if _, err := s.app.SetConfig(cfg); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	// Новый пароль — старые сеансы гостей больше не действуют; гость, который
	// менял его сам, остаётся с новой кукой.
	s.sess.reset()
	if !s.tokenOK(r) && req.Password != "" {
		setSession(w, s.sess.add())
	}
	writeJSON(w, map[string]bool{"ok": true, "set": req.Password != ""})
}
