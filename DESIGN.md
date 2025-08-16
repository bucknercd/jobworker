# Job Worker Service Design Document (Level 5)

## Author

Christopher Buckner

## Goal

Implement a secure gRPC-based job worker service that allows clients to start, stop, monitor, and stream the output of arbitrary Linux processes, with resource isolation via cgroups. The system will use mTLS, and allow multiple concurrent clients to interact with the same job’s output. The system will directly run executables and **not** shell out.

---

## Architecture Overview

```text
                                +----------------------+
                                |      CLI Client      |
                                |                      |
                                | $ jobctl start ...   |
                                +----------+-----------+
                                           |
                                   mTLS-secured gRPC
                                           |
                         +---------------- v-----------------+
                         |         Job Worker Server         |
                         | (gRPC Server + AuthZ Layer)       |
                         +-----------------+-----------------+
                                           |
                                 gRPC Handler + AuthN
                                           |
                             +-------------v--------------+
                             |     Manager Library        |
                             |     Job Controller         |
                             +------+------+--------------+
                                           | 
                             +-------------v-------------+
                             |    Job Library (low level)|
                             |    Core Job functionality |
                             +------+------------+-------+
                                    |            |
                           +--------v-----+    +-v-----------+
                           | Process Ctl  |    | Cgroup Ctl  |
                           |(Start/Stop/  |    | Limit       |
                           |Status/Stream)|    |CPU/Memory/IO|
                           +--------------+    +-------------+
```
---

## Scope

### Included

- Start/stop/query status of arbitrary processes. Note: Will **always** stream messages in chunks from the beginning of the output
- Stream process stdout/stderr. Data will persist across server reboots.
- Semi-persistent storage of job stream info
- Terminate child processes correctly, no orphans or zombies (done via `cgroups` and `setpgid`)
- Set resource limits (CPU, memory, I/O) using cgroups v2 (manual implementation)
- Secure gRPC communication via mTLS (strong cipher suites)
- Simple authorization via client CA (`CN` for username)
- CLI utility to interact with server
- Minimal Unit testing
- Happy path and common error scenarios integration testing
- Basic end to end tests
- All processes running as `nobody:nogroup`
- Job isolation via cgroups and user/group based access control
- Job metadata management (in memory only for now)
- Job ID generation (unique, lexicographically sortable)
- Logging 

### Excluded
  (Due to time constraints)
- Horizontal scalability. I will only implement this in one instance of compute.
- Availability (multiple instances of compute) - This is a single source of failure. If the server goes down, the service goes down or if the server is overloaded, cli experience will suffer.
- Containerization of compute (ie Kubernetes). Using kubernetes is a clean way to implement distributed compute. 
- Persistent storage of client CAs in a database like sqlite
- Configuration - Will use hardcoding as much as possible for very simple configurations server/client  port, limits, denylist and anything else that could come up.
- DDOS Prevention. No rate limiting middleware will be implemented

---

## Security Considerations

- **mTLS** for mutual authentication and transport encryption
  - TLS 1.3 only
  - Strong cipher suite (TLS\_AES\_256\_GCM\_SHA384 preferred)
- **Authorization policy**
  - In the client certificate itself, `CN` will be the `username`
  - A user can **only** can perform operations such as stream output from any job as long as they have the job id 
  - Client CA certificates will be stored in the running server's filesystem rather than in a database, to reduce initial implementation complexity.
- **Shell invocation**
  - No use of shells to run commands; Direct process execution
- **Job isolation**
  - Cgroups used for resource control (custom implementation)
  - Each job gets its own cgroup directory
- **General Hardening**
  - Each process will run a `chroot jail` with `nobody:nogroup` permissions
- **DDOS**
  - I will not implement a rate limiting middleware but am aware one would be beneficial to have. This would prevent resource exhaustion. For example, making many connections and just leaving all of them open by starting a TCP handshake but never closing it.
- **Logging**
  - The server will maintain logs to disk to keep track of all events and to see who has tried to do what to be able to have visibility 
---

## CLI Examples
### Streaming Note
- The `stream` command has `tail` like functionality. This means that if the job is still running, the stream will continue to follow the output in real-time. If the job has exited, the stdout and stderr file descriptors will be closed and the stream will end.
### Limits Default
- If no limits are specified, the job will run with default cgroup limits:
  - CPU: 500m (500 milli-cores)
  - Memory: 100M
  - I/O: low
