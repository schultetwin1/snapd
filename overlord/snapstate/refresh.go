// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
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

package snapstate

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snaplock"
)

type refreshCheckDelegate interface {
	canAppRunDuringRefresh(app *snap.AppInfo) bool
	canHookRunDuringRefresh(hook *snap.HookInfo) bool
}

func genericRefreshCheck(info *snap.Info, delegate refreshCheckDelegate) error {
	// Grab per-snap lock to prevent new processes from starting. This is
	// sufficient to perform the check, even though individual processes
	// may fork or exit, we will have per-security-tag information about
	// what is running.
	lock, err := snaplock.OpenLock(info.SnapName())
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lock.Lock(); err != nil {
		return err
	}

	var busyAppNames []string
	var busyHookNames []string
	var busyPIDs []int

	for name, app := range info.Apps {
		if delegate.canAppRunDuringRefresh(app) {
			continue
		}
		PIDs, err := pidsOfSecurityTag(app.SecurityTag())
		if err != nil {
			return err
		}
		if len(PIDs) > 0 {
			busyAppNames = append(busyAppNames, name)
			busyPIDs = append(busyPIDs, PIDs...)
		}
	}

	for name, hook := range info.Hooks {
		if delegate.canHookRunDuringRefresh(hook) {
			continue
		}
		PIDs, err := pidsOfSecurityTag(hook.SecurityTag())
		if err != nil {
			return err
		}
		if len(PIDs) > 0 {
			busyHookNames = append(busyHookNames, name)
			busyPIDs = append(busyPIDs, PIDs...)
		}
	}
	if len(busyAppNames) == 0 && len(busyHookNames) == 0 {
		return nil
	}
	sort.Strings(busyAppNames)
	sort.Strings(busyHookNames)
	sort.Ints(busyPIDs)
	return &BusySnapError{
		snapName:      info.SnapName(),
		busyAppNames:  busyAppNames,
		busyHookNames: busyHookNames,
		pids:          busyPIDs,
	}
}

// SoftNothingRunningRefreshCheck looks if there are at most only service processes alive.
//
// The check is designed to run early in the refresh pipeline. Before
// downloading or stopping services for the update, we can check that only
// services are running, that is, that no non-service apps or hooks are
// currently running.
//
// Since services are stopped during the update this provides a good early
// precondition check.  The check is also deliberately racy, both at the level
// of processes forking and exiting and at the level of snap-confine launching
// new commands. The hard check needs to be synchronized but the soft check
// doesn't require this since it would serve no purpose. After the soft check
// passes the user is free to start snap applications and block the hard check.
func SoftNothingRunningRefreshCheck(info *snap.Info) error {
	return genericRefreshCheck(info, &softRefreshCheckDelegate{})
}

type softRefreshCheckDelegate struct{}

func (*softRefreshCheckDelegate) canAppRunDuringRefresh(app *snap.AppInfo) bool {
	return app.IsService()
}

func (*softRefreshCheckDelegate) canHookRunDuringRefresh(hook *snap.HookInfo) bool {
	return false
}

// HardNothingRunningRefreshCheck looks if there are any processes alive.
//
// The check is designed to run late in the refresh pipeline, after stopping
// snap services. At this point services should be stopped, hooks should no
// longer run, and applications should be barred from running externally (e.g.
// by grabbing the per-snap lock around that phase of the update).
//
// The check looks at the set of PIDs in the freezer cgroup associated with a
// given snap. Presence of any processes indicates that a snap is busy and
// refresh cannot proceed.
func HardNothingRunningRefreshCheck(info *snap.Info) error {
	return genericRefreshCheck(info, &hardRefreshCheckDelegate{})
}

type hardRefreshCheckDelegate struct{}

func (*hardRefreshCheckDelegate) canAppRunDuringRefresh(app *snap.AppInfo) bool {
	// TODO: use a constant instead of "endure"
	return app.IsService() && app.RefreshMode == "endure"
}

func (*hardRefreshCheckDelegate) canHookRunDuringRefresh(hook *snap.HookInfo) bool {
	return false
}

// BusySnapError indicates that snap has apps or hooks running and cannot refresh.
type BusySnapError struct {
	snapName      string
	pids          []int
	busyAppNames  []string
	busyHookNames []string
}

// Error formats an error string describing what is running.
func (err *BusySnapError) Error() string {
	switch {
	case len(err.busyAppNames) > 0 && len(err.busyHookNames) > 0:
		return fmt.Sprintf("snap %q has running apps (%s) and hooks (%s)",
			err.snapName, strings.Join(err.busyAppNames, ", "), strings.Join(err.busyHookNames, ", "))
	case len(err.busyAppNames) > 0:
		return fmt.Sprintf("snap %q has running apps (%s)",
			err.snapName, strings.Join(err.busyAppNames, ", "))
	case len(err.busyHookNames) > 0:
		return fmt.Sprintf("snap %q has running hooks (%s)",
			err.snapName, strings.Join(err.busyHookNames, ", "))
	default:
		return fmt.Sprintf("snap %q has running apps or hooks", err.snapName)
	}
}

// Pids returns the set of process identifiers that are running.
//
// Since this list is a snapshot it should be only acted upon if there is an
// external synchronization system applied (e.g. all processes are frozen) at
// the time the snapshot was taken.
//
// The list is intended for snapd to forcefully kill all processes for a forced
// refresh scenario.
func (err BusySnapError) Pids() []int {
	return err.pids
}

// parsePid parses a string as a process identifier.
func parsePid(text string) (int, error) {
	pid, err := strconv.Atoi(text)
	if err == nil && pid <= 0 {
		return 0, fmt.Errorf("cannot parse pid %q", text)
	}
	return pid, err
}

// parsePids parses a list of pids, one per line, from a reader.
func parsePids(reader io.Reader) ([]int, error) {
	scanner := bufio.NewScanner(reader)
	var pids []int
	for scanner.Scan() {
		s := scanner.Text()
		pid, err := parsePid(s)
		if err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return pids, nil
}

// pidsOfSecurityTag returns a list of PIDs belonging to a given security tag.
//
// The list is obtained from a pids cgroup.
func pidsOfSecurityTag(securityTag string) ([]int, error) {
	fname := filepath.Join(dirs.PidsCgroupDir, securityTag, "cgroup.procs")
	file, err := os.Open(fname)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parsePids(bufio.NewReader(file))
}
