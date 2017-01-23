package swarm

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"
	"time"
	"xd/lib/bittorrent"
	"xd/lib/common"
	"xd/lib/log"
	"xd/lib/metainfo"
	"xd/lib/network"
	"xd/lib/storage"
	"xd/lib/tracker"
)

// how big should we download pieces at a time (bytes)?
const BlockSize = 1024 * 8

const Missing = 0
const Pending = 1
const Obtained = 2

// an event triggered when we get an inbound wire message from a peer we are connected with on this torrent asking for a piece
type pieceEvent struct {
	c *PeerConn
	r *bittorrent.PieceRequest
}

// cached downloading piece
type cachedPiece struct {
	piece    *common.Piece
	progress []byte
}

// get unfilled available block offset
func (p *cachedPiece) nextOffset() (idx int) {
	for idx < len(p.progress) {
		if p.progress[idx] == Missing {
			break
		}
		// mark progress as pending
		var i int
		for i < BlockSize {
			p.progress[idx] = Pending
			i++
			idx++
		}
		idx += BlockSize
	}
	return
}

// is this piece done downloading ?
func (p *cachedPiece) done() bool {
	for _, b := range p.progress {
		if b != Obtained {
			return false
		}
	}
	return true
}

// put a slice of data at offset
func (p *cachedPiece) put(offset int, data []byte) {
	if offset+len(data) <= len(p.progress) {
		// put data
		copy(p.piece.Data[offset:], data)
		// put progress
		for idx, _ := range data {
			p.progress[idx+offset] = Obtained
		}
	}
}

// cancel a slice
func (p *cachedPiece) cancel(offset, length int) {
	if offset+length <= len(p.progress) {
		for length > 0 {
			length--
			p.progress[offset+length] = Missing
		}
	}
}

// single torrent tracked in a swarm
type Torrent struct {
	// network context
	Net       network.Network
	Trackers  []tracker.Announcer
	announcer *time.Ticker
	// our peer id
	id    common.PeerID
	st    storage.Torrent
	bf    *bittorrent.Bitfield
	piece chan pieceEvent
	// pending incomplete pieces
	pending map[uint32]*cachedPiece
	pmtx    sync.RWMutex
}

// start annoucing on all trackers
func (t *Torrent) StartAnnouncing() {
	for _, tr := range t.Trackers {
		t.Announce(tr, "started")
	}
	if t.announcer == nil {
		t.announcer = time.NewTicker(time.Second)
	}
	go t.pollAnnounce()
}

// stop annoucing on all trackers
func (t *Torrent) StopAnnouncing() {
	if t.announcer != nil {
		t.announcer.Stop()
	}
	for _, tr := range t.Trackers {
		t.Announce(tr, "stopped")
	}
}

// poll announce ticker channel and issue announces
func (t *Torrent) pollAnnounce() {
	for {
		_, ok := <-t.announcer.C
		if !ok {
			// done
			return
		}
		for _, tr := range t.Trackers {
			if tr.ShouldAnnounce() {
				go t.Announce(tr, "")
			}
		}
	}
}

// do an announce
func (t *Torrent) Announce(tr tracker.Announcer, event string) {
	req := &tracker.Request{
		Infohash: t.st.Infohash(),
		PeerID:   t.id,
		IP:       t.Net.Addr(),
		Port:     6881,
		Event:    event,
		NumWant:  10, // TODO: don't hardcode
		Left:     t.st.DownloadRemaining(),
		Compact:  true,
	}
	resp, err := tr.Announce(req)
	if err == nil {
		for _, p := range resp.Peers {
			a, e := p.Resolve(t.Net)
			if e == nil {
				// no error resolving
				go t.AddPeer(a, p.ID)
			} else {
				log.Warnf("failed to resolve peer %s", e.Error())
			}
		}
	} else {
		log.Warnf("failed to announce to %s: %s", tr.Name(), err)
	}
}

