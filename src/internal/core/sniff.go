package core

// Имя соединения по его первым байтам.
//
// Без подставных адресов приложение приходит в адаптер с настоящим адресом,
// и имя, нужное правилам («всегда напрямую», «всегда через сервер»), надо
// узнать иначе. Почти всё в интернете говорит первым и называет сайт сам:
//
//   - TLS (HTTPS и всё поверх него) — имя в ClientHello (SNI);
//   - HTTP без шифрования — заголовок Host;
//   - QUIC (HTTP/3) — тот же ClientHello внутри первого пакета Initial. Он
//     зашифрован, но ключ выводится из открытого идентификатора соединения
//     (RFC 9001, раздел 5.2) — это не взлом, а так задумано протоколом.
//
// Не нашлось — ядро берёт имя из таблицы недавних DNS-ответов (см.
// tunnel.LearnName). Сами функции ничего не решают: только достают имя.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/netip"
	"sort"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// SniffHost достаёт имя сайта из начала TCP-потока: TLS ClientHello или
// запрос HTTP. more — данных мало, имя может найтись в следующих байтах;
// false вместе с пустым host — это не TLS и не HTTP, ждать нечего.
func SniffHost(b []byte) (host string, more bool) {
	if len(b) == 0 {
		return "", true
	}
	if b[0] == 0x16 {
		return sniffTLS(b)
	}
	if httpStart(b) {
		return sniffHTTP(b)
	}
	return "", false
}

// sniffTLS — SNI из записей TLS. ClientHello бывает длиннее одной записи
// (постквантовый обмен ключами в Chrome — около 1,8 КБ), поэтому записи
// склеиваются.
func sniffTLS(b []byte) (string, bool) {
	var hs []byte
	for len(b) > 0 {
		if b[0] != 0x16 {
			break
		}
		if len(b) < 5 {
			return "", true
		}
		n := int(binary.BigEndian.Uint16(b[3:5]))
		body := b[5:]
		if len(body) < n {
			hs = append(hs, body...)
			host, ok, more := parseClientHello(hs)
			if ok {
				return host, false
			}
			return "", more
		}
		hs = append(hs, body[:n]...)
		b = body[n:]
	}
	host, _, more := parseClientHello(hs)
	return host, more
}

// parseClientHello разбирает сообщение ClientHello (с заголовком рукопожатия)
// до расширения server_name. ok — разбор закончен (имени может и не быть);
// more — сообщение оборвалось раньше, чем нашлось имя.
func parseClientHello(hs []byte) (host string, ok, more bool) {
	r := reader{b: hs}
	typ, okT := r.u8()
	if !okT {
		return "", false, true
	}
	if typ != 1 {
		return "", true, false
	}
	if _, ok := r.skip(3 + 2 + 32); !ok { // длина, версия, random
		return "", false, true
	}
	if !r.skipVec(1) || !r.skipVec(2) || !r.skipVec(1) { // session_id, шифры, сжатие
		return "", false, true
	}
	extLen, ok2 := r.u16()
	if !ok2 {
		return "", false, true
	}
	end := r.off + int(extLen)
	for r.off+4 <= end {
		et, _ := r.u16()
		el, _ := r.u16()
		data, ok := r.skip(int(el))
		if !ok {
			return "", false, true
		}
		if et != 0 { // server_name
			continue
		}
		// server_name_list: длина, затем тип имени (0 — host_name) и имя.
		s := reader{b: data}
		if _, ok := s.u16(); !ok {
			return "", true, false
		}
		if nt, ok := s.u8(); !ok || nt != 0 {
			return "", true, false
		}
		nl, _ := s.u16()
		name, ok := s.skip(int(nl))
		if !ok {
			return "", true, false
		}
		return cleanHost(string(name)), true, false
	}
	if r.off < end {
		return "", false, true
	}
	return "", true, false
}

var httpMethods = []string{"GET ", "POST ", "HEAD ", "PUT ", "DELETE ", "OPTIONS ", "PATCH ", "CONNECT ", "TRACE "}

// httpStart — начало похоже на запрос HTTP (или на его первые буквы).
func httpStart(b []byte) bool {
	for _, m := range httpMethods {
		n := min(len(b), len(m))
		if string(b[:n]) == m[:n] {
			return true
		}
	}
	return false
}

