/*
 * libcdaemon — the L2 helper for the child-creation paths that a Go program
 * cannot express, and that the retired LD_PRELOAD shim scored zero on
 * (designs/docs/next/03-process-model.md §2):
 *
 *   daemon(3)       the canonical daemonisation call; glibc calls its internal
 *                   __fork alias, so the interposed fork symbol never sees it
 *   posix_spawn(3)  goes through clone(2) directly
 *   vfork(2)        likewise
 *
 * Built by the test harness when a C toolchain is present; the harness skips
 * the corresponding cases when it is not.
 *
 *   cc -O1 -o libcdaemon libcdaemon.c
 */
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <spawn.h>
#include <sys/types.h>
#include <sys/wait.h>

extern char **environ;

static void park(void)
{
    /* Stay alive until signalled, like a real service. */
    for (;;)
        pause();
}

int main(int argc, char **argv)
{
    if (argc < 2) {
        fprintf(stderr, "usage: libcdaemon <libc-daemon|posix-spawn|vfork|park>\n");
        return 2;
    }

    if (strcmp(argv[1], "park") == 0) {
        printf("libcdaemon: parked pid=%d ppid=%d\n", getpid(), getppid());
        fflush(stdout);
        park();
    }

    if (strcmp(argv[1], "libc-daemon") == 0) {
        /* daemon(3): fork, parent exits, child setsid()s and continues. */
        if (daemon(1, 1) != 0) {
            perror("daemon");
            return 1;
        }
        printf("libcdaemon: daemon pid=%d ppid=%d\n", getpid(), getppid());
        fflush(stdout);
        park();
    }

    if (strcmp(argv[1], "posix-spawn") == 0) {
        pid_t pid;
        char *args[] = { argv[0], "park", NULL };
        if (posix_spawn(&pid, argv[0], NULL, NULL, args, environ) != 0) {
            perror("posix_spawn");
            return 1;
        }
        printf("libcdaemon: posix_spawn child=%d, parent exiting\n", pid);
        fflush(stdout);
        return 0;
    }

    if (strcmp(argv[1], "vfork") == 0) {
        pid_t pid = vfork();
        if (pid == 0) {
            char *args[] = { argv[0], "park", NULL };
            execv(argv[0], args);
            _exit(127);
        }
        if (pid < 0) {
            perror("vfork");
            return 1;
        }
        printf("libcdaemon: vfork child=%d, parent exiting\n", pid);
        fflush(stdout);
        return 0;
    }

    fprintf(stderr, "libcdaemon: unknown mode %s\n", argv[1]);
    return 2;
}
