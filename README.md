#  Mirror Go

A Grand Sync Controller for MirrorSite

一个用于 MirrorSite 的通用同步控制器，负责任务排队、调度分发、执行与监控，面向多节点与容器化执行场景。

## Features

- [ ] Job Queuing
    - [ ] Retry & Backoff
    - [ ] Cron Scheduling
- [ ] Docker Execution
- [ ] Multi-Node Sync
- [ ] Job Monitoring
- [ ] Web UI
- [ ] Metrics & Alerting


##  Process Of Queue

1. When program starts, read all jobs from data folder, each repo is a json file in `Configs` folder, the Job info will be loaded into memory `Repos`
2. When program starts, read sync job state file (sqlite), store job runtime status into `Jobs`
3. When program starts, read sync activeaction state file (sqlite), store job runtime status into `ActiveActions`

4. Then do some migrations(if have)
    if there is a new repo that doest exist in Jobs, create a new job in `Jobs`, or update status from "Orphan" to "Waiting"
    if there is a job no longer exists in Repos, mark it as "orphan job" (status: Orphan), remove it from Queue (if have), remove it from active actions (if have) (we just dropped the container running wildly)


5. Start Scheduling
    A Job have following status:
        - Waiting (Waiting for NextAttemptAt to be arrived)
        - Scheduled (NextAttemptAt is passed)
        - Running (if the max_parallel is not reached, from Scheduled to Running)
        - Orphan (if the job no longer exists in Repos)

    Status Transition:
        - Waiting to Scheduled: NextAttemptAt is passed, add to FIFO queue
        - Scheduled to Running: if the max_parallel is not reached, Start A New Action (container),
        - Running to Waiting: if the action is finished (container exited), update NextAttemptAt = Now + Interval


    Polling:
        - Every 10 seconds, check if there is a job in FIFO queue, if there is, start a new action if active actions is less than max_parallel
        - for every action, a goroutine will be started to monitor the action, if the action is finished, update the action status, and update the job status
        - Every 5 second, poll the jobs, if the time is passed, update the job status to "Scheduled"

    there should be  locks for "Jobs" map, ActiveActions map, and Queue

    Use sqlite to store the state of Jobs and ActiveActions

    In this first version, docker Part should be "mocked", every container should be exited in 20-60 seconds, and the exit code will be 0