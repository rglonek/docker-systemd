package main

import (
	"docker-systemd/common"
	"docker-systemd/journalctl"
	"docker-systemd/systemctl"
	"docker-systemd/systemd"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	_ "embed"

	"github.com/bestmethod/inslice"
)

//go:embed VERSION
var version string

func main() {
	_, name := path.Split(os.Args[0])
	if os.Getpid() == 1 {
		name = "init"
	}
	switch name {
	case "journalctl":
		journalctl.Main()
	case "systemctl":
		if len(os.Args) == 2 && os.Args[1] == "version" {
			fmt.Println(strings.Trim(version, "\r\n\t "))
			return
		}
		systemctl.Main(os.Args)
	case "poweroff":
		systemctl.Main([]string{os.Args[0], "poweroff"})
	case "shutdown":
		systemctl.Main([]string{os.Args[0], "poweroff"})
	case "service":
		args := os.Args
		if len(args) > 2 {
			prev := args[1]
			args[1] = os.Args[2]
			args[2] = prev
		}
		systemctl.Main(args)
	default:
		startTime := time.Now().Format(time.RFC3339)
		log.Printf("INIT: Booting <%s>", startTime)
		install()
		os.WriteFile(common.GetBootFile(), []byte(startTime), 0644)
		systemd.Main()
	}
}

func install() {
	me, err := os.Executable()
	if err != nil {
		log.Fatalf("Could not get excutable name of self: %s", err)
	}
	fp := filepath.SplitList(os.Getenv("PATH"))
	suitablePaths := []string{"/usr/local/sbin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	basePath := "/usr/sbin"
	for _, item := range fp {
		if inslice.HasString(suitablePaths, item) {
			basePath = strings.TrimRight(item, "/")
			break
		}
	}
	for _, f := range []string{"/journalctl", "/systemctl", "/systemd", "/init", "/poweroff", "/shutdown", "/service"} {
		f = basePath + f
		if me == f {
			continue
		}
		if nstat, err := os.Lstat(f); err != nil || nstat.Mode()&os.ModeSymlink == 0 {
			if err == nil {
				log.Printf("%v", nstat.Mode())
			}
			log.Printf("File not found, or not a symlink; Linking %s => %s", me, f)
			os.Rename(f, f+".old")
			err = os.Symlink(me, f)
			if err != nil {
				log.Fatalf("Could not link %s to %s: %s", me, f, err)
			}
		} else if nstat.Mode()&os.ModeSymlink != 0 {
			dest, err := os.Readlink(f)
			if err != nil || dest != me {
				log.Printf("Can not read symlink or symlink points to the wrong file; Linking %s => %s", me, f)
				os.Rename(f, f+".old")
				err = os.Symlink(me, f)
				if err != nil {
					log.Fatalf("Could not link %s to %s: %s", me, f, err)
				}
			}
		}
	}
}
