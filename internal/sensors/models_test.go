package sensors

import "testing"

func TestDiskClassification(t *testing.T) {
	tests := []struct {
		name string
		disk Disk
		want DiskKind
	}{
		{name: "NVMe transport", disk: Disk{Transport: "NVMe"}, want: DiskKindNVMe},
		{name: "NVMe device", disk: Disk{Device: "nvme0n1"}, want: DiskKindNVMe},
		{name: "SATA SSD", disk: Disk{Transport: "ata"}, want: DiskKindSATASSD},
		{name: "other SSD", disk: Disk{Transport: "sas"}, want: DiskKindOtherSSD},
		{name: "rotational disk", disk: Disk{Transport: "ata", Rotational: true}, want: DiskKindHDD},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.disk.Kind(); got != tt.want {
				t.Errorf("Kind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDiskIsExternal(t *testing.T) {
	tests := []struct {
		name string
		disk Disk
		want bool
	}{
		{name: "USB disk", disk: Disk{Transport: "USB"}, want: true},
		{name: "SATA disk", disk: Disk{Transport: "ata"}},
		{name: "NVMe disk", disk: Disk{Transport: "nvme"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.disk.IsExternal(); got != tt.want {
				t.Errorf("IsExternal() = %v, want %v", got, tt.want)
			}
		})
	}
}
