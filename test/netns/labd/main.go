// labd — «интернет» стенда test/netns: HTTP-эхо, которое отвечает адресом
// того, кто пришёл, и DNS-сервер (UDP и TCP), который пишет в журнал, кто
// и через какой адрес спрашивал. Только стандартная библиотека.
//
//	labd -http 192.0.2.80:80,192.0.2.81:80 -dns 192.0.2.53:53,1.1.1.1:53 \
//	     -a direct=192.0.2.81 -default 192.0.2.80 -zone test-lab.net -log dns.log
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	zone    = flag.String("zone", "test-lab.net", "зона, на имена в которой отвечаем адресом")
	answers = flag.String("a", "", "первое слово имени=адрес через запятую (direct=192.0.2.81)")
	defIP   = flag.String("default", "192.0.2.80", "адрес для остальных имён зоны")
	logPath = flag.String("log", "", "журнал запросов DNS")

	logMu sync.Mutex
	logF  *os.File
)

func main() {
	httpAddrs := flag.String("http", "", "адреса HTTP-эха через запятую")
	dnsAddrs := flag.String("dns", "", "адреса DNS через запятую")
	flag.Parse()
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		logF = f
	}
	for _, a := range split(*httpAddrs) {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			log.Fatal(err)
		}
		go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			fmt.Fprintln(w, host)
		}))
	}
	for _, a := range split(*dnsAddrs) {
		pc, err := net.ListenPacket("udp", a)
		if err != nil {
			log.Fatal(err)
		}
		go serveUDP(pc, a)
		ln, err := net.Listen("tcp", a)
		if err != nil {
			log.Fatal(err)
		}
		go serveTCP(ln, a)
	}
	fmt.Println("labd: готов")
	select {}
}

func split(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func serveUDP(pc net.PacketConn, listen string) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if r, err := answer(buf[:n], listen, from.String()); err == nil {
			pc.WriteTo(r, from)
		}
	}
}

func serveTCP(ln net.Listener, listen string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			for {
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				r, err := answer(q, listen, c.RemoteAddr().String())
				if err != nil {
					return
				}
				binary.BigEndian.PutUint16(l[:], uint16(len(r)))
				c.Write(append(l[:], r...))
			}
		}()
	}
}

// answer — ответ на запрос: A для имён зоны, пустой NOERROR для остального
// типа, NXDOMAIN для чужих имён.
func answer(q []byte, listen, from string) ([]byte, error) {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:]) != 1 {
		return nil, errors.New("не запрос")
	}
	i := 12
	var labels []string
	for {
		if i >= len(q) {
			return nil, errors.New("обрезано")
		}
		l := int(q[i])
		i++
		if l == 0 {
			break
		}
		if l > 63 || i+l > len(q) {
			return nil, errors.New("плохое имя")
		}
		labels = append(labels, string(q[i:i+l]))
		i += l
	}
	if i+4 > len(q) {
		return nil, errors.New("обрезано")
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	qend := i + 4
	name := strings.ToLower(strings.Join(labels, "."))
	fromHost, _, _ := net.SplitHostPort(from)
	listenHost, _, _ := net.SplitHostPort(listen)
	if logF != nil {
		logMu.Lock()
		fmt.Fprintf(logF, "%d %s %s %s\n", time.Now().Unix(), listenHost, fromHost, name)
		logMu.Unlock()
	}
	r := make([]byte, 0, 64)
	r = append(r, q[0], q[1])
	inZone := name == *zone || strings.HasSuffix(name, "."+*zone)
	flags := uint16(0x8180)
	if !inZone {
		flags |= 3 // NXDOMAIN
	}
	var ip net.IP
	if inZone && qtype == 1 {
		ip = net.ParseIP(*defIP).To4()
		first := labels[0]
		for _, kv := range split(*answers) {
			k, v, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(first, k) {
				ip = net.ParseIP(v).To4()
			}
		}
	}
	an := uint16(0)
	if ip != nil {
		an = 1
	}
	r = binary.BigEndian.AppendUint16(r, flags)
	r = binary.BigEndian.AppendUint16(r, 1)
	r = binary.BigEndian.AppendUint16(r, an)
	r = append(r, 0, 0, 0, 0)
	r = append(r, q[12:qend]...)
	if ip != nil {
		r = append(r, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4)
		r = append(r, ip...)
	}
	return r, nil
}
