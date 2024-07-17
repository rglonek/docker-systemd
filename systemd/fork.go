package systemd

import _ "embed"

//go:generate touch fork_amd64.so fakefork_amd64.so fork_arm64.so fakefork_arm64.so

//go:embed fork_amd64.so
var amdforkfile []byte

//go:embed fakefork_amd64.so
var amdfakeforkfile []byte

//go:embed fork_arm64.so
var armforkfile []byte

//go:embed fakefork_arm64.so
var armfakeforkfile []byte
