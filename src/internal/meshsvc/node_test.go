package meshsvc

import (
	"os/exec"
	"strings"
	"testing"

	"sshtunnel/internal/nodeapi"
)

func TestNodeInstallScriptParses(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("нет bash")
	}
	script := NodeInstallScript("v1.9.0")
	for _, want := range []string{"ssh_tunnel_node_arm64", nodeapi.Binary, "RuntimeDirectory=" + nodeapi.User,
		"StateDirectory=" + nodeapi.User, "DynamicUser=yes", NodeUnitName} {
		if !strings.Contains(script, want) {
			t.Errorf("в скрипте нет %q", want)
		}
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func TestNodeUninstallScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("нет bash")
	}
	for _, del := range []bool{false, true} {
		script := NodeUninstallScript(del)
		if strings.Contains(script, "rm -rf "+nodeapi.DataDir) != del {
			t.Fatalf("deleteData=%v: %s", del, script)
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n: %v\n%s", err, out)
		}
	}
}
