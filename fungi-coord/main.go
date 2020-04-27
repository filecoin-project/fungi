package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/fungi"
	"golang.org/x/xerrors"
	cli "gopkg.in/urfave/cli.v2"
)

type Coordinator struct {
	lk   sync.Mutex
	Jobs map[int]*Job

	Project    string
	ResultsDir string
	JobsDir    string

	Completions int
	Failures    int

	AuthSecret      string
	CheckinInterval time.Duration
}

func NewCoordinator(projectName string, jobsd, resd string, secret string, chint time.Duration) (*Coordinator, error) {
	if err := ensureWritable(resd); err != nil {
		return nil, fmt.Errorf("results directory was not writeable by the coordinator: %w", err)
	}

	if chint <= 0 {
		return nil, fmt.Errorf("invalid checkin interval: %s", chint)
	}

	jcfgs, err := fungi.LoadJobs(jobsd)
	if err != nil {
		return nil, fmt.Errorf("failed to load jobs: %w", err)
	}

	log.Printf("coordinator loaded %d jobs", len(jcfgs))

	c := &Coordinator{
		Project:         projectName,
		ResultsDir:      resd,
		JobsDir:         jobsd,
		AuthSecret:      secret,
		CheckinInterval: chint,
	}

	jobs := make(map[int]*Job)
	for id, j := range jcfgs {
		jobs[id] = &Job{
			JobID:  id,
			Config: j,
		}
	}

	// TODO: scan results directory to find already completed tasks
	c.Jobs = jobs

	if err := c.checkExistingResults(); err != nil {
		return nil, xerrors.Errorf("failed to check existing job results: %w", err)
	}

	return c, nil
}

func (c *Coordinator) checkExistingResults() error {
	resd, err := os.Open(c.ResultsDir)
	if err != nil {
		return err
	}
	defer resd.Close()

	names, err := resd.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, n := range names {
		s, job, err := parseResultFileName(n)
		if err != nil {
			return err
		}
		log.Printf("sim %s job %d already complete", s, job)
		j, ok := c.Jobs[job]
		if !ok {
			log.Printf("have result file for job we don't know about: %s", n)
		} else {
			j.Complete = true
		}
	}

	return nil
}

func parseResultFileName(fn string) (string, int, error) {
	if !strings.HasPrefix(fn, "sim-") {
		return "", 0, fmt.Errorf("incorrectly formatted name, must match format sim-SIMNAME-job-JOBID.json")
	}

	suff := "-results.json"
	if !strings.HasSuffix(fn, suff) {
		return "", 0, fmt.Errorf("incorrectly formatted name, must match format sim-SIMNAME-job-JOBID-result.json")
	}

	fn = fn[:len(fn)-len(suff)]
	n := strings.LastIndex(fn, "-")
	jobstr := fn[n+1:]

	jobid, err := strconv.Atoi(jobstr)
	if err != nil {
		return "", 0, fmt.Errorf("failed to parse job id from name: %s", err)
	}

	return fn[4:n], jobid, nil
}

var ErrNoMoreJobs = fmt.Errorf("no more available jobs")

func (c *Coordinator) AllocateJob(worker string) (*fungi.JobAllocation, error) {
	c.lk.Lock()
	defer c.lk.Unlock()

	for _, j := range c.Jobs {
		if !j.Complete && j.Executor == "" {
			j.Executor = worker
			j.StartTime = time.Now()
			j.LastCheckin = j.StartTime

			log.Printf("assigning job %d to worker %s", j.JobID, worker)
			return &fungi.JobAllocation{
				Config: j.Config,
				ID:     j.JobID,
			}, nil
		}
	}

	return nil, ErrNoMoreJobs
}

func (c *Coordinator) RegisterCheckin(worker string, checkin *fungi.CheckinBody) {
	c.lk.Lock()
	defer c.lk.Unlock()
	for _, job := range checkin.Jobs {
		j, ok := c.Jobs[job]
		if !ok {
			log.Printf("attempted to register checkin for job that didnt exist")
			return
		}

		log.Printf("worker %s checking in on job %d. Time elapsed: %s", worker, job, time.Since(j.StartTime))
		j.LastCheckin = time.Now()
	}
}

