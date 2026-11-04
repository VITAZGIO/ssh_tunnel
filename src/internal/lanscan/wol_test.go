package lanscan

import "testing"

func TestNormMAC(t *testing.T) {
	for _, in := range []string{"AA:BB:CC:DD:EE:FF", "aa-bb-cc-dd-ee-ff", "aabb.ccdd.eeff", " AABBCCDDEEFF "} {
		if got, _, err := normMAC(in); err != nil || got != "aa:bb:cc:dd:ee:ff" {
			t.Errorf("%q → %q, %v", in, got, err)
		}
	}
	if _, _, err := normMAC("12:34"); err == nil {
		t.Error("короткий MAC принят")
	}
}
