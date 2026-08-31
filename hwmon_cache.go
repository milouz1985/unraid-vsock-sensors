package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

type cachedHWMonInventory struct {
	Version int                `json:"version"`
	Disks   *cachedHWMonFamily `json:"disks,omitempty"`
	HBAs    *cachedHWMonFamily `json:"hbas,omitempty"`
}

type cachedHWMonFamily struct {
	Sensors []cachedHWMonSensor `json:"readings"`
}

type cachedHWMonSensor struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Members []string `json:"members,omitempty"`
}

func (publisher *hwmonPublisher) restore(device string) (bool, error) {
	if err := os.Chmod(publisher.cachePath, 0600); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	data, err := os.ReadFile(publisher.cachePath)
	if err != nil {
		return false, err
	}
	var cached cachedHWMonInventory
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cached); err != nil {
		return false, fmt.Errorf("decode %s: %w", publisher.cachePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return false, fmt.Errorf("decode %s: unexpected data after inventory", publisher.cachePath)
	}
	if cached.Version != 1 {
		return false, fmt.Errorf("unsupported cache version %d", cached.Version)
	}
	reconfigured := false
	if cached.Disks != nil {
		readings := samplesFromCache(cached.Disks.Sensors)
		changed, err := publishHWMonFamily(device, "disk", &publisher.disks, readings, false)
		if err != nil {
			return reconfigured, fmt.Errorf("restore disks: %w", err)
		}
		reconfigured = reconfigured || changed
	}
	if cached.HBAs != nil {
		readings := samplesFromCache(cached.HBAs.Sensors)
		changed, err := publishHWMonFamily(device, "hba", &publisher.hbas, readings, true)
		if err != nil {
			return reconfigured, fmt.Errorf("restore HBA: %w", err)
		}
		reconfigured = reconfigured || changed
	}
	log.Printf("restored cached hwmon inventory from %s", publisher.cachePath)
	return reconfigured, nil
}

func (publisher *hwmonPublisher) saveCache() error {
	cached := cachedHWMonInventory{Version: 1}
	if publisher.disks.initialized {
		cached.Disks = &cachedHWMonFamily{Sensors: sensorsToCache(publisher.disks.sensors)}
	}
	if publisher.hbas.initialized {
		cached.HBAs = &cachedHWMonFamily{Sensors: sensorsToCache(publisher.hbas.sensors)}
	}
	data, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directoryPath := filepath.Dir(publisher.cachePath)
	if err := os.MkdirAll(directoryPath, 0755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directoryPath, ".hwmon-inventory-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, publisher.cachePath); err != nil {
		return err
	}
	return syncDirectory(directoryPath)
}

func syncDirectory(path string) (err error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, directory.Close())
	}()
	return directory.Sync()
}

func sensorsToCache(sensors []hwmonSensor) []cachedHWMonSensor {
	cached := make([]cachedHWMonSensor, 0, len(sensors))
	for _, sensor := range sensors {
		cached = append(cached, cachedHWMonSensor{
			ID: sensor.id, Label: sensor.label, Members: append([]string(nil), sensor.members...),
		})
	}
	return cached
}

func samplesFromCache(cached []cachedHWMonSensor) []hwmonSample {
	readings := make([]hwmonSample, 0, len(cached))
	for _, reading := range cached {
		readings = append(readings, hwmonSample{
			sensor: hwmonSensor{
				id: reading.ID, label: reading.Label, members: append([]string(nil), reading.Members...),
			},
			temperature: hwmonFailsafeTemp,
		})
	}
	return readings
}
