// SPDX-License-Identifier: GPL-3.0-or-later
// Package sensors defines the shared sensor model and transport protocol.

package sensors

import "strings"

// ProtocolVersion identifies incompatible revisions of the VSOCK snapshot.
const ProtocolVersion = 1

// Disk describes an Unraid disk and its current safe control temperature.
// Temp=0 with Unavailable=false can be a synthetic standby/waking sentinel;
// it does not represent a physical 0 °C SMART measurement.
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
	// DiskKindSATASSD identifies a non-rotational SATA disk.
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
	case strings.EqualFold(d.Transport, "ata"),
		strings.EqualFold(d.Transport, "scsi-sata"),
		strings.EqualFold(d.Transport, "scsi-1ata"):
		return DiskKindSATASSD
	default:
		return DiskKindOtherSSD
	}
}

// HBA describes a host bus adapter by its backend-independent stable identity.
type HBA struct {
	ID         string  `json:"id"`
	Model      string  `json:"model,omitempty"`
	PCIAddress string  `json:"pci_address,omitempty"`
	Temp       float64 `json:"temp_c"`
}

// Response contains a snapshot of every sensor exposed by the Unraid agent.
// A nil disk or HBA inventory means missing or unavailable data; a non-nil
// empty slice is authoritative and may remove that family's host sensors.
type Response struct {
	Protocol int    `json:"protocol"`
	Disks    []Disk `json:"disks"`
	HBAs     []HBA  `json:"hbas"`
	HBAError string `json:"hba_error,omitempty"`
	Error    string `json:"error,omitempty"`
}
