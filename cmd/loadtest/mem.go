package main

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type peakMem struct {
	peak     atomic.Uint64
	fromProc atomic.Bool
}

func (p *peakMem) sample(done <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		n, proc := currentMem()
		if n > p.peak.Load() {
			p.peak.Store(n)
			p.fromProc.Store(proc)
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

func (p *peakMem) load() uint64 { return p.peak.Load() }

func (p *peakMem) method() string {
	if p.fromProc.Load() {
		return "VmRSS from /proc/self/status; includes in-process load clients"
	}
	return "runtime MemStats.Sys; includes in-process load clients"
}

func currentMem() (bytes uint64, fromProc bool) {
	if rss, ok := procRSS(); ok {
		return rss, true
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys, false
}

func procRSS() (uint64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}
