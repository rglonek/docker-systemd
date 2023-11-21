# CHANGELOG

## v0.3.0
* big change - not using double-process-init method for controlling and reaping processes; instead using a single init with a single dispatching `syscall.Wait4(-1,...)`
  * this allows for a more streamlined approach, single-pid init process, as well as improved tracking of other PIDs
  * more importantly, the forking-process wait system now can pause and wait on a mutex instead of timed polling, making this much more efficient

## v0.2.1
* support tracking processes that fork-detach themselves late (corner-case)
* running enable/start on a multi-instance will create that instance automatically

## v0.2.0
* first runnable release which properly tracks pids and reaps zombie processes
* added `ld_preload` feature for fork process tracking and multiple bugfixes
