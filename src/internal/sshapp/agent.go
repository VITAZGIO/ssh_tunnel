package sshapp

// agentEndpoint — свой SSH-агент на время входа: отдаёт ключ программы
// терминалу (ssh), не кладя его на диск. Живёт agentLife и закрывается.
type agentEndpoint interface {
	Path() string
	Close()
}
