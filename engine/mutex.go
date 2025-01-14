package engine

import (
	"fmt"
	"time"
)

func lockRunWithTimeout(run func(chan error), duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	r := make(chan error)
	go run(r)

	select {
	case err := <-r:
		return err
	case <-timer.C:
		return fmt.Errorf("lockRunWithTimeout(%s) => timeout", duration)
	}
}
