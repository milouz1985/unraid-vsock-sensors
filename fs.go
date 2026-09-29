// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"os"
)

// syncDirectory makes a preceding filesystem change durable on filesystems
// that require the parent directory itself to be flushed.
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
