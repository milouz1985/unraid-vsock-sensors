package sensors

import (
	"strings"
	"time"
)

type Disk struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Device     string  `json:"device"`
	Transport  string  `json:"transport,omitempty"`
	Rotational bool    `json:"rotational"`
	Temp       float64 `json:"temp_c"`
}

type DiskKind string

const (
	DiskKindHDD      DiskKind = "hdd"
	DiskKindSATASSD  DiskKind = "ssd"
	DiskKindNVMe     DiskKind = "nvme"
	DiskKindOtherSSD DiskKind = "other-ssd"
)

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

func (d Disk) IsExternal() bool {
	return strings.EqualFold(d.Transport, "usb")
}

type HBA struct {
	Name string  `json:"name"`
	Temp float64 `json:"temp_c"`
}

type Response struct {
	Version   string    `json:"version"`
	Timestamp time.Time `json:"timestamp"`
	Disks     []Disk    `json:"disks"`
	HBAs      []HBA     `json:"hbas,omitempty"`
	HBAError  string    `json:"hba_error,omitempty"`
	Error     string    `json:"error,omitempty"`
}
