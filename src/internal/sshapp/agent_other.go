//go:build !windows

package sshapp

import (
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh/agent"
)

// Агент на Linux — сокет в своей временной папке с правами 700.
type sockAgent struct {
	dir  string
	ln   net.Listener
	once sync.Once
}

func startAgent(kr agent.Agent) (agentEndpoint, error) {
	d, err := os.MkdirTemp("", "ssh_tunnel-agent-")
	if err != nil {
		return nil, err
	}
	os.Chmod(d, 0o700)
	ln, err := net.Listen("unix", filepath.Join(d, "agent.sock"))
	if err != nil {
		os.RemoveAll(d)
		return nil, err
	}
	a := &sockAgent{dir: d, ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				agent.ServeAgent(kr, c)
				c.Close()
			}()
		}
	}()
	return a, nil
}

func (a *sockAgent) Path() string { return filepath.Join(a.dir, "agent.sock") }

func (a *sockAgent) Close() {
	a.once.Do(func() {
		a.ln.Close()
		os.RemoveAll(a.dir)
	})
}
