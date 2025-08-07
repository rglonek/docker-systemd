#define _GNU_SOURCE
#include <unistd.h>
#include <stdio.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <dlfcn.h>
#include <inttypes.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <stdint.h>

void __docker_systemd_log_relationship(pid_t *infant);
void __docker_systemd_log_relationship_to_file(pid_t *infant, char *name);
void __docker_systemd_log_relationship_to_socket(pid_t *infant, char *name);
int (*real_execve)(const char*, char* const*, char* const*)=NULL;
pid_t (*real_fork)(void)=NULL;

pid_t fork(void) {
    if (!real_fork) {
        real_fork = dlsym(RTLD_NEXT, "fork");
        if (!real_fork) {
            fprintf(stderr, "LD_PRELOAD: failed to resolve fork(): %s\n", dlerror());
            errno = ENOSYS;
            return -1;
        };
    };
    pid_t child = real_fork();
    if (child != 0) {
        __docker_systemd_log_relationship(&child);
    }
    return child;
}

int execve(const char *pathname, char *const argv[], char *const envp[]) {
    __docker_systemd_log_relationship(NULL);
    if (!real_execve) {
        real_execve = dlsym(RTLD_NEXT, "execve");
        if (!real_execve) {
            fprintf(stderr, "LD_PRELOAD: failed to resolve execve(): %s\n", dlerror());
            errno = ENOSYS;
            return -1;
        }
    };
    return real_execve(pathname, argv, envp);
}

void __docker_systemd_log_relationship(pid_t *infant) {
    char *filename = "/var/log/pidtrack.log";
    char *sockname = "/tmp/docker-systemd-pidtrack.sock";
    if (access(filename, F_OK) == 0) {
        __docker_systemd_log_relationship_to_file(infant, filename);
    }
    if (access(sockname, F_OK) == 0) {
        __docker_systemd_log_relationship_to_socket(infant, sockname);
    }
}

void __docker_systemd_log_relationship_to_file(pid_t *infant, char *name) {
    FILE* fp = fopen(name ,"a");
    if (!fp) return;
    pid_t parent = getppid();
    pid_t child = getpid();
    if (infant) {
        fprintf(fp, "%jd:%jd:%jd\n", (intmax_t)parent, (intmax_t)child, (intmax_t)*infant);
    } else {
        fprintf(fp, "%jd:%jd\n", (intmax_t)parent, (intmax_t)child);
    }
    fclose(fp);
}

void __docker_systemd_log_relationship_to_socket(pid_t *infant, char *name) {
    int server_socket = socket(AF_UNIX, SOCK_STREAM, 0);
    if (server_socket < 0) return;

    struct sockaddr_un server_addr;
    memset(&server_addr, 0, sizeof(struct sockaddr_un));
    server_addr.sun_family = AF_UNIX;
    strncpy(server_addr.sun_path, name, sizeof(server_addr.sun_path) - 1);

    int connection_result = connect(server_socket, (struct sockaddr *)&server_addr, sizeof(server_addr));
    if (connection_result < 0) {
        close(server_socket);
        return;
    }
    pid_t parent = getppid();
    pid_t child = getpid();
    char relations[36];
    if (infant) {
        snprintf(relations, sizeof(relations), "%jd:%jd:%jd", (intmax_t)parent, (intmax_t)child, (intmax_t)*infant);
    } else {
        snprintf(relations, sizeof(relations), "%jd:%jd", (intmax_t)parent, (intmax_t)child);
    }
    write(server_socket, relations, strlen(relations));
    close(server_socket);
}
