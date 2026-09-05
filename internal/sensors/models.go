// Package sensors defines the shared sensor model and VSOCK client.
package sensors

import (
	"strings"
	"time"
)

// Disk describes an Unraid disk and its latest collected temperature.
type Disk struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Device      string  `json:"device"`
	Transport   string  `json:"transport,omitempty"`
	Rotational  bool    `json:"rotational"`
	Temp        float64 `json:"temp_c"`
	Unavailable bool    `json:"unavailable,omitempty"`
}

// DiskKind identifies the storage technology used to group disk temperatures.
type DiskKind string

const (
	// DiskKindHDD identifies a rotational disk.
	DiskKindHDD DiskKind = "hdd"
	// DiskKindSATASSD identifies a non-rotational disk using ATA transport.
	DiskKindSATASSD DiskKind = "ssd"
	// DiskKindNVMe identifies a non-rotational NVMe disk.
	DiskKindNVMe DiskKind = "nvme"
	// DiskKindOtherSSD identifies a non-rotational disk using another transport.
	DiskKindOtherSSD DiskKind = "other-ssd"
)

// Kind classifies the disk by its rotational state and transport.
func (d Disk) Kind() DiskKind {
	switch {
	case d.Rotational:
		return DiskKindHDD
	case strings.EqualFold(d.Transport, "nvme") || strings.HasPrefix(d.Device, "nvme"):
		return DiskKindNVMe
	case strings.EqualFold(d.Transport, "ata"):
		return DiskKindSATASSD
	default:
		return DiskKindOtherSSD
	}
}

// IsExternal reports whether Unraid exposes the disk through USB transport.
func (d Disk) IsExternal() bool {
	return strings.EqualFold(d.Transport, "usb")
}

// HBA describes a host bus adapter by its backend-independent stable identity.
type HBA struct {
	ID         string  `json:"id"`
	Model      string  `json:"model,omitempty"`
	PCIAddress string  `json:"pci_address,omitempty"`
	Temp       float64 `json:"temp_c"`
}

// Response contains a snapshot of every sensor exposed by the server.
type Response struct {
	Version     string    `json:"version"`
	Timestamp   time.Time `json:"timestamp"`
	Disks       []Disk    `json:"disks"`
	HBAs        []HBA     `json:"hbas,omitempty"`
	HBADisabled bool      `json:"hba_disabled,omitempty"`
	HBAError    string    `json:"hba_error,omitempty"`
	Error       string    `json:"error,omitempty"`
}
