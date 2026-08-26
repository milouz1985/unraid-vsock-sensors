// Package vsockaddr validates VSOCK context IDs and ports.
package vsockaddr

import "fmt"

// Max is the largest usable VSOCK CID or port. UINT32_MAX is reserved for
// VMADDR_CID_ANY and VMADDR_PORT_ANY.
const Max uint64 = 1<<32 - 2

// ValidateCID verifies that cid is in the usable guest VSOCK CID range.
func ValidateCID(cid uint64) error {
	if cid < 3 || cid > Max {
		return fmt.Errorf("cid must be between 3 and %d", Max)
	}
	return nil
}

// ValidatePort verifies that port is in the usable VSOCK port range.
func ValidatePort(port uint64) error {
	if port == 0 || port > Max {
		return fmt.Errorf("port must be between 1 and %d", Max)
	}
	return nil
}
