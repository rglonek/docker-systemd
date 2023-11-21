package systemd

import _ "embed"

//go:generate touch fork.so fakefork.so

//go:embed fork.so
var forkfile []byte

//go:embed fakefork.so
var fakeforkfile []byte
