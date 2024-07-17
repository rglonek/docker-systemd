package systemd

import (
	"docker-systemd/common"
	"docker-systemd/procwait"
	"docker-systemd/systemd/daemons"
	"docker-systemd/systemd/pidtracker"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

/* receive buffer is (os.Args[1:] values essentially):
16-bit int: size of array
for each slice:
	16-bit int: size of string
	string
*/

/* send buffer is:
string: to print out, followed by 0x00 byte
- wait for 0x00 response
7 bytes - magic 5-byte and 16-bit int: return value
*/

func pidtrack() error {
	contents, _ := os.ReadFile("/etc/ld.so.preload")
	preloadc := string(contents)
	if !strings.Contains(preloadc, "/usr/local/lib/fork.so") {
		if preloadc == "" {
			preloadc = "/usr/local/lib/fork.so\n"
		} else {
			preloadc += "\n/usr/local/lib/fork.so\n"
		}
		os.WriteFile("/etc/ld.so.preload", []byte(preloadc), 0644)
	}
	sockPath := "/tmp/docker-systemd-pidtrack.sock"
	os.Remove(sockPath)
	socket, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := socket.Accept()
			if err != nil && errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				log.Printf("PID-TRACK: ERROR: %s", err)
				time.Sleep(time.Second)
				continue
			}
			func() {
				defer conn.Close()
				buf := make([]byte, 65536)
				n, err := conn.Read(buf)
				if err != nil {
					log.Print(err)
					return
				}
				kids := strings.Split(string(buf[0:n]), ":")
				parent := -1
				child := -1
				infant := -1
				switch len(kids) {
				case 3:
					infant, err = strconv.Atoi(kids[2])
					if err != nil {
						log.Printf("PID:TRACK: ERROR: Message malformed: %s", err)
						return
					}
					fallthrough
				case 2:
					child, err = strconv.Atoi(kids[1])
					if err != nil {
						log.Printf("PID:TRACK: ERROR: Message malformed: %s", err)
						return
					}
					parent, err = strconv.Atoi(kids[0])
					if err != nil {
						log.Printf("PID:TRACK: ERROR: Message malformed: %s", err)
						return
					}
				default:
					log.Print("PID-TRACK: ERROR: Message malformed")
					return
				}
				pidtracker.Add(parent, child, infant)
			}()
		}
	}()
	return nil
}

func Main() {
	procwait.Init()
	defer procwait.FinalReap()
	ldPreload := true
	for _, item := range os.Args[1:] {
		if item == "--log-to-stderr" {
			daemons.LogToStderr = true
		} else if item == "--no-logfile" {
			daemons.LogToFile = false
		} else if item == "--no-pidtrack" {
			ldPreload = false
		} else if item == "--debug-reaper" {
			procwait.Debug = true
		} else {
			log.Fatalf("Invalid parameter: %s", item)
		}
	}
	os.MkdirAll(common.GetLogPath(), 0755)
	forkfile := amdforkfile
	fakeforkfile := amdfakeforkfile
	if runtime.GOARCH == "arm64" {
		forkfile = armforkfile
		fakeforkfile = armfakeforkfile
	}
	if ldPreload {
		log.Println("INIT: Creating pidtrack socket")
		os.WriteFile("/usr/local/lib/fork.so", forkfile, 0755)
		err := pidtrack()
		if err != nil {
			log.Printf("INIT: ERROR: Could not create pidtrack socket, will not fully track forking processes: %s", err)
		}
	} else {
		os.WriteFile("/usr/local/lib/fork.so", fakeforkfile, 0755)
	}
	log.Println("INIT: Creating control socket")
	os.Remove(common.SocketPath())
	socket, err := net.Listen("unix", common.SocketPath())
	if err != nil {
		log.Fatal(err)
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-c
		shutdown(socket)
	}()
	log.Println("INIT: Starting services")
	go func() {
		err = startup()
		if err != nil {
			log.Print(err)
			shutdown(socket)
		}
		log.Println("INIT: Complete")
	}()
	for {
		conn, err := socket.Accept()
		if err != nil && errors.Is(err, net.ErrClosed) {
			shutdown(socket)
		}
		log.Print("NEWCONN")
		if err != nil {
			log.Print(err)
			time.Sleep(time.Second)
			continue
		}
		go func(conn net.Conn) {
			defer conn.Close()
			buf := make([]byte, 65536)
			n, err := conn.Read(buf)
			if err != nil {
				log.Print(err)
				return
			}
			if n < 2 {
				log.Print("received data too small")
				return
			}
			params := int(binary.LittleEndian.Uint16(buf[0:2]))
			args := []string{}
			bufloc := 2
			for i := 0; i < params; i++ {
				if n < bufloc+2 {
					log.Print("message malformed")
					return
				}
				argn := int(binary.LittleEndian.Uint16(buf[bufloc : bufloc+2]))
				bufloc += 2
				if n < bufloc+argn {
					log.Print("message malformed - string short")
					return
				}
				args = append(args, string(buf[bufloc:bufloc+argn]))
				bufloc += argn
			}
			retCode := command(args, &NetConn{Conn: conn})
			_, err = conn.Write([]byte{0x00})
			if err != nil {
				log.Printf("error ending response message: %s", err)
			}
			_, err = conn.Read(buf)
			if err != nil {
				log.Printf("client did not respond with readiness to receive response code: %s", err)
			}
			_, err = conn.Write(binary.LittleEndian.AppendUint16(common.SystemctlExitCodeMagic, uint16(retCode)))
			if err != nil {
				log.Printf("response code send error: %s", err)
			}
		}(conn)
	}
}
