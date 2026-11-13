package panel

// Хост плагинов сервера (ssh_tunnel_node, ТЗ 9): зеркальные папки, файлы
// устройства и общий буфер на самом сервере. Панель только ставит,
// запускает, останавливает и настраивает его — сама логика плагинов там,
// в internal/node, за управляющим сокетом (internal/nodeapi).

import (
	"context"
	"sync"
	"time"

	"sshtunnel/internal/meshsvc"
	"sshtunnel/internal/nodeapi"
)

// nodeUnit — юнит systemd хоста плагинов.
const nodeUnit = meshsvc.NodeUnitName

// NodeManager — хост плагинов в панели.
type NodeManager struct {
	sys     MeshSystem // те же общие операции с systemd, что у сети устройств
	api     nodeapi.API
	version string

	mu  sync.Mutex
	job *meshJob
}

// NewNodeManager собирает хост плагинов панели. api может быть nil только
// в тестах, которые не доходят до Status/SetConfig.
func NewNodeManager(sys MeshSystem, api nodeapi.API, version string) *NodeManager {
	return &NodeManager{sys: sys, api: api, version: version}
}

func (m *NodeManager) runJob(kind string, fn func(onLine func(string)) error) error {
	m.mu.Lock()
	if m.job != nil && m.job.Running {
		m.mu.Unlock()
		return errJobRunning
	}
	job := &meshJob{Kind: kind, Running: true, Started: time.Now()}
	m.job = job
	m.mu.Unlock()

	onLine := func(line string) {
		m.mu.Lock()
		job.Lines = append(job.Lines, line)
		if len(job.Lines) > maxJobLines {
			job.Lines = job.Lines[len(job.Lines)-maxJobLines:]
		}
		m.mu.Unlock()
	}
	go func() {
		err := fn(onLine)
		m.mu.Lock()
		job.Running = false
		job.Finished = time.Now()
		if err != nil {
			job.Error = err.Error()
		}
		m.mu.Unlock()
	}()
	return nil
}

func (m *NodeManager) jobSnapshot() *meshJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil {
		return nil
	}
	j := *m.job
	j.Lines = append([]string(nil), m.job.Lines...)
	return &j
}

// Install ставит или обновляет хост плагинов до версии этого релиза.
func (m *NodeManager) Install() error {
	return m.runJob("install", func(onLine func(string)) error {
		onLine("Ставлю ssh_tunnel_node")
		if err := m.sys.RunScript(meshsvc.NodeInstallScript(m.version), onLine); err != nil {
			return err
		}
		onLine("Готово: хост плагинов запущен")
		return nil
	})
}

// Remove снимает службу; deleteData — стереть и данные плагинов (папки,
// буфер). Без неё данные остаются: поставишь снова — всё вернётся.
func (m *NodeManager) Remove(deleteData bool) error {
	m.mu.Lock()
	if m.job != nil && m.job.Running {
		m.mu.Unlock()
		return errJobRunning
	}
	m.mu.Unlock()
	return m.sys.RunScript(meshsvc.NodeUninstallScript(deleteData), func(string) {})
}

// NodeStatus — то, что видит страница.
type NodeStatus struct {
	Unit  UnitState      `json:"unit"`
	State *nodeapi.State `json:"state,omitempty"`
	Error string         `json:"error,omitempty"`
	Job   *meshJob       `json:"job,omitempty"`
}

// Status — состояние хоста: установлен ли юнит, и если да — живое
// состояние плагинов через управляющий сокет.
func (m *NodeManager) Status(ctx context.Context) NodeStatus {
	st := NodeStatus{Unit: m.sys.UnitState(nodeUnit), Job: m.jobSnapshot()}
	if !st.Unit.Installed {
		return st
	}
	s, err := m.api.State(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.State = &s
	return st
}

func (m *NodeManager) SetConfig(ctx context.Context, p nodeapi.ConfigPatch) (nodeapi.State, error) {
	return m.api.SetConfig(ctx, p)
}

func (m *NodeManager) AddFolder(ctx context.Context, r nodeapi.FolderReq) (nodeapi.Folder, error) {
	return m.api.AddFolder(ctx, r)
}

func (m *NodeManager) RemoveFolder(ctx context.Context, r nodeapi.FolderReq) error {
	return m.api.RemoveFolder(ctx, r)
}

func (m *NodeManager) SetFolderQuota(ctx context.Context, r nodeapi.FolderReq) error {
	return m.api.SetFolderQuota(ctx, r)
}

func (m *NodeManager) ClearClipboard(ctx context.Context, netID string) error {
	return m.api.ClearClipboard(ctx, netID)
}
