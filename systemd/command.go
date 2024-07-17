package systemd

import (
	"bytes"
	"docker-systemd/systemd/daemons"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"strings"
	"syscall"

	"github.com/jessevdk/go-flags"
)

type cmd struct {
	Poweroff       cmdPoweroff       `command:"poweroff" description:"shutdown the system"`
	Enable         cmdEnable         `command:"enable" description:"enable services" subcommands-optional:"true"`
	Disable        cmdDisable        `command:"disable" description:"disable services"`
	DaemonReload   cmdDaemonReload   `command:"daemon-reload" description:"reload unit files"`
	Start          cmdStart          `command:"start" description:"start a service"`
	Stop           cmdStop           `command:"stop" description:"stop a service"`
	Restart        cmdRestart        `command:"restart" description:"restart a service"`
	Reload         cmdReload         `command:"reload" description:"reload a service (send SIGHUP)"`
	Status         cmdStatus         `command:"status" description:"status of a service"`
	Mask           cmdMask           `command:"mask" description:"mask a service"`
	Unmask         cmdUnmask         `command:"unmask" description:"unmask a service"`
	Show           cmdShow           `command:"show" description:"show details of a service"`
	CreateInstance cmdCreateInstance `command:"create-instance" description:"create a new instance (for multi-instance services)"`
	DeleteInstance cmdDeleteInstance `command:"delete-instance" description:"delete an instance (for multi-instance services)"`
	List           cmdList           `command:"list" description:"list services"`
}

type cmdPoweroff struct{}
type cmdEnable struct {
	Now  bool `long:"now" description:"Start services after enabling"`
	Conn *NetConn
}
type cmdDisable struct {
	Conn *NetConn
}
type cmdDaemonReload struct{}
type cmdStart struct {
	Conn *NetConn
}
type cmdStop struct {
	Conn *NetConn
}
type cmdRestart struct {
	Conn *NetConn
}
type cmdReload struct {
	Conn *NetConn
}
type cmdStatus struct{}
type cmdMask struct {
	Conn *NetConn
}
type cmdUnmask struct {
	Conn *NetConn
}
type cmdShow struct{}
type cmdList struct{}
type cmdCreateInstance struct{}
type cmdDeleteInstance struct{}

type cmdResponse struct {
	message string
	isError bool
}

func (c cmdResponse) Error() string {
	if c.isError {
		return c.message
	}
	return ""
}

func MakeResponse(msg string, isError bool) cmdResponse {
	return cmdResponse{
		message: msg,
		isError: isError,
	}
}

func findDaemons(names []string) ([]daemons.Daemon, error) {
	if len(names) == 0 {
		return nil, errors.New("service name not provided; usage: systemctl command servicename")
	}
	ds := []daemons.Daemon{}
	if d == nil {
		return ds, nil
	}
	for _, service := range names {
		service = strings.TrimSuffix(service, ".service")
		daemon, err := d.Find(service)
		if err != nil {
			return ds, fmt.Errorf("%s: %s", service, err)
		}
		ds = append(ds, daemon)
	}
	return ds, nil
}

func (c *cmdCreateInstance) Execute(args []string) error {
	for _, arg := range args {
		sp := strings.Split(arg, "@")
		ds, err := d.Find(sp[0] + "@")
		if err != nil {
			return MakeResponse(err.Error(), true)
		}
		err = ds.CreateInstance(sp[1])
		if err != nil {
			return MakeResponse(ds.Name()+": "+err.Error(), true)
		}
	}
	err := d.Reload()
	if err != nil {
		return MakeResponse("Reload(): "+err.Error(), true)
	}
	return MakeResponse("Services Created", false)
}

func (c *cmdDeleteInstance) Execute(args []string) error {
	for _, arg := range args {
		ds, err := d.Find(arg)
		if err != nil {
			return MakeResponse(err.Error(), true)
		}
		err = ds.DeleteService()
		if err != nil {
			return MakeResponse(ds.Name()+": "+err.Error(), true)
		}
	}
	err := d.Reload()
	if err != nil {
		return MakeResponse("Reload(): "+err.Error(), true)
	}
	return MakeResponse("Services Deleted", false)
}

func (c *cmdEnable) Execute(args []string) error {
	needReload := false
	for _, arg := range args {
		if !strings.Contains(arg, "@") {
			continue
		}
		if _, err := findDaemons([]string{arg}); err != nil {
			needReload = true
			sp := strings.Split(arg, "@")
			ds, err := d.Find(sp[0] + "@")
			if err != nil {
				return MakeResponse(err.Error(), true)
			}
			err = ds.CreateInstance(sp[1])
			if err != nil {
				return MakeResponse(ds.Name()+": "+err.Error(), true)
			}
		}
	}
	if needReload {
		err := d.Reload()
		if err != nil {
			return MakeResponse("Reload(): "+err.Error(), true)
		}
	}
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Enabling %s ... ", daemon.Name())
		err := daemon.Enable()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	if c.Now {
		for _, daemon := range ds {
			c.Conn.Printf("Starting %s ... ", daemon.Name())
			err := daemon.Start()
			if err != nil {
				c.Conn.Println("FAIL")
				return MakeResponse(daemon.Name()+": "+err.Error(), true)
			} else {
				c.Conn.Println("OK")
			}
		}
		return nil
	}
	return nil
}

func (c *cmdDisable) Execute(args []string) error {
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Disabling %s ... ", daemon.Name())
		err := daemon.Disable()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	return nil
}

func (c *cmdDaemonReload) Execute(args []string) error {
	return d.Reload()
}

