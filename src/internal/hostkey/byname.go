package hostkey

// Проверка ключа промежуточного сервера цепочки выхода (ТЗ 8).
//
// До сервера Б соединение идёт каналом внутри SSH-сессии с сервером А, и
// «удалённый адрес» такого соединения — пустой 0.0.0.0: адрес Б знает
// только А. Обычный Callback сверяет и имя, и адрес — и записал бы ключ Б
// ещё и под 0.0.0.0, а следующий сервер цепочки тогда выглядел бы «сменившим
// ключ». Поэтому здесь сверяется только имя (host:port), в том же файле
// known_hosts и с той же ошибкой ErrChanged, что у прямого подключения.

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// CallbackByName — HostKeyCallback, который сверяет ключ только по имени
// сервера, без адреса соединения. Первый ключ запоминается (TOFU).
func CallbackByName(knownHostsPath string, onLearn func(host, fingerprint string)) ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		known, match, err := lookupByName(knownHostsPath, hostname, key)
		if err != nil {
			return err
		}
		switch {
		case match:
			return nil
		case known:
			return &ErrChanged{Host: hostname, KnownHosts: knownHostsPath, Fingerprint: fp}
		}
		return learn(knownHostsPath, hostname, nil, key, fp, onLearn)
	}
}

// lookupByName ищет строки known_hosts для имени: known — имя записано с
// ключом того же типа, match — ключ совпал. Ключ другого типа под тем же
// именем — тоже смена ключа, как у knownhosts.
func lookupByName(path, hostname string, key ssh.PublicKey) (known, match bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	want := knownhosts.Normalize(hostname)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '@' {
			continue // пометки @revoked/@cert-authority здесь не нужны
		}
		f := strings.Fields(line)
		if len(f) < 3 || !hostsMatch(f[0], want) {
			continue
		}
		pk, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(f[1] + " " + f[2]))
		if perr != nil {
			continue
		}
		known = true
		if pk.Type() == key.Type() && bytes.Equal(pk.Marshal(), key.Marshal()) {
			return true, true, nil
		}
	}
	return known, false, nil
}

// hostsMatch — поле имён строки known_hosts содержит имя want (уже
// нормализованное): простой список через запятую или хэш |1|соль|хэш.
func hostsMatch(field, want string) bool {
	if strings.HasPrefix(field, "|1|") {
		parts := strings.Split(field[3:], "|")
		if len(parts) != 2 {
			return false
		}
		salt, err1 := base64.StdEncoding.DecodeString(parts[0])
		sum, err2 := base64.StdEncoding.DecodeString(parts[1])
		if err1 != nil || err2 != nil {
			return false
		}
		m := hmac.New(sha1.New, salt)
		m.Write([]byte(want))
		return hmac.Equal(m.Sum(nil), sum)
	}
	for _, h := range strings.Split(field, ",") {
		if h == want {
			return true
		}
	}
	return false
}
