#!/bin/bash

# This script checks that a job with given PID has been properly
# placed into its cgroup, and that limits/accounting are working.

PID=$1

if [ -z $PID ]; then
    echo "PID is required. First and only args"
    exit 1
fi

# 1) find your job cgroup dir
ls /sys/fs/cgroup/jobs

# assume it’s /sys/fs/cgroup/jobs/<jobid>
cg=/sys/fs/cgroup/jobs/$PID

# 2) confirm attachment
cat $cg/cgroup.procs
cat $cg/pids.current

# 3) confirm limits were set
cat $cg/cpu.max
cat $cg/memory.max
cat $cg/io.max

# 4) confirm accounting changes while job runs
cat $cg/cpu.stat | head
cat $cg/memory.current