```bash
# Start a job
jobctl start ls / -lah

# Start a job with resource limits
# all limits are optional
jobctl start echo hello world \
  --cpu=500m \
  --mem=100M \
  --io=low

# Stream the output (stdout)
# id is required (positional argument)
jobctl stream  xxxxxxxx

# Stream the output (stderr)
# id is required (positional argument), --stderr flag is optional
jobctl stream xxxxxxxx --stderr

# Get job status
# id is required (positional argument)
jobctl status xxxxxxxx

# Stop a job
# id is required (positional argument)
jobctl stop xxxxxxxx
```

---
## Component Overview

### 1. `joblib/manager` (High Level Library)

| Function                                        | Description                                             |
| ----------------------------------------------- | ------------------------------------------------------- |
| `StartJob(cmd []string, limits ResourceLimits)` | Generates an id and creates, then starts a job with cgroup limits via `joblib`. Update internal metadata and returns a jobid. |
| `StopJob(id string)`                         | Stops job via `joblib`. Makes sure all cgroup resources are released and all processes terminated. |
| `GetStatus(id string)`                       | Returns Job status protobuf response via `joblib`. |
| `StreamOutput(id string, streamStderr bool)` | Streams output (stdout or stderr) to the client. The default is to stream stdout. All messages will be retrieved until the the file descriptor(s) are closed or the stream is cancelled. |

#### Notes
- A `JobController` will be implemented to handle `StartJob` and `StreamOutput` as these will get their own dedicated goroutines.
- The `cmd` parameter is actually composed of an executable part plus its argument(s). For example when `cmd` is "ls -lah". The executable part is `ls` and the argument is `-lah`. These are passed in separately to the `joblib` underlying package.
- Location of job output: `/var/lib/jobs/<jobid>/(stdout|stderr).log`
- Location of job metadata: In memory only for now
  - Structure of metadata
  ```
  {
    "user": "bob",
    "status": "Exited",
    "exit_code": 0
  }
  ```

Exited = process ended normally with exit_code = 0
Stopped = process terminated via StopJob() or external signal
Failed = process exited with non-zero code or setup error


- Statuses
```bash
    StatusUnknown // initial state
    StatusRunning // process is running
    StatusExited // process has exited normally or with an error
    StatusStopped // process has been stopped
    StatusFailed // process failed to start or encountered an error even lanching 
```
### Status Flow
`Unknown` -> `Running` -> `Exited`
`Unknown` -> `Running` -> `Stopped`
`Unknown` -> `Failed`

- Server Config
### Note: These will be hardcoded for simplicity but would normally be in a config file or env vars
```bash
    # streaming output
    streamOutputChunkSizeKB = 32

    # general
    jobsLocation = `/var/lib/jobs`
    port = `50051`
```
- Job ID
  - will use UUIDv7 (36 character length alphanumeric ascii string) 
```go
id, err := uuid.NewV7()
```

#### Details

##### StartJob
- Generate unique job id
- JobController via `joblib` creates a job
- update job metadata
- return jobid

#### StopJob
- Terminates the job via `joblib`
- This closes stdout/stderr file descriptors which will be used to signal the end of the stream
- Cleans up cgroup resources
- Updates job metadata (exit code, status)

#### StreamOutput Visual Logic
```bash
+------------------+
|  Jobcontroller   |
|   `joblib`       |
|  (writes to disk)|
+------------------+
         |
         v
+------------------+
|   stdout/stderr  |
|   written to     |
|   disk file      |
+------------------+
         |
         v
+------------------------------+
|   On client StreamOutput()   |
|   request:                   |
|                              |
|   - start goroutine:         |
|     read from file           |
|     and send to client       |
+------------------------------+
         |
         v
+------------------+
|  gRPC Stream     |
|   Response       |
+------------------+
         |
         v
+------------------+
|     Client       |
+------------------+
```
##### `StreamOutput`
- **Initial note**: The majority of this logic will be implemented in the `joblib` but `StreamOutput` will have this functionality.
- `StreamOutput` will **only** stream from job output that is written to disk. Stdout and Stderr will be written to disk by JobController via `joblib` itself upon invocation of `StartJob`. A closed file descriptor (stdout/stderr) will signify the end of the file/stream.
- `StreamOutput` will, after reading to EOF, block on inotify (Linux) and resume reading; no busy polling
- Each `StreamOutput` call runs in its own goroutine. The handler reads the job’s persisted log file and writes chunks directly to the gRPC stream. We rely on gRPC flow control for backpressure and `context.Context` for cancellation. No intermediate per-client channels are used. Due to relying on `context.Context`, we can assure that no unneeded reading occurs (**joblib**)
-  `Replay`: A client will be able to stream any output of a job even after the job has exited. (**joblib**)

