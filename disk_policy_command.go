// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"unicode/utf8"
)

type diskPolicyRow struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Device    string     `json:"device"`
	Transport string     `json:"transport"`
	Bus       diskBus    `json:"bus"`
	Policy    diskPolicy `json:"policy"`
	Included  bool       `json:"included"`
}

func diskPolicyCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("disks requires list or set")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("disks list", flag.ContinueOnError)
		disksINI := fs.String("disks-ini", defaultDisksINIPath, "assigned disk inventory")
		devsINI := fs.String("devs-ini", defaultDevsINIPath, "unassigned disk inventory")
		sysBlockRoot := fs.String("sys-block-root", defaultSysBlockRoot, "sysfs block root")
		policyFile := fs.String("policy-file", defaultDiskPolicyFile, "persistent disk policies")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("disks list does not accept positional arguments")
		}
		policies, err := readDiskPolicies(*policyFile)
		if err != nil {
			return err
		}
		selector := &diskSelector{sysBlockRoot: *sysBlockRoot, policies: policies}
		entries, err := readDiskInventoryEntries(*disksINI, *devsINI, selector, false)
		if err != nil {
			return err
		}
		rows := make([]diskPolicyRow, 0, len(entries))
		for _, entry := range entries {
			rows = append(rows, diskPolicyRow{
				ID: entry.disk.id, Name: entry.disk.name, Device: entry.disk.device,
				Transport: entry.disk.transport, Bus: entry.bus, Policy: entry.policy,
				Included: entry.included,
			})
		}
		return json.NewEncoder(output).Encode(rows)
	case "set":
		fs := flag.NewFlagSet("disks set", flag.ContinueOnError)
		encodedID := fs.String("id-base64", "", "base64-encoded stable Unraid disk ID")
		policy := fs.String("policy", "", "auto, include or exclude")
		policyFile := fs.String("policy-file", defaultDiskPolicyFile, "persistent disk policies")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("disks set does not accept positional arguments")
		}
		idBytes, err := base64.StdEncoding.Strict().DecodeString(*encodedID)
		if err != nil || !utf8.Valid(idBytes) {
			return errors.New("invalid base64 disk ID")
		}
		if err := writeDiskPolicy(*policyFile, string(idBytes), diskPolicy(*policy)); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "disk policy saved")
		return err
	default:
		return fmt.Errorf("unknown disks command %q", args[0])
	}
}
