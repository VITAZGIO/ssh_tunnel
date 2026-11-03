package sshapp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/windows"
)

// Агент на Windows — именованный канал (\\.\pipe\…), его понимает ssh.exe
// из OpenSSH для Windows (IdentityAgent). Доступ к каналу — только у
// текущего пользователя (DACL с его SID), соединения по сети запрещены.

const pipeRejectRemote = 0x00000008 // PIPE_REJECT_REMOTE_CLIENTS

type pipeAgent struct {
	name string
	once sync.Once
	stop chan struct{}
}

// startAgent отдаёт kr по каналу со случайным именем; закрыть — Close.
func startAgent(kr agent.Agent) (agentEndpoint, error) {
	b := make([]byte, 12)
	rand.Read(b)
	a := &pipeAgent{name: `\\.\pipe\ssh_tunnel-agent-` + hex.EncodeToString(b), stop: make(chan struct{})}
	sa, err := userOnly()
	if err != nil {
		return nil, err
	}
	first, err := a.create(sa, true)
	if err != nil {
		return nil, err
	}
	go a.loop(kr, sa, first)
	return a, nil
}

func userOnly() (*windows.SecurityAttributes, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + tu.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func (a *pipeAgent) create(sa *windows.SecurityAttributes, first bool) (windows.Handle, error) {
	mode := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		mode |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	return windows.CreateNamedPipe(windows.StringToUTF16Ptr(a.name), mode,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|pipeRejectRemote,
		windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, sa)
}

func (a *pipeAgent) loop(kr agent.Agent, sa *windows.SecurityAttributes, h windows.Handle) {
	for {
		err := windows.ConnectNamedPipe(h, nil)
		select {
		case <-a.stop:
			windows.CloseHandle(h)
			return
		default:
		}
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			windows.CloseHandle(h)
			time.Sleep(100 * time.Millisecond)
		} else {
			f := os.NewFile(uintptr(h), a.name)
			go func() {
				agent.ServeAgent(kr, f)
				f.Close()
			}()
		}
		if h, err = a.create(sa, false); err != nil {
			return
		}
	}
}

// Path — что передать ssh.exe в IdentityAgent.
func (a *pipeAgent) Path() string { return a.name }

// Close перестаёт принимать соединения. Ждущий ConnectNamedPipe будится
// своим же подключением.
func (a *pipeAgent) Close() {
	a.once.Do(func() {
		close(a.stop)
		if h, err := windows.CreateFile(windows.StringToUTF16Ptr(a.name),
			windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0); err == nil {
			windows.CloseHandle(h)
		}
	})
}
