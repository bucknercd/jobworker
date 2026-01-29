# JobWorker (Go)

JobWorker is a gRPC-based job execution service and CLI for running controlled host-level jobs with strong process isolation, disk-backed output, and per-job resource limits using Linux cgroups v2.

It is a systems-level job runner (not a container runtime) built on native Linux primitives:
- processes
- cgroups v2
- procfs
- privilege dropping
- process groups
- kernel-enforced resource control
- TLS-secured gRPC transport

Repo: https://github.com/bucknercd/jobworker

---

## Overview

JobWorker consists of two components:

- **jobworker-server**  
  A gRPC server that manages job lifecycle, execution, isolation, logging, and cleanup.

- **jobctl**  
  A CLI client for starting jobs, checking status, stopping jobs, and streaming output.

Jobs execute directly on the host and are isolated using:
- Linux process groups
- Linux cgroups v2 (CPU, memory, IO, pids)
- privilege dropping (`nobody:nogroup`)
- per-job filesystem directories
- disk-backed stdout/stderr
- TLS-secured gRPC communication

---

## Execution Model

- Jobs run directly on the host (no containers)
- Each job runs in:
  - its own process group
  - its own cgroup v2 subtree
- Jobs are launched with:
  - `UseCgroupFD` (atomic cgroup attachment)
  - `Setpgid=true` (separate process group)
  - `Pdeathsig=SIGKILL` (child dies if server dies)
  - dropped privileges (`nobody:nogroup`)
- Output is written directly to disk

---

## Output Model (Source of Truth)

Each job has disk-backed output:

/var/lib/jobs/<job-id>/stdout.log
/var/lib/jobs/<job-id>/stderr.log

markdown
Copy code

Behavior:
- stdout/stderr are written directly to disk
- on job completion:
  - server reads these files
  - dumps their contents into the server log
- disk files are the source of truth
- the server also writes a centralized log file in the repo root:

./jobworker-server.log

---

## Cgroup Isolation (Core Feature)

Each job runs inside a dedicated cgroup v2 directory:

/sys/fs/cgroup/jobs/<job-id>

markdown
Copy code

Supported controllers:
- `cpu.max`
- `memory.max`
- `io.max`
- `pids.current`

Features:
- per-job CPU limits
- per-job memory limits
- per-job IO throttling (device/kernel dependent)
- per-job accounting (`cpu.stat`, `memory.current`)
- atomic attachment via `UseCgroupFD`
- forced termination via `cgroup.kill`

The server logs:
- job ID
- OS PID
- cgroup path
- attached PIDs
- configured limits
- live usage
- throttling counters

This provides runtime verification that enforcement is active.

---

## Security Model & Disclaimer

JobWorker is not a sandbox and not a container runtime.

Security properties:
- jobs run as `nobody:nogroup`
- privilege dropping enforced
- per-job cgroup isolation
- per-job process group isolation
- TLS gRPC transport
- disk-backed output

Limitations:
- no chroot isolation enabled
- no containerization
- no namespace isolation
- jobs execute on the host filesystem
- host access is possible unless externally restricted

This is a controlled execution system, not a hardened sandbox.

---

## Feature Matrix

| Feature                   | Status |
|---------------------------|--------|
| gRPC API                  | Implemented |
| TLS transport             | Implemented |
| CLI (`jobctl`)            | Implemented |
| Disk-backed output        | Implemented |
| Post-exec log dumping     | Implemented |
| Process groups            | Implemented |
| Pdeathsig cleanup         | Implemented |
| Privilege dropping        | Implemented |
| Cgroup v2 isolation       | Implemented |
| CPU limits                | Implemented |
| Memory limits             | Implemented |
| IO throttling             | Implemented (device/kernel dependent) |
| Streaming output          | Not implemented |
| chroot isolation          | Not enabled |
| Container runtime         | Not used |
| Namespace isolation       | Not used |

---

## Build & Run

### Build everything
```bash
make all
```

### Run Server

- Default
```bash
make run.server
```
- Custom listen address, certs path, log path
```bash
sudo ./bin/jobworker-server -listen :50051 -certs ./certs -log ./jobworker-server.log
```

### Run Client
```bash
make certs user
```
- then run `jobctl` commands as shown below

### Generate TLS certificates
bash
make certs server
make certs user

## CLI Usage (jobctl)
```bash
./bin/jobctl --help
./bin/jobctl -cmd start -exe sleep -args "10"
```

### Start with CPU + memory limits
```bash
./bin/jobctl \
  -cmd start \
  -exe bash \
  -args "-c 'yes > /dev/null'" \
  -cpu 500m \
  -mem 100M
```

### Start with IO class
```bash
./bin/jobctl -cmd start -exe ls -args "-lah /" -io low
./bin/jobctl -cmd status -id <job-id>
```

### Stop a job
```bash
./bin/jobctl -cmd stop -id <job-id>
```

---

## Observability

For each job, the server logs:

- job ID  
- OS PID  
- cgroup path  
- attached PIDs  
- configured limits  
- live usage  
- throttling counters  
- stdout/stderr dump on completion  

This provides auditable, verifiable enforcement and execution tracing.

---

## Design Philosophy

JobWorker is built on native kernel primitives rather than container abstractions:

- Linux process model  
- cgroups v2  
- procfs  
- syscall-level control  
- privilege separation  
- explicit resource enforcement  
- deterministic execution  
- observable state  

It is designed to be:

- debuggable  
- inspectable  
- auditable  
- systems-oriented  
- infrastructure-first  
- correctness-driven
- minimal dependencies
- transparent in behavior
  
