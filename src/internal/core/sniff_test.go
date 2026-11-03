package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"net"
	"testing"
	"time"
)

// clientHello — первые байты настоящего TLS-клиента Go к имени sni.
func clientHello(t *testing.T, sni string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		c := tls.Client(a, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		c.SetDeadline(time.Now().Add(2 * time.Second))
		c.Handshake()
	}()
	buf := make([]byte, 16384)
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	var got []byte
	for {
		n, err := b.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil || len(got) >= 5 && len(got) >= 5+int(binary.BigEndian.Uint16(got[3:5])) {
			return got
		}
	}
}

func TestSniffTLS(t *testing.T) {
	hello := clientHello(t, "Example.COM")
	if host, more := SniffHost(hello); host != "example.com" || more {
		t.Fatalf("SniffHost(ClientHello) = %q, %v", host, more)
	}
	// Начало ClientHello — имя ещё впереди, ждать.
	if host, more := SniffHost(hello[:20]); host != "" || !more {
		t.Fatalf("обрывок: %q, %v", host, more)
	}
	// Тот же ClientHello двумя записями TLS.
	rec := hello[5:]
	cut := len(rec) / 2
	two := append([]byte{0x16, 3, 1, byte(cut >> 8), byte(cut)}, rec[:cut]...)
	two = append(two, 0x16, 3, 1, byte((len(rec)-cut)>>8), byte(len(rec)-cut))
	two = append(two, rec[cut:]...)
	if host, _ := SniffHost(two); host != "example.com" {
		t.Fatalf("две записи: %q", host)
	}
}

func TestSniffHTTP(t *testing.T) {
	for in, want := range map[string]string{
		"GET / HTTP/1.1\r\nHost: Site.ru:8080\r\nAccept: */*\r\n\r\n":  "site.ru",
		"POST /x HTTP/1.1\r\nUser-Agent: a\r\nhost: b.example\r\n\r\n": "b.example",
		"GET / HTTP/1.1\r\nHost: 1.2.3.4\r\n\r\n":                      "",
	} {
		if host, more := SniffHost([]byte(in)); host != want || more {
			t.Errorf("%q → %q, %v; ждали %q", in, host, more, want)
		}
	}
	if _, more := SniffHost([]byte("GET / HTTP/1.1\r\nHost: si")); !more {
		t.Error("недописанный Host — надо ждать")
	}
	if _, more := SniffHost([]byte("GE")); !more {
		t.Error("первые буквы метода — надо ждать")
	}
	if host, more := SniffHost([]byte("SSH-2.0-OpenSSH_9.6\r\n")); host != "" || more {
		t.Error("не TLS и не HTTP — ждать нечего")
	}
}

// Ключи из RFC 9001, приложение A.1.
func TestQUICKeysRFC9001(t *testing.T) {
	dcid, _ := hex.DecodeString("8394c8f03e515708")
	key, iv, hp := quicClientKeys(quicVersions[1], dcid)
	for name, pair := range map[string][2]string{
		"key": {hex.EncodeToString(key), "1f369613dd76d5467730efcbe3b1a22d"},
		"iv":  {hex.EncodeToString(iv), "fa044b2f42a3fd3b46fb255c"},
		"hp":  {hex.EncodeToString(hp), "9f50449e04a0e810283a1e9933adedd2"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %s, ждали %s", name, pair[0], pair[1])
		}
	}
}

// quicInitial собирает пакет Initial клиента с ClientHello в кадрах CRYPTO
// (два кадра не по порядку — как режет Chrome).
func quicInitial(t *testing.T, version uint32, hello []byte) []byte {
	t.Helper()
	v := quicVersions[version]
	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	key, iv, hp := quicClientKeys(v, dcid)

	hs := hello[5:] // без заголовка записи TLS
	cut := len(hs) / 3
	var plain []byte
	frame := func(off int, data []byte) {
		plain = append(plain, 0x06)
		plain = appendVarint(plain, uint64(off))
		plain = appendVarint(plain, uint64(len(data)))
		plain = append(plain, data...)
	}
	frame(cut, hs[cut:])
	plain = append(plain, 0x01) // PING
	frame(0, hs[:cut])
	for len(plain) < 1162 {
		plain = append(plain, 0) // PADDING
	}

	const pnLen = 2
	pn := uint64(0)
	hdr := []byte{0xc0 | v.initialType<<4 | (pnLen - 1)}
	hdr = binary.BigEndian.AppendUint32(hdr, version)
	hdr = append(hdr, byte(len(dcid)))
	hdr = append(hdr, dcid...)
	hdr = append(hdr, 0) // SCID
	hdr = append(hdr, 0) // токен
	hdr = appendVarint2(hdr, uint64(pnLen+len(plain)+16))
	pnOff := len(hdr)
	hdr = append(hdr, byte(pn>>8), byte(pn))

	kb, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(kb)
	nonce := append([]byte(nil), iv...)
	pkt := aead.Seal(append([]byte(nil), hdr...), nonce, plain, hdr)

	hb, _ := aes.NewCipher(hp)
	var mask [16]byte
	hb.Encrypt(mask[:], pkt[pnOff+4:pnOff+20])
	pkt[0] ^= mask[0] & 0x0f
	for i := 0; i < pnLen; i++ {
		pkt[pnOff+i] ^= mask[1+i]
	}
	return pkt
}

func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, byte(v>>8)|0x40, byte(v))
	}
	return append(b, byte(v>>24)|0x80, byte(v>>16), byte(v>>8), byte(v))
}

// appendVarint2 — всегда два байта (поле длины до шифрования известно).
func appendVarint2(b []byte, v uint64) []byte { return append(b, byte(v>>8)|0x40, byte(v)) }

func TestSniffQUIC(t *testing.T) {
	hello := clientHello(t, "quic.example.org")
	for _, ver := range []uint32{0x00000001, 0x6b3343cf} {
		pkt := quicInitial(t, ver, hello)
		if host := SniffQUIC(pkt); host != "quic.example.org" {
			t.Errorf("версия %x: %q", ver, host)
		}
		bad := append([]byte(nil), pkt...)
		bad[len(bad)-1] ^= 1
		if host := SniffQUIC(bad); host != "" {
			t.Errorf("испорченный пакет дал %q", host)
		}
	}
	if SniffQUIC([]byte{0x40, 1, 2, 3}) != "" || SniffQUIC(nil) != "" {
		t.Error("не QUIC — пусто")
	}
}
