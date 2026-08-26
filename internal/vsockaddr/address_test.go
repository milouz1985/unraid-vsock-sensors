package vsockaddr

import "testing"

func TestValidateCID(t *testing.T) {
	for _, cid := range []uint64{3, Max} {
		if err := ValidateCID(cid); err != nil {
			t.Fatalf("valid CID %d rejected: %v", cid, err)
		}
	}
	for _, cid := range []uint64{0, 2, Max + 1} {
		if err := ValidateCID(cid); err == nil {
			t.Fatalf("invalid CID %d accepted", cid)
		}
	}
}

func TestValidatePort(t *testing.T) {
	for _, port := range []uint64{1, Max} {
		if err := ValidatePort(port); err != nil {
			t.Fatalf("valid port %d rejected: %v", port, err)
		}
	}
	for _, port := range []uint64{0, Max + 1} {
		if err := ValidatePort(port); err == nil {
			t.Fatalf("invalid port %d accepted", port)
		}
	}
}
