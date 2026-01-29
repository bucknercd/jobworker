package cgroups

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	jobCgroupRoot = "/sys/fs/cgroup/jobs"
)

type CgroupManager struct {
	cgPath string
}

type Snapshot struct {
	Path string

	PidsCurrent int
	Procs       []int

	CPUMax    string
	MemoryMax string
	IOMax     string

	MemoryCurrent uint64

	CPUStat map[string]uint64
}

func NewCgroupManager(jobID string) *CgroupManager {
	return &CgroupManager{cgPath: filepath.Join(jobCgroupRoot, jobID)}
}

// Create ensures the parent cgroup delegates controllers, creates the job cgroup,
// applies limits, and returns an FD opened on the job cgroup directory suitable
// for SysProcAttr{UseCgroupFD: true, CgroupFD: fd}.
func (m *CgroupManager) Create(jobID string, limits []string) (int, error) {
	if jobID == "" {
		return -1, fmt.Errorf("jobID required")
	}

	// Basic sanity: cgroup v2 expects this file to exist.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return -1, fmt.Errorf("cgroup v2 not available: %w", err)
	}

	// Ensure root exists.
	if err := os.MkdirAll(jobCgroupRoot, 0o755); err != nil {
		return -1, fmt.Errorf("mkdir %s: %w", jobCgroupRoot, err)
	}

	// Ensure controllers are delegated to children of /jobs.
	// Without this, job cgroups won't have cpu.max/memory.max/io.max/etc.
	if err := ensureDelegatedControllers(jobCgroupRoot, []string{"cpu", "memory", "io", "pids"}); err != nil {
		// Treat this as a hard error: without controller delegation, per-job limits won't exist.
		return -1, err
	}

	// Create the job cgroup dir.
	if err := os.MkdirAll(m.cgPath, 0o755); err != nil {
		return -1, fmt.Errorf("mkdir %s: %w", m.cgPath, err)
	}

	// Apply limits (now the controller files should exist if delegation succeeded).
	if err := applyLimits(m.cgPath, limits); err != nil {
		_ = m.Delete(jobID) // best-effort rollback
		return -1, err
	}

	// Open FD on the job cgroup directory for UseCgroupFD.
	fd, err := syscall.Open(m.cgPath, syscall.O_DIRECTORY|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = m.Delete(jobID)
		return -1, fmt.Errorf("open cgroup dir fd: %w", err)
	}

	return fd, nil
}

// ensureDelegatedControllers ensures that parent has the requested controllers enabled
// in cgroup.subtree_control so children get controller interface files.
//
// IMPORTANT: This often requires root or systemd delegation. If it fails with EPERM,
// you need to run jobworker-server with appropriate privileges or configure systemd
// Delegate=yes for the service and create a delegated slice.
func ensureDelegatedControllers(parent string, want []string) error {
	controllersPath := filepath.Join(parent, "cgroup.controllers")
	subtreePath := filepath.Join(parent, "cgroup.subtree_control")

	ctrlBytes, err := os.ReadFile(controllersPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", controllersPath, err)
	}
	available := make(map[string]bool)
	for _, c := range strings.Fields(string(ctrlBytes)) {
		available[c] = true
	}

	// Only try to enable controllers that are actually available here.
	var enable []string
	for _, c := range want {
		if available[c] {
			enable = append(enable, c)
		}
	}
	if len(enable) == 0 {
		return fmt.Errorf("no requested controllers available under %s (have: %s)", parent, strings.TrimSpace(string(ctrlBytes)))
	}

	// Check what's already enabled to avoid unnecessary writes.
	curBytes, err := os.ReadFile(subtreePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", subtreePath, err)
	}
	cur := make(map[string]bool)
	for _, c := range strings.Fields(string(curBytes)) {
		cur[c] = true
	}

	var toWrite []string
	for _, c := range enable {
		if !cur[c] {
			toWrite = append(toWrite, "+"+c)
		}
	}
	if len(toWrite) == 0 {
		return nil // already delegated
	}

	// cgroup.subtree_control expects space-separated "+controller" tokens.
	payload := strings.Join(toWrite, " ") + "\n"
	if err := os.WriteFile(subtreePath, []byte(payload), 0o644); err != nil {
		// Make the error message extremely actionable.
		return fmt.Errorf(
			"enable controllers on %s failed: %w (tried %q). "+
				"Without delegation, per-job files like cpu.max/memory.max will not appear. "+
				"Run as root or configure systemd delegation (Delegate=yes) for the service.",
			subtreePath, err, strings.TrimSpace(payload),
		)
	}
	return nil
}

