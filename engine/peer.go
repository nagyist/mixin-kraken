package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/MixinNetwork/mixin/logger"
	"github.com/gofrs/uuid/v5"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	peerTrackClosedId          = "CLOSED"
	peerTrackConnectionTimeout = 20 * time.Second
	peerTrackReadTimeout       = 5 * time.Second
)

var clbkClient *http.Client

func init() {
	clbkClient = &http.Client{
		Timeout: 5 * time.Second,
	}
}

type Sender struct {
	id  string
	rtp *webrtc.RTPSender
}

type Peer struct {
	sync.RWMutex
	rid        string
	uid        string
	cid        string
	callback   string
	listenOnly bool
	pc         *webrtc.PeerConnection
	track      *webrtc.TrackLocalStaticRTP
	publishers map[string]*Sender
	queue      chan *rtp.Packet
	connected  chan bool
}

func BuildPeer(rid, uid string, pc *webrtc.PeerConnection, callback string, listenOnly bool) *Peer {
	cid, err := uuid.NewV4()
	if err != nil {
		panic(err)
	}
	peer := new(Peer)
	peer.rid = rid
	peer.uid = uid
	peer.cid = cid.String()
	peer.pc = pc
	peer.callback = callback
	peer.listenOnly = listenOnly
	peer.connected = make(chan bool, 1)
	peer.queue = make(chan *rtp.Packet, 8)
	peer.publishers = make(map[string]*Sender)
	peer.handle()
	return peer
}

func (p *Peer) id() string {
	return fmt.Sprintf("%s:%s:%s", p.rid, p.uid, p.cid)
}

func (p *Peer) CloseWithTimeout() error {
	logger.Printf("PeerClose(%s) now\n", p.id())
	p.Lock()
	defer p.Unlock()

	err := lockRunWithTimeout(func() error {
		return p.close()
	}, peerTrackReadTimeout)
	logger.Printf("PeerClose(%s) with %v\n", p.id(), err)
	return err
}

func (p *Peer) close() error {
	if p.cid == peerTrackClosedId {
		return nil
	}

	p.track = nil
	p.cid = peerTrackClosedId
	return p.pc.Close()
}

func (peer *Peer) handle() {
	go func() {
		timer := time.NewTimer(peerTrackConnectionTimeout)
		defer timer.Stop()

		select {
		case <-peer.connected:
		case <-timer.C:
			logger.Printf("HandlePeer(%s) OnTrackTimeout()\n", peer.id())
			_ = peer.CloseWithTimeout()
		}
	}()

	peer.pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		logger.Printf("HandlePeer(%s) OnSignalingStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Printf("HandlePeer(%s) OnConnectionStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Printf("HandlePeer(%s) OnICEConnectionStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnTrack(func(rt *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		logger.Printf("HandlePeer(%s) OnTrack(%s, %d, %d)\n", peer.id(), rt.ID(), rt.PayloadType(), rt.SSRC())
		added, err := peer.addTrackFromRemote(rt)
		if err != nil {
			panic(err)
		}
		if !added {
			return
		}
		peer.connected <- true

		err = peer.callbackOnTrack()
		if err != nil {
			logger.Printf("HandlePeer(%s) OnTrack(%d, %d) callback error %v\n", peer.id(), rt.PayloadType(), rt.SSRC(), err)
		} else {
			err = peer.copyTrack(rt)
			logger.Printf("HandlePeer(%s) OnTrack(%d, %d) end with %v\n", peer.id(), rt.PayloadType(), rt.SSRC(), err)
		}
		err = peer.CloseWithTimeout()
		logger.Printf("HandlePeer(%s) OnTrack(%d, %d) DONE %v\n", peer.id(), rt.PayloadType(), rt.SSRC(), err)
	})
}

func (peer *Peer) addTrackFromRemote(rt *webrtc.TrackRemote) (bool, error) {
	peer.Lock()
	defer peer.Unlock()

	if peer.cid == peerTrackClosedId {
		return false, nil
	}

	rpt := rt.PayloadType()
	if peer.track != nil || (rpt != 111 && rpt != 109) {
		return false, nil
	}
	lt, err := webrtc.NewTrackLocalStaticRTP(rt.Codec().RTPCodecCapability, peer.cid, peer.uid)
	if err != nil {
		return false, err
	}
	peer.track = lt
	return true, nil
}

func (peer *Peer) callbackOnTrack() error {
	if peer.callback == "" {
		return nil
	}

	body, _ := json.Marshal(map[string]string{
		"rid":    peer.rid,
		"uid":    peer.uid,
		"cid":    peer.cid,
		"action": "ontrack",
	})
	req, err := http.NewRequest("POST", peer.callback, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := clbkClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("status: %d", resp.StatusCode)
	}
	return nil
}

func (peer *Peer) copyTrack(src *webrtc.TrackRemote) error {
	go func() {
		defer close(peer.queue)

		for {
			pkt, _, err := src.ReadRTP()
			if err == io.EOF {
				logger.Verbosef("copyTrack(%s) EOF\n", peer.id())
				return
			}
			if err != nil {
				logger.Verbosef("copyTrack(%s) error %s\n", peer.id(), err.Error())
				return
			}
			peer.queue <- pkt
		}
	}()

	for {
		err := peer.consumeQueue()
		if err != nil {
			return err
		}
	}
}

func (peer *Peer) consumeQueue() error {
	timer := time.NewTimer(peerTrackReadTimeout)
	defer timer.Stop()

	select {
	case pkt, ok := <-peer.queue:
		if !ok {
			return fmt.Errorf("peer %s queue closed", peer.uid)
		}
		track := peer.track
		if track == nil {
			return fmt.Errorf("peer %s closed", peer.uid)
		}
		if peer.listenOnly {
			// FIXME make real silent opus packet
			pkt.Payload = make([]byte, len(pkt.Payload))
		}
		err := track.WriteRTP(pkt)
		if err != nil {
			return fmt.Errorf("peer %s track write %v", peer.uid, err)
		}
	case <-timer.C:
		return fmt.Errorf("peer %s track read timeout", peer.uid)
	}

	return nil
}
