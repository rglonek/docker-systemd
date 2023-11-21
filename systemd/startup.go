package systemd

import "docker-systemd/systemd/daemons"

var d daemons.Daemons

func startup() error {
	var err error
	d, err = daemons.New()
	return err
}
