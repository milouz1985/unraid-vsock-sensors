package vsockaddr

import "fmt"

// Max is the largest usable VSOCK CID or port. UINT32_MAX is reserved for
// VMADDR_CID_ANY and VMADDR_PORT_ANY.
const Max uint64 = 1<<32 - 2

func ValidateCID(cid uint64) error {
	if cid < 3 || cid > Max {
		return fmt.Errorf("cid must be between 3 and %d", Max)
	}
	return nil
}

func ValidatePort(port uint64) error {
	if port == 0 || port > Max {
		return fmt.Errorf("port must be between 1 and %d", Max)
	}
	return nil
}