##### StreamOutput Requirements
- Clients stream from the **beginning of the output**
- Support **multiple clients** connecting at different times.
- Avoid race conditions when jobs are writing and clients are reading.
- Designed for **CLI usage** via a single `StreamOutput` gRPC call.


#### Example Manager API Usage
```go
package main
import (
    "github.com/buckercd/jobworker/joblib/manager"
)

func main() {
	m := manager.NewJobManager(...)
	jobID, err := m.StartJob([]string{"/bin/sleep", "5"}, joblib.ResourceLimits{CPU: "500m"})
	status, _ := m.GetStatus(jobID)
	stream, _ := m.StreamOutput(jobID, false)
	m.StopJob(jobID)
}
```

### 2. `joblib` (Core Library)

| Function             | Description                                             |
| -------------------- | ------------------------------------------------------- |
| `Start()`            | Creates a cgroup via `cgroup` library, sets up the command with resource limits, and starts the process. It will also handle logging to stdout/stderr files. The function will return a job ID.
| `Stop()`             | Has cgroup library write `1` to `/sys/fs/cgroup/jobs/<jobid>/cgroup.kill` to forcefully terminate and then remove the cgroup.
| `GetStatusResponse()`| Returns a structured status response with job ID, current state (running, exited, etc.), and exit code (if available). Used by both CLI and gRPC server.

#### Notes
- `Go 1.21+` is required for `UseCgroupFD` functionality
- `CLONE_INTO_CGROUP` is required for the `UseCgroupFD` to work properly. (zero time a process runs outside of the cgroup)
- `Linux Kernel 5.7+` is required for this functionality
- No need to write a `pid` to `/sys/fs/cgroup/jobs/<jobid>/cgroup.procs` as `UseCgroupFD` will handle this automatically

#### Details

##### Start (Launching an actual process)
- The `Start()` function will take in a command and its arguments, along with resource limits. It will then:

1. **Set Cgroup**
  - Create a `cgroup` via `cgroup` library with the provided limits.
  - Get a file descriptor for the cgroup directory

2. **Build the Command**
    - Open log files for stdout and stderr
    - Build the command using `exec.Command` with the provided command and arguments

3. **Use SysProcAttr to set up the child process**
  - Set `UseCgroupFD` to true and pass the cgroup file descriptor
  - Set `Credential` to drop privileges to `nobody:nogroup` (uid/gid 65534)
  - Set `Pdeathsig` to `SIGKILL` to ensure the child is killed if the parent dies
  - Set `Setpgid` to true to set the process group ID to its own PID

4. **Start the Process**
   - Use `cmd.Start()` to start the process in the cgroup

5. **Supervision**
   - Wait for the child process to exit and capture its exit status.
   - Delete the cgroup directory after the process exits

```go
// Go 1.21+ is required for this code to work
	cgroupPath := os.Args[1]
	cmdPath := os.Args[2]
	cmdArgs := os.Args[3:]

	// 1. Open the cgroup directory
	cfd, err := syscall.Open(cgroupPath, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
	if err != nil {
		log.Fatalf("failed to open cgroup dir: %v", err)
	}
	defer syscall.Close(cfd)

	// 2a. Open log files
	stdoutFile, err := os.OpenFile("/tmp/stdout.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("failed to open stdout file: %v", err)
	}
	defer stdoutFile.Close()

	stderrFile, err := os.OpenFile("/tmp/stderr.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("failed to open stderr file: %v", err)
	}
	defer stderrFile.Close()

	// 2b. Build command
	cmd := exec.Command(cmdPath, cmdArgs...)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.Stdin = nil

	// 3. Set jail + priv drop + cgroup integration
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    cfd, // directory FD for cgroup
    Chroot: chrootFD

        // Drop privileges to nobody:nogroup
		Credential: &syscall.Credential{
			Uid: 65534,
			Gid: 65534,
		},

		Pdeathsig: syscall.SIGKILL, // kill child if parent dies
		Setpgid:   true, // set process group ID to its own PID
	}

	// 4. Start the process
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start target: %v", err)
	}

	// 5. Wait for completion
	if err := cmd.Wait(); err != nil {
		log.Fatalf("job failed: %v", err)
	}
    // 5b. cleanup cgroup
    ...

```

