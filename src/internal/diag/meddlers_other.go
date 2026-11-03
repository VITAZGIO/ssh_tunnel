//go:build !windows && (!linux || android)

package diag

import (
	"context"
	"errors"
)

// На других системах (и на Android, где проверки делает сама служба —
// следующий этап) искать нечем.

func scanPlatform(context.Context) []Finding { return nil }

var errUnsupported = errors.New("на этой системе не поддерживается")

func stopFinding(Finding) (Undo, error)  { return Undo{}, errUnsupported }
func restoreUndo(Undo) error             { return errUnsupported }
func disableAutostart(*Undo) error       { return errUnsupported }
func appendLines(string, []string) error { return errUnsupported }
