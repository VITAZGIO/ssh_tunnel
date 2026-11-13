package mesh

// Что сеть устройств помнит на диске (Config.StateDir), чтобы работать без
// сервера: свой ключ прямых соединений и последний список устройств.
//
//	<StateDir>/<id сети>/key        — ключ ed25519 этого устройства (0600)
//	<StateDir>/<id сети>/peers.json — последний список устройств от meshd
//
// Папка своя у каждой сети: сменили ключ сети — и ключ устройства, и список
// начинаются заново, старые не мешают. StateDir пустой — всё только в памяти
// (как до этого: ключ на каждый запуск новый, без сервера после перезапуска
// устройства друг друга не найдут).

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// netID — id сети по ключу; так же считает meshd (cmd/meshd netID). По нему
// не восстановить ключ, поэтому его можно показывать и слать в маяках.
func netID(key string) string {
	h := sha256.Sum256([]byte("ssh_tunnel mesh v1\x00" + key))
	return hex.EncodeToString(h[:])
}

// stateDir — папка этой сети; "" — хранить негде.
func (c *Client) stateDir() string {
	if c.cfg.StateDir == "" || c.cfg.Key == "" {
		return ""
	}
	return filepath.Join(c.cfg.StateDir, netID(c.cfg.Key)[:16])
}

// writeFileAtomic пишет файл целиком или никак: сначала во временный рядом,
// потом переименование (на Windows Go заменяет существующий файл).
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o600)
	}
	if err == nil {
		// На Windows замена не проходит, пока файл кто-то держит открытым
		// (антивирус, поиск, чтение): немного подождать и повторить.
		for try := 0; ; try++ {
			if err = os.Rename(name, path); err == nil || try >= 10 {
				break
			}
			time.Sleep(time.Duration(try+1) * 10 * time.Millisecond)
		}
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

// ---------- ключ устройства ----------

// loadIdentity — постоянный ключ прямых соединений: создаётся один раз и
// дальше тот же, поэтому отпечаток, который другие устройства запомнили,
// пока сервер был на связи, годится и без сервера. dir == "" — свежий ключ.
func loadIdentity(dir string) (tls.Certificate, string, error) {
	if dir == "" {
		return p2pCert()
	}
	path := filepath.Join(dir, "key")
	var priv ed25519.PrivateKey
	if b, err := os.ReadFile(path); err == nil {
		if seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b))); err == nil && len(seed) == ed25519.SeedSize {
			priv = ed25519.NewKeyFromSeed(seed)
		}
	}
	if priv == nil {
		// Нет или испорчен — новый. Прежний отпечаток другие устройства
		// узнают от сервера при следующем прямом соединении.
		_, p, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		priv = p
		if err := writeFileAtomic(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n")); err != nil {
			return tls.Certificate{}, "", err
		}
	}
	return certFor(priv)
}

// p2pCert — свежий ключ (без StateDir и в тестах).
func p2pCert() (tls.Certificate, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return certFor(priv)
}

