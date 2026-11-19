package machines

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

type workerStats struct {
	CPU *cpuCounters `json:"cpu,omitempty"`
	RAM *ramStats    `json:"ram,omitempty"`
}

type cpuCounters struct {
	Busy  float64 `json:"busy"`
	Total float64 `json:"total"`
}

type ramStats struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

func readWorkerStats(ctx context.Context) *workerStats {
	var stats workerStats
	// Send cumulative counters. The controller measures the delta between
	// polls, so the worker command does not wait to take a second reading.
	if times, err := cpu.TimesWithContext(ctx, false); err == nil && len(times) > 0 {
		t := times[0]
		busy := t.User + t.Nice + t.System + t.Irq + t.Softirq + t.Steal
		stats.CPU = &cpuCounters{Busy: busy, Total: busy + t.Idle + t.Iowait}
	}
	if memory, err := mem.VirtualMemoryWithContext(ctx); err == nil && memory != nil && memory.Total > 0 && memory.Available <= memory.Total {
		stats.RAM = &ramStats{Used: memory.Total - memory.Available, Total: memory.Total}
	}
	if stats.CPU == nil && stats.RAM == nil {
		return nil
	}
	return &stats
}

type workerReading struct {
	cpu     *cpuCounters
	summary string
	stale   bool
}

func (r workerReading) update(stats *workerStats) workerReading {
	if stats == nil || (stats.CPU == nil && stats.RAM == nil) {
		return workerReading{summary: r.summary, stale: true}
	}
	var parts []string
	if stats.CPU != nil && r.cpu != nil {
		total := stats.CPU.Total - r.cpu.Total
		busy := stats.CPU.Busy - r.cpu.Busy
		if total > 0 && busy >= 0 && busy <= total {
			parts = append(parts, fmt.Sprintf("CPU %.0f%%", 100*(busy/total)))
		}
	}
	if ram := stats.RAM; ram != nil && ram.Total > 0 && ram.Used <= ram.Total {
		parts = append(parts, "RAM "+gibibytes(ram.Used)+"/"+gibibytes(ram.Total)+" GiB")
	}
	return workerReading{cpu: stats.CPU, summary: strings.Join(parts, " · ")}
}

func gibibytes(bytes uint64) string {
	return strconv.FormatFloat(math.Round(float64(bytes)*10/(1<<30))/10, 'f', -1, 64)
}