// sniffHTTP — заголовок Host из запроса HTTP/1.x.
func sniffHTTP(b []byte) (string, bool) {
	head := b
	complete := false
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		head, complete = b[:i], true
	}
	lines := strings.Split(string(head), "\r\n")
	for i, line := range lines {
		if i == 0 {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "host") {
			continue
		}
		// Строка дочитана, только если за ней уже есть следующая.
		if !complete && i == len(lines)-1 {
			return "", true
		}
		return cleanHost(stripPort(strings.TrimSpace(v))), false
	}
	return "", !complete && len(b) < sniffMax
}

// sniffMax — больше этого начало соединения не копим.
const sniffMax = 8192

func stripPort(h string) string {
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			return h[1:i]
		}
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[:i], ":") {
		return h[:i]
	}
	return h
}

// cleanHost — имя в нижнем регистре без точки в конце; пусто, если это не
// имя (адрес или мусор): правилам по именам такое не поможет.
func cleanHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "" || len(h) > 253 {
		return ""
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return ""
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return ""
		}
	}
	return h
}

// QUIC: версии и их константы (RFC 9001, раздел 5.2; RFC 9369 для v2).
type quicVersion struct {
	salt        []byte
	key, iv, hp string
	initialType byte // тип пакета Initial в битах 4–5 первого байта
}

var quicVersions = map[uint32]quicVersion{
	0x00000001: {
		salt: []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a},
		key:  "quic key", iv: "quic iv", hp: "quic hp", initialType: 0,
	},
	0x6b3343cf: {
		salt: []byte{0x0d, 0xed, 0xe3, 0xde, 0xf7, 0x00, 0xa6, 0xdb, 0x81, 0x93, 0x81, 0xbe, 0x6e, 0x26, 0x9d, 0xcb, 0xf9, 0xbd, 0x2e, 0xd9},
		key:  "quicv2 key", iv: "quicv2 iv", hp: "quicv2 hp", initialType: 1,
	},
}

// SniffQUIC достаёт SNI из первой датаграммы QUIC (пакет Initial клиента).
// Пусто — не QUIC, другая версия или имя не в первой датаграмме (Chrome
// иногда режет ClientHello на куски по двум пакетам): тогда решает таблица
// DNS-ответов.
func SniffQUIC(dgram []byte) string {
	host, _ := sniffQUIC(dgram)
	return host
}

