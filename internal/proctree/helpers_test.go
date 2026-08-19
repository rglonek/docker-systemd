package proctree

import "time"

func timeoutChan() <-chan time.Time { return time.After(5 * time.Second) }
