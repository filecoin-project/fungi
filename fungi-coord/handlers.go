package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/whyrusleeping/fungi"
	"golang.org/x/xerrors"
)

type FungiContext struct {
	echo.Context
	Worker string
}

func (c *Coordinator) ServeJobs(addr string) error {
	e := echo.New()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if err := next(ctx); err != nil {
				log.Println("got error: ", err)
			}
			return nil
		}
	})
	e.Use(c.checkAuth)
	e.GET("/stats", c.handleStats)

	j := e.Group("/jobs", c.extractWorker)
	j.GET("/new", c.handleJobRequest)
	j.POST("/checkin", c.handleJobCheckin)
	j.POST("/:job/complete", c.handleJobCompletion)
	j.GET("/hello", c.handleHello)

	return e.Start(addr)
}

func (c *Coordinator) checkAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(ctx echo.Context) error {
		if c.AuthSecret != "" {
			auth := ctx.Request().Header.Get("auth-secret")
			if auth != c.AuthSecret {
				return fmt.Errorf("invalid auth token")
			}
		}
		return next(ctx)
	}
}

func (c *Coordinator) extractWorker(next echo.HandlerFunc) echo.HandlerFunc {
	return func(ctx echo.Context) error {
		worker := ctx.Request().Header.Get("worker-id")
		if worker == "" {
			return fmt.Errorf("must specify worker ID")
		}

		return next(&FungiContext{Context: ctx, Worker: worker})
	}
}

type FungiStats struct {
	CompletedJobs  int
	InProgressJobs int
	TotalJobs      int
	Failures       int
	Workers        []string
}

func (c *Coordinator) handleStats(ctx echo.Context) error {
	c.lk.Lock()

	var stats FungiStats
	for _, j := range c.Jobs {
		stats.TotalJobs++
		stats.Failures += j.Failures

		if j.Complete {
			stats.CompletedJobs++
		} else if j.Executor != "" {
			stats.InProgressJobs++
		}
	}
	c.lk.Unlock()

	return ctx.JSON(200, &stats)
}

func (c *Coordinator) handleHello(ectx echo.Context) error {
	ctx := ectx.(*FungiContext)

	log.Printf("got hello from worker %s", ctx.Worker)
	hresp := &fungi.HelloResponse{
		CheckinInterval: c.CheckinInterval,
	}
	return ctx.JSON(200, hresp)
}

func (c *Coordinator) handleJobRequest(ectx echo.Context) error {
	ctx := ectx.(*FungiContext)

	jalloc, err := c.AllocateJob(ctx.Worker)
	if err != nil {
		if err == ErrNoMoreJobs {
			return ctx.String(http.StatusNoContent, "no available jobs")
		}
		log.Printf("failed to allocate job for worker %s: %s", ctx.Worker, err)
		return fmt.Errorf("failed to allocate job")
	}

	return ctx.JSON(200, jalloc)
}

func getJobID(ctx *FungiContext) (int, error) {
	job := ctx.Param("job")
	if job == "" {
		log.Printf("got checkin request from worker %s without a job ID", ctx.Worker)
		return 0, fmt.Errorf("must specify job ID")
	}

	jobid, err := strconv.Atoi(job)
	if err != nil {
		log.Printf("failed to parse job ID (%q) for checkin from worker %s: %s", job, ctx.Worker, err)
		return 0, fmt.Errorf("failed to parse job ID: %w", err)
	}

	return jobid, nil
}

func (c *Coordinator) handleJobCheckin(ectx echo.Context) error {
	ctx := ectx.(*FungiContext)

	var checkin fungi.CheckinBody
	if err := ctx.Bind(&checkin); err != nil {
		return xerrors.Errorf("failed to bind request body to proper type: %w", err)
	}

	c.RegisterCheckin(ctx.Worker, &checkin)
	return nil
}

func (c *Coordinator) handleJobCompletion(ectx echo.Context) error {
	ctx := ectx.(*FungiContext)

	jobid, err := getJobID(ctx)
	if err != nil {
		log.Printf("failed to get job id: %s", err)
		return err
	}

	var res fungi.JobResult
	if err := ctx.Bind(&res); err != nil {
		log.Printf("failed to read job result for job %d from worker %s: %s", jobid, ctx.Worker, err)
		return err
	}

	c.JobComplete(ctx.Worker, jobid, &res)
	return nil
}
