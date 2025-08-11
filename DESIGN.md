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

- Start/stop/query status of arbitrary processes.
- Stream process stdout/stderr. Data will persist across server reboots.
- Semi-persistent storage of job stream info. A TTL (time to live) for exited process will be established
- Terminate child processes correctly, no orphans or zombies
- Set resource limits (CPU, memory, I/O) using cgroups v2 (manual implementation)
- Secure gRPC communication via mTLS (strong cipher suites)
- Simple authorization via client CA
- CLI utility to interact with server
- Minimal Unit testing
- Happy path and common error scenarios integration testing
- Basic end to end tests
- Command allowlist for normal users
- Command denylist for admin users
- Logging 

### Excluded
  (Due to time constraints)
- Horizontal scalability. I will only implement this in one instance of compute.
- Availability (multiple instances of compute) - This is a single source of failure. If the server goes down, the service goes down or if the server is overloaded, cli experience will suffer.
- Containerization of compute (ie Kubernetes). Using kubernetes is a clean way to implement distributed compute. 
- Persistent storage of client CAs in a database like sqlite
- Configuration - Will use hardcoding as much as possible for very simple configurations server/client  port, limits and allowlist/denylist and anything else that could come up.
- DDOS Prevention. No rate limiting middleware will be implemented

---

## Security Considerations

- **mTLS** for mutual authentication and transport encryption
  - TLS 1.3 only
  - Strong cipher suite (TLS\_AES\_256\_GCM\_SHA384 preferred)
- **Authorization policy**
  - In the client certificate itself, `CN` will be the `username` and `O` will be the `role-group`
  - The role value of `admin-<group>` will be allowed to run any executables **not** on the denylist
  - The role value of `user-<group>` will be allowed to run any executables **in** the allowlist
  - Any members of the same group can perform operations such as stream output from any job as long as they have the job id 
  - Client CA certificates will be stored in the running server's filesystem rather than in a database, to reduce initial implementation complexity.
- **No shell invocation**
  - Avoids shell injection; executes directly with a `exec` style syscall. No shelling out will be permitted. Shell executables will **not** be on allowlist for normal users and **will** be on blacklist for admin users.
- **Job isolation**
  - Cgroups used for resource control (custom implementation)
  - Each job gets its own cgroup directory
  - **Only** users that belong to the same group as the group that created the job id will be able to obtain any data at all from the said job.
- **DDOS**
  - I will not implement a rate limiting middleware but am aware one would be beneficial to have. This would prevent resource exhaustion. For example, making many connections and just leaving all of them open by starting a TCP handshake but never closing it.
- **Secure Commands Allowlist**
  - In order to prevent a user from destroying or exploiting the system there **must** be a command allowlist, for users of type `user`, and a denylist for users of type `admin`.
- **Resource Exhaustion**
  - To prevent resource exhaustion, launching a job and streaming output will each have a goroutine, apart from initial gRPC server, so they can be working asynchronously. There will be limits with regards to the amount of `jobs` , which have a one to one mapping to goroutines so as to prevent a goroutine explosion or a fork bomb.
  - The stdout.log and stderr.log files will be limited (refer to limits below) ie. 100MB, 20MB
  - Disk usage will be limited as well (refer to limits below) ie. 80%
  - Memory will be limited (refer to limits below) ie. 8GB
- **Logging**
  - The server will maintain logs to disk to keep track of all events and to see who has tried to do what to be able to have visibility 
---

## CLI Examples
### Offset Notes
- The `--offset` argument specifies the `chunk number`. The **actual** byte position is `offset x chunk size`. For example, if `chunk size` is `32KB` then `--offset=2` means that the **actual** byte position is `64KB`
- If `--offset` is **not** specified , it will default to **0**
### Tail Note
- If `--tail` is **not** specified, the server streams from the requested offset until **EOF**, emitting one or more messages (each up to `streamOutputChunkSizeKB`). If the file is smaller than a chunk, you’ll just get the smaller, final chunk.

```bash
# Start a job
jobctl start --cmd "ls / -lah"

# Start a job with resource limits
jobctl start \
  --cmd="echo hello world" \
  --cpu=500m \
  --mem=100M \
  --io=low

# Stream the output (stdout) (currently running or terminated but within TTL)
jobctl stream --id=xxxxxxxx

# Stream the output (stderr)
jobctl stream --id=xxxxxxxx --stderr

# Stream the last output of a currently running job (stdout)
jobctl stream --id=xxxxxxxx --tail 

# Stream from offset 10 (chunk-based; ie. 10 × 32KB = 320KB into file)
jobctl stream --id=xxxxxxxx --offset 10

# Get job status
jobctl status --id=xxxxxxxx

# Stop a job
jobctl stop --id xxxxxxxx
```