// Delete kills remaining tasks (best effort) and removes the job cgroup directory.
// NOTE: if your job is still running and you rely on cmd.Wait() to reap, call Stop() first.
func (m *CgroupManager) Delete(jobID string) error {
	if jobID == "" {
		return fmt.Errorf("jobID required")
	}

	if _, err := os.Stat(m.cgPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat cgroup: %w", err)
	}

	_ = os.WriteFile(filepath.Join(m.cgPath, "cgroup.kill"), []byte("1"), 0o644)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		empty, err := cgroupEmpty(filepath.Join(m.cgPath, "cgroup.procs"))
		if err != nil {
			break
		}
		if empty {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Use RemoveAll because the cgroup dir contains pseudo-files; os.Remove often fails.
	if err := os.RemoveAll(m.cgPath); err != nil {
		return fmt.Errorf("remove cgroup dir %s: %w", m.cgPath, err)
	}
	return nil
}

func applyLimits(cgPath string, limits []string) error {
	for _, raw := range limits {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		k, v, ok := strings.Cut(raw, "=")
		if !ok {
			return fmt.Errorf("invalid limit format %q (expected key=value)", raw)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			return fmt.Errorf("invalid limit %q (empty key or value)", raw)
		}

		switch k {
		case "cpu.max", "memory.max", "io.max":
		default:
			return fmt.Errorf("unsupported cgroup limit key %q", k)
		}

		path := filepath.Join(cgPath, k)

		// Make missing controller files an explicit, helpful error.
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf(
					"cgroup file %s does not exist (controller not delegated?). "+
						"Ensure %s has controllers enabled in cgroup.subtree_control",
					path, jobCgroupRoot,
				)
			}
			return fmt.Errorf("stat %s: %w", path, err)
		}

		if err := os.WriteFile(path, []byte(v+"\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func cgroupEmpty(procsPath string) (bool, error) {
	b, err := os.ReadFile(procsPath)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "", nil
}

func (m *CgroupManager) Snapshot() (*Snapshot, error) {
	s := &Snapshot{Path: m.cgPath, CPUStat: map[string]uint64{}}

	// Membership
	if v, err := readInt(filepath.Join(m.cgPath, "pids.current")); err == nil {
		s.PidsCurrent = v
	}
	if procs, err := readProcs(filepath.Join(m.cgPath, "cgroup.procs")); err == nil {
		s.Procs = procs
	}

	// Limits (read back exactly what kernel sees)
	s.CPUMax, _ = readTrim(filepath.Join(m.cgPath, "cpu.max"))
	s.MemoryMax, _ = readTrim(filepath.Join(m.cgPath, "memory.max"))
	s.IOMax, _ = readTrim(filepath.Join(m.cgPath, "io.max"))

	// Usage
	if v, err := readUint64(filepath.Join(m.cgPath, "memory.current")); err == nil {
		s.MemoryCurrent = v
	}
	if st, err := readKeyVals(filepath.Join(m.cgPath, "cpu.stat")); err == nil {
		s.CPUStat = st
	}

	// If the cgroup doesn’t have controllers enabled, these files won’t exist.
	// Snapshot should still succeed and return partial data.
	return s, nil
}

func readTrim(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func readInt(p string) (int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return n, nil
}

func readUint64(p string) (uint64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func readProcs(p string) ([]int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, line := range strings.Fields(string(b)) {
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func readKeyVals(p string) (map[string]uint64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		out[parts[0]] = v
	}
	return out, nil
}

// Optional helper: detect "file missing" without blowing up logs.
func IsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
