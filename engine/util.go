package engine

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
)

func lockRunWithTimeout(run func() error, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	r := make(chan error)
	go func() { r <- run() }()

	select {
	case err := <-r:
		return err
	case <-timer.C:
		return buildError(ErrorServerTimeout, fmt.Errorf("timeout after %s", duration))
	}
}

func setLocalDescription(pc *webrtc.PeerConnection, desc webrtc.SessionDescription) error {
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	err := pc.SetLocalDescription(desc)
	if err != nil {
		return err
	}
	<-gatherComplete
	return nil
}