func (c *Coordinator) JobComplete(worker string, job int, result *fungi.JobResult) {
	c.lk.Lock()
	defer c.lk.Unlock()

	j, ok := c.Jobs[job]
	if !ok {
		log.Printf("result was submitted for non-existant job %d by worker %s", job, worker)
		return
	}

	if j.Executor != worker {
		log.Printf("got job completion submission from different worker than was assigned to job %d", job)
		// TODO: eh, if they did the job they did the job
	}

	if !result.Success {
		log.Printf("Worker %s returned a failure for job %d", worker, job)
		j.Executor = ""
		j.LastCheckin = time.Time{}
		j.StartTime = time.Time{}
		j.Failures++
		c.Failures++
		return
	}

	log.Printf("got successful result for job %d from worker %s", j.JobID, worker)
	j.LastCheckin = time.Now()
	j.Complete = true
	c.Completions++

	for retry := 0; retry < 5; retry++ {
		if err := c.writeResult(job, result); err == nil {
			break
		} else {
			if retry == 4 {
				log.Printf("failed to write results after 5 retries, giving up")
				// TODO: what now?
				return
			}
			log.Printf("Failed to write result to disk: %s. Retrying in 30s...", err)
			time.Sleep(time.Second * 30)
		}
	}
}

func (c *Coordinator) writeResult(job int, res *fungi.JobResult) error {
	fname := filepath.Join(c.ResultsDir, fmt.Sprintf("sim-%s-job-%d-results.json", c.Project, job))
	fi, err := os.Create(fname)
	if err != nil {
		return xerrors.Errorf("failed to create results file: %w", err)
	}
	defer fi.Close()

	if _, err := fi.Write(res.Output); err != nil {
		return xerrors.Errorf("failed to write result to file: %w", err)
	}
	return nil
}

type Job struct {
	Executor    string // the ID of the node currently running this job
	StartTime   time.Time
	LastCheckin time.Time
	Complete    bool
	Failures    int

	JobID  int
	Config *fungi.JobConfig
}

const JobNameTemplate = "sim-%s-job-%d.json"

func ensureWritable(dir string) error {
	tfname := filepath.Join(dir, ".testwrite")

	fi, err := os.Create(tfname)
	if err != nil {
		return xerrors.Errorf("failed to create file in directory: %w", err)
	}

	n, err := fi.Write([]byte("test"))
	if err != nil {
		return xerrors.Errorf("write failed: %w", err)
	}

	if n != 4 {
		return xerrors.Errorf("write test failed, only wrote %d bytes", n)
	}

	_ = fi.Close()
	_ = os.Remove(tfname)

	return nil
}

func main() {
	app := &cli.App{
		Commands: []*cli.Command{
			RunCmd,
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println("command failed: ", err)
	}
}

var RunCmd = &cli.Command{
	Name: "run",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "jobs-dir",
			Usage: "specify the directory containing the job files to execute",
		},
		&cli.StringFlag{
			Name:  "results-dir",
			Usage: "specify the directory to place execution outputs in",
		},
		&cli.StringFlag{
			Name:  "project",
			Usage: "Give a name to this particular set of work",
		},
		&cli.StringFlag{
			Name:    "auth-secret",
			EnvVars: []string{"FUNGI_AUTH_SECRET"},
			Usage:   "specify a secret that workers must use in order to connect",
		},
		&cli.DurationFlag{
			Name:  "checkin-interval",
			Usage: "specify how often workers should check in",
			Value: time.Second * 15,
		},
	},
	Action: func(cctx *cli.Context) error {
		jobsd := cctx.String("jobs-dir")
		if jobsd == "" {
			return fmt.Errorf("must specify a jobs directory with --jobs-dir")
		}

		resd := cctx.String("results-dir")
		if resd == "" {
			return fmt.Errorf("must specify a results directory with --results-dir")
		}

		project := cctx.String("project")
		if project == "" {
			project = "fungi"
		}

		c, err := NewCoordinator(project, jobsd, resd, cctx.String("auth-secret"), cctx.Duration("checkin-interval"))
		if err != nil {
			return fmt.Errorf("failed to set up coordinator: %w", err)
		}

		if err := c.ServeJobs(":5292"); err != nil {
			return fmt.Errorf("failed to start jobs server: %s", err)
		}
		return nil
	},
}
