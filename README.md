# fungi

A very simple distribute task runner.


# Architecture

Fungi is comprised of two main components, the coordinator, and the worker. A fungi coordinator is started up with a set of jobs to complete, and starts up a job server to serve those jobs to workers.
Fungi workers can run on any machine and connect to the coordinator over http. Once they start up they will issue a quick hello to handshake with the coordinator, and then will start requesting tasks, executing them, and returning the results.

# Building
Just run `make`.

# Running

## 1. Job Configuration
First, you need to generate job files. Ideally this is an automated process, but you can also manually create the job files yourself. The job files should all be placed in the same directory, and follow the naming scheme of `sim-SIMNAME-job-JOBID.json`.

The files (as of writing) should look something like:
```json
{
 "Cmd": "python",
 "Args": ["-c", "import time; time.sleep(20); print(\"1\")"]
}
```

## 2. Run Coordinator
Once you have your job files written, you can start up the coordinator.
```
coord run --jobs-dir=/path/to/jobs/ --results-dir=/path/for/outputs/
```

This will spin up the coordinator process and start the jobs server on port :5292

## 3. Run Workers
Now, you can run as many workers as you'd like, pointing them at your coordinator.
The worker binary is called 'spore'.
```
spore run http://localhost:5292
```



## License
MIT