---
## Component Overview

### 1. `joblib/manager` (High Level Library)

| Function                                        | Description                                             |
| ----------------------------------------------- | ------------------------------------------------------- |
| `StartJob(cmd []string, limits ResourceLimits)` | Performs resource exhaustion checks. Generates an id and creates, then starts a job with cgroup limits, or without, via `joblib`. Adds it to its internal job mapping. Returns a jobid or a resource exhaustion message |
| `StopJob(id string)`                         | Stops job via `joblib`. Records a TTL (time to live) indicating when the job's resources (logs, metadata) should be kept around for. Important for reading Stdout and Stderr from exited job(s) |
| `GetStatus(id string)`                       | Returns Job status protobuf response via `joblib`. |
| `StreamOutput(id string, streamStderr bool, offset uint64, tailOutput bool)` | Streams output (stdout or stderr) to the client in chunks size `32KB` (from config). The default is to stream stdout. The client should provide an offset. The offset state is handled by the client. The server does not handle the offset. The offset is in increments of the `chunk size`  |

#### Notes
- A `JobController` will be implemented to handle `StartJob` and `StreamOutput` as these will get their own dedicated goroutines. The amount of goroutines will be limited. This is so the server won't be overwhelmed by requests.
- TTL (Time to Live) will be a Unix epoch timestamp. Eviction time, or the Unix epoch timestamp in the future, will be 8 hours from when job is run.
- The `cmd` parameter is actually composed of an executable part plus its argument(s). For example when `cmd` is "ls -lah". The executable part is `ls` and the argument is `-lah`. These are passed in separately to the `joblib` underlying package.
- Location of job output: `/var/lib/jobs/<jobid>/(stdout|stderr).log`
- Location of job metadata: `/var/lib/jobs/<jobid>/metadata`
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
- Limits
  - These are set via a config. An example is provided below
```bash
    # launching a process
    maxJobsPerNode = 8 // limit the amount of goroutines 
    maxProcsPerJob = 12 // limit the amount of child procs
    maxMemoryPerNodeGB = 8 // limit RAM
    maxDiskPercentagePerNode = 80 // limit disk usage (root partition)

    # truncating stdout/stderr files
    maxStdoutFileSizeMB = 100
    maxStderrFileSizeMB = 20
    truncatePercentage = 10

    # streaming output
    streamOutputChunkSizeKB = 32

    # general
    jobsLocation = `/var/lib/jobs`
```
- Job ID
  - 8 character length alphanumeric ascii string.
  - The first 3 characters are the **last 3 characters** of a base36-encoded Unix timestamp.
  - Lexicographically sortable 
  - This protects against collisions and is user friendly.
```go
func generateID() string {
	// First 3 chars from time (sortable)
	now := time.Now().Unix()
	timePart := strconv.FormatInt(now, 36) // base36 is compact
	if len(timePart) > 3 {
		timePart = timePart[len(timePart)-3:] // last 3 chars
	}

	// Last 5 chars random
	randomPart := randomString(5)

	return timePart + randomPart
}
```

- TTL file location: `/var/lib/jobs/<jobid>/ttl`
- TTL file contents will be very simple. Just an int64 binary representation.

#### Details

##### StartJob
- Checks disk space on partition where `/var/lib` is at (I am assuming root `/`). We need to have 15% of that partition available
- Controller will check if all the limit requirements are ok. Refer to **limits** above.
- Generate unique job id
- JobController via `joblib` creates a job
  - with limits or without limits
- write job metadata out to a file
- return jobid

#### StopJob
- Terminates the job via `joblib` if it is still running and writes a Unix epoch timestamp as its TTL. 

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
|   - create new Go channel    |
|   - start goroutine:         |
|     read from file           |
|     and send to channel      |
+------------------------------+
         |
         v
+------------------+
|   Go channel     |  (1 per client)
+------------------+
         |
         v
