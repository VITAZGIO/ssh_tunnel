package mesh

import (
	"os"
	"testing"
)

// В тестах маяки не ходят на настоящий порт и в настоящую локальную сеть:
// кому они нужны, тот задаёт beaconListen/beaconTargets сам.
func TestMain(m *testing.M) {
	defaultBeaconPort = 0
	os.Exit(m.Run())
}
