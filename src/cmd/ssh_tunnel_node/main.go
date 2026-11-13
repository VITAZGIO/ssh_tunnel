// Команда ssh_tunnel_node — хост плагинов сервера (ТЗ 9): «Зеркальные
// папки», «Файлы устройства» и «Общий буфер» на самом сервере, как
// отдельный процесс без root. Логика — в internal/node; здесь — только
// обвязка процесса: флаги, сокет для панели, сигналы.
//
// Ставит и настраивает его панель (ssh_tunnel_panel), юнит — systemd с
// DynamicUser (как у meshd): отдельный пользователь системы без входа,
// данные — StateDirectory (/var/lib/ssh_tunnel_node), управляющий сокет —
// RuntimeDirectory (/run/ssh_tunnel_node/control.sock, права 0600 — панель
// работает от root и достаёт его в обход прав файла).
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"sshtunnel/internal/node"
	"sshtunnel/internal/nodeapi"
	"sshtunnel/internal/updater"
)

func main() {
	dir := flag.String("data-dir", nodeapi.DataDir, "папка данных плагинов")
	meshd := flag.String("meshd", "127.0.0.1:47831", "адрес meshd (сеть устройств)")
	listen := flag.String("listen", "127.0.0.1", "адрес, на котором слушают плагины (только localhost)")
	control := flag.String("control", nodeapi.ControlSocket, "unix-сокет для панели")
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	flag.Parse()
	if *showVersion {
		os.Stdout.WriteString(updater.Version + "\n")
		return
	}

	if ip := net.ParseIP(*listen); ip == nil || !ip.IsLoopback() {
		log.Fatalf("плагины можно слушать только на localhost, а не %q", *listen)
	}

	h, err := node.New(node.Options{Dir: *dir, Meshd: *meshd, Listen: *listen, Version: updater.Version})
	if err != nil {
		log.Fatalf("не открылся: %v", err)
	}
	if err := h.Start(); err != nil {
		log.Printf("не все плагины включились: %v", err)
	}
	defer h.Close()

	if err := serveControl(*control, h.ControlHandler()); err != nil {
		log.Fatalf("вход для панели не открылся (%v) — без него панель не увидит этот хост", err)
	}
	log.Printf("ssh_tunnel_node запущен, данные в %s", *dir)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

// serveControl — unix-сокет для панели: только владелец (root панели всё
// равно достаёт в обход прав файла — у него доступ есть всегда).
func serveControl(path string, h http.Handler) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	go (&http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	return nil
}
