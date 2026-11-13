package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sshtunnel/internal/backup"
	"sshtunnel/internal/mirror"
	"sshtunnel/internal/nodeapi"
)

// Копия уже лежащего на сервере содержимого (второй экземпляр, без
// загрузки) учитывается в квоте сразу, а не при пересчёте раз в 30 с; не
// влезает — отказ прямо при commit.
func TestКвотаТочнаяПриКопииНаСервере(t *testing.T) {
	h := newTestHost(t)
	on := true
	h.SetConfig(nodeapi.ConfigPatch{Mirror: &on})
	net := strings.Repeat("7", 64)
	f, err := h.AddFolder(nodeapi.FolderReq{Net: net, Name: "П", QuotaMB: 3})
	if err != nil {
		t.Fatal(err)
	}
	srv := serverFor(h, nodeapi.PluginMirror, nodeapi.Caller{Net: net, Device: "pc", Host: "pc"})
	defer srv.Close()
	dir := t.TempDir()
	rep := &mirror.Replica{Folder: f.ID, Path: dir, Mode: mirror.ModeBoth, Device: "pc",
		State: filepath.Join(t.TempDir(), "s.json"), R: remoteOf(srv)}
	one := make([]byte, 1<<20)
	os.WriteFile(filepath.Join(dir, "a.bin"), one, 0o644)
	if _, err := rep.Sync(context.Background(), 0, false); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	before := h.used
	h.mu.Unlock()
	os.WriteFile(filepath.Join(dir, "b.bin"), one, 0o644) // то же содержимое
	if _, err := rep.Sync(context.Background(), 0, false); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	after := h.used
	h.mu.Unlock()
	if after-before != 1<<20 {
		t.Fatalf("копия на сервере учтена на %d байт, ждали %d", after-before, 1<<20)
	}
	nd, _ := h.netFor(net)
	nd.mu.Lock()
	by := nd.folderBy[f.ID]
	nd.mu.Unlock()
	if by != 2<<20 {
		t.Fatalf("папка: %d", by)
	}
	// Квота папки 3 МБ: третий экземпляр ложится ровно в край, четвёртый — нет.
	os.WriteFile(filepath.Join(dir, "c.bin"), one, 0o644)
	rep.Sync(context.Background(), 0, false)
	os.WriteFile(filepath.Join(dir, "d.bin"), one, 0o644)
	if _, err := rep.Sync(context.Background(), 0, false); err == nil || !strings.Contains(err.Error(), "заполнена") {
		t.Fatalf("четвёртая копия сверх квоты: %v", err)
	}
}

// Резервные копии через хост: опознанный SSH-пользователь решает, чьи
// копии видны; панель видит все копии сети с размером и версиями и может
// убрать.
func TestРезервныеКопииЧерезХост(t *testing.T) {
	h := newTestHost(t)
	on := true
	h.SetConfig(nodeapi.ConfigPatch{Backup: &on})
	net := strings.Repeat("8", 64)
	pc := serverFor(h, nodeapi.PluginBackup, nodeapi.Caller{Net: net, Device: "pc", Name: "ПК", User: "vit"})
	defer pc.Close()
	phone := serverFor(h, nodeapi.PluginBackup, nodeapi.Caller{Net: net, Device: "phone", Name: "Телефон", User: "vit"})
	defer phone.Close()
	guest := serverFor(h, nodeapi.PluginBackup, nodeapi.Caller{Net: net, Device: "tv", Name: "ТВ", User: "guest"})
	defer guest.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("содержимое"), 0o644)
	ctx := context.Background()
	rmPC := &backup.Remote{Base: pc.URL, HC: pc.Client()}
	if _, err := backup.RunBackup(ctx, rmPC, backup.Job{ID: "home", Name: "Дом", Path: dir, Schedule: backup.Daily, Keep: 2}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if sets, _ := (&backup.Remote{Base: phone.URL, HC: phone.Client()}).Sets(ctx); len(sets) != 1 {
		t.Fatalf("телефон того же пользователя: %+v", sets)
	}
	if sets, _ := (&backup.Remote{Base: guest.URL, HC: guest.Client()}).Sets(ctx); len(sets) != 0 {
		t.Fatalf("чужой пользователь видит копию: %+v", sets)
	}
	st := h.State()
	if len(st.Networks) != 1 || len(st.Networks[0].Backups) != 1 {
		t.Fatalf("панель: %+v", st.Networks)
	}
	b := st.Networks[0].Backups[0]
	if b.DeviceName != "ПК" || b.Versions != 1 || b.Bytes != int64(len("содержимое")) || st.Networks[0].BackupBytes == 0 {
		t.Fatalf("копия в панели: %+v", b)
	}
	if st.UsedBytes == 0 {
		t.Fatal("место копий не учтено")
	}
	if err := h.DeleteBackup(nodeapi.BackupReq{Net: net, Scope: b.Scope, Device: b.Device, Set: b.Set}); err != nil {
		t.Fatal(err)
	}
	if st := h.State(); len(st.Networks[0].Backups) != 0 {
		t.Fatalf("копия не убрана: %+v", st.Networks[0].Backups)
	}
}

// Данные сети, удалённой в meshd, стираются только по просьбе (панель), все
// сразу: папки, буфер, файлы, копии, настройки папок.
func TestУдалениеДанныхСети(t *testing.T) {
	h := newTestHost(t)
	on := true
	h.SetConfig(nodeapi.ConfigPatch{Mirror: &on, Files: &on, FilesWrite: &on})
	net := strings.Repeat("9", 64)
	keep := strings.Repeat("6", 64)
	if _, err := h.AddFolder(nodeapi.FolderReq{Net: net, Name: "П"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddFolder(nodeapi.FolderReq{Net: keep, Name: "Остаётся"}); err != nil {
		t.Fatal(err)
	}
	if len(h.State().Networks) != 2 {
		t.Fatal("сетей не две")
	}
	if err := h.DeleteNet(net); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.netDir(net)); err == nil {
		t.Fatal("папка сети осталась")
	}
	st := h.State()
	if len(st.Networks) != 1 || st.Networks[0].ID != keep || len(st.Networks[0].Folders) != 1 {
		t.Fatalf("после удаления: %+v", st.Networks)
	}
	if err := h.DeleteNet("../x"); err == nil {
		t.Fatal("принят id сети с «..»")
	}
}
