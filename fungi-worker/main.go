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

func NewWorker(coord, id, secret string) *Worker {
	return &Worker{
		ID:          id,
		Coordinator: coord,
		AuthSecret:  secret,
		JobLimiter:  make(chan struct{}, 1),
		Results:     make(chan *fungi.JobResult, 16),
	}
}

type Worker struct {
	ID          string
	Coordinator string
	AuthSecret  string

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

func (w *Worker) Checkin(ja *fungi.JobAllocation) error {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/jobs/%d/checkin", w.Coordinator, ja.ID), nil)
	if err != nil {
		return xerrors.Errorf("failed to create checkin request: %w", err)
	}

	w.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return xerrors.Errorf("failed to perform checkin request: %w", err)
	}
	resp.Body.Close()

	return nil
}

func (w *Worker) Execute(ja *fungi.JobAllocation) {
	log.Printf("starting job %d", ja.ID)

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

func (w *Worker) SayHello() error {
	req, err := http.NewRequest("GET", w.Coordinator+"/hello", nil)
	if err != nil {
		return xerrors.Errorf("failed to construct hello request: %w", err)
	}

	w.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return xerrors.Errorf("request failed: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("say hello returned http %d", resp.StatusCode)
	}
	return nil

}

func (w *Worker) Run() {
	var checkinTick <-chan time.Time
	var curJob *fungi.JobAllocation
	for {
		select {
		case w.JobLimiter <- struct{}{}:
			ja, err := w.requestJob()
			if err != nil {
				if err == ErrNoMoreJobs {
					log.Println("server has no more jobs, sleeping 30 seconds and trying again...")
					<-w.JobLimiter
					time.Sleep(time.Second * 30)
					continue
				}
				log.Printf("got error from request job: %s", err)
				return
			}

			cinterval := ja.CheckinInterval
			if cinterval == 0 {
				cinterval = time.Second * 10
			}

			checkinTick = time.Tick(cinterval) // todo: this is a leak, fix
			go w.Execute(ja)
			curJob = ja

		case <-checkinTick:
			if err := w.Checkin(curJob); err != nil {
				log.Printf("checkin with coordinator failed: %s", err)
			}
		case res := <-w.Results:
			<-w.JobLimiter
			if err := w.SubmitResult(res); err != nil {
				log.Printf("failed to submit result for job %d: %s", res.JobID, err)
			}
			curJob = nil
			checkinTick = nil
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
			Name:  "auth-secret",
			Usage: "specify a secret that will be used to authenticate with the coordinator",
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

		w := NewWorker(curl, id, cctx.String("auth-secret"))

		log.Printf("starting up worker %s", id)

		if err := w.SayHello(); err != nil {
			return fmt.Errorf("failed to say hello to coordinator: %s", err)
		}

		w.Run()
		return nil
	},
}
