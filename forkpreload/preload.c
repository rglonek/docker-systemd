#define _GNU_SOURCE
#include <unistd.h>
#include <stdio.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <dlfcn.h>

void __docker_systemd_log_relationship(pid_t *infant);
void __docker_systemd_log_relationship_to_file(pid_t *infant, char *name);
void __docker_systemd_log_relationship_to_socket(pid_t *infant, char *name);
int (*real_execve)(const char*, char* const*, char* const*)=NULL;
pid_t (*real_fork)(void)=NULL;

pid_t fork(void) {
    real_fork = dlsym(RTLD_NEXT, "fork");
    pid_t child = real_fork();
    if (child == 0) {
        return child;
    }
    __docker_systemd_log_relationship(&child);
    return child;
}

int execve(const char *pathname, char *const argv[], char *const envp[]) {
    __docker_systemd_log_relationship(NULL);
    real_execve = dlsym(RTLD_NEXT, "execve");
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
    if (fp == NULL) {
        return;
    }
    pid_t parent = getppid();
    pid_t child = getpid();
    if (infant == NULL) {
        fprintf(fp, "%d:%d\n", parent, child);
    } else {
        fprintf(fp, "%d:%d:%d\n", parent, child, *infant);
    }
    fclose(fp);
}

void __docker_systemd_log_relationship_to_socket(pid_t *infant, char *name) {
    int server_socket = socket(AF_UNIX, SOCK_STREAM, 0);
    struct sockaddr_un server_addr;
    server_addr.sun_family = AF_UNIX;
    strcpy(server_addr.sun_path, name);
    int connection_result = connect(server_socket, (struct sockaddr *)&server_addr, sizeof(server_addr));
    if (connection_result < 0) {
        return;
    }
    pid_t parent = getppid();
    pid_t child = getpid();
    char relations[24];
    if (infant == NULL) {
        sprintf(relations, "%d:%d", parent, child);
    } else {
        sprintf(relations, "%d:%d:%d", parent, child, *infant);
    }
    write(server_socket, &relations, strlen(relations));
    close(server_socket);
}
