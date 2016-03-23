package main

import (
	"encoding/hex"
	"fmt"

	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/lndc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/net/context"
)

var (
	defaultAccount uint32 = waddrmgr.DefaultAccountNum
)

// rpcServer...
type rpcServer struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	server *server

	wg sync.WaitGroup

	quit chan struct{}
}

var _ lnrpc.LightningServer = (*rpcServer)(nil)

// newRpcServer...
func newRpcServer(s *server) *rpcServer {
	return &rpcServer{server: s, quit: make(chan struct{}, 1)}
}

// Start...
func (r *rpcServer) Start() error {
	if atomic.AddInt32(&r.started, 1) != 1 {
		return nil
	}

	return nil
}

// Stop...
func (r *rpcServer) Stop() error {
	if atomic.AddInt32(&r.shutdown, 1) != 1 {
		return nil
	}

	return nil
}

// SendMany...
func (r *rpcServer) SendMany(ctx context.Context, in *lnrpc.SendManyRequest) (*lnrpc.SendManyResponse, error) {

	outputs := make([]*wire.TxOut, 0, len(in.AddrToAmount))
	for addr, amt := range in.AddrToAmount {
		addr, err := btcutil.DecodeAddress(addr, activeNetParams)
		if err != nil {
			return nil, err
		}

		pkscript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}

		outputs = append(outputs, wire.NewTxOut(amt, pkscript))
	}

	txid, err := r.server.lnwallet.SendOutputs(outputs, defaultAccount, 1)
	if err != nil {
		return nil, err
	}

	return &lnrpc.SendManyResponse{Txid: hex.EncodeToString(txid[:])}, nil
}

// NewAddress...
func (r *rpcServer) NewAddress(ctx context.Context, in *lnrpc.NewAddressRequest) (*lnrpc.NewAddressResponse, error) {

	r.server.lnwallet.KeyGenMtx.Lock()
	defer r.server.lnwallet.KeyGenMtx.Unlock()

	addr, err := r.server.lnwallet.NewAddress(defaultAccount)
	if err != nil {
		return nil, err
	}

	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

// LNConnect...
func (r *rpcServer) ConnectPeer(ctx context.Context,
	in *lnrpc.ConnectPeerRequest) (*lnrpc.ConnectPeerResponse, error) {

	if len(in.IdAtHost) == 0 {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}

	peerAddr, err := lndc.LnAddrFromString(in.IdAtHost)
	if err != nil {
		return nil, err
	}

	if err := r.server.ConnectToPeer(peerAddr); err != nil {
		return nil, err
	}

	return &lnrpc.ConnectPeerResponse{[]byte(peerAddr.String())}, nil
}
