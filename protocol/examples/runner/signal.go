package runner

import (
	"os"
	"os/signal"
	"syscall"
)

func notify(c chan os.Signal) { signal.Notify(c, syscall.SIGINT, syscall.SIGTERM) }
