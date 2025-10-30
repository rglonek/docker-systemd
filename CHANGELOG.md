# CHANGELOG

## v0.4.5
* add `dockerbuild.sh` script to build `LD_PRELOAD` libraries for different architectures
* build against older supported versions of `glibc`

## v0.4.2
* add error handling to `LD_PRELOAD` libraries
* fix `arm64` versions of `LD_PRELOAD` libraries

## v0.4.1
* daemons should also inherit `os.Environ()` of systemd process
* support `set-environment` and `unset-environment` features of systemd/systemctl
* disable journalctl pager; feature will be ignored; to page, simply pipe to `more` or `less`

## v0.4.0
* redo the `systemd` daemon handler, using signals and states instead of long-lasting mutex locks to allow for better state flow and querying capability
* add `version` systemctl command to print version

## v0.3.3
* bump dependencies
* automated docker build system

## v0.3.2
* improve `syscall.Wait4` call error handling in `procwait`
* improve communication protocol between `systemd.command` and `systemctl` so that multiple messages can be sent and printed to user during the command execution process

## v0.3.1
* add `--now` option to `systemctl enable`

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
