package systemd

import (
	"docker-systemd/common"
	"docker-systemd/procwait"
	"log"
	"net"
	"os"
	"sync"
)

var shutdownLock = new(sync.Mutex)

func shutdown(socket net.Listener) {
	shutdownLock.Lock()
	log.Println("SHUTDOWN: Signal received")
	socket.Close()
	os.Remove(common.SocketPath())
	log.Println("SHUTDOWN: Stopping services")
	err := d.StopAll()
	if err != nil {
		log.Printf("SHUTDOWN: Error, unclean exit: %s", err)
		procwait.FinalReap()
		os.Exit(1)
	}
	log.Println("SHUTDOWN: Complete")
	procwait.FinalReap()
	os.Exit(0)
}
