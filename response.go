package main

import "time"

// response is the JSON message sent by the Unraid server to the Proxmox client.
type response struct {
	Timestamp time.Time `json:"timestamp"`
	Disks     []disk    `json:"disks"`
	HBAs      []hba     `json:"hbas,omitempty"`
	HBAError  string    `json:"hba_error,omitempty"`
	Error     string    `json:"error,omitempty"`
}
