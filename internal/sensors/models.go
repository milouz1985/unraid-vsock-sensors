package sensors

import "time"

type Disk struct {
	Name       string  `json:"name"`
	Device     string  `json:"device"`
	Transport  string  `json:"transport,omitempty"`
	Rotational bool    `json:"rotational"`
	Temp       float64 `json:"temp_c"`
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
