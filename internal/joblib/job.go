package joblib

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/bucknercd/jobworker/internal/cgroups"
)

type Status int32

const (
	maxLogDumpBytes = 64 * 1024 // 64KB per stream; tune as you like
)

const (
	StatusUnknown Status = iota // Initial state, before job is started
	StatusStarted               // Job has been started
	StatusRunning               // Job is currently running
	StatusExited                // Job has exited cleanly (any exit code)
	StatusStopped               // Job has been stopped
	StatusFailed                // Job has failed (e.g. cgroup setup failure, process start failure, etc.)
)

func (s Status) String() string {
	switch s {
	case StatusStarted:
		return "started"
	case StatusRunning:
		return "running"
	case StatusExited:
		return "exited"
	case StatusStopped:
		return "stopped"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

const (
	exitCodeUnknown        = -10 // when the exit code is not known
	exitCodeFailedToStart  = -11 // when the job fails to start
	exitCodeFailedCgroup   = -12 // when the cgroup setup fails
	exitCodeKilledBySignal = -13 // when the job is killed by a signal
)

const (
	jobsBaseDir    = "/var/lib/jobs"
	stdoutFilename = "stdout.log"
	stderrFilename = "stderr.log"
	chrootDir      = "/opt/jobroot"
)

// Job is a concrete job instance. We deliberately do NOT expose
// channels here; consumers should stream from the persisted files.
type Job struct {
	id     string
	cmd    *exec.Cmd
	limits []string
	log    *log.Logger

	cgManager  *cgroups.CgroupManager
	jobsDir    string
	stdoutPath string
	stderrPath string
	stdoutFile *os.File
	stderrFile *os.File

	status   int32
	exitCode int32
	stopped  atomic.Bool
	waitOnce sync.Once
	doneCh   chan struct{}
}

// NewJob creates a new Job instance with the given parameters.
// It initializes the job directory and log files, but does not start the job.
func NewJob(id, command string, args []string, limits []string, logger *log.Logger) (*Job, error) {
	if id == "" {
		return nil, errors.New("job id required")
	}
	if command == "" {
		return nil, errors.New("Command required")
	}

	job := &Job{
		id:      id,
		log:     logger,
		cmd:     exec.Command(command, args...),
		limits:  limits,
		doneCh:  make(chan struct{}),
		jobsDir: filepath.Join(jobsBaseDir, id),
	}

	job.stdoutPath = filepath.Join(job.jobsDir, stdoutFilename)
	job.stderrPath = filepath.Join(job.jobsDir, stderrFilename)
	job.setStatus(StatusUnknown)
	job.exitCode = exitCodeUnknown

	return job, nil
}

// ===== Public getters =====
func (j *Job) StdoutPath() string    { return j.stdoutPath }
func (j *Job) StderrPath() string    { return j.stderrPath }
func (j *Job) Done() <-chan struct{} { return j.doneCh }
func (j *Job) ID() string            { return j.id }
func (j *Job) Status() Status        { return Status(atomic.LoadInt32(&j.status)) }
func (j *Job) ExitCode() int32       { return atomic.LoadInt32(&j.exitCode) }

// ===== Public methods =====

// Start initializes the job, creates the cgroup, and starts the process.
func (j *Job) Start() error {
	if !j.tryTransition(StatusUnknown, StatusStarted) {
		return fmt.Errorf("cannot start job %s: current status=%s", j.id, j.Status())
	}

	j.cgManager = cgroups.NewCgroupManager(j.id)

	cgroupFD, err := j.cgManager.Create(j.id, j.limits)
	if err != nil {
		if delErr := j.cgManager.Delete(j.id); delErr != nil {
			j.log.Printf("failed to delete cgroup for job %s", j.id)
		}
		return j.failStart("failed to create cgroup: %v", exitCodeFailedCgroup, StatusFailed, err)
	}

	if err := j.prepareJobFilesystem(); err != nil {
		return j.failStart("failed to prepare filesystem", exitCodeFailedToStart, StatusFailed, err)
	}

	j.cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    cgroupFD, // directory FD for cgroup
		//Chroot:      chrootDir,  // Not chroot for now; can enable later

		// Drop privileges to nobody:nogroup
		Credential: &syscall.Credential{
			Uid: 65534,
			Gid: 65534,
		},
		Pdeathsig: syscall.SIGKILL, // kill child if parent dies
		Setpgid:   true,            // set process group ID to its own PID
	}

	if err := j.cmd.Start(); err != nil {
		if delErr := j.cgManager.Delete(j.id); delErr != nil {
			j.log.Printf("failed to delete cgroup for job %s", j.id)
		}
		return j.failStart("failed to start target", exitCodeFailedToStart, StatusFailed, err)
	}

	pid := -1
	if j.cmd.Process != nil {
		pid = j.cmd.Process.Pid
	}

	if snap, err := j.cgManager.Snapshot(); err != nil {
		j.log.Printf("[cgroup] job=%s snapshot failed: %v", j.id, err)
	} else {
		j.log.Printf(
			"[cgroup] job=%s pid=%d path=%s pids.current=%d procs=%v cpu.max=%q mem.max=%q io.max=%q mem.current=%dB cpu.usage_usec=%d throttled=%d throttled_usec=%d",
			j.id,
			pid,
			snap.Path,
			snap.PidsCurrent,
			firstNInts(snap.Procs, 4),
			snap.CPUMax,
			snap.MemoryMax,
			snap.IOMax,
			snap.MemoryCurrent,
			snap.CPUStat["usage_usec"],
			snap.CPUStat["nr_throttled"],
			snap.CPUStat["throttled_usec"],
		)
	}

	if !j.tryTransition(StatusStarted, StatusRunning) {
		j.log.Printf("job %s was unable to transition to StatusRunning state", j.id)
	}

	j.log.Printf("job %s: started: %s", j.id, j.cmd.String())

	go j.waitForExit()
	return nil
}

func (j *Job) Wait() {
	<-j.doneCh
}

func (j *Job) Stop() error {
	if !j.stopped.CompareAndSwap(false, true) {
		return nil
	}

	var errs []error

	// Attempt to kill the process or its process group
	if j.cmd != nil && j.cmd.Process != nil {
		pgid, errPgid := syscall.Getpgid(j.cmd.Process.Pid)
		if errPgid == nil {
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
				j.log.Printf("failed to kill process group for job %s: %v", j.id, err)
				errs = append(errs, fmt.Errorf("kill pgid: %w", err))
			}
		} else {
			if err := j.cmd.Process.Kill(); err != nil {
				j.log.Printf("failed to kill process for job %s: %v", j.id, err)
				errs = append(errs, fmt.Errorf("kill process: %w", err))
			}
		}
	}

	// Attempt to clean up cgroup
	if j.cgManager != nil {
		if err := j.cgManager.Delete(j.id); err != nil {
			j.log.Printf("failed to cleanup cgroup for job %s: %v", j.id, err)
			errs = append(errs, fmt.Errorf("cleanup cgroup: %w", err))
		} else {
			j.log.Printf("Deleted cgroup for job %s", j.id)
		}
	}

	j.setStatus(StatusStopped)

	j.waitOnce.Do(j.doWait)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// ===== Private methods =====

func (j *Job) failStart(reason string, code int32, status Status, err error) error {
	// Force status -> Failed (or whatever 'status' you pass), regardless of current state.
	j.setStatus(status)

	j.setExitCode(code)
	j.log.Printf("%s: %v", reason, err)

	// Ensure Waiters don't hang if Start fails before waitForExit goroutine runs
	j.waitOnce.Do(func() {
		defer close(j.doneCh)

		// close log files if they were opened
		if cerr := j.closeLogFiles(); cerr != nil {
			j.log.Printf("job %s: error closing log files during failStart: %v", j.id, cerr)
		}

		// best-effort cgroup cleanup
		if j.cgManager != nil {
			if derr := j.cgManager.Delete(j.id); derr != nil {
				j.log.Printf("job %s: cgroup cleanup failed during failStart: %v", j.id, derr)
			}
		}
	})

	return fmt.Errorf("%s: %w", reason, err)
}

func (j *Job) prepareJobFilesystem() error {
	if err := os.MkdirAll(j.jobsDir, 0755); err != nil {
		return fmt.Errorf("failed to create job dir %s: %w", j.jobsDir, err)
	}

	stdoutFile, err := os.OpenFile(j.stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return fmt.Errorf("failed to open stdout file: %w", err)
	}
	j.stdoutFile = stdoutFile

	stderrFile, err := os.OpenFile(j.stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return fmt.Errorf("failed to open stderr file: %w", err)
	}
	j.stderrFile = stderrFile

	j.cmd.Stdout = j.stdoutFile
	j.cmd.Stderr = j.stderrFile
	j.cmd.Stdin = nil

	return nil
}

func (j *Job) setStatus(s Status) {
	atomic.StoreInt32(&j.status, int32(s))
}

func (j *Job) setExitCode(exitCode int32) {
	atomic.StoreInt32(&j.exitCode, exitCode)
}

func (j *Job) tryTransition(from, to Status) bool {
	ok := atomic.CompareAndSwapInt32(&j.status, int32(from), int32(to))
	if !ok {
		j.log.Printf("failed status transition: %v -> %v", from, to)
	} else {
		j.log.Printf("successfully made status transition: %v -> %v", from, to)
	}
	return ok
}

func (j *Job) getExitCodeFromError(err error) int32 {
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ProcessState == nil {
		j.log.Printf("job %s exited with unexpected/unknown error: %v", j.id, err)
		return exitCodeUnknown
	}

	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		sig := status.Signal()
		j.log.Printf("job %s was terminated by signal: %s", j.id, sig.String())
		return exitCodeKilledBySignal
	}

	code := exitErr.ProcessState.ExitCode()
	j.log.Printf("job %s exited with non-zero exit code: %d", j.id, code)
	return int32(code)
}

