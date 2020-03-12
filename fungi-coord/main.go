package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
}

func NewCoordinator(projectName string, jobsd, resd string) (*Coordinator, error) {
	if err := ensureWritable(resd); err != nil {
		return nil, fmt.Errorf("results directory was not writeable by the coordinator: %w", err)
	}

	jcfgs, err := fungi.LoadJobs(jobsd)
	if err != nil {
		return nil, fmt.Errorf("failed to load jobs: %w", err)
	}

	log.Printf("coordinator loaded %d jobs", len(jcfgs))

	c := &Coordinator{
		Project:    projectName,
		ResultsDir: resd,
		JobsDir:    jobsd,
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

	return c, nil
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
				Config:          j.Config,
				ID:              j.JobID,
				CheckinInterval: time.Second * 5,
			}, nil
		}
	}

	return nil, ErrNoMoreJobs
}

func (c *Coordinator) RegisterCheckin(worker string, job int) {
	c.lk.Lock()
	defer c.lk.Unlock()
	j, ok := c.Jobs[job]
	if !ok {
		log.Printf("attempted to register checkin for job that didnt exist")
		return
	}

	log.Printf("worker %s checking in on job %d. Time elapsed: %s", worker, job, time.Since(j.StartTime))
	j.LastCheckin = time.Now()
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

	if err := json.NewEncoder(fi).Encode(res); err != nil {
		return xerrors.Errorf("failed to write result to file: %w", err)
	}
	return nil
}

func (c *Coordinator) handleHello(w http.ResponseWriter, r *http.Request) {
	worker := r.Header.Get("worker-id")
	if worker == "" {
		http.Error(w, "must specify worker ID", 400)
		return
	}

	log.Printf("got hello from worker %s", worker)
	w.WriteHeader(200)
}

func (c *Coordinator) handleJobRequest(w http.ResponseWriter, r *http.Request) {
	worker := r.Header.Get("worker-id")
	if worker == "" {
		http.Error(w, "must specify worker ID", 400)
		return
	}

	jalloc, err := c.AllocateJob(worker)
	if err != nil {
		if err == ErrNoMoreJobs {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		log.Printf("failed to allocate job for worker %s: %s", worker, err)
		http.Error(w, "failed to allocate job", 400)
		return
	}

	if err := json.NewEncoder(w).Encode(jalloc); err != nil {
		log.Printf("failed to send job allocation back to worker %s: %s", worker, err)
		// TODO: unallocate job?
		return
	}
}

func (c *Coordinator) handleJobCheckin(w http.ResponseWriter, r *http.Request) {
	worker := r.Header.Get("worker-id")
	if worker == "" {
		log.Println("got a checkin request without a worker ID")
		http.Error(w, "must specify worker ID", 400)
		return
	}

	job := r.Header.Get("job-id")
	if job == "" {
		log.Printf("got checkin request from worker %s without a job ID", worker)
		http.Error(w, "must specify job ID", 400)
		return
	}

	jobid, err := strconv.Atoi(job)
	if err != nil {
		log.Printf("failed to parse job ID (%q) for checkin from worker %s: %s", job, worker, err)
		http.Error(w, "failed to parse job ID", 400)
		return
	}

	c.RegisterCheckin(worker, jobid)
}

func (c *Coordinator) handleJobCompletion(w http.ResponseWriter, r *http.Request) {
	worker := r.Header.Get("worker-id")
	if worker == "" {
		log.Println("got a checkin request without a worker ID")
		http.Error(w, "must specify worker ID", 400)
		return
	}

	job := r.Header.Get("job-id")
	if job == "" {
		log.Printf("got checkin request from worker %s without a job ID", worker)
		http.Error(w, "must specify job ID", 400)
		return
	}

	jobid, err := strconv.Atoi(job)
	if err != nil {
		log.Printf("failed to parse job ID (%q) for checkin from worker %s: %s", job, worker, err)
		http.Error(w, "failed to parse job ID", 400)
		return
	}

	var res fungi.JobResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		log.Printf("failed to read job result for job %d from worker %s: %s", jobid, worker, err)
		http.Error(w, "failed to read result", 400)
		return
	}

	c.JobComplete(worker, jobid, &res)
}

func (c *Coordinator) ServeJobs(addr string) error {
	http.HandleFunc("/jobs/new", c.handleJobRequest)
	http.HandleFunc("/jobs/checkin", c.handleJobCheckin)
	http.HandleFunc("/jobs/complete", c.handleJobCompletion)
	http.HandleFunc("/hello", c.handleHello)

	return http.ListenAndServe(addr, nil)
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

		c, err := NewCoordinator(project, jobsd, resd)
		if err != nil {
			return fmt.Errorf("failed to set up coordinator: %w", err)
		}

		if err := c.ServeJobs(":5292"); err != nil {
			return fmt.Errorf("failed to start jobs server: %s", err)
		}
		return nil
	},
}