func (c *cmdStart) Execute(args []string) error {
	needReload := false
	for _, arg := range args {
		if !strings.Contains(arg, "@") {
			continue
		}
		if _, err := findDaemons([]string{arg}); err != nil {
			needReload = true
			sp := strings.Split(arg, "@")
			ds, err := d.Find(sp[0] + "@")
			if err != nil {
				return MakeResponse(err.Error(), true)
			}
			err = ds.CreateInstance(sp[1])
			if err != nil {
				return MakeResponse(ds.Name()+": "+err.Error(), true)
			}
		}
	}
	if needReload {
		err := d.Reload()
		if err != nil {
			return MakeResponse("Reload(): "+err.Error(), true)
		}
	}
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Starting %s ... ", daemon.Name())
		err := daemon.Start()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	return nil
}

func (c *cmdStop) Execute(args []string) error {
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Stopping %s ... ", daemon.Name())
		err := daemon.Stop()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	return nil
}

func (c *cmdRestart) Execute(args []string) error {
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Restarting %s ... ", daemon.Name())
		err := daemon.Restart()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	return nil
}

func (c *cmdReload) Execute(args []string) error {
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Reloading %s ... ", daemon.Name())
		err := daemon.Reload()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	return nil
}

func (c *cmdMask) Execute(args []string) error {
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Masking %s ... ", daemon.Name())
		err := daemon.Mask()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	return nil
}

func (c *cmdUnmask) Execute(args []string) error {
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	for _, daemon := range ds {
		c.Conn.Printf("Unmasking %s ... ", daemon.Name())
		err := daemon.Unmask()
		if err != nil {
			c.Conn.Println("FAIL")
			return MakeResponse(daemon.Name()+": "+err.Error(), true)
		} else {
			c.Conn.Println("OK")
		}
	}
	return nil
}

func (c *cmdStatus) Execute(args []string) error {
	if len(args) == 0 {
		args = d.List()
	}
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	retMsg := ""
	for _, daemon := range ds {
		ret, err := daemon.Status()
		if retMsg == "" {
			retMsg = ret + ": " + daemon.Name()
		} else {
			retMsg += "\n" + ret + ": " + daemon.Name()
		}
		if err != nil {
			retMsg += " ERROR: " + err.Error()
		}
	}
	return MakeResponse(retMsg, false)
}

func (c *cmdPoweroff) Execute(args []string) error {
	defer syscall.Kill(os.Getpid(), syscall.SIGTERM)
	var response cmdResponse
	response.message = "Shutting down system..."
	return response
}

func (c *cmdShow) Execute(args []string) error {
	ds, err := findDaemons(args)
	if err != nil {
		return MakeResponse(err.Error(), true)
	}
	details := ""
	for _, daemon := range ds {
		details = details + fmt.Sprintf("=== %s ===\n", daemon.Name())
		out := daemon.Detail()
		details = details + out
	}
	return MakeResponse(details, false)
}

func (c *cmdList) Execute(args []string) error {
	ds := d.List()
	for i := range ds {
		x, err := d.Find(ds[i])
		if err == nil && x.IsEnabled() {
			ds[i] += " (enabled)"
		}
	}
	return MakeResponse(strings.Join(ds, "\n"), false)
}

func command(args []string, conn *NetConn) (retCode int) {
	log.Printf("COMMAND: Received command %v", args)
	c := NewCmd(conn)
	p := flags.NewParser(c, flags.HelpFlag|flags.PassDoubleDash)
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		var helpMsg bytes.Buffer
		p.WriteHelp(&helpMsg)
		conn.Println(helpMsg.String())
		return 0
	}
	_, err := p.ParseArgs(args)
	if err != nil {
		switch msg := err.(type) {
		case cmdResponse:
			if msg.isError {
				log.Printf("COMMAND: %v Error %s", args, msg.Error())
				conn.Println(msg.Error())
				return 1
			}
			log.Printf("COMMAND: %v Success", args)
			conn.Println(msg.message)
			return 0
		case *flags.Error:
			if msg.Type == flags.ErrUnknownCommand {
				log.Printf("COMMAND: %v soft-error %s", args, msg.Error())
				conn.Println(msg.Error())
				return 0
			}
		default:
			if strings.HasPrefix(err.Error(), "Unknown command") {
				log.Printf("COMMAND: %v soft-error %s", args, msg.Error())
				conn.Println(msg.Error())
				return 0
			}
			log.Printf("COMMAND: %v ERROR %s", args, msg.Error())
			conn.Println(msg.Error())
			return 1
		}
	}
	log.Printf("COMMAND: %v success", args)
	return 0
}

type NetConn struct {
	Conn net.Conn
}

func (c *NetConn) Print(s string) {
	c.Conn.Write([]byte(s))
}

func (c *NetConn) Println(s string) {
	c.Conn.Write(append([]byte(s), '\n'))
}

func (c *NetConn) Printf(s string, params ...any) {
	c.Conn.Write([]byte(fmt.Sprintf(s, params...)))
}

func (c *NetConn) Printfln(s string, params ...any) {
	c.Conn.Write([]byte(fmt.Sprintf(s+"\n", params...)))
}

func NewCmd(connection *NetConn) *cmd {
	c := new(cmd)
	value := reflect.Indirect(reflect.ValueOf(c))
	for i := 0; i < value.NumField(); i++ {
		if value.Type().Field(i).Type.Kind() == reflect.Struct {
			_, ok := value.Type().Field(i).Type.FieldByName("Conn")
			if ok {
				conn := value.Field(i).FieldByName("Conn")
				conn.Set(reflect.ValueOf(connection))
			}
		}
	}
	return c
}