#### Key Subcomponents: (other potential packages)
- `devinfo`: A helper to provide device info. Like what /dev to set the IO cgroup limits on. This will be determined by grabbing the device where `/opt` directory is mounted.  (ie. /dev/sda might be `8:0`)
- `cgroups`: Actually creates and deletes cgroups v2 limits (implementation described below)
- `resources`: A helper package to abstract away all the many low level resource requirements (TBD if needed)

### 3. Cgroups

##### Notes
- Cgroups v2 will be used. Linux Kernel 5.7+ is required for this functionality
  - This custom implementation of cgroups will **only** work with Linux Kernel 5.7+ as `CLONE_INTO_CGROUP` is required for the `UseCgroupFD` to work properly. (zero time a process runs outside of the cgroup)
- Upon job termination the Job Controller via `joblib` will cleanup the cgroup files
- I have provided a mapping of the CLI limit flags to the cgroup files below
  - `Period` is fixed at `100000µs` (100ms) for CPU limits
  - `Quota` is calculated based on the CLI flag value, which is in milli-cores (m) or whole cores (2, 4, etc.) or even `max` for unlimited.
  - `Memory` is set in bytes, so `100M` will be `104857600` bytes.
- `IO` limits are set based on the device major and minor numbers, which will be determined by the device where the `/opt` directory is mounted on. The format is `<major>:<minor> rbps=<readBps> wbps=<writeBps>`. For example, `8:0 rbps=1048576 wbps=1048576` for 1MB/s read and write.
- `wbps` and `rbps` will be mapped from the CLI flag values like `low`, `med`, `high` to specific values:
  - `low` -> `1M/s` read & write
  - `med` -> `10M/s` read & write
  - `high` -> unlimited (`rbps=0 wbps=0`)

#### Details
1. Create a custom cgroup directory `/sys/fs/cgroup/jobs/<jobid>`
  - Create `/sys/fs/cgroup/jobs` if it doesnt exist yet
2. Inside  `<jobid>` directory:
  - set `memory.max` in bytes (one token)
  - set `cpu.max` to "<quota> <period>" where they are in microseconds. (2 tokens)
    - cpu.max is written as "<quota> <period>" in microseconds. Typical period is 100000µs (100ms), and quota ≤ period.
  - set `io.max` to "<device> <rbps> <wbps>" (3 tokens). An example: `8:0 rbps=1048576 wbps=1048576`
    - Note: The major(8) and minor(0) will try to be determined based on device where the `/opt` directory is mounted on.
3. If the process stopped, write `1` to `/sys/fs/cgroup/jobs/<jobid>/cgroup.kill` to forcefully kill the processes in the cgroup
4. When the process exits or is stopped, the cgroup directory `/sys/fs/cgroup/jobs/<jobid>` will be deleted.


### Flag to Cgroup Limit Mapping

| CLI Flag          | Cgroup Controller / File                                   | Value Mapping Rules |
|-------------------|------------------------------------------------------------|----------------------|
| `--cpu=<value>`   | `cpu.max`                                                  | **Period:** fixed at `100000` µs (100ms)<br>**Quota:** calculated from `<value>`:<br> `"max"` -> `"max"` (unlimited)<br> `"500m"` -> `(period * 0.5)` = `50000`<br> `"2"` -> `(period * 2)` = `200000`    |
| `--memory=<value>`| `memory.max`                                               | `<value>` in bytes<br> `"max"` -> `"max"` (unlimited) |
| `--io=<profile>`  | `io.max`                                                   | `<major>:<minor> rbps=<readBps> wbps=<writeBps>`<br> `"low"` -> `1M/s` read & write<br> `"med"` -> `10M/s` read & write<br> `"high"` -> unlimited (`rbps=0 wbps=0`) |


### Example Mappings

| Flag Example            | cgroup v2 Setting(s) |
|-------------------------|------------------------------------------|
| `--cpu=500m`            | `cpu.max = 50000 100000`                 |
| `--cpu=max`             | `cpu.max = max 100000`                   |
| `--memory=268435456`    | `memory.max = 268435456`                 |
| `--io=low`              | `io.max = 8:0 rbps=1048576 wbps=1048576` |
| `--io=high`             | `io.max = 8:0 rbps=0 wbps=0`             |



### 4. `CLI` (JobCtl)

