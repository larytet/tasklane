package redismock

import "time"

func sleepMS(n int) { time.Sleep(time.Duration(n) * time.Millisecond) }
