package main

import (
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

const (
	// htlcQueueSize...
	// buffer bloat ;)
	htlcQueueSize = 50
)

// link represents a an active channel capable of forwarding HTLC's. Each
// active channel registered with the htlc switch creates a new link which will
// be used for forwarding outgoing HTLC's. The link also has additional
// meta-data such as the current available bandwidth of the link (in satoshis)
// which aide the switch in optimally forwarding HTLC's.
type link struct {
	capacity btcutil.Amount

	availableBandwidth btcutil.Amount

	linkChan chan *htlcPacket

	peer *peer

	chanPoint *wire.OutPoint
}

// htlcPacket is a wrapper around an lnwire message which adds, times out, or
// settles an active HTLC. The dest field denotes the name of the interface to
// forward this htlcPacket on.
type htlcPacket struct {
	src  wire.ShaHash
	dest wire.ShaHash

	msg lnwire.Message
	amt btcutil.Amount

	err chan error
}

// HtlcSwitch is a central messaging bus for all incoming/outgoing HTLC's.
// Connected peers with active channels are treated as named interfaces which
// refer to active channels as links. A link is the switche's message
// communication point with the goroutine that manages an active channel. New
// links are registered each time a channel is created, and unregistered once
// the channel is closed. The switch manages the hand-off process for multi-hop
// HTLC's, forwarding HTLC's initiated from within the daemon, and additionally
// splitting up incoming/outgoing HTLC's to a particular interface amongst many
// links (payment fragmentation).
type htlcSwitch struct {
	started  int32 // atomic
	shutdown int32 // atomic

	chanIndex  map[wire.OutPoint]*link
	interfaces map[wire.ShaHash][]*link

	// TODO(roasbeef): msgs for dynamic link quality
	linkControl chan interface{}

	outgoingPayments chan *htlcPacket

	htlcPlex chan *htlcPacket

	// TODO(roasbeef): messaging chan to/from upper layer (routing - L3)

	// TODO(roasbeef): sampler to log sat/sec and tx/sec

	wg   sync.WaitGroup
	quit chan struct{}
}

// newHtlcSwitch creates a new htlcSwitch.
func newHtlcSwitch() *htlcSwitch {
	return &htlcSwitch{
		chanIndex:        make(map[wire.OutPoint]*link),
		interfaces:       make(map[wire.ShaHash][]*link),
		linkControl:      make(chan interface{}),
		htlcPlex:         make(chan *htlcPacket, htlcQueueSize),
		outgoingPayments: make(chan *htlcPacket, htlcQueueSize),
	}
}

// Start starts all helper goroutines required for the operation of the switch.
func (h *htlcSwitch) Start() error {
	if !atomic.CompareAndSwapInt32(&h.started, 0, 1) {
		return nil
	}

	h.wg.Add(2)
	go h.networkAdmin()
	go h.htlcForwarder()

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
func (h *htlcSwitch) Stop() error {
	if !atomic.CompareAndSwapInt32(&h.shutdown, 0, 1) {
		return nil
	}

	close(h.quit)
	h.wg.Wait()

	return nil
}

// SendHTLC queues a HTLC packet for forwarding over the designated interface.
// In the event that the interface has insufficient capacity for the payment,
// an error is returned. Additionally, if the interface cannot be found, an
// alternative error is returned.
func (h *htlcSwitch) SendHTLC(htlcPkt *htlcPacket) error {
	htlcPkt.err = make(chan error, 1)

	h.outgoingPayments <- htlcPkt

	return <-htlcPkt.err
}

// htlcForwarder is responsible for optimally forwarding (and possibly
// fragmenting) incoming/outgoing HTLC's amongst all active interfaces and
// their links. The duties of the forwarder are similar to that of a network
// switch, in that it facilitates multi-hop payments by acting as a central
// messaging bus. Each active channel is modeled as networked device with
// meta-data such as the available payment bandwidth, and total link capacity.
func (h *htlcSwitch) htlcForwarder() {
	// TODO(roasbeef): track pending payments here instead of within each peer?
	// Examine settles/timeouts from htl cplex. Add src to htlcPacket, key by
	// (src, htlcKey).

	// TODO(roasbeef): cleared vs settled distinction
	var numUpdates uint64
	var satSent, satRecv btcutil.Amount
	logTicker := time.NewTicker(10 * time.Second)
out:
	for {
		select {
		case htlcPkt := <-h.outgoingPayments:
			dest := htlcPkt.dest
			chanInterface, ok := h.interfaces[dest]
			if !ok {
				err := fmt.Errorf("Unable to locate link %x", dest)
				hswcLog.Errorf(err.Error())
				htlcPkt.err <- err
				continue
			}

			wireMsg := htlcPkt.msg.(*lnwire.HTLCAddRequest)
			amt := btcutil.Amount(wireMsg.Amount)

			// Handle this send request in a distinct goroutine in
			// order to avoid a possible deadlock between the htlc
			// switch and channel's htlc manager.
			var sent bool
			for _, link := range chanInterface {
				// TODO(roasbeef): implement HTLC fragmentation
				//  * avoid full channel depletion at higher
				//    level (here) instead of within state
				//    machine?
				if link.availableBandwidth < amt {
					continue
				}

				hswcLog.Tracef("Sending %v to %x", amt, dest[:])

				// TODO(roasbeef): peer downstream should set chanPoint
				wireMsg.ChannelPoint = link.chanPoint
				go func() {
					link.linkChan <- htlcPkt
				}()

				// TODO(roasbeef): update link info on
				// timeout/settle
				link.availableBandwidth -= amt
				sent = true
			}

			if sent {
				continue
			}

			hswcLog.Errorf("Unable to send payment, insufficient capacity")
			htlcPkt.err <- fmt.Errorf("Insufficient capacity")
		case pkt := <-h.htlcPlex:
			numUpdates += 1
			// TODO(roasbeef): properly account with cleared vs settled
			switch pkt.msg.(type) {
			case *lnwire.HTLCAddRequest:
				satRecv += pkt.amt
			case *lnwire.HTLCSettleRequest:
				satSent += pkt.amt
			}

			// TODO(roasbeef): parse dest/src, forward on outgoing
			// link to complete multi-hop payments.
		case <-logTicker.C:
			if numUpdates == 0 {
				continue
			}

			hswcLog.Infof("Sent %v satoshis, received %v satoshi in "+
				"the last 10 seconds (%v tx/sec)",
				satSent.ToUnit(btcutil.AmountSatoshi),
				satRecv.ToUnit(btcutil.AmountSatoshi),
				float64(numUpdates)/10)
			satSent = 0
			satRecv = 0
			numUpdates = 0
		case <-h.quit:
			break out
		}
	}
	h.wg.Done()
}

// networkAdmin is responsible for handline requests to register, unregister,
// and close any link. In the event that a unregister requests leaves an
// interface with no active links, that interface is garbage collected.
func (h *htlcSwitch) networkAdmin() {
out:
	for {
		select {
		case msg := <-h.linkControl:
			switch req := msg.(type) {
			case *closeLinkReq:
				h.handleCloseLink(req)
			case *registerLinkMsg:
				h.handleRegisterLink(req)
			case *unregisterLinkMsg:
				h.handleUnregisterLink(req)
			}
		case <-h.quit:
			break out
		}
	}
	h.wg.Done()
}

// handleRegisterLink registers a new link within the channel index, and also
// adds the link to the existing set of links for the target interface.
func (h *htlcSwitch) handleRegisterLink(req *registerLinkMsg) {
	chanPoint := req.linkInfo.ChannelPoint
	newLink := &link{
		capacity:           req.linkInfo.Capacity,
		availableBandwidth: req.linkInfo.LocalBalance,
		linkChan:           req.linkChan,
		peer:               req.peer,
		chanPoint:          chanPoint,
	}
	h.chanIndex[*chanPoint] = newLink

	interfaceID := req.peer.lightningID
	h.interfaces[interfaceID] = append(h.interfaces[interfaceID], newLink)

	hswcLog.Infof("registering new link, interface=%v, chan_point=%v, capacity=%v",
		hex.EncodeToString(interfaceID[:]), chanPoint, newLink.capacity)

	if req.done != nil {
		req.done <- struct{}{}
	}
}

// handleUnregisterLink unregisters a currently active link. If the deletion of
// this link leaves the interface empty, then the interface entry itself is
// also deleted.
func (h *htlcSwitch) handleUnregisterLink(req *unregisterLinkMsg) {
	hswcLog.Infof("unregistering active link, interface=%v, chan_point=%v",
		hex.EncodeToString(req.chanInterface[:]), req.chanPoint)

	chanInterface := req.chanInterface
	links := h.interfaces[chanInterface]

	// A request with a nil channel point indicates that all the current
	// links for this channel should be cleared.
	if req.chanPoint == nil {
		hswcLog.Infof("purging all active links for interface %v",
			hex.EncodeToString(chanInterface[:]))

		for _, link := range links {
			delete(h.chanIndex, *link.chanPoint)
		}
		links = nil
	} else {
		delete(h.chanIndex, *req.chanPoint)

		for i := 0; i < len(links); i++ {
			chanLink := links[i]
			if chanLink.chanPoint == req.chanPoint {
				copy(links[i:], links[i+1:])
				links[len(links)-1] = nil
				links = links[:len(links)-1]

				break
			}
		}
	}

	if len(links) == 0 {
		hswcLog.Infof("interface %v has no active links, destroying",
			hex.EncodeToString(chanInterface[:]))
		delete(h.interfaces, chanInterface)
	}

	if req.done != nil {
		req.done <- struct{}{}
	}
}

// handleCloseLink sends a message to the peer responsible for the target
// channel point, instructing it to initiate a cooperative channel closure.
func (h *htlcSwitch) handleCloseLink(req *closeLinkReq) {
	targetLink, ok := h.chanIndex[*req.chanPoint]
	if !ok {
		req.resp <- nil
		req.err <- fmt.Errorf("channel point %v not found", req.chanPoint)
		return
	}

	hswcLog.Infof("requesting interface %v to close link %v",
		hex.EncodeToString(targetLink.peer.lightningID[:]), req.chanPoint)
	targetLink.peer.localCloseChanReqs <- req
}

// registerLinkMsg is message which requests a new link to be registered.
type registerLinkMsg struct {
	peer     *peer
	linkInfo *channeldb.ChannelSnapshot

	linkChan chan *htlcPacket

	done chan struct{}
}

// RegisterLink requests the htlcSwitch to register a new active link. The new
// link encapsulates an active channel. The htlc plex channel is returned. The
// plex channel allows the switch to properly de-multiplex incoming/outgoing
// HTLC messages forwarding them to their proper destination in the multi-hop
// settings.
func (h *htlcSwitch) RegisterLink(p *peer, linkInfo *channeldb.ChannelSnapshot,
	linkChan chan *htlcPacket) chan *htlcPacket {

	done := make(chan struct{}, 1)
	req := &registerLinkMsg{p, linkInfo, linkChan, done}
	h.linkControl <- req

	<-done

	return h.htlcPlex
}

// unregisterLinkMsg is a message which requests the active ink be unregistered.
type unregisterLinkMsg struct {
	chanInterface [32]byte
	chanPoint     *wire.OutPoint

	done chan struct{}
}

// UnregisterLink requets the htlcSwitch to unregiser the new active link. An
// unregistered link will no longer be considered a candidate to forward
// HTLC's.
func (h *htlcSwitch) UnregisterLink(chanInterface [32]byte, chanPoint *wire.OutPoint) {
	done := make(chan struct{}, 1)

	h.linkControl <- &unregisterLinkMsg{chanInterface, chanPoint, done}

	<-done
}

// closeChanReq represents a request to close a particular channel specified
// by its outpoint.
type closeLinkReq struct {
	chanPoint *wire.OutPoint

	resp chan *closeLinkResp
	err  chan error
}

// closeChanResp is the response to a closeChanReq is simply houses a boolean
// value indicating if the channel coopertive channel closure was succesful or not.
type closeLinkResp struct {
	txid    *wire.ShaHash
	success bool
}

// CloseLink closes an active link targetted by it's channel point. Closing the
// link initiates a cooperative channel closure.
// TODO(roabeef): bool flag for timeout/force
func (h *htlcSwitch) CloseLink(chanPoint *wire.OutPoint) (chan *closeLinkResp, chan error) {
	respChan := make(chan *closeLinkResp, 1)
	errChan := make(chan error, 1)

	h.linkControl <- &closeLinkReq{chanPoint, respChan, errChan}

	return respChan, errChan
}
