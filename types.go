package fungi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/xerrors"
)

type JobConfig struct {
	Cmd  string
	Args []string
}
type JobResult struct {
	JobID   int
	Success bool
	Output  string
}

func LoadJobConfig(fn string) (*JobConfig, error) {
	fi, err := os.Open(fn)
	if err != nil {
		return nil, xerrors.Errorf("failed to open job file: %w", err)
	}
	defer fi.Close()

	var jc JobConfig
	if err := json.NewDecoder(fi).Decode(&jc); err != nil {
		return nil, xerrors.Errorf("failed to decode job file %s: %w", fn, err)
	}

	return &jc, nil
}

func LoadJobs(dir string) (map[int]*JobConfig, error) {
	dhnd, err := os.Open(dir)
	if err != nil {
		return nil, xerrors.Errorf("failed to open directory: %w", err)
	}

	names, err := dhnd.Readdirnames(-1)
	if err != nil {
		return nil, xerrors.Errorf("failed to read job files from directory: %s", err)
	}

	jobs := make(map[int]*JobConfig)
	for _, fn := range names {
		fmt.Println("name: ", fn)
		job, err := LoadJobConfig(filepath.Join(dir, fn))
		if err != nil {
			return nil, xerrors.Errorf("failed to load job config: %w", err)
		}

		_, jobid, err := parseJobFileName(fn)
		if err != nil {
			return nil, xerrors.Errorf("failed to parse job file name: %w", err)
		}
		jobs[jobid] = job
	}

	return jobs, nil
}

func parseJobFileName(fn string) (string, int, error) {
	if !strings.HasPrefix(fn, "sim-") {
		return "", 0, fmt.Errorf("incorrectly formatted name, must match format sim-SIMNAME-job-JOBID.json")
	}

	if !strings.HasSuffix(fn, ".json") {
		return "", 0, fmt.Errorf("incorrectly formatted name, must match format sim-SIMNAME-job-JOBID.json")
	}

	n := strings.LastIndex(fn, "-")
	jobstr := fn[n+1 : len(fn)-5]
	jobid, err := strconv.Atoi(jobstr)
	if err != nil {
		return "", 0, fmt.Errorf("failed to parse job id from name: %s", err)
	}

	return fn[4:n], jobid, nil
}

type JobAllocation struct {
	Config          *JobConfig
	ID              int
	CheckinInterval time.Duration
}