- Command line interface using [`spf13/cobra`](https://github.com/spf13/cobra)
- gRPC TLS client with `x509` cert loading
- Usage is listed above
- On `StreamOutput`, the CLI will read and when the end of the stream is reached, it will stop reading and exit. If the job is still running, it will continue to read in real-time until the job exits or the user interrupts the stream.
- The CLI will need to have a config to indicate the client `username` at a minimum. 


### 5. AuthN/Z
- The client must authenticate the server **and** the server must authenticate the client. This means that the client **and** the server must trust each other via CA's. For simplicity, the server will have the client's CA's when running.
- `CN` will be used for `<username>`
- The client will have the server's CA when running
- The client will have its own client certificate and private key when running
- The server will authorize the client based on the `CN` in the client certificate.
- The server will have a list of allowed users based on the client CA's it has
- Only users with valid client certificates signed by a trusted CA will be allowed to connect
- Process streaming or getting statuses, reading of any job will be limited to each user.
- The `CLI` client will need a job id to stream or get status of a job. If the user is not the same as the job creator, the request will be denied.
- `Trust Boundary` - All client requests must traverse an mTLS connection and present a valid certificate signed by the server’s trusted CA(s).

#### Auth Flow
1. Valid user ?
    - Yes -> proceed
    - No -> deny connection
2. Valid job id ?
    - Yes -> proceed
    - No -> deny request
3. Is user the same as job creator ?
    - Yes -> proceed
    - No -> deny request

#### mTLS Certificate Trust & Selection Summary

| Component | How It Knows What to Trust | How It Selects Which Cert to Use |
|-----------|----------------------------|-----------------------------------|
| **Server** | - Loads **trusted client CA certificates** from a fixed location on disk.(`ca.crt`)<br>- Configured with `ClientAuth: RequireAndVerifyClientCert`.<br>- Verifies presented client cert chains to a trusted CA and is valid (expiry, key usage). | *Not applicable* — server only verifies client certs. |
| **CLI Client** | - Loads **server CA certificate** from fixed location on disk.(`ca.crt`)<br>- Uses this to verify the server’s certificate during TLS handshake. | - Loads **its own client certificate** (`.../bob/client.crt`) and private key (`.../bob/client.key`) from fixed, hardcoded paths.<br>- The CN in the cert acts as the `username` for AuthZ.<br>- Always uses the same keypair for all requests. |

**Notes:**
- All trust material is **static**
- `CN` in the client certificate = `username`
- Trust is based solely on local CA files.

---
## Testing Strategy

### Notes
- Time will determine how much of this testing I will be able to complete, but for sure all happy paths and common unhappy paths will be tested. Just unsure of how much time I will put into properly mocking everything for unittests.

### Unit Tests

| Component             | Tests                                                             |
| -------------------   | ----------------------------------------------------------------- |
| `joblib`              | start/stop procs, multiple starts, stops, full child proc cleanup, edge cases |
| `cgroups` (mock)      | apply limits, fail cases                                          |
| `joblib/manager`      | start job, stop job, get status, stream stderr, stream stdout, stream multiple same jobs, stream exited jobs |
| `Auth`                | valid/invalid client certs         |


### Integration Tests
- `joblib` - Verify jobs can actually start/stop properly and implement cgroups
- `manager`- Verify a job can start/stop/get status and stream
- Run CLI commands against test server
- Validate:
  - TLS handshake
  - AuthZ rejection
  - Output streaming with multiple clients

### End to End
- Implement a script to leverage the new cli and try to do all common scenarios and some uncommon ones as well testing the both the server and the cli
- create a job, stream output, get status, stop job, try to stream again, try to get status again
- try to create a job with an invalid command
- try to create a job with a user that is not allowed to run that command
- try to stream output with a user that is not in the same group as the job creator
- try to get status with a user that is not in the same group as the job creator
- try to stream output from a job that has exited

### Manual Tests

- System-level tests for:
  - Job kill propagation
  - Actual resource enforcement via peeking at cgroups location
  - Stress testing with long-running `yes` or `dd` jobs

---


## Milestones

| Phase              | Description                            |
| ------------------ | -------------------------------------- |
| PR #1: Design Doc  | Submit full design for approval        |
| PR #2: Job Library | Implement                              |
| PR #3: Cgroups + Helper Libs | Implement                    |
| PR #4: Integ + Unittests | Implement                        |
| PR #5: Manager     | Implement + Integ tests                |
| PR #6: gRPC Server | Add secure server + AuthN/AuthZ        |
| PR #7: CLI Client  | CLI with TLS setup and job controls    |
| PR #8: E2E Tests   | Add final E2E tests + anything else    |

---