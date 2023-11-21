package journalctl

import (
	"bufio"
	"docker-systemd/common"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strconv"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/mattn/go-isatty"
)

type opts struct {
	Since   string `short:"S" long:"since" description:"format: 2012-10-30 18:17:16"`
	since   time.Time
	Until   string `short:"U" long:"until" description:"format: 2012-10-30 18:17:16"`
	until   time.Time
	Boot    bool   `short:"b" long:"boot" description:"since reboot"`
	Unit    string `short:"u" long:"unit" description:"unit name"`
	Lines   int    `short:"n" long:"lines" description:"show max X last lines"`
	Follow  bool   `short:"f" long:"follow" description:"follow log; implies lines"`
	NoPager bool   `long:"no-pager" description:"do not page results; implied with follow"`
	Help    bool   `short:"h" long:"help" description:"display help"`
}

func Main() {
	opt := &opts{}
	parser := flags.NewParser(opt, flags.IgnoreUnknown)
	_, err := parser.Parse()
	if err != nil {
		log.Fatal(err)
	}
	if opt.Help {
		parser.WriteHelp(os.Stderr)
		os.Exit(1)
	}
	if opt.Unit == "" {
		log.Fatal("ERR Unit name is required")
	}

	logFile := path.Join(common.GetLogPath(), common.GetUnitLogPath(opt.Unit))
	if _, err := os.Stat(logFile); err != nil {
		log.Fatalf("ERR Log file %s not accessible: %s", logFile, err)
	}

	if opt.Since != "" {
		opt.since, err = time.Parse(time.DateTime, opt.Since)
		if err != nil {
			log.Fatalf("ERR Wrong 'since' time format: %s", err)
		}
	}
	if opt.Until != "" {
		opt.until, err = time.Parse(time.DateTime, opt.Until)
		if err != nil {
			log.Fatalf("ERR Wrong 'until' time format: %s", err)
		}
	}

	if opt.Boot {
		bootTimeString, err := os.ReadFile(common.GetBootFile())
		if err == nil {
			bootTime, err := time.Parse(time.RFC3339, string(bootTimeString))
			if err == nil && !bootTime.IsZero() {
				if opt.since.IsZero() || bootTime.After(opt.since) {
					opt.since = bootTime
				}
			}
		}
	}

	if opt.Follow {
		if opt.Lines == 0 {
			opt.Lines = 10
		}
		cmd := exec.Command("tail", "-n", strconv.Itoa(opt.Lines), "-f", logFile)
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		err = cmd.Run()
		if err != nil {
			os.Exit(1)
		}
		return
	}

	if opt.Lines > 0 {
		cmd := exec.Command("tail", "-n", strconv.Itoa(opt.Lines))
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		err = cmd.Run()
		if err != nil {
			os.Exit(1)
		}
		return
	}

	f, err := os.Open(logFile)
	if err != nil {
		log.Fatalf("ERR Cannot open log file %s for reading: %s", logFile, err)
	}
	defer f.Close()

	if !opt.NoPager {
		p := &pager{}
		p.Page()
		defer p.Close()
	}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !opt.since.IsZero() && !opt.until.IsZero() {
			if len(line) < 19 {
				continue
			}
			ts, err := time.Parse(common.LogTimeFormat(), line[0:19])
			if err != nil {
				log.Printf("TIME FORMAT ERROR ON LINE %s: %s", line, err)
			}
			if !opt.since.IsZero() && ts.Before(opt.since) {
				continue
			}
			if !opt.until.IsZero() && ts.After(opt.until) {
				return
			}
		}
		fmt.Println(line)
	}
}

type pager struct {
	isClosable bool
	cmd        *exec.Cmd // wait
	w          *os.File
}

func (p *pager) Close() {
	if !p.isClosable {
		return
	}
	p.cmd.Wait()
	p.w.Close()
}

func (p *pager) Page() {
	if !isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd()) {
		return
	}

	lessCmd, lessParams := getPagerCommand()

	if lessCmd != "" {
		origStdout := os.Stdout // store original
		origStderr := os.Stderr // store original
		defer func() {          // on exit, last thing we do, we recover stdout/stderr
			os.Stdout = origStdout
			os.Stderr = origStderr
		}()
		less := exec.Command(lessCmd, lessParams...)
		less.Stdout = origStdout // less will write
		less.Stderr = origStderr // less will write
		r, w, err := os.Pipe()   // writer writes, reader reads
		if err == nil {
			os.Stdout = w      // we will write to os.Pipe
			os.Stderr = w      // we will write to os.Pipe
			less.Stdin = r     // less will read from os.Pipe
			err = less.Start() // start less so it can do it's magic
			if err != nil {    // on pagination failure, revert to non-paginated output
				os.Stdout = origStdout
				os.Stderr = origStderr
				log.Printf("Pagination failed, %s returned: %s", lessCmd, err)
			} else {
				p.cmd = less
				p.w = w
				p.isClosable = true
			}
		}
		// close pipes on less exit to actually exit if less is terminated prior to reaching EOF
		go func() {
			less.Wait()
			p.isClosable = false
			w.Close()
			r.Close()
		}()
	}
}

func getPagerCommand() (string, []string) {
	l, e := exec.LookPath("less")
	if e == nil && l != "" {
		return "less", []string{"-S", "-R"}
	}
	l, e = exec.LookPath("more")
	if e == nil && l != "" {
		return "more", []string{"-R"}
	}
	return "", nil
}