// connect to a new peer for this swarm, blocks
func (t *Torrent) AddPeer(a net.Addr, id common.PeerID) {
	c, err := t.Net.Dial(a.Network(), a.String())
	if err == nil {
		// connected
		ih := t.st.Infohash()
		// build handshake
		h := new(bittorrent.Handshake)
		copy(h.Infohash[:], ih[:])
		copy(h.PeerID[:], t.id[:])
		// send handshake
		err = h.Send(c)
		if err == nil {
			// get response to handshake
			err = h.Recv(c)
			if err == nil {
				if bytes.Equal(ih[:], h.Infohash[:]) {
					// infohashes match
					pc := makePeerConn(c, t, h.PeerID)
					t.onNewPeer(pc)
					return
				}
			}
		}
		log.Warnf("didn't complete handshake with peer: %s", err)
		// bad thing happened
		c.Close()
		return
	}
	log.Infof("didn't connect to %s: %s", a, err)
}

// get metainfo for this torrent
func (t *Torrent) MetaInfo() *metainfo.TorrentFile {
	return t.st.MetaInfo()
}

// called when we got piece data
func (t *Torrent) gotPieceData(d *bittorrent.PieceData) {
	t.visitPendingPiece(d.Index, func(p *cachedPiece) {
		if p != nil {
			p.put(int(d.Begin), d.Data)
		}
	})
}

// callback called when we get a new inbound peer
func (t *Torrent) onNewPeer(c *PeerConn) {
	log.Infof("New peer (%s) for %s", c.id.String(), t.st.Infohash().Hex())
	// send our bitfields to them
	c.Send(t.bf.ToWireMessage())
	// send unchoke message
	c.Unchoke()
}

// handle a wire message
func (t *Torrent) onPieceRequest(c *PeerConn, req *bittorrent.PieceRequest) {
	t.piece <- pieceEvent{c, req}
}

func (t *Torrent) Run() {
	log.Infof("%s running", t.MetaInfo().TorrentName())
	for {
		ev, ok := <-t.piece
		if !ok {
			log.Warnf("%s torrent run exit", t.MetaInfo().TorrentName())
			// channel closed
			return
		}
		r := ev.r
		if r.Length > 0 {
			log.Debugf("%s asked for piece %d %d-%d", ev.c.id.String(), r.Index, r.Begin, r.Begin+r.Length)
			// TODO: cache common pieces
			p := t.st.GetPiece(r.Index)
			if p == nil {
				// we don't have the piece
				log.Infof("%s asked for a piece we don't have", ev.c.id.String())
			} else {
				// have the piece, send it
				d := make([]byte, 8+r.Length)
				binary.BigEndian.PutUint32(d, r.Index)
				binary.BigEndian.PutUint32(d[4:], r.Begin)
				copy(d[8:], p.Data[r.Begin:r.Begin+r.Length])
				msg := bittorrent.NewWireMessage(bittorrent.Piece, d)
				ev.c.Send(msg)
			}
		} else {
			log.Infof("%s asked for a zero length piece", ev.c.id.String())
		}

	}
}

// safely visit pending downloading piece , calls v(piece) if we have it or v(nil) if we don't
func (t *Torrent) visitPendingPiece(idx uint32, v func(*cachedPiece)) {
	t.pmtx.Lock()
	p, _ := t.pending[idx]
	v(p)
	t.pmtx.Unlock()
}

// implements client.Algorithm
func (t *Torrent) Next(id common.PeerID, remote, local *bittorrent.Bitfield) *bittorrent.PieceRequest {
	set := local.CountSet()
	for remote.Has(int(set)) {
		set++
	}
	if set >= int64(local.Length) {
		// no more for now
		return nil
	}

	sz := t.MetaInfo().Info.PieceLength
	req := &bittorrent.PieceRequest{}

	t.visitPendingPiece(uint32(set), func(p *cachedPiece) {
		if p == nil {
			// new cached piece
			p = new(cachedPiece)
			p.piece = &common.Piece{
				Index: set,
				Data:  make([]byte, sz),
			}
			p.progress = make([]byte, sz)
			// put the cached piece
			t.pending[uint32(set)] = p
		}
		req.Begin = uint32(p.nextOffset())
		req.Index = uint32(set)
		req.Length = BlockSize
	})

	return req
}

// implements client.Algorithm
func (t *Torrent) Done() bool {
	return t.bf.Completed()
}

// implements client.Algorithm
func (t *Torrent) Choke(id common.PeerID) bool {
	// TODO: implement choking
	return false
}
