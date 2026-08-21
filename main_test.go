package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAndSelect(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disks.ini")
	data := "[\"disk1\"]\nname=\"disk1\"\ndevice=\"sdb\"\ntemp=\"35\"\nrotational=\"1\"\ntransport=\"ata\"\n\n[\"fast\"]\ndevice=\"nvme0n1\"\ntemp=\"48\"\nrotational=\"0\"\ntransport=\"nvme\"\n\n[\"disk2\"]\ndevice=\"sdc\"\ntemp=\"*\"\nrotational=\"1\"\n"
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	disks, err := readDisks(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("got %d disks", len(disks))
	}
	if got := selectDisks(disks, "hdd"); len(got) != 1 || got[0].Temp != 35 {
		t.Fatalf("hdd: %#v", got)
	}
	if got := selectDisks(disks, "nvme"); len(got) != 1 || got[0].Temp != 48 {
		t.Fatalf("nvme: %#v", got)
	}
	if got := selectDisks(disks, "fast"); len(got) != 1 || got[0].Device != "nvme0n1" {
		t.Fatalf("name: %#v", got)
	}
}

func TestParseStorCLI(t *testing.T) {
	data := []byte(`{"Controllers":[
		{"Command Status":{"CLI Version":"007.3404.0000.0000 April 18, 2025","Operating system":"Linux 6.18.38-Unraid","Controller":0,"Status":"Success","Description":"None"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"49"}]}},
		{"Command Status":{"Controller":1,"Status":"Success"},"Response Data":{"Controller Properties":[{"Ctrl_Prop":"ROC temperature(Degree Celsius)","Value":"60"}]}}
	]}`)
	hbas, err := parseStorCLI(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(hbas) != 2 || hbas[0].Name != "hba0" || hbas[0].Temp != 49 || hbas[1].Temp != 60 {
		t.Fatalf("unexpected HBA readings: %#v", hbas)
	}
	if got := selectHBAs(hbas, "hba"); len(got) != 2 {
		t.Fatalf("hba selector: %#v", got)
	}
	if got := selectHBAs(hbas, "hba1"); len(got) != 1 || got[0].Temp != 60 {
		t.Fatalf("hba1 selector: %#v", got)
	}
}

func TestParseStorCLIRejectsUnexpectedOutput(t *testing.T) {
	data := []byte(`{"Controllers":[{"Command Status":{"Controller":0,"Status":"Failure"},"Response Data":{}}]}`)
	if _, err := parseStorCLI(data); err == nil {
		t.Fatal("expected failed controller status to be rejected")
	}
}
