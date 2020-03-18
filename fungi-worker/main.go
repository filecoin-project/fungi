package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"golang.org/x/xerrors"
	"gopkg.in/urfave/cli.v2"

	"github.com/whyrusleeping/fungi"
)

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

func NewWorker(coord, id string, nworkers int, secret string) *Worker {
	return &Worker{
		ID:             id,
		Coordinator:    coord,
		AuthSecret:     secret,
		JobLimiter:     make(chan struct{}, nworkers),
		JobsInProgress: make(map[int]struct{}),
		Results:        make(chan *fungi.JobResult, 16),
	}
}

type Worker struct {
	ID          string
	Coordinator string
	AuthSecret  string

	JobsInProgress map[int]struct{}
	lk             sync.Mutex

	JobLimiter chan struct{}
	Results    chan *fungi.JobResult
}

var ErrNoMoreJobs = fmt.Errorf("coordinator had no available jobs")

func (w *Worker) setHeaders(req *http.Request) {
	req.Header.Set("worker-id", w.ID)
	if w.AuthSecret != "" {
		req.Header.Set("auth-secret", w.AuthSecret)
	}
}

func (w *Worker) requestJob() (*fungi.JobAllocation, error) {
	req, err := http.NewRequest("GET", w.Coordinator+"/jobs/new", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %s", err)
	}

	w.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	switch resp.StatusCode {
	case 400:
		// TODO: read error message
		return nil, fmt.Errorf("response code 400")
	case http.StatusNoContent:
		return nil, ErrNoMoreJobs
	default:
		return nil, fmt.Errorf("unexpected response code %d", resp.StatusCode)
	case 200:
		// correct
	}

	defer resp.Body.Close()

	var alloc fungi.JobAllocation
	if err := json.NewDecoder(resp.Body).Decode(&alloc); err != nil {
		return nil, fmt.Errorf("failed to decode job allocation response: %s", err)
	}

	return &alloc, nil
}

func (w *Worker) getActiveJobs() []int {
	var activeJobs []int
	w.lk.Lock()
	for j := range w.JobsInProgress {
		activeJobs = append(activeJobs, j)
	}
	w.lk.Unlock()
	sort.Ints(activeJobs)
	log.Println("active jobs: ", activeJobs)
	return activeJobs
}

func (w *Worker) Checkin() error {
	data, err := json.Marshal(&fungi.CheckinBody{
		Jobs: w.getActiveJobs(),
	})
	if err != nil {
		return xerrors.Errorf("failed to marshal checkin body: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/jobs/checkin", w.Coordinator), bytes.NewReader(data))
	if err != nil {
		return xerrors.Errorf("failed to create checkin request: %w", err)
	}

	w.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return xerrors.Errorf("failed to perform checkin request: %w", err)
	}
	resp.Body.Close()

	return nil
}

func (w *Worker) markJobActive(id int, active bool) {
	w.lk.Lock()
	defer w.lk.Unlock()
	log.Println("setting job activity: ", id, active)
	if active {
		w.JobsInProgress[id] = struct{}{}
	} else {
		delete(w.JobsInProgress, id)
	}
}

func (w *Worker) Execute(ja *fungi.JobAllocation) {
	log.Printf("starting job %d", ja.ID)
	w.markJobActive(ja.ID, true)
	defer w.markJobActive(ja.ID, false)

	cmd := exec.Command(ja.Config.Cmd, ja.Config.Args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("command failure: ", err, string(out))
		res := &fungi.JobResult{
			JobID:   ja.ID,
			Success: false,
			Output:  string(out),
		}
		w.Results <- res
		return
	}

	w.Results <- &fungi.JobResult{
		JobID:   ja.ID,
		Success: true,
		Output:  string(out),
	}
}

func (w *Worker) SubmitResult(res *fungi.JobResult) error {
	log.Printf("submitting result for job %d", res.JobID)
	data, err := json.Marshal(res)
	if err != nil {
		return xerrors.Errorf("failed to marshal job result: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/jobs/%d/complete", w.Coordinator, res.JobID), bytes.NewReader(data))
	if err != nil {
		return xerrors.Errorf("failed to construct job result submission request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	w.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return xerrors.Errorf("request failed: %w", err)
	}

	if resp.StatusCode != 200 {
		log.Printf("job %d result submission returned http %d", res.JobID, resp.StatusCode)
	}
	return nil
}

func (w *Worker) SayHello() (*fungi.HelloResponse, error) {
	req, err := http.NewRequest("GET", w.Coordinator+"/jobs/hello", nil)
	if err != nil {
		return nil, xerrors.Errorf("failed to construct hello request: %w", err)
	}

	w.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, xerrors.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("say hello returned http %d", resp.StatusCode)
	}

	var hello fungi.HelloResponse
	if err := json.NewDecoder(resp.Body).Decode(&hello); err != nil {
		return nil, fmt.Errorf("failed to decode hello response: %w", err)
	}

	return &hello, nil
}

func (w *Worker) Run(checkinInterval time.Duration) {
	checkinTick := time.Tick(checkinInterval)
	for {
		select {
		case w.JobLimiter <- struct{}{}:
			ja, err := w.requestJob()
			if err != nil {
				if err == ErrNoMoreJobs {
					log.Println("server has no more jobs, sleeping 30 seconds and trying again...")
					go func() {
						time.Sleep(time.Second * 30)
						<-w.JobLimiter
					}()
					continue
				}
				log.Printf("got error from request job: %s", err)
				return
			}

			go w.Execute(ja)

		case <-checkinTick:
			log.Printf("checkin time!")
			if err := w.Checkin(); err != nil {
				log.Printf("checkin with coordinator failed: %s", err)
			}
		case res := <-w.Results:
			<-w.JobLimiter
			if err := w.SubmitResult(res); err != nil {
				log.Printf("failed to submit result for job %d: %s", res.JobID, err)
			}
		}

	}
}

func makeRandomName() string {
	buf := make([]byte, 6)
	rand.Read(buf)

	host, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	return host + "-" + fmt.Sprintf("%x", buf)
}

var RunCmd = &cli.Command{
	Name: "run",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "id",
			Usage: "manually specify ID for worker (optional)",
		},
		&cli.StringFlag{
			Name:    "auth-secret",
			EnvVars: []string{"FUNGI_AUTH_SECRET"},
			Usage:   "specify a secret that will be used to authenticate with the coordinator",
		},
		&cli.IntFlag{
			Name:  "task-count",
			Usage: "specify the number of tasks the worker will work on at a time",
			Value: 1,
		},
	},
	Action: func(cctx *cli.Context) error {
		if !cctx.Args().Present() {
			return fmt.Errorf("must specify coordinator URL")
		}

		curl := cctx.Args().First()

		id := cctx.String("id")
		if id == "" {
			id = makeRandomName()
		}

		w := NewWorker(curl, id, cctx.Int("task-count"), cctx.String("auth-secret"))

		log.Printf("starting up worker %s", id)

		hresp, err := w.SayHello()
		if err != nil {
			return fmt.Errorf("failed to say hello to coordinator: %s", err)
		}

		log.Printf("server checkin interval is: %s", hresp.CheckinInterval)
		w.Run(hresp.CheckinInterval)
		return nil
	},
}