func (j *Job) closeLogFiles() error {
	var errs []error

	if j.stdoutFile != nil {
		if err := j.stdoutFile.Close(); err != nil {
			j.log.Printf("job %s: error closing stdout file: %v", j.id, err)
			errs = append(errs, fmt.Errorf("closing stdout: %w", err))
		}
	}

	if j.stderrFile != nil {
		if err := j.stderrFile.Close(); err != nil {
			j.log.Printf("job %s: error closing stderr file: %v", j.id, err)
			errs = append(errs, fmt.Errorf("closing stderr: %w", err))
		}
	}

	return errors.Join(errs...)
}

func (j *Job) doWait() {
	defer close(j.doneCh)

	// If we never started successfully, we still must not hang waiters.
	if j.Status() != StatusRunning {
		j.log.Printf("job %s: never made it to StatusRunning (status=%s)", j.id, j.Status())

		// Best-effort cleanup in case we created things before failing.
		if err := j.closeLogFiles(); err != nil {
			j.log.Printf("job %s: error closing log files: %v", j.id, err)
		}
		if j.cgManager != nil {
			if err := j.cgManager.Delete(j.id); err != nil {
				j.log.Printf("job %s: failed to cleanup cgroup: %v", j.id, err)
			}
		}
		return
	}

	waitErr := j.cmd.Wait()

	// Exit handling
	if waitErr != nil {
		j.setExitCode(j.getExitCodeFromError(waitErr))
	} else {
		if j.cmd.ProcessState != nil {
			j.setExitCode(int32(j.cmd.ProcessState.ExitCode()))
			j.log.Printf("job %s exited cleanly (exit code %d)", j.id, j.ExitCode())
		} else {
			j.setExitCode(exitCodeUnknown)
			j.log.Printf("job %s exited cleanly but ProcessState was nil (exit code unknown)", j.id)
		}
	}

	if j.stopped.Load() {
		if j.Status() != StatusStopped {
			j.log.Printf("job %s was externally stopped, overriding status to STOPPED", j.id)
			j.setStatus(StatusStopped)
		}
	} else {
		// If something already marked it failed, don't overwrite.
		if j.Status() != StatusFailed {
			j.setStatus(StatusExited)
		}
	}

	if err := j.closeLogFiles(); err != nil {
		j.log.Printf("job %s: error closing log files: %v", j.id, err)
	}

	// Dump stdout/stderr into server logs (streaming not implemented yet)
	j.dumpLogFileToLogger("STDOUT", j.stdoutPath, maxLogDumpBytes)
	j.dumpLogFileToLogger("STDERR", j.stderrPath, maxLogDumpBytes)

	if j.cgManager != nil {
		if err := j.cgManager.Delete(j.id); err != nil {
			j.log.Printf("job %s: failed to cleanup cgroup: %v", j.id, err)
		}
	}
}

func (j *Job) waitForExit() {
	j.waitOnce.Do(j.doWait)
}

func (j *Job) dumpLogFileToLogger(label, path string, maxBytes int64) {
	data, err := os.ReadFile(path)
	if err != nil {
		j.log.Printf("job %s: failed to read %s log (%s): %v", j.id, label, path, err)
		return
	}

	// Truncate if huge
	if int64(len(data)) > maxBytes {
		// Keep tail (usually most useful)
		data = data[int64(len(data))-maxBytes:]
		j.log.Printf("job %s: %s log truncated to last %d bytes", j.id, label, maxBytes)
	}

	// Avoid logging empty output as noise
	if len(data) == 0 {
		j.log.Printf("job %s: %s log empty", j.id, label)
		return
	}

	// Print with framing. If output has multiple lines, keep it readable.
	j.log.Printf("job %s: ===== BEGIN %s =====", j.id, label)
	j.log.Print(string(data)) // log.Print already adds timestamp/prefix
	j.log.Printf("job %s: ===== END %s =====", j.id, label)
}

func firstNInts(xs []int, n int) []int {
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}
