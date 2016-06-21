package chainntnfs

import "github.com/roasbeef/btcd/wire"

// ChainNotifier represents a trusted source to receive notifications concerning
// targeted events on the Bitcoin blockchain. The interface specification is
// intentionally general in order to support a wide array of chain notification
// implementations such as: btcd's websockets notifications, Bitcoin Core's
// ZeroMQ notifications, various Bitcoin API services, Electrum servers, etc.
//
// Concrete implementations of ChainNotifier should be able to support multiple
// concurrent client requests, as well as multiple concurrent notification events.
type ChainNotifier interface {
	// RegisterConfirmationsNtfn registers an intent to be notified once
	// txid reaches numConfs confirmations. The returned ConfirmationEvent
	// should properly notify the client once the specified number of
	// confirmations has been reached for the txid, as well as if the
	// original tx gets re-org'd out of the mainchain.
	RegisterConfirmationsNtfn(txid *wire.ShaHash, numConfs uint32) (*ConfirmationEvent, error)

	// RegisterSpendNtfn registers an intent to be notified once the target
	// outpoint is succesfully spent within a confirmed transaction. The
	// returned SpendEvent will receive a send on the 'Spend' transaction
	// once a transaction spending the input is detected on the blockchain.
	//
	// NOTE: This notifications should be triggered once the transaction is
	// *seen* on the network, not when it has received a single confirmation.
	RegisterSpendNtfn(outpoint *wire.OutPoint) (*SpendEvent, error)

	// RegisterBlockEpochNtfn registers an intent to be notified of each
	// new block connected to the tip of the main chain. The returned
	// BlockEpochEvent struct contains a channel which will be sent upon
	// for each new block discovered.
	RegisterBlockEpochNtfn(targetHeight int32) (*BlockEpochEvent, error)

	// Start the ChainNotifier. Once started, the implementation should be
	// ready, and able to receive notification registrations from clients.
	Start() error

	// Stops the concrete ChainNotifier. Once stopped, the ChainNotifier
	// should disallow any future requests from potential clients.
	// Additionally, all pending client notifications will be cancelled
	// by closing the related channels on the *Event's.
	Stop() error
}

// TODO(roasbeef): ln channels should request spend ntfns for counterparty's
// inputs to funding tx also, consider channel closed if funding tx re-org'd
// out and inputs double spent.

// TODO(roasbeef): all chans should be receive only.

// ConfirmationEvent encapsulates a confirmation notification. With this struct,
// callers can be notified of: the instance the target txid reaches the targeted
// number of confirmations, and also in the event that the original txid becomes
// disconnected from the blockchain as a result of a re-org.
//
// Once the txid reaches the specified number of confirmations, the 'Confirmed'
// channel will be sent upon fufulling the notification.
//
// If the event that the original transaction becomes re-org'd out of the main
// chain, the 'NegativeConf' will be sent upon with a value representing the
// depth of the re-org.
type ConfirmationEvent struct {
	Confirmed chan int32 // MUST be buffered.
	// TODO(roasbeef): all goroutines on ln channel updates should also
	// have a struct chan that's closed if funding gets re-org out. Need
	// to sync, to request another confirmation event ntfn, then re-open
	// channel after confs.

	NegativeConf chan int32 // MUST be buffered.
}

// SpendDetail contains details pertaining to a spent output. This struct itself
// is the spentness notification. It includes the original outpoint which triggered
// the notification, the hash of the transaction spending the output, the
// spending transaction itself, and finally the input index which spent the
// target output.
type SpendDetail struct {
	SpentOutPoint     *wire.OutPoint
	SpenderTxHash     *wire.ShaHash
	SpendingTx        *wire.MsgTx
	SpenderInputIndex uint32
	SpendingHeight    int32
}

// SpendEvent encapsulates a spentness notification. Its only field 'Spend' will
// be sent upon once the target output passed into RegisterSpendNtfn has been
// spent on the blockchain.
type SpendEvent struct {
	Spend chan *SpendDetail // MUST be buffered.
}

// BlockEpoch represents meta-data concerning each new block connected to the
// main chain.
type BlockEpoch struct {
	Height int32
	Hash   *wire.ShaHash
}

// BlockEpochEvent encapsulates an on-going stream of block epoch
// notifications. Its only field 'Epoochs' will be sent upon for each new block
// connected to the main-chain.
type BlockEpochEvent struct {
	Epochs chan *BlockEpoch // MUST be buffered.
}
