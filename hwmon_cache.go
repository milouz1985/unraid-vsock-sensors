package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// cachedHWMonInventory is the versioned on-disk representation of the virtual
// hwmon topology. A nil family means that it has never been initialized, which
// is different from an initialized family containing no sensors.
type cachedHWMonInventory struct {
	Version int                `json:"version"`
	Disks   *cachedHWMonFamily `json:"disks,omitempty"`
	HBAs    *cachedHWMonFamily `json:"hbas,omitempty"`
}

// cachedHWMonFamily wraps a family's sensors instead of storing a pointer to a
// slice directly. Besides keeping the absent-versus-empty distinction explicit,
// the object can later carry family-level metadata without changing its JSON
// shape again.
type cachedHWMonFamily struct {
	Sensors []cachedHWMonSensor `json:"readings"`
}

// cachedHWMonSensor contains only the stable metadata needed to recreate a
// virtual sensor. Temperatures are deliberately not persisted: restored sensors
// start at the failsafe temperature until fresh data arrives from the guest.
type cachedHWMonSensor struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func loadHWMonCache(path string) (*cachedHWMonInventory, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cached cachedHWMonInventory
	// Ignore retired optional metadata so caches written by an older release
	// remain usable when their cache version is still supported.
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if cached.Version != 1 {
		return nil, fmt.Errorf("unsupported cache version %d", cached.Version)
	}
	return &cached, nil
}

// restore loads the last known topology and recreates its virtual hwmon
// devices at the failsafe temperature. Families are applied in order, so a
// family restored before a later validation failure remains usable.
func (publisher *hwmonPublisher) restore(device string) error {
	cached, err := loadHWMonCache(publisher.cachePath)
	if err != nil || cached == nil {
		return err
	}
	if cached.Disks != nil {
		readings := samplesFromCache(cached.Disks.Sensors)
		if _, err := publishHWMonFamily(device, "disk", &publisher.disks, readings); err != nil {
			return fmt.Errorf("restore disks: %w", err)
		}
	}
	if cached.HBAs != nil {
		readings := samplesFromCache(cached.HBAs.Sensors)
		if _, err := publishHWMonFamily(device, "hba", &publisher.hbas, readings); err != nil {
			return fmt.Errorf("restore HBA: %w", err)
		}
	}
	log.Printf("restored cached hwmon inventory from %s", publisher.cachePath)
	return nil
}

// saveCache atomically persists every initialized family. The temporary file,
// its contents, and the containing directory are synced so a successful return
// means the new topology survives a crash or power loss.
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

// syncDirectory makes the preceding atomic rename durable on filesystems that
// require the parent directory itself to be flushed.
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

// sensorsToCache copies the stable topology fields out of the live inventory.
func sensorsToCache(sensors []hwmonSensor) []cachedHWMonSensor {
	cached := make([]cachedHWMonSensor, 0, len(sensors))
	for _, sensor := range sensors {
		cached = append(cached, cachedHWMonSensor{ID: sensor.id, Label: sensor.label})
	}
	return cached
}

// samplesFromCache converts cached topology into publishable samples. Every
// restored sample starts at the failsafe temperature because cached topology
// must never be mistaken for a fresh reading from the guest.
func samplesFromCache(cached []cachedHWMonSensor) []hwmonSample {
	readings := make([]hwmonSample, 0, len(cached))
	for _, reading := range cached {
		readings = append(readings, hwmonSample{
			sensor:      hwmonSensor{id: reading.ID, label: reading.Label},
			temperature: hwmonFailsafeTemp,
		})
	}
	return readings
}
