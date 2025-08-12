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
- Simple authorization via client CA
- CLI utility to interact with server
- Minimal Unit testing
- Happy path and common error scenarios integration testing
- Basic end to end tests
- Command denylist for normal users, but will run as `nobody` user
- Same command denylist for admin users and will run as `1000:1000` user
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
  - In the client certificate itself, `CN` will be the `username` and `O` will be the `role-group`
  - The role value of `admin-<group>` will be allowed to run any executables **not** on the denylist as user `1000:1000`
  - The role value of `user-<group>` will be allowed to run commands as user `nobody`
  - Any members of the same group can perform operations such as stream output from any job as long as they have the job id 
  - Client CA certificates will be stored in the running server's filesystem rather than in a database, to reduce initial implementation complexity.
- **Shell invocation**
  - Avoids shell injection; executes directly with a `exec` style syscall. No shelling out will be permitted. Shell executables will be on the denylist.
- **Job isolation**
  - Cgroups used for resource control (custom implementation)
  - Each job gets its own cgroup directory
  - **Only** users that belong to the same group as the group that created the job id will be able to obtain any data at all from the said job.
- **DDOS**
  - I will not implement a rate limiting middleware but am aware one would be beneficial to have. This would prevent resource exhaustion. For example, making many connections and just leaving all of them open by starting a TCP handshake but never closing it.
- **Secure Commands Denylist**
  - Admin users will be restricted from running certain destructive commands (e.g., `rm`, `mv`, `dd`, etc.) to prevent system damage as well as some simple privilege escalation commands. Normal users will be limited with the same denylist as well.
- **Logging**
  - The server will maintain logs to disk to keep track of all events and to see who has tried to do what to be able to have visibility 
---


## CLI Examples
### Streaming Note
- All the `stream` commands have `tail` like functionality. This means that if the job is still running, the stream will continue to follow the output in real-time. If the job has exited, the stdout and stderr file descriptors will be closed and the stream will end.
### Limits Default
- If no limits are specified, the job will run with default cgroup limits:
  - CPU: 500m (500 milli-cores)
  - Memory: 100M
  - I/O: low
```bash
# Start a job
jobctl start --cmd "ls / -lah"

# Start a job with resource limits
jobctl start \
  --cmd="echo hello world" \
  --cpu=500m \
  --mem=100M \
  --io=low

# Stream the output (stdout)
jobctl stream --id=xxxxxxxx

# Stream the output (stderr)
jobctl stream --id=xxxxxxxx --stderr

# Stream the last output of a currently running job (stdout)
jobctl stream --id=xxxxxxxx

# Get job status
jobctl status --id=xxxxxxxx

# Stop a job
jobctl stop --id=xxxxxxxx
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
    "role": "admin|user",
    "group": "team1",
    "status": "Exited",
    "exit_code": 0
  }
  ```

- Statuses
```bash
    StatusUnknown
    StatusRunning
    StatusExited
    StatusStopped
    StatusFailed
```
- Server Config
### Note: These will be hardcoded for simplicity but would normally be in a config file or env vars
```bash
    # streaming output
    streamOutputChunkSizeKB = 32

    # general
    jobsLocation = `/var/lib/jobs`
    safePATH = `/usr/bin:/bin`
    port = `50051`
```
- Job ID
  - 16 character length alphanumeric ascii string.
  - First `6`: fixed-width base36 Unix timestamp (left-padded with 0; if ever longer, use the rightmost 6).
  - Lexicographically sortable
  - Last `10`: `crypto-random` [a-z0-9]
  - Total of 36^10 (3.66 trillion) combinations for the random part
  - This protects against collisions
```go
const letters = "abcdefghijklmnopqrstuvwxyz0123456789"

func generateID() string {
    t := strconv.FormatInt(time.Now().Unix(), 36)
    if len(t) < 6 {
        t = strings.Repeat("0", 6-len(t)) + t
    } else if len(t) > 6 {
        t = t[len(t)-6:] // keep rightmost 6 if it ever grows
    }
    return t + randomString(10)
}

func randomString(n int) string {
    var sb strings.Builder
    for i := 0; i < n; i++ {
        num, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
        sb.WriteByte(letters[num.Int64()])
    }
    return sb.String()
}
```

- TTL file location: `/var/lib/jobs/<jobid>/ttl`
- TTL file contents will be very simple. Just an int64 binary representation.

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


### 2. `joblib` (Core Library)

| Function             | Description                                             |
| -------------------- | ------------------------------------------------------- |
| `Start()`            | Starts a process via setpgid(), takes in an executable and args. Sets job status, cgroup limits and executes the process. See **Start** for details.  It also sets up the process' stdout and stderr and to be read from and written to files on disk. These file names should map up to the job id assigned and also on disk as well. Then wait for job to finish, or be terminated, get its exit status and set it job status as well.
| `Stop()`             | Sends `SIGTERM` to the process group for as an initial try. If the process (or children) is still running after the timeout, it writes `1` to `/sys/fs/cgroup/jobs/<jobid>/cgroup.kill` to forcefully terminate it and remove the cgroup.
| `Status()`           | Return the job status response 
| `GetStatusResponse()`| Returns a structured status response with job ID, current state (running, exited, etc.), and exit code (if available). Used by both CLI and gRPC server.

#### Details

