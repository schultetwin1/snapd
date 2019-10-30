// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2020 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/snapcore/snapd/cmd/snaplock/runinhibit"
	"github.com/snapcore/snapd/i18n"
	"github.com/snapcore/snapd/osutil"
)

func inhibitMessage(snapName string, hint runinhibit.Hint) string {
	switch hint {
	case runinhibit.HintInhibitedForRefresh:
		return fmt.Sprintf(i18n.G("snap package %q is being refreshed, please wait"), snapName)
	default:
		return fmt.Sprintf(i18n.G("snap package cannot be used now: %s"), string(hint))
	}
}

func isGraphicalSession() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_SOCKET") != ""
}

func isInteractiveConsole() bool {
	return isStdoutTTY
}

var hasZenityExecutable = func() bool {
	return osutil.ExecutableExists("zenity")
}

func zenityFlow(snapName string, hint runinhibit.Hint) error {
	zenityTitle := i18n.G("snap package cannot be used")

	// Run zenity with a progress bar.
	// TODO: while we are waiting ask snapd for progress updates and send those
	// to zenity via stdin.
	zenityDied := make(chan error, 1)
	cmd := exec.Command(
		"zenity",
		// [generic options]
		"--title="+zenityTitle,
		// [progress options]
		"--progress",
		"--text="+inhibitMessage(snapName, hint),
		"--pulsate",
		"--no-cancel",
	)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Make sure that zenity is eventually terminated.
	defer cmd.Process.Signal(os.Interrupt)
	// Wait for zenity to terminate and store the error code.
	// The way we invoke zenity --progress makes it wait forever.
	// so it will typically be an external operation.
	go func() {
		zenityDied <- cmd.Wait()
	}()

	// Every second check if the inhibition file is still present.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
loop:
	for {
		select {
		case err := <-zenityDied:
			if err != nil {
				fmt.Fprintf(Stderr, "zenity error: %s\n", err)
			}
			break loop
		case <-ticker.C:
			// A second has elapsed, let's check again.
			hint, err := runinhibit.IsLocked(snapName)
			if err != nil {
				return err
			}
			if hint == runinhibit.HintNotInhibited {
				break loop
			}
		}
	}

	return nil
}

func textFlow(snapName string, hint runinhibit.Hint) error {
	fmt.Fprintf(Stdout, "%s\n", inhibitMessage(snapName, hint))
	fmt.Fprintf(Stdout, "%s\n", i18n.G("please wait..."))
	// TODO: display a spinner or something like that
	// Every second check if the inhibition file is still present.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ticker.C:
			// A second has elapsed, let's check again.
			hint, err := runinhibit.IsLocked(snapName)
			if err != nil {
				return err
			}
			if hint == runinhibit.HintNotInhibited {
				break loop
			}
		}
	}
	return nil
}

func headlessFlow(snapName string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ticker.C:
			// A second has elapsed, let's check again.
			hint, err := runinhibit.IsLocked(snapName)
			if err != nil {
				return err
			}
			if hint == runinhibit.HintNotInhibited {
				break loop
			}
		}
	}
	return nil
}

func waitWhileInhibited(snapName string) error {
	hint, err := runinhibit.IsLocked(snapName)
	if err != nil {
		return err
	}
	if hint == runinhibit.HintNotInhibited {
		return nil
	}

	if isGraphicalSession() && hasZenityExecutable() {
		return zenityFlow(snapName, hint)
	}
	if isInteractiveConsole() {
		return textFlow(snapName, hint)
	}
	return headlessFlow(snapName)
}