// certFor — самоподписанный сертификат на ключ. Цепочку никто не проверяет,
// только отпечаток ключа, так что срок — с запасом.
func certFor(priv ed25519.PrivateKey) (tls.Certificate, string, error) {
	pub := priv.Public().(ed25519.PublicKey)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     time.Now().Add(20 * 365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, keyFP(pub), nil
}

// ---------- список устройств ----------

// cachedPeer — что помним об устройстве сети.
type cachedPeer struct {
	IP       string `json:"ip"`
	Host     string `json:"host"`
	Name     string `json:"name,omitempty"`
	Platform string `json:"platform,omitempty"`
	OS       string `json:"os,omitempty"`
	Via      string `json:"via,omitempty"`
	// FP — отпечаток ключа прямых соединений, полученный через meshd (в
	// предложении или ответе p2p) и подтверждённый рукопожатием. Без него
	// без сервера к устройству не пойти: его не по чему проверить.
	FP string `json:"fp,omitempty"`
	// Cands — адреса, по которым прямой путь последний раз работал (первый —
	// самый свежий).
	Cands []string `json:"cands,omitempty"`
	// LastSeen — когда устройство последний раз было на связи (unix, с):
	// по серверу или по прямому соединению.
	LastSeen int64 `json:"lastSeen,omitempty"`
	// Только у самого устройства (Self): с какого времени оно держит адрес
	// (unix) и имя из панели с его временем — ТЗ 12, см. cmd/meshd.
	Since  int64  `json:"since,omitempty"`
	Alias  string `json:"alias,omitempty"`
	AliasT int64  `json:"aliasT,omitempty"`
}

type cacheFile struct {
	V       int          `json:"v"`
	Updated int64        `json:"updated"`
	Self    cachedPeer   `json:"self"`
	Peers   []cachedPeer `json:"peers"`
}

// peerCache — последний список устройств от meshd с тем, что узнали о них
// напрямую. Его замок — «лист»: под ним других не берут.
type peerCache struct {
	path string // "" — только в памяти

	mu    sync.Mutex
	data  cacheFile
	saved []byte

	wmu sync.Mutex // запись файла по одной
}

const maxCands = 6

func loadCache(dir string) *peerCache {
	pc := &peerCache{data: cacheFile{V: 1}}
	if dir == "" {
		return pc
	}
	pc.path = filepath.Join(dir, "peers.json")
	if b, err := os.ReadFile(pc.path); err == nil {
		var f cacheFile
		if json.Unmarshal(b, &f) == nil && f.V == 1 {
			pc.data = f
			g := f
			g.Updated = 0
			pc.saved, _ = json.MarshalIndent(g, "", " ")
		}
	}
	return pc
}

func (pc *peerCache) snapshot() cacheFile {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	f := pc.data
	f.Peers = append([]cachedPeer(nil), pc.data.Peers...)
	return f
}

// setList заменяет список тем, что прислал meshd. Отпечатки и адреса
// оставшихся устройств сохраняются; возвращает адреса устройств, которых в
// списке больше нет («Убрать» в панели).
func (pc *peerCache) setList(self cachedPeer, list []cachedPeer) (removed []string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	old := map[string]cachedPeer{}
	for _, p := range pc.data.Peers {
		old[p.IP] = p
	}
	now := time.Now().Unix()
	next := make([]cachedPeer, 0, len(list))
	for _, p := range list {
		if o, ok := old[p.IP]; ok {
			p.FP, p.Cands = o.FP, o.Cands
			if o.LastSeen > p.LastSeen {
				p.LastSeen = o.LastSeen
			}
			delete(old, p.IP)
		}
		next = append(next, p)
	}
	for ip := range old {
		removed = append(removed, ip)
	}
	if self.IP != pc.data.Self.IP {
		self.LastSeen = now
	} else {
		self.LastSeen = pc.data.Self.LastSeen
	}
	pc.data.Self, pc.data.Peers = self, next
	return removed
}

// setSelf — адрес, имя и давность самого устройства.
func (pc *peerCache) setSelf(self cachedPeer) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if self.IP != pc.data.Self.IP {
		self.LastSeen = time.Now().Unix()
	} else {
		self.LastSeen = pc.data.Self.LastSeen
	}
	pc.data.Self = self
}

// worked — прямой путь к устройству ip работает: запомнить отпечаток и
// адрес. Устройства не из списка не запоминаются.
func (pc *peerCache) worked(ip, fp, addr string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for i := range pc.data.Peers {
		p := &pc.data.Peers[i]
		if p.IP != ip {
			continue
		}
		if fp != "" {
			p.FP = fp
		}
		if addr != "" {
			cands := []string{addr}
			for _, c := range p.Cands {
				if c != addr && len(cands) < maxCands {
					cands = append(cands, c)
				}
			}
			p.Cands = cands
		}
		p.LastSeen = time.Now().Unix()
		return
	}
}

// byFP — адрес устройства из списка с этим отпечатком; "" — нет такого.
func (pc *peerCache) byFP(fp string) string {
	if fp == "" {
		return ""
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for _, p := range pc.data.Peers {
		if p.FP == fp {
			return p.IP
		}
	}
	return ""
}

// peer — запись об устройстве ip.
func (pc *peerCache) peer(ip string) (cachedPeer, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for _, p := range pc.data.Peers {
		if p.IP == ip {
			p.Cands = append([]string(nil), p.Cands...)
			return p, true
		}
	}
	return cachedPeer{}, false
}

// save пишет файл, если что-то изменилось (метка Updated не в счёт).
func (pc *peerCache) save() error {
	if pc.path == "" {
		return nil
	}
	pc.wmu.Lock()
	defer pc.wmu.Unlock()
	pc.mu.Lock()
	f := pc.data
	f.Updated = 0
	cmp, _ := json.MarshalIndent(f, "", " ")
	if string(cmp) == string(pc.saved) {
		pc.mu.Unlock()
		return nil
	}
	f.Updated = time.Now().Unix()
	b, err := json.MarshalIndent(f, "", " ")
	pc.mu.Unlock()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(pc.path, b); err != nil {
		return err
	}
	pc.mu.Lock()
	pc.saved = cmp
	pc.mu.Unlock()
	return nil
}

var errNoServer = errors.New("нет связи с сервером сети")
