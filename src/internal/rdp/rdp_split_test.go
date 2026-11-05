package rdp

import "testing"

func TestSplitUser(t *testing.T) {
	for _, c := range [][4]string{{"vitazcomp/VitazComp", "", "VitazComp", "vitazcomp"}, {`DOM\ivan`, "", "ivan", "DOM"}, {"ivan", "", "ivan", ""}, {"a/b", "X", "a/b", "X"}} {
		u, d := splitUser(c[0], c[1])
		if u != c[2] || d != c[3] {
			t.Errorf("%q → %q %q", c[0], u, d)
		}
	}
}