##### Start (Launching an actual process)
- The `Start()` function will take in a command and its arguments, along with resource limits. It will then:
  - Generate a unique job ID
  - Create a cgroup for the job and apply resource limits
  - Create a new process using `syscall.ForkExec()` with the following attributes:
    - `ProcAttr.Env` = `[]string{safePATH}` (set a safe PATH for the job)
    - `SysProcAttr.Setpgid` = true (start as its own process group leader).
    - `SysProcAttr.Pdeathsig` = SIGKILL (if the supervisor dies, the child is killed).
    - `SysProcAttr.Credential` = {Uid,Gid} set to `nobody` for normal users or `1000:1000` for `admin`.
    - Set up stdout and stderr to write to `/var/lib/jobs/<jobid>/stdout.log` and `/var/lib/jobs/<jobid>/stderr.log`
      - ProcAttr.Files = []uintptr{stdinFD, stdoutFD, stderrFD} mapping FD 0/1/2 to:
        - stdin: `/dev/null`
        - stdout: `<jobdir>/stdout.log`
        - stderr: `<jobdir>/stderr.log`
  - Immediately move the process to the cgroup by writing its PID to `/sys/fs/cgroup/jobs/<jobid>/cgroup.procs`
  - Wait for the processes to finish, capturing its exit code

```go
	attr := &syscall.ProcAttr{
		Env:   []string{safePATH},
		Files: []uintptr{devNull.Fd(), stdout.Fd(), stderr.Fd()},
		Sys: &syscall.SysProcAttr{
			Setpgid:    true, // Start as its own process group leader
			Pdeathsig:  syscall.SIGKILL, // If the supervisor dies, the child is killed
            // Set the user and group to `nobody` for normal users or `1000:1000` for admin users
			Credential: &syscall.Credential{Uid: uid, Gid: gid},
		},
	}

	argv := append([]string{fullPath}, cmdArgs...)
	pid, err := syscall.ForkExec(fullPath, argv, attr)
    // argv is the command and its arguments
    // if the command is `ls` then we will need to use `exec.LookPath("ls")` to get the full path to the executable
    // not shown here for brevity
```

##### Security note about Start()
- The **only** environment variable that will be set is `PATH` to a safe value. This is to prevent any potential PATH injection attacks. The `PATH` will be set to a safe value like `/usr/bin:/bin` or similar.

#### Key Subcomponents: (other potential packages)
- `devinfo`: A helper to provide device info. Like what /dev to set the IO cgroup limits on. This will be determined by grabbing the device where the root (/) dir is mounted.  (ie. /dev/sda might be 8:0)
- `cgroups`: Actually creates and deletes cgroups v2 limits (implementation described below)
- `resources`: A helper package to abstract away all the many low level resource requirements (TBD if needed)

#### Cgroups

##### Notes
- Cgroups v2 will be used. Kernel version 5.2+ is required for this functionality
- Upon job termination the Job Controller via `joblib` will cleanup the cgroup files

#### Details
1. Create a custom cgroup directory `/sys/fs/cgroup/jobs/<jobid>`
  - Create `/sys/fs/cgroup/jobs` if it doesnt exist yet
2. Inside  `<jobid>` directory:
  - set `memory.max` in bytes (one token)
  - set `cpu.max` to "<quota> <period>" where they are in microseconds. (2 tokens)
    - cpu.max is written as "<quota> <period>" in microseconds. Typical period is 100000µs (100ms), and quota ≤ period.
  - set `io.max` to "<device> <rbps> <wbps>" (3 tokens). An example: `8:0 rbps=1048576 wbps=1048576`
    - Note: The major(8) and minor(0) will try to be determined based on device where the root(/) directory is mounted on.
3. Immediately after `ForkExec` returns (in the parent), write the child PID to `/sys/fs/cgroup/jobs/<jobid>/cgroup.procs`.
4. When the process exits, delete the cgroup directory `/sys/fs/cgroup/jobs/<jobid>`.
5. If the process is still running, write `1` to `/sys/fs/cgroup/jobs/<jobid>/cgroup.kill` to forcefully kill the process and remove the cgroup.


### 3. `server` (gRPC API)

- Implements:
  - `StartJob` (takes command, resource limits)
  - `StopJob`
  - `GetStatus`
  - `StreamOutput` (streaming gRPC)
- Secures:
  - mTLS setup using TLS 1.3
  - authorization done via CA. O=admin|user

### 4. `CLI` (JobCtl)

- Command line interface using [`spf13/cobra`](https://github.com/spf13/cobra)
- gRPC TLS client with `x509` cert loading
- Usage is listed above
- On `StreamOutput`, the CLI will read and when the end of the stream is reached, it will stop reading and exit. If the job is still running, it will continue to read in real-time until the job exits or the user interrupts the stream.


### 5. AuthN/Z
- The client must authenticate the server **and** the server must authenticate the client. This means that the client **and** the server must trust each other via CA's. For simplicity, the server will have the client's CA's when running.
- `CN` will be used for `<username>`
- `O` will be used for `<role>-<group>`
- Process streaming or getting statuses, reading, will be limited to members of any said group. This will be for isolation purposes.
- Depending on the `role` (admin|user) , the running of particular commands will be limited.

#### NOTE: All lists will be hardcoded in the implementation

### Denylist for `admin` users and normal users
```bash
rm              # delete files, including /
mv              # move critical system files
cp              # copy files into critical places
dd              # raw disk access
reboot
shutdown
halt
poweroff
sudo
su
gdb
sh
bash
zsh
ksh
csh
fish
```
#### Final Protection Notes
- This Admin denylist is very basic and is meant to only cover the most common destructive commands. It is not exhaustive by any means. Since `admin` users will run as `1000:1000`, they still could potentially run commands that could destroy the system, but this is a safeguard against the most common ones. (privelege escalation, file deletion, etc.)
- Setting cgroups for all jobs will be done so I can better limit potential abuse (limit blast radius of a malicious job)
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