+------------------+
|  gRPC Stream     |  <- StreamOutput()
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
- `StreamOutput` will **only** stream from job output that is written to disk. Stdout and Stderr will be written to disk by JobController via `joblib` itself upon invocation of `StartJob`. During the call to `StreamOutput` if the process has exited and the TTL has expired, those files will be deleted by this `manager` package and nothing will be streamed to client. Client will receive some message indicating that the job has expired. (**manager**)
- Each `StreamOutput` call spawns a goroutine that reads from the persisted log file and pushes content to a **client-specific Go channel**, which is then streamed via gRPC. (**joblib**)
- `StreamOutput` is called, per client, and the pertinent file with stdout/stderr information will be read from and sent to a channel for streaming purposes. Now, the process could still be running and that's fine or it could be terminated but still within its TTL and client could still stream output. (**joblib**) 
- `Tailing Output`: Output can be "tailed" if a process is still running. (**joblib**)
- `Limiting Output`: stdout.log and stderr.log MUST have a max size or this could represent an issue if for some reason a process is very long lived and produces much output or never stops producing output. The max size can be set to `100MB` for stdout and `20MB` for stderr. Since the files have size limits they can be truncated so during truncation the file will be **locked** so ONLY writes can happen during that time preventing a race condition. The strategy will be to keep the last 10%. `10MB` (stdout), `2MB`(stderr) of a file in this case. A message indicating truncation will be received by the client. (**joblib**). **Note**: These limits can be changed. Refer to limits section above.
-  `Replay`: A client will be able to stream any output of a job even after the job has exited as long as it's within its TTL (time to live). (**joblib**)
- `Offsets`: If an offset is provided by the client it will be calculated by multiplying by `chunk size`. The client will be returned the **chunk** from the particular offset. Furthermore, if the file with stdout/stderr were to be truncated during the client's request and the offset were to be invalid, then offset 0 will be chosen from truncated file and an **truncated** message prepended letting the client know. If the offset is valid but file has been **truncated** the same behavior as previously described will happen as well. 
  - If `--tail` and `--offset` are both provided, then the output will be streamed in real-time from the offset. The client will receive chunks of data starting from the offset and continuing until the end of the file. If the `offset` is invalid then the client will receive a message indicating that the file has been truncated and the offset is now 0.
- `streamOutputChunkSizeKB`: The `StreamOutput` call will stream the data in a particular chunk size and the client will keep track of any particular offset it's on and on subsequent calls, it will just auto increment said offset. Only if an offset is specifically set, then will the client potentially `skip ahead` or `look back` at output. 


##### StreamOutput Requirements
- Clients stream either:
  - From the **beginning of the file** (or new start after truncation).
  - From an offset if specified
  - Or both. They can **tail** the output in real-time plus use an offset
- Only the **latter portion of the file** is kept (after size limit reached). (last 10%. Refer to limits section above. Can be modified)
- Support **multiple clients** connecting at different times.
- Avoid race conditions when jobs are writing and clients are reading.
- Designed for **CLI usage** via a single `StreamOutput` gRPC call.



 
### 2. `joblib` (Core Library)

| Function             | Description                                             |
| -------------------- | ------------------------------------------------------- |
| `Start()`            | Starts a process via setpgid(), takes in an executable and args. Sets job status and executes the process. It also sets up the process' stdout and stderr and to be read from and written to files on disk. These file names should map up to the job id assigned and also on disk as well. Then wait for job to finish, or be terminated, get its exit status and set it job status as well.
| `Stop()`             | Get PGID via getpgid() and send KILL signal to it. If there is a cgroup created, delete it. Set job status.
| `Status()`           | Return the job status response from 
| `Stdout()`           | Returns a buffered channel(1024) which is filled with data previously written to disk
| `Stderr()`           | Returns a buffered channel(1024) which is filled with data previously written to disk
| `GetStatusResponse()`| Returns a structured status response with job ID, current state (running, exited, etc.), and exit code (if available). Used by both CLI and gRPC server.

#### Details

##### Start (Launching an actual process)
- Job status will be set to running
- If a particular job has limits, then a call to the `cgroups` library will be made to create the needed cgroup to limit the process' CPU, Memory or Disk IO.
- The process is setup by using `setpgid` and setting `uid` and `gid` to `nobody` for normal users. `uid` and `gid` should be `1000` when running as an admin user. The process uses `setpgid` so that it can clean up children processes later if it needs to.
- The PID of this process is added to its cgroup, if the limits were provided. (This **must** be done in this order)
- The process' stdout and stderr will be captured and written to stdout and stderr files and any metadata by the root process itself via cmd.StdoutPipe() and cmd.StderrPipe(). The appropriate metadata is written to its location in `/var/lib/jobs/<jobid>/...` as well.
- The process is then waited on for its exit to reap it and obtain its exitcode. The exitcode will be written to the job metadata.
##### Security note about Start()
- The executable can be a full path or a bare command name (e.g., `ls` vs. `/usr/bin/ls`). This possibly introduces a `PATH` injection vulnerability, since the system will resolve the executable by searching `$PATH`. The design acknowledges this risk and permits it for this iteration, but future revisions should require full, trusted paths or implement strict validation.

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
3. After the process has been launched obtain its `pid` and write it out to `cgroup.procs` as text


