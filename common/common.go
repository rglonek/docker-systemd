package common

import (
	"os"
	"strings"
	"time"
)

func GetSystemdPaths() []string {
	if nstat, err := os.Lstat("/lib/systemd/system"); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
		return []string{
			"/etc/systemd/system",
			"/usr/lib/systemd/system",
		}
	}
	if nstat, err := os.Lstat("/usr/lib/systemd/system"); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
		return []string{
			"/etc/systemd/system",
			"/lib/systemd/system",
		}
	}
	if nstat, err := os.Lstat("/lib/systemd"); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
		return []string{
			"/etc/systemd/system",
			"/usr/lib/systemd/system",
		}
	}
	if nstat, err := os.Lstat("/usr/lib/systemd"); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
		return []string{
			"/etc/systemd/system",
			"/lib/systemd/system",
		}
	}
	if nstat, err := os.Lstat("/lib"); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
		return []string{
			"/etc/systemd/system",
			"/usr/lib/systemd/system",
		}
	}
	if nstat, err := os.Lstat("/usr/lib"); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
		return []string{
			"/etc/systemd/system",
			"/lib/systemd/system",
		}
	}
	return []string{
		"/etc/systemd/system",
		"/usr/lib/systemd/system",
		"/lib/systemd/system",
	}
}

func GetLogPath() string {
	return "/var/log/services"
}

func GetUnitLogPath(unit string) string {
	return strings.Trim(strings.TrimSuffix(unit, ".service"), ".") + ".log"
}

func GetBootFile() string {
	return "/etc/boot-time"
}

func LogTimeFormat() string {
	return time.DateTime
}

func SocketPath() string {
	return "/tmp/docker-systemd.sock"
}

var SystemctlExitCodeMagic = []byte{00, 0xFF, 0x55, 0xAA, 00}

// enable: Created symlink /etc/systemd/system/multi-user.target.wants/aerospike.service → /lib/systemd/system/aerospike.service
// disable: Removed /etc/systemd/system/multi-user.target.wants/aerospike.service
// mask: Created symlink /etc/systemd/system/aerospike.service → /dev/null
// unmask: Removed /etc/systemd/system/aerospike.service
// Failed to mask unit: File /etc/systemd/system/snap.lxd.activate.service already exists
/*
	ls -l /etc/systemd/system/multi-user.target.wants/
	lrwxrwxrwx 1 root root 34 Sep 19 02:25  chrony.service -> /lib/systemd/system/chrony.service
	lrwxrwxrwx 1 root root 41 Sep 19 02:19  console-setup.service -> /lib/systemd/system/console-setup.service
	lrwxrwxrwx 1 root root 32 Sep 19 02:19  cron.service -> /lib/systemd/system/cron.service
*/

/*
	[Unit]
	Description=Aerospike Server
	After=network-online.target
	Wants=network.target

	[Service]
	LimitNOFILE=100000
	TimeoutSec=600
	User=root
	Group=root
	EnvironmentFile=/etc/sysconfig/aerospike
	PermissionsStartOnly=True
	ExecStartPre=/usr/bin/asd-systemd-helper
	ExecStart=/usr/bin/asd $ASD_OPTIONS --config-file $ASD_CONFIG_FILE --fgdaemon

	[Install]
	WantedBy=multi-user.target
	------------
	# cat /etc/sysconfig/aerospike
	ASD_CONFIG_FILE=/etc/aerospike/aerospike.conf

	# Uncomment to start with cold start
	#ASD_COLDSTART="--cold-start"
	-------------
	root@mydc-1:/etc/systemd/system/aerospike.service.d# cat aerolab-thp.conf
	[Service]
	ExecStartPre=/bin/bash -c "echo 'never' > /sys/kernel/mm/transparent_hugepage/enabled || echo"
	ExecStartPre=/bin/bash -c "echo 'never' > /sys/kernel/mm/transparent_hugepage/defrag || echo"
	ExecStartPre=/bin/bash -c "echo 'never' > /sys/kernel/mm/redhat_transparent_hugepage/enabled || echo"
	ExecStartPre=/bin/bash -c "echo 'never' > /sys/kernel/mm/redhat_transparent_hugepage/defrag || echo"
	ExecStartPre=/bin/bash -c "echo 0 > /sys/kernel/mm/transparent_hugepage/khugepaged/defrag || echo"
	ExecStartPre=/bin/bash -c "echo 0 > /sys/kernel/mm/redhat_transparent_hugepage/khugepaged/defrag || echo"
	ExecStartPre=/bin/bash -c "sysctl -w vm.min_free_kbytes=1310720 || echo"
	ExecStartPre=/bin/bash -c "sysctl -w vm.swappiness=0 || echo"
	root@mydc-1:/etc/systemd/system/aerospike.service.d#
	root@mydc-1:/etc/systemd/system/aerospike.service.d#
	root@mydc-1:/etc/systemd/system/aerospike.service.d# cat aerolab-early-late.conf
	[Service]
	ExecStartPre=/bin/bash /usr/local/bin/early.sh
	ExecStopPost=/bin/bash /usr/local/bin/late.sh
	root@mydc-1:/etc/systemd/system/aerospike.service.d# cat aerospike.conf.default
	#
	root@mydc-1:/etc/systemd/system/aerospike.service.d# cat aerospike.conf.coldstart
	[Service]
	Environment="ASD_OPTIONS=--cold-start"
*/
