// Copyright 2019-2022 The Inspektor Gadget authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tracer

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inspektor-gadget/inspektor-gadget/pkg/bpfstats"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets/top/ebpf/piditer"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets/top/ebpf/types"

	"github.com/cilium/ebpf"
)

type Config struct {
	MaxRows  int
	Interval time.Duration
	SortBy   types.SortBy
}

type programStats struct {
	runtime  int64
	runCount uint64
}

type Tracer struct {
	config        *Config
	eventCallback func(*types.Event)
	done          chan bool

	iter                *piditer.PidIter
	useFallbackIterator bool

	startStats map[string]programStats
	prevStats  map[string]programStats
}

func NewTracer(config *Config, eventCallback func(*types.Event),
) (*Tracer, error) {
	t := &Tracer{
		config:        config,
		eventCallback: eventCallback,
		done:          make(chan bool),
		prevStats:     make(map[string]programStats),
	}

	if err := t.start(); err != nil {
		t.Stop()
		return nil, err
	}

	t.run()

	return t, nil
}

func (t *Tracer) start() error {
	// Enable stats collection
	err := bpfstats.EnableBPFStats()
	if err != nil {
		return err
	}

	t.useFallbackIterator = false

	// To resolve pids, we will first try to iterate using a bpf
	// program. If that doesn't work, we will fall back to scanning
	// all used fds in all processes /proc/$pid/fdinfo/$fd.
	iter, err := piditer.NewTracer()
	if err != nil {
		t.useFallbackIterator = true
	} else {
		t.iter = iter
	}

	return nil
}

func (t *Tracer) Stop() {
	close(t.done)

	if t.iter != nil {
		t.iter.Close()
	}

	bpfstats.DisableBPFStats()
}

func getPidMapFromPids(pids []*piditer.PidIterEntry) map[uint32][]*types.PidInfo {
	pidmap := make(map[uint32][]*types.PidInfo)
	for _, e := range pids {
		if _, ok := pidmap[e.ProgID]; !ok {
			pidmap[e.ProgID] = make([]*types.PidInfo, 0, 1)
		}
		pidmap[e.ProgID] = append(pidmap[e.ProgID], &types.PidInfo{
			Pid:  e.Pid,
			Comm: e.Comm,
		})
	}
	return pidmap
}

func getProgIDFromFile(fn string) (uint32, error) {
	f, err := os.Open(fn)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "prog_id:") {
			progID, _ := strconv.ParseUint(strings.TrimSpace(strings.Split(sc.Text(), ":")[1]), 10, 32)
			return uint32(progID), nil
		}
	}
	return 0, os.ErrNotExist
}

func getPidMapFromProcFs() (map[uint32][]*types.PidInfo, error) {
	processes, err := os.ReadDir("/proc/")
	if err != nil {
		return nil, err
	}
	pidmap := make(map[uint32][]*types.PidInfo)
	for _, p := range processes {
		if !p.IsDir() {
			continue
		}
		_, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fdescs, err := os.ReadDir(filepath.Join("/proc", p.Name(), "fdinfo"))
		if err != nil {
			continue
		}
		for _, fd := range fdescs {
			if progID, err := getProgIDFromFile(filepath.Join("/proc", p.Name(), "fdinfo", fd.Name())); err == nil {
				pid, _ := strconv.ParseUint(p.Name(), 10, 32)
				if _, ok := pidmap[progID]; !ok {
					pidmap[progID] = make([]*types.PidInfo, 0, 1)
				}
				comm, _ := os.ReadFile(filepath.Join("/proc", p.Name(), "comm"))
				pidmap[progID] = append(pidmap[progID], &types.PidInfo{
					Pid:  uint32(pid),
					Comm: strings.TrimSpace(string(comm)),
				})
			}
		}
	}
	return pidmap, nil
}

