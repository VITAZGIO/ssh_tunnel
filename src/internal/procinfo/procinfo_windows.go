//go:build windows

// Package procinfo определяет, какая программа открыла соединение к нашему
// прокси. Приложение приходит с локального порта (например 127.0.0.1:63675),
// и по этому порту Windows умеет сказать PID владельца сокета, а по PID —
// путь к exe. Именно это позволяет показывать в логе "chrome.exe" вместо
// бессмысленного номера порта.
package procinfo

import (
	"encoding/binary"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi              = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTCPTab = iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUDPTab = iphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	tcpTableOwnerPIDAll = 5 // TCP_TABLE_OWNER_PID_ALL
	udpTableOwnerPID    = 1 // UDP_TABLE_OWNER_PID
	afInet              = 2
	afInet6             = 23
)

// table — как читать одну таблицу сокетов: размер строки и где в ней порт и
// PID (смещения по MIB_*ROW_OWNER_PID).
type table struct {
	proc            *windows.LazyProc
	family, class   uintptr
	rowSize         uintptr
	portOff, pidOff uintptr
}

var (
	tcpTables = []table{
		// MIB_TCPROW_OWNER_PID: State, LocalAddr, LocalPort, RemoteAddr, RemotePort, OwningPid.
		{procGetExtendedTCPTab, afInet, tcpTableOwnerPIDAll, 24, 8, 20},
		// MIB_TCP6ROW_OWNER_PID: LocalAddr[16], ScopeId, LocalPort, RemoteAddr[16], ScopeId, RemotePort, State, OwningPid.
		{procGetExtendedTCPTab, afInet6, tcpTableOwnerPIDAll, 56, 20, 52},
	}
	udpTables = []table{
		// MIB_UDPROW_OWNER_PID: LocalAddr, LocalPort, OwningPid.
		{procGetExtendedUDPTab, afInet, udpTableOwnerPID, 12, 4, 8},
		// MIB_UDP6ROW_OWNER_PID: LocalAddr[16], ScopeId, LocalPort, OwningPid.
		{procGetExtendedUDPTab, afInet6, udpTableOwnerPID, 28, 20, 24},
	}
)

type portCache struct {
	tables    []table
	portToPID map[uint16]uint32
	at        time.Time
}

var (
	mu    sync.Mutex
	tcp   = portCache{tables: tcpTables, portToPID: map[uint16]uint32{}}
	udp   = portCache{tables: udpTables, portToPID: map[uint16]uint32{}}
	names = map[uint32]string{}
)

// Lookup возвращает имя процесса, которому принадлежит локальный TCP-порт.
// Пустая строка означает "не удалось определить" — это не ошибка: сокет мог
// уже закрыться, или процесс принадлежит другому пользователю.
func Lookup(localPort uint16) (string, int) { return lookup(&tcp, localPort) }

// LookupUDP — то же для UDP-сокета (поток датаграмм из адаптера режима VPN).
func LookupUDP(localPort uint16) (string, int) { return lookup(&udp, localPort) }

func lookup(c *portCache, localPort uint16) (string, int) {
	mu.Lock()
	defer mu.Unlock()

	// Таблица сокетов снимается целиком и стоит недёшево, а соединения при
	// загрузке страницы идут пачками — поэтому кэшируем на короткое время.
	if time.Since(c.at) > 300*time.Millisecond {
		if m := snapshot(c.tables); m != nil {
			c.portToPID = m
			c.at = time.Now()
		}
	}

	pid, ok := c.portToPID[localPort]
	if !ok {
		// Сокет мог появиться уже после снимка — обновляемся один раз.
		if m := snapshot(c.tables); m != nil {
			c.portToPID = m
			c.at = time.Now()
			pid, ok = c.portToPID[localPort]
		}
		if !ok {
			return "", 0
		}
	}

	if name, ok := names[pid]; ok {
		return name, int(pid)
	}
	name := processName(pid)
	if name != "" {
		if len(names) > 512 { // не растим кэш имён бесконечно
			names = map[uint32]string{}
		}
		names[pid] = name
	}
	return name, int(pid)
}

// snapshot снимает таблицы сокетов (IPv4 и IPv6) и отдаёт порт → PID. nil —
// не удалось ни одной.
func snapshot(tables []table) map[uint16]uint32 {
	var out map[uint16]uint32
	for _, t := range tables {
		buf := readTable(t)
		if len(buf) < 4 {
			continue
		}
		if out == nil {
			out = make(map[uint16]uint32)
		}
		n := binary.LittleEndian.Uint32(buf)
		for i := uint32(0); i < n; i++ {
			off := uintptr(4) + uintptr(i)*t.rowSize
			if off+t.rowSize > uintptr(len(buf)) {
				break
			}
			row := buf[off : off+t.rowSize]
			// Порт лежит в сетевом порядке байт в младших двух байтах поля.
			port := binary.BigEndian.Uint16(row[t.portOff:])
			out[port] = binary.LittleEndian.Uint32(row[t.pidOff:])
		}
	}
	return out
}

func readTable(t table) []byte {
	var size uint32
	// Первый вызов с нулевым буфером сообщает нужный размер.
	t.proc.Call(0, uintptr(unsafe.Pointer(&size)), 0, t.family, t.class, 0)
	if size == 0 {
		return nil
	}
	buf := make([]byte, size+4096) // запас: таблица могла подрасти между вызовами
	size = uint32(len(buf))
	ret, _, _ := t.proc.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0, t.family, t.class, 0,
	)
	if ret != 0 {
		return nil
	}
	return buf[:size]
}

func processName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:size]))
}
