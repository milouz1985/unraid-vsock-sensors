// SPDX-License-Identifier: GPL-3.0-or-later

package main

import "log"

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// stickyErrorLog only deduplicates repeated log messages. Runtime error state
// belongs to the component that produced the error.
type stickyErrorLog struct {
	context string
	last    string
}

func (state *stickyErrorLog) update(err error) {
	message := errorText(err)
	if message == state.last {
		return
	}
	state.last = message
	if message == "" {
		log.Printf("%s recovered", state.context)
		return
	}
	log.Printf("%s warning: %s", state.context, message)
}