func (t *Tracer) nextStats() ([]types.Stats, error) {
	stats := make([]types.Stats, 0)

	var err error
	var prog *ebpf.Program
	var mapData *ebpf.Map
	var pids []*piditer.PidIterEntry
	curID := ebpf.ProgramID(0)
	nextID := ebpf.ProgramID(0)

	curMapID := ebpf.MapID(0)
	nextMapID := ebpf.MapID(0)

	curStats := make(map[string]programStats)

	mapSizes := make(map[ebpf.MapID]uint64)

	// Get memory usage by maps
	for {
		nextMapID, err = ebpf.MapGetNextID(curMapID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			return nil, fmt.Errorf("could not get next map ID: %w", err)
		}
		if nextMapID <= curMapID {
			break
		}
		curMapID = nextMapID
		mapData, err = ebpf.NewMapFromID(curMapID)
		if err != nil {
			continue
		}
		mapSizes[curMapID] = (uint64(mapData.KeySize()) + uint64(mapData.ValueSize())) * uint64(mapData.MaxEntries())

		mapData.Close()
	}

	for {
		nextID, err = ebpf.ProgramGetNextID(curID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			return nil, fmt.Errorf("could not get next program ID: %w", err)
		}
		if nextID <= curID {
			break
		}
		curID = nextID
		prog, err = ebpf.NewProgramFromID(curID)
		if err != nil {
			continue
		}
		pi, err := prog.Info()
		if err != nil {
			prog.Close()
			continue
		}

		totalMapMemory := uint64(0)
		mapIDs, _ := pi.MapIDs()
		for _, mapID := range mapIDs {
			if size, ok := mapSizes[mapID]; ok {
				totalMapMemory += size
			}
		}

		totalRuntime, _ := pi.Runtime()
		totalRunCount, _ := pi.RunCount()

		curRuntime := int64(0)
		curRunCount := uint64(0)
		cumulativeRuntime := int64(0)
		cumulativeRunCount := uint64(0)

		pkey := fmt.Sprintf("%d-%s", curID, pi.Tag)

		// calculate delta, if possible
		if old, ok := t.prevStats[pkey]; ok {
			curRuntime = int64(totalRuntime) - old.runtime
			curRunCount = totalRunCount - old.runCount
		}
		if t.startStats != nil {
			if start, ok := t.startStats[pkey]; ok {
				cumulativeRuntime = int64(totalRuntime) - start.runtime
				cumulativeRunCount = totalRunCount - start.runCount
			} else {
				cumulativeRuntime = int64(totalRuntime)
				cumulativeRunCount = totalRunCount
			}
		}

		curStats[pkey] = programStats{
			runtime:  int64(totalRuntime),
			runCount: totalRunCount,
		}

		stats = append(stats, types.Stats{
			ProgramID:          uint32(curID),
			Name:               pi.Name,
			Type:               pi.Type.String(),
			CurrentRuntime:     curRuntime,
			CurrentRunCount:    curRunCount,
			TotalRuntime:       int64(totalRuntime),
			TotalRunCount:      totalRunCount,
			CumulativeRuntime:  cumulativeRuntime,
			CumulativeRunCount: cumulativeRunCount,
			MapMemory:          totalMapMemory,
			MapCount:           uint32(len(mapIDs)),
		})

		prog.Close()
	}

	if t.startStats == nil {
		t.startStats = curStats
	}

	t.prevStats = curStats

	var pidmap map[uint32][]*types.PidInfo

	if !t.useFallbackIterator {
		pids, err = t.iter.DumpPids()
		if err != nil {
			return nil, fmt.Errorf("could not get pids for programs using iterator: %w", err)
		}
		pidmap = getPidMapFromPids(pids)
	} else {
		// Fallback...
		pidmap, err = getPidMapFromProcFs()
		if err != nil {
			return nil, fmt.Errorf("could not get pids for programs using fallback method: %w", err)
		}
	}

	for i := range stats {
		if tmpPids, ok := pidmap[stats[i].ProgramID]; ok {
			stats[i].Pids = tmpPids
		}
	}

	types.SortStats(stats, t.config.SortBy)

	return stats, nil
}

func (t *Tracer) run() {
	timer := time.NewTicker(t.config.Interval)
	go func() {
		for {
			select {
			case <-t.done:
				return
			case <-timer.C:
				stats, err := t.nextStats()
				if err != nil {
					t.eventCallback(&types.Event{
						Error: fmt.Sprintf("could not get next stats: %s", err),
					})
					return
				}

				n := len(stats)
				if n > t.config.MaxRows {
					n = t.config.MaxRows
				}
				t.eventCallback(&types.Event{Stats: stats[:n]})
			}
		}
	}()
}
