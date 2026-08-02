// Package pgconf generates postgresql.conf from an instance size, and knows
// which parameters can be reloaded and which need a restart.
//
// The generated file is a computed base plus the operator's overrides, never
// a file the operator owns outright: that is what keeps the tuning correct
// after a resize, when the base recomputes and the overrides survive.
package pgconf

import (
	"fmt"
	"sort"
	"strings"
)

// Size describes an offered EC2 instance type. Only types listed here can be
// picked, so the tuning maths always has real numbers to work from.
type Size struct {
	Type    string `json:"type"`
	VCPU    int    `json:"vcpu"`
	MemMB   int    `json:"mem_mb"`
	Label   string `json:"label"`
	Purpose string `json:"purpose"`
}

// Sizes is the menu, smallest first.
var Sizes = []Size{
	{"t3.small", 2, 2048, "t3.small · 2 vCPU · 2 GB", "dev and small workloads"},
	{"t3.medium", 2, 4096, "t3.medium · 2 vCPU · 4 GB", "dev and small workloads"},
	{"t3.large", 2, 8192, "t3.large · 2 vCPU · 8 GB", "general purpose"},
	{"m7i.large", 2, 8192, "m7i.large · 2 vCPU · 8 GB", "general purpose"},
	{"m7i.xlarge", 4, 16384, "m7i.xlarge · 4 vCPU · 16 GB", "general purpose"},
	{"m7i.2xlarge", 8, 32768, "m7i.2xlarge · 8 vCPU · 32 GB", "general purpose"},
	{"r7i.large", 2, 16384, "r7i.large · 2 vCPU · 16 GB", "memory optimized"},
	{"r7i.xlarge", 4, 32768, "r7i.xlarge · 4 vCPU · 32 GB", "memory optimized"},
	{"r7i.2xlarge", 8, 65536, "r7i.2xlarge · 8 vCPU · 64 GB", "memory optimized"},
}

func SizeFor(instanceType string) (Size, bool) {
	for _, s := range Sizes {
		if s.Type == instanceType {
			return s, true
		}
	}
	return Size{}, false
}

// restartRequired lists the postmaster-context parameters goku's base touches
// or an operator is likely to reach for. Changing one interrupts connections;
// everything else is a reload.
var restartRequired = map[string]bool{
	"shared_buffers":                 true,
	"max_connections":                true,
	"shared_preload_libraries":       true,
	"listen_addresses":               true,
	"port":                           true,
	"wal_level":                      true,
	"archive_mode":                   true,
	"max_wal_senders":                true,
	"max_replication_slots":          true,
	"max_worker_processes":           true,
	"max_prepared_transactions":      true,
	"max_locks_per_transaction":      true,
	"superuser_reserved_connections": true,
	"huge_pages":                     true,
	"track_activity_query_size":      true,
	"logical_decoding_work_mem":      false,
}

// NeedsRestart reports whether applying these overrides interrupts
// connections, given what was set before.
func NeedsRestart(before, after map[string]string) bool {
	for k, v := range after {
		if before[k] != v && restartRequired[k] {
			return true
		}
	}
	for k := range before {
		if _, still := after[k]; !still && restartRequired[k] {
			return true
		}
	}
	return false
}

// RestartParams is the set the UI marks, so an operator can see the cost of a
// change before making it.
func RestartParams() []string {
	out := []string{}
	for k, v := range restartRequired {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Base computes the tuned defaults for a size. Values are strings because
// that is what postgresql.conf holds and what an override replaces.
func Base(s Size, storageGB int) map[string]string {
	mem := s.MemMB
	maxConns := clamp(mem/64, 50, 500)
	// work_mem is per sort node, so it multiplies by connections: keep the
	// worst case inside a quarter of RAM.
	workMem := clamp(mem/4/maxConns, 4, 64)
	maintenance := clamp(mem/16, 64, 2048)
	// WAL sizing follows the data volume, not RAM — a small disk with a huge
	// max_wal_size is how you run out of space during a bulk load.
	maxWAL := clamp(storageGB*1024/20, 1024, 16384)

	return map[string]string{
		"listen_addresses":                 "'*'",
		"max_connections":                  fmt.Sprint(maxConns),
		"shared_buffers":                   fmt.Sprintf("%dMB", mem/4),
		"effective_cache_size":             fmt.Sprintf("%dMB", mem*3/4),
		"maintenance_work_mem":             fmt.Sprintf("%dMB", maintenance),
		"work_mem":                         fmt.Sprintf("%dMB", workMem),
		"wal_buffers":                      "16MB",
		"min_wal_size":                     "1GB",
		"max_wal_size":                     fmt.Sprintf("%dMB", maxWAL),
		"checkpoint_completion_target":     "0.9",
		"random_page_cost":                 "1.1",
		"effective_io_concurrency":         "200",
		"max_worker_processes":             fmt.Sprint(s.VCPU),
		"max_parallel_workers":             fmt.Sprint(s.VCPU),
		"max_parallel_workers_per_gather":  fmt.Sprint(max(1, s.VCPU/2)),
		"max_parallel_maintenance_workers": fmt.Sprint(max(1, s.VCPU/2)),
		// pg_stat_statements is preloaded from day one: adding it later is a
		// restart, and the stats daemon will want it.
		"shared_preload_libraries":   "'pg_stat_statements'",
		"password_encryption":        "scram-sha-256",
		"log_min_duration_statement": "1000",
		"log_checkpoints":            "on",
		"log_connections":            "off",
		"log_line_prefix":            "'%m [%p] %q%u@%d '",
		"log_timezone":               "'UTC'",
		"timezone":                   "'UTC'",
		"datestyle":                  "'iso, mdy'",
	}
}

// Render produces the file: the computed base with overrides applied, each
// overridden line marked so the operator can see what they changed.
func Render(s Size, storageGB int, overrides map[string]string) string {
	base := Base(s, storageGB)
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range overrides {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	fmt.Fprintf(&b, "# postgresql.conf — generated by goku for %s (%d GB data volume).\n", s.Type, storageGB)
	b.WriteString("# Values marked (override) were set by an operator; the rest are computed\n")
	b.WriteString("# from the instance size and are recomputed when it changes.\n\n")
	for _, name := range names {
		value, overridden := overrides[name]
		if !overridden {
			value = base[name]
		}
		line := fmt.Sprintf("%s = %s", name, value)
		if overridden {
			if was, ok := base[name]; ok {
				fmt.Fprintf(&b, "%-42s # (override, computed was %s)\n", line, was)
				continue
			}
			fmt.Fprintf(&b, "%-42s # (override)\n", line)
			continue
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
