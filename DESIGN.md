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
- Chroot jail limited binaries which processes running as `nobody:nogroup`
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
  - Avoids shell injection; Process execution happens via an `init` binary in a chroot jail.
- **Job isolation**
  - Cgroups used for resource control (custom implementation)
  - Each job gets its own cgroup directory
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
    chrootRoot = `/opt/jobroot`
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
| `Start()`            | Generates job id, sets a up cgroup, launches an init binary which moves its `pid` into the `cgroup` and uses  replacing itself with the actual target command and args. Then it waits for process to finish, or be terminated, get its exit status and set it job status as well.
| `Stop()`             | Has cgroup library write `1` to `/sys/fs/cgroup/jobs/<jobid>/cgroup.kill` to forcefully terminate and remove the cgroup.
| `Status()`           | Return the job status response 
| `GetStatusResponse()`| Returns a structured status response with job ID, current state (running, exited, etc.), and exit code (if available). Used by both CLI and gRPC server.

#### Details

##### Start (Launching an actual process)
- The `Start()` function will take in a command and its arguments, along with resource limits. It will then:

1. **Set Up Cgroup** – Create the job’s cgroup and apply resource limits.

2. **Setup and Launch Init Binary**
  - Use exec.Command to setup/start a small `init` helper binary 
```go
//Supervisor (parent process)
//Just starts the wrapper and handles logging; no sandboxing here because the wrapper will do it.
cmd := exec.Command("/usr/local/bin/init-wrapper", append([]string{jobId, targetCmd}, args...)...)
cmd.Env = []string{"PATH=/bin:/usr/bin"}
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr
cmd.Stdin = nil

if err := cmd.Start(); err != nil {
	log.Fatalf("failed to start job: %v", err)
}
```

4. **Init Binary Responsibilities** – Once started, the init binary will:
- Write its own PID to `/sys/fs/cgroup/jobs/<jobid>/cgroup.procs` to join the cgroup immediately.
- Set up the environment, drop privileges, and set process group ID.
- Use `exec.Command` and `cmd.Start()` with the target command so it runs entirely inside the cgroup from its first instruction in a chroot jail.
```go
//This is where the actual restrictions happen — chroot, drop privileges, set PGID, death signal.
    cgroupPath := "/sys/fs/cgroup/jobs/" + os.Args[1] // cgroup path will just be /sys/fs/cgroup/jobs/<jobid>
    cmdPath := os.Args[2]
    cmdArgs := os.Args[3:]

    // 1. Add *this* wrapper PID to cgroup immediately
    pid := os.Getpid()
    procsFile := cgroupPath + "/cgroup.procs"
    if err := os.WriteFile(procsFile, []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
        log.Fatalf("failed to add PID to cgroup: %v", err)
    }

    // 2. Prepare target process
    newRoot := "/opt/jobroot"
    uid := uint32(65534) // nobody
    gid := uint32(65534)

    cmd := exec.Command(cmdPath, cmdArgs...)
    cmd.Env = []string{"PATH=/bin:/usr/bin"}
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    cmd.Stdin = nil

    cmd.SysProcAttr = &syscall.SysProcAttr{
        Chroot:     newRoot, // chroot jail
        Credential: &syscall.Credential{Uid: uid, Gid: gid}, // drop privileges
        Setpgid:    true, // new process group
        // Ensure child is killed if this init process dies
        Pdeathsig:  syscall.SIGKILL, // send SIGKILL to child if parent dies
    }

    // 3. Start target (fork + exec inside jail)
    if err := cmd.Start(); err != nil {
        log.Fatalf("failed to start target: %v", err)
    }

```

5. **Supervision** – The main process can then monitor the job and capture its exit code.

##### Security note about Start()
- The **only** environment variable that will be set is `PATH` to a safe value. This is to prevent any potential PATH injection attacks. The `PATH` will be set to a safe value like `/usr/bin:/bin` or similar.

#### Key Subcomponents: (other potential packages)
- `devinfo`: A helper to provide device info. Like what /dev to set the IO cgroup limits on. This will be determined by grabbing the device where the root (/) dir is mounted.  (ie. /dev/sda might be 8:0)
- `cgroups`: Actually creates and deletes cgroups v2 limits (implementation described below)
- `resources`: A helper package to abstract away all the many low level resource requirements (TBD if needed)

### 3. Cgroups

##### Notes
- Cgroups v2 will be used. Kernel version 5.2+ is required for this functionality
- Upon job termination the Job Controller via `joblib` will cleanup the cgroup files
- I have provided a mapping of the CLI limit flags to the cgroup files below
  - `Period` is fixed at `100000µs` (100ms) for CPU limits
  - `Quota` is calculated based on the CLI flag value, which is in milli-cores (m) or whole cores (2, 4, etc.) or even `max` for unlimited.
  - `Memory` is set in bytes, so `100M` will be `104857600` bytes.
- `IO` limits are set based on the device major and minor numbers, which will be determined by the device where the root (/) directory is mounted on. The format is `<major>:<minor> rbps=<readBps> wbps=<writeBps>`. For example, `8:0 rbps=1048576 wbps=1048576` for 1MB/s read and write.
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
    - Note: The major(8) and minor(0) will try to be determined based on device where the root(/) directory is mounted on.
3. The init binary will write the child PID to `/sys/fs/cgroup/jobs/<jobid>/cgroup.procs`.
4. If the process stopped, write `1` to `/sys/fs/cgroup/jobs/<jobid>/cgroup.kill` to forcefully kill the process and remove the cgroup.
5. When the process exits or is stopped, delete the cgroup directory `/sys/fs/cgroup/jobs/<jobid>`.


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
- Process streaming or getting statuses, reading of any job will be limited to each user.

#### mTLS Certificate Trust & Selection Summary

| Component | How It Knows What to Trust | How It Selects Which Cert to Use |
|-----------|----------------------------|-----------------------------------|
| **Server** | - Loads **trusted client CA certificates** from a fixed location on disk.(`ca.crt`)<br>- Configured with `ClientAuth: RequireAndVerifyClientCert`.<br>- Verifies presented client cert chains to a trusted CA and is valid (expiry, key usage). | *Not applicable* — server only verifies client certs. |
| **CLI Client** | - Loads **server CA certificate** from fixed location on disk.(`ca.crt`)<br>- Uses this to verify the server’s certificate during TLS handshake. | - Loads **its own client certificate** (`.../bob/client.crt`) and private key (`.../bob/client.key`) from fixed, hardcoded paths.<br>- The CN in the cert acts as the `username` for AuthZ.<br>- Always uses the same keypair for all requests. |

**Notes:**
- All trust material is **static**
- `CN` in the client certificate = `username`
- Trust is based solely on local CA files.
##### NOTE: All lists will be hardcoded in the implementation
---
### Limited list of binaries for chroot jail
#### Will allow more in the future
```bash
/bin/ls
/bin/echo
/bin/cat
/bin/sleep
/bin/date
...
...
...
```

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