### 3. `server` (gRPC API)

- Implements:
  - `StartJob` (takes command, resource limits)
  - `StopJob`
  - `GetStatus`
  - `StreamOutput` (streaming gRPC)
- Secures:
  - mTLS setup using TLS 1.3
  - authorization done via CA. O=admin|user

### 4. `cli` (JobCtl)

- Command line interface using [`spf13/cobra`](https://github.com/spf13/cobra)
- gRPC TLS client with `x509` cert loading
- Usage is listed above


### 5. AuthN/Z
- The client must authenticate the server **and** the server must authenticate the client. This means that the client **and** the server must trust each other via CA's. For simplicity, the server will have the client's CA's when running.
- `CN` will be used for `<username>`
- `O` will be used for `<role>-<group>`
- Process streaming or getting statuses, reading, will be limited to members of any said group. This will be for isolation purposes.
- Depending on the `role` (admin|user) , the running of particular commands will be limited.

#### NOTE: All lists will be hardcoded in the implementation
### Cmd Allowlist (normal users)

#### File & Directory Listing
```
basename
dirname
file
find
ls
readlink
realpath
stat
```
#### Viewing & Paging
```
cat
cut
diff
head
less
more
tail
tac
tee
wc
```
#### Text Searching & Parsing
```
awk
egrep
grep
sed
tr
uniq
sort
strings
```
#### System Info
```
arch
date
df
du
env
free
hostname
id
printenv
uptime
uname
vmstat
```
#### Process Info
```
pgrep
pidof
ps
top
whoami
```
#### Safe Utility Binaries
```
echo
expr
seq
sleep
test
timeout
true
false
yes
```

### Admin Denylist


#### System-Destroying Commands
```bash
rm              # delete files, including /
mv              # move critical system files
cp              # copy files into critical places
dd              # raw disk access
mkfs.*          # format disks
fsck            # filesystem check
init            # change runlevel
reboot
shutdown
halt
poweroff
```
#### Kernel / Systemctl / Reboot
```bash
systemctl       # restart system daemons or reboot
modprobe        # load kernel modules
insmod
rmmod
kexec
```
#### Filesystem / Mounting
```bash
mount
umount
losetup
```
#### Networking & Firewalls
```bash
ifconfig
ip
iptables
ip6tables
nft
brctl
tc
ethtool
route
arp
```
#### Shell Access
```bash
sh
bash
dash
zsh
fish
```
#### Remote / Network Tools
```bash
curl
wget
scp
ftp
nc
telnet
nmap
ssh
sftp
```
#### Scripting / Code Execution
```bash
python
python3
perl
ruby
php
lua
node
go
gcc
g++
javac
java
make
cmake
cargo
pip
npm
```
#### Privilege Escalation & Users
```bash
sudo
su
useradd
usermod
userdel
groupadd
passwd
chage
chpasswd
chown
chmod
setfacl
```
#### Namespace / container / chroot escapes & cluster control
```bash
nsenter
unshare
chroot
runc
docker
podman
nerdctl
ctr
crictl
kubectl
```

#### Process Control
```bash
kill
killall
pkill
nice
renice
gdb     
```
#### Final Protection Notes
- Admin users will **not** be allowed to have `/var/lib/jobs` or `jobworker` anywhere in their command  arguments as a safeguard. All arguments will be checked for these substrings. 
- Possibly implement setting cgroups for all jobs requested so I can better limit potential abuse (limit blast radius of a malicious job)
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
- `manager` - Verify a job can start/stop/get status and stream. Also, verify all metadata is written correctly and state of jobs is managed properly 
- Run CLI commands against test server
- Validate:
  - TLS handshake
  - AuthZ rejection
  - Output streaming with multiple clients

### End to End
- Implement a script to leverage the new cli and try to do all common scenarios and some uncommon ones as well. 

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