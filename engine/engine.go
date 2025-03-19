package engine

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/MixinNetwork/mixin/logger"
)

const (
	engineStateLoopPeriod = 60 * time.Second
)

type State struct {
	BootedAt    time.Time `json:"booted_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ActivePeers int       `json:"active_peers"`
	ClosedPeers int       `json:"closed_peers"`
	PeakPeers   int       `json:"peak_peers"`
	ActiveRooms int       `json:"active_rooms"`
	ClosedRooms int       `json:"closed_rooms"`
	PeakRooms   int       `json:"peak_rooms"`
}

type Engine struct {
	IP        string
	Interface string
	PortMin   uint16
	PortMax   uint16

	peakPeers int
	peakRooms int
	state     *State
	rooms     *rmap
}

func BuildEngine(conf *Configuration) (*Engine, error) {
	ip, err := getIPFromInterface(conf.Engine.Interface, conf.Engine.Address)
	if err != nil {
		return nil, err
	}
	engine := &Engine{
		IP:        ip,
		Interface: conf.Engine.Interface,
		PortMin:   conf.Engine.PortMin,
		PortMax:   conf.Engine.PortMax,
		rooms:     rmapAllocate(),
	}
	logger.Printf("BuildEngine(IP: %s, Interface: %s, Ports: %d-%d)\n", engine.IP, engine.Interface, engine.PortMin, engine.PortMax)
	return engine, nil
}

func (engine *Engine) Loop() {
	bootedAt := time.Now()

	for {
		engine.rooms.RLock()
		rooms := make(map[string]*pmap, len(engine.rooms.m))
		for k, v := range engine.rooms.m {
			rooms[k] = v
		}
		engine.rooms.RUnlock()

		state := &State{
			BootedAt:  bootedAt,
			UpdatedAt: time.Now(),
		}
		for _, pm := range rooms {
			peers := pm.PeersCopy()
			ap, cp := 0, 0
			for _, p := range peers {
				if p.cid == peerTrackClosedId {
					cp += 1
				} else {
					ap += 1
				}
			}
			state.ActivePeers += ap
			state.ClosedPeers += cp
			if ap > 0 {
				state.ActiveRooms += 1
				logger.Printf("room#%s with %d active and %d closed peers", pm.id, ap, cp)
			} else {
				state.ClosedRooms += 1
			}
		}
		if state.ActiveRooms > engine.peakRooms {
			engine.peakRooms = state.ActiveRooms
		}
		if state.ActivePeers > engine.peakPeers {
			engine.peakPeers = state.ActivePeers
		}
		state.PeakPeers = engine.peakPeers
		state.PeakRooms = engine.peakRooms
		engine.state = state

		time.Sleep(engineStateLoopPeriod)
	}
}

func getIPFromInterface(iname string, addr string) (string, error) {
	if addr != "" {
		return addr, nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, i := range ifaces {
		if i.Name != iname {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				return v.IP.String(), nil
			case *net.IPAddr:
				return v.IP.String(), nil
			}
		}
	}

	return "", fmt.Errorf("no address for interface %s", iname)
}

type pmap struct {
	sync.RWMutex
	id string
	m  map[string]*Peer
}

func pmapAllocate(id string) *pmap {
	pm := new(pmap)
	pm.id = id
	pm.m = make(map[string]*Peer)
	return pm
}

type rmap struct {
	sync.RWMutex
	m map[string]*pmap
}

func rmapAllocate() *rmap {
	rm := new(rmap)
	rm.m = make(map[string]*pmap)
	return rm
}

func (engine *Engine) getRoom(rid string) *pmap {
	rm := engine.rooms
	rm.RLock()
	defer rm.RUnlock()

	return rm.m[rid]
}

func (engine *Engine) GetRoom(rid string) *pmap {
	pm := engine.getRoom(rid)
	if pm != nil {
		return pm
	}

	rm := engine.rooms
	rm.Lock()
	defer rm.Unlock()
	if rm.m[rid] == nil {
		rm.m[rid] = pmapAllocate(rid)
	}
	return rm.m[rid]
}

func (room *pmap) PeersCopy() map[string]*Peer {
	room.RLock()
	defer room.RUnlock()

	peers := make(map[string]*Peer, len(room.m))
	for k, v := range room.m {
		peers[k] = v
	}
	return peers
}

func (room *pmap) GetPeer(uid, cid string) (*Peer, error) {
	room.RLock()
	defer room.RUnlock()

	return room.getPeer(uid, cid)
}

func (room *pmap) getPeer(uid, cid string) (*Peer, error) {
	peer := room.m[uid]
	if peer == nil {
		return nil, buildError(ErrorPeerNotFound, fmt.Errorf("peer %s not found in %s", uid, room.id))
	}
	if peer.cid == peerTrackClosedId {
		return nil, buildError(ErrorPeerClosed, fmt.Errorf("peer %s closed in %s", uid, room.id))
	}
	if peer.cid != cid {
		return nil, buildError(ErrorTrackNotFound, fmt.Errorf("peer %s track not match %s %s in %s", uid, cid, peer.cid, room.id))
	}
	return peer, nil
}