func sniffQUIC(dgram []byte) (string, error) {
	if len(dgram) < 7 || dgram[0]&0xc0 != 0xc0 {
		return "", errors.New("не длинный заголовок QUIC")
	}
	v, ok := quicVersions[binary.BigEndian.Uint32(dgram[1:5])]
	if !ok || (dgram[0]>>4)&0x03 != v.initialType {
		return "", errors.New("не Initial известной версии")
	}
	r := reader{b: dgram, off: 5}
	dcid, ok := r.vec8()
	if !ok || len(dcid) > 20 {
		return "", errors.New("битый DCID")
	}
	if !r.skipVec(1) { // SCID
		return "", errors.New("битый SCID")
	}
	tokLen, ok := r.varint()
	if !ok {
		return "", errors.New("битый токен")
	}
	if _, ok := r.skip(int(tokLen)); !ok {
		return "", errors.New("битый токен")
	}
	length, ok := r.varint()
	if !ok {
		return "", errors.New("битая длина")
	}
	pnOff := r.off
	if pnOff+int(length) > len(dgram) || length < 20 {
		return "", errors.New("пакет короче заявленного")
	}
	key, iv, hp := quicClientKeys(v, dcid)

	pkt := append([]byte(nil), dgram[:pnOff+int(length)]...)
	block, err := aes.NewCipher(hp)
	if err != nil {
		return "", err
	}
	var mask [16]byte
	block.Encrypt(mask[:], pkt[pnOff+4:pnOff+20])
	pkt[0] ^= mask[0] & 0x0f
	pnLen := int(pkt[0]&0x03) + 1
	var pn uint64
	for i := 0; i < pnLen; i++ {
		pkt[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(pkt[pnOff+i])
	}
	hdr := pkt[:pnOff+pnLen]
	nonce := append([]byte(nil), iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	kb, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(kb)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, nonce, pkt[pnOff+pnLen:], hdr)
	if err != nil {
		return "", errors.New("не расшифровалось")
	}
	hs := cryptoStream(plain)
	host, _, _ := parseClientHello(hs)
	return host, nil
}

// cryptoStream собирает данные кадров CRYPTO с нулевого смещения подряд.
func cryptoStream(p []byte) []byte {
	type chunk struct {
		off  uint64
		data []byte
	}
	var chunks []chunk
	r := reader{b: p}
	for r.off < len(p) {
		ft, ok := r.varint()
		if !ok {
			break
		}
		switch {
		case ft == 0x00 || ft == 0x01: // PADDING, PING
		case ft == 0x02 || ft == 0x03: // ACK
			var n [4]uint64
			for i := range n {
				if n[i], ok = r.varint(); !ok {
					return nil
				}
			}
			for i := uint64(0); i < n[2]*2; i++ {
				if _, ok = r.varint(); !ok {
					return nil
				}
			}
			if ft == 0x03 {
				for i := 0; i < 3; i++ {
					if _, ok = r.varint(); !ok {
						return nil
					}
				}
			}
		case ft == 0x06: // CRYPTO
			off, ok1 := r.varint()
			n, ok2 := r.varint()
			data, ok3 := r.skip(int(n))
			if !ok1 || !ok2 || !ok3 {
				return nil
			}
			chunks = append(chunks, chunk{off, data})
		default:
			r.off = len(p) // остальные кадры в Initial клиента не нужны
		}
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].off < chunks[j].off })
	var out []byte
	for _, c := range chunks {
		end := c.off + uint64(len(c.data))
		if c.off > uint64(len(out)) {
			break // дыра: дальше данные из другого пакета
		}
		if end > uint64(len(out)) {
			out = append(out, c.data[uint64(len(out))-c.off:]...)
		}
	}
	return out
}

// quicClientKeys — ключи пакетов Initial клиента (RFC 9001, раздел 5.2).
func quicClientKeys(v quicVersion, dcid []byte) (key, iv, hp []byte) {
	initial := hkdf.Extract(sha256.New, dcid, v.salt)
	client := expandLabel(initial, "client in", 32)
	return expandLabel(client, v.key, 16), expandLabel(client, v.iv, 12), expandLabel(client, v.hp, 16)
}

// expandLabel — HKDF-Expand-Label из TLS 1.3 с пустым контекстом.
func expandLabel(secret []byte, label string, n int) []byte {
	full := "tls13 " + label
	info := make([]byte, 0, 4+len(full))
	info = binary.BigEndian.AppendUint16(info, uint16(n))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0)
	out := make([]byte, n)
	if _, err := hkdf.Expand(sha256.New, secret, info).Read(out); err != nil {
		panic(err) // n ≤ 255*32 — не бывает
	}
	return out
}

// reader — разбор байтов без паник на обрывах.
type reader struct {
	b   []byte
	off int
}

func (r *reader) skip(n int) ([]byte, bool) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, false
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, true
}

func (r *reader) u8() (byte, bool) {
	s, ok := r.skip(1)
	if !ok {
		return 0, false
	}
	return s[0], true
}

func (r *reader) u16() (uint16, bool) {
	s, ok := r.skip(2)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint16(s), true
}

// skipVec пропускает вектор с длиной в lenBytes байтах.
func (r *reader) skipVec(lenBytes int) bool {
	s, ok := r.skip(lenBytes)
	if !ok {
		return false
	}
	n := 0
	for _, c := range s {
		n = n<<8 | int(c)
	}
	_, ok = r.skip(n)
	return ok
}

func (r *reader) vec8() ([]byte, bool) {
	n, ok := r.u8()
	if !ok {
		return nil, false
	}
	return r.skip(int(n))
}

// varint — целое переменной длины QUIC (RFC 9000, раздел 16).
func (r *reader) varint() (uint64, bool) {
	first, ok := r.u8()
	if !ok {
		return 0, false
	}
	n := 1 << (first >> 6)
	v := uint64(first & 0x3f)
	rest, ok := r.skip(n - 1)
	if !ok {
		return 0, false
	}
	for _, c := range rest {
		v = v<<8 | uint64(c)
	}
	return v, true
}
