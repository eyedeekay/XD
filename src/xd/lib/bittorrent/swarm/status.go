package swarm

import (
	"fmt"
	"xd/lib/metainfo"
	"xd/lib/util"
)

type TorrentFileInfo struct {
	FileInfo metainfo.FileInfo
	Progress float64
}

type TorrentPeers []*PeerConnStats

func (p TorrentPeers) RX() (rx float64) {
	for idx := range p {
		if p[idx] != nil {
			rx += p[idx].RX
		}
	}
	return
}

func (p TorrentPeers) TX() (tx float64) {
	for idx := range p {
		if p[idx] != nil {
			tx += p[idx].TX
		}
	}
	return
}

func (p TorrentPeers) Len() int {
	return len(p)
}

func (p TorrentPeers) Less(i, j int) bool {
	return p[i].Less(p[j])
}

func (p *TorrentPeers) Swap(i, j int) {
	(*p)[i], (*p)[j] = (*p)[j], (*p)[i]
}

// connection statistics
type PeerConnStats struct {
	TX   float64
	RX   float64
	ID   string
	Addr string
}

func (p *PeerConnStats) Less(o *PeerConnStats) bool {
	return util.StringCompare(p.ID, o.ID) < 0
}

type TorrentState string

const Seeding = TorrentState("seeding")
const Stopped = TorrentState("stopped")
const Downloading = TorrentState("downloading")

func (t TorrentState) String() string {
	return string(t)
}

// immutable status of torrent
type TorrentStatus struct {
	Files    []TorrentFileInfo
	Peers    TorrentPeers
	Name     string
	State    TorrentState
	Infohash string
	Progress float64
}

func (t TorrentStatus) Ratio() (r float64) {
	r = t.Peers.TX() / t.Peers.RX()
	return
}

type TorrentStatusList []TorrentStatus

func (l TorrentStatusList) TX() (tx float64) {
	for idx := range l {
		tx += l[idx].Peers.TX()
	}
	return
}

func (l TorrentStatusList) Ratio() (r float64) {
	var tx, rx float64
	for idx := range l {
		tx += l[idx].Peers.TX()
		rx += l[idx].Peers.RX()
	}
	r = tx / rx
	return
}

func (l TorrentStatusList) RX() (rx float64) {
	for idx := range l {
		rx += l[idx].Peers.RX()
	}
	return
}

func (l TorrentStatusList) Len() int {
	return len(l)
}
func (l TorrentStatusList) Less(i, j int) bool {
	return util.StringCompare(l[i].Name, l[j].Name) < 0
}

func (l *TorrentStatusList) Swap(i, j int) {
	(*l)[i], (*l)[j] = (*l)[j], (*l)[i]
}

// SwarmBandwidth is a string tuple for bandwith
type SwarmBandwidth struct {
	Upload   string
	Download string
}

func (sb SwarmBandwidth) String() string {
	return fmt.Sprintf("Upload: %s Download: %s", sb.Upload, sb.Download)
}

// infohash -> torrent status map
type SwarmStatus map[string]TorrentStatus
