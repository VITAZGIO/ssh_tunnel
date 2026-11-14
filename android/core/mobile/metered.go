package mobile

// Лимитная связь (ТЗ 20): резервные копии с «только по Wi-Fi» не идут по
// мобильной сети. Узнать, лимитная ли связь, может только Android
// (ConnectivityManager.isActiveNetworkMetered) — служба сообщает сюда при
// старте и на каждую смену сети.

import (
	"sync"
	"sync/atomic"

	"sshtunnel/internal/backup"
)

var (
	metered     atomic.Bool
	meteredOnce sync.Once
)

// SetMetered — сейчас связь лимитная (мобильная сеть, точка доступа с
// лимитом). Зовётся из Kotlin; до первого вызова считается «не лимитная».
func SetMetered(on bool) {
	meteredOnce.Do(func() { backup.SetMetered(metered.Load) })
	metered.Store(on)
}

// Metered — что сейчас сообщила служба (для проверки и журнала).
func Metered() bool { return metered.Load() }
