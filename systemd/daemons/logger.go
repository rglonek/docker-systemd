package daemons

import (
	"docker-systemd/common"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
)

var LogToStderr = false
var LogToFile = true

type Logger struct {
	f   *os.File
	out *log.Logger
}

func NewLogger(service string) (*Logger, error) {
	destPath := path.Join(common.GetLogPath(), common.GetUnitLogPath(service))
	var nlog *log.Logger
	if LogToStderr {
		nlog = log.New(os.Stderr, fmt.Sprintf("<%s> ", service), log.Default().Flags())
	}
	if !LogToFile {
		return &Logger{
			out: nlog,
		}, nil
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	l := &Logger{
		f:   f,
		out: nlog,
	}
	return l, nil
}

func (l *Logger) Write(p []byte) (n int, err error) {
	if l.out != nil {
		for _, line := range strings.Split(string(p), "\n") {
			if line == "" {
				continue
			}
			l.out.Print(strings.TrimRight(line, "\r\n"))
		}
	}
	if l.f != nil {
		return l.f.Write(p)
	}
	return len(p), nil
}

func (l *Logger) Close() error {
	return l.f.Close()
}
