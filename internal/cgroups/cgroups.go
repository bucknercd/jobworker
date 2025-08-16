package cgroups

import "path/filepath"

const (
	jobCgroupPath = "/sys/fs/cgroup/jobs"
)

type CgroupManager struct {
	cgPath string
}

func NewCgroupManager(jobId string) *CgroupManager {
	return &CgroupManager{cgPath: filepath.Join(jobCgroupPath, jobId)}
}

func (m *CgroupManager) Create(jobID string, limits []string) (int, error) {
	// Implementation for creating a cgroup; return cgroup file descriptor for a dir
	return 0, nil
}
func (m *CgroupManager) Delete(jobID string) error {
	// Implementation for removing a cgroup
	return nil
}
