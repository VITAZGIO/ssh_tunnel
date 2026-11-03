//go:build !windows

package sshapp

// На Linux своего шифровальщика нет (keyring есть не везде, а сервер без
// рабочего стола — частый случай): файл с правами 600. Android ставит
// Keystore через SetSealer.
func osSealer() Sealer { return nil }
