package lnwallet

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/chaincfg/chainhash"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/txsort"
)

var zeroHash chainhash.Hash

var (
	// ErrChanClosing is returned when a caller attempts to close a channel
	// that has already been closed or is in the process of being closed.
	ErrChanClosing = fmt.Errorf("channel is being closed, operation disallowed")

	// ErrNoWindow is returned when revocation window is exausted.
	ErrNoWindow = fmt.Errorf("unable to sign new commitment, the current" +
		" revocation window is exhausted")

	// ErrMaxWeightCost is returned when the cost/weight (see segwit)
	// exceeds the widely used maximum allowed policy weight limit. In this
	// case the commitment transaction can't be propagated through the
	// network.
	ErrMaxWeightCost = fmt.Errorf("commitment transaction exceed max " +
		"available cost")

	// ErrMaxHTLCNumber is returned when a proposed HTLC would exceed the
	// maximum number of allowed HTLC's if committed in a state transition
	ErrMaxHTLCNumber = fmt.Errorf("commitment transaction exceed max " +
		"htlc number")

	// ErrInsufficientBalance is returned when a proposed HTLC would
	// exceed the available balance.
	ErrInsufficientBalance = fmt.Errorf("insufficient local balance")

	// ErrCannotSyncCommitChains is returned if, upon receiving a ChanSync
	// message, the state machine deems that is unable to properly
	// synchronize states with the remote peer.
	ErrCannotSyncCommitChains = fmt.Errorf("unable to sync commit chains")

	// ErrInvalidLastCommitSecret is returned in the case that the
	// commitment secret sent by the remote party in their
	// ChannelReestablish message doesn't match the last secret we sent.
	ErrInvalidLastCommitSecret = fmt.Errorf("commit secret is incorrect")

	// ErrCommitSyncDataLoss is returned in the case that we receive a
	// valid commit secret within the ChannelReestablish message from the
	// remote node AND they advertise a RemoteCommitTailHeight higher than
	// our current known height.
	ErrCommitSyncDataLoss = fmt.Errorf("possible commitment state data " +
		"loss")
)

// channelState is an enum like type which represents the current state of a
// particular channel.
// TODO(roasbeef): actually update state
type channelState uint8

const (
	// channelPending indicates this channel is still going through the
	// funding workflow, and isn't yet open.
	channelPending channelState = iota

	// channelOpen represents an open, active channel capable of
	// sending/receiving HTLCs.
	channelOpen

	// channelClosing represents a channel which is in the process of being
	// closed.
	channelClosing

	// channelClosed represents a channel which has been fully closed. Note
	// that before a channel can be closed, ALL pending HTLCs must be
	// settled/removed.
	channelClosed

	// channelDispute indicates that an un-cooperative closure has been
	// detected within the channel.
	channelDispute

	// channelPendingPayment indicates that there a currently outstanding
	// HTLCs within the channel.
	channelPendingPayment
)

// PaymentHash represents the sha256 of a random value. This hash is used to
// uniquely track incoming/outgoing payments within this channel, as well as
// payments requested by the wallet/daemon.
type PaymentHash [32]byte

// updateType is the exact type of an entry within the shared HTLC log.
type updateType uint8

const (
	// Add is an update type that adds a new HTLC entry into the log.
	// Either side can add a new pending HTLC by adding a new Add entry
	// into their update log.
	Add updateType = iota

	// Fail is an update type which removes a prior HTLC entry from the
	// log. Adding a Fail entry to ones log will modify the _remote_
	// parties update log once a new commitment view has been evaluated
	// which contains the Fail entry.
	Fail

	// MalformedFail is an update type which removes a prior HTLC entry
	// from the log. Adding a MalformedFail entry to ones log will modify
	// the _remote_ parties update log once a new commitment view has been
	// evaluated which contains the MalformedFail entry. The difference
	// from Fail type lie in
	// the different data we have to store.
	MalformedFail

	// Settle is an update type which settles a prior HTLC crediting the
	// balance of the receiving node. Adding a Settle entry to a log will
	// result in the settle entry being removed on the log as well as the
	// original add entry from the remote party's log after the next state
	// transition.
	Settle
)

// String returns a human readable string that uniquely identifies the target
// update type.
func (u updateType) String() string {
	switch u {
	case Add:
		return "Add"
	case Fail:
		return "Fail"
	case MalformedFail:
		return "MalformedFail"
	case Settle:
		return "Settle"
	default:
		return "<unknown type>"
	}
}

// PaymentDescriptor represents a commitment state update which either adds,
// settles, or removes an HTLC. PaymentDescriptors encapsulate all necessary
// metadata w.r.t to an HTLC, and additional data pairing a settle message to
// the original added HTLC.
//
// TODO(roasbeef): LogEntry interface??
//  * need to separate attrs for cancel/add/settle
type PaymentDescriptor struct {
	// RHash is the payment hash for this HTLC. The HTLC can be settled iff
	// the preimage to this hash is presented.
	RHash PaymentHash

	// RPreimage is the preimage that settles the HTLC pointed to within the
	// log by the ParentIndex.
	RPreimage PaymentHash

	// Timeout is the absolute timeout in blocks, after which this HTLC
	// expires.
	Timeout uint32

	// Amount is the HTLC amount in milli-satoshis.
	Amount lnwire.MilliSatoshi

	// LogIndex is the log entry number that his HTLC update has within the
	// log. Depending on if IsIncoming is true, this is either an entry the
	// remote party added, or one that we added locally.
	LogIndex uint64

	// HtlcIndex is the index within the main update log for this HTLC.
	// Entries within the log of type Add will have this field populated,
	// as other entries will point to the entry via this counter.
	//
	// NOTE: This field will only be populate if EntryType is Add.
	HtlcIndex uint64

	// ParentIndex is the HTLC index of the entry that this update settles or
	// times out.
	//
	// NOTE: This field will only be populate if EntryType is Fail or
	// Settle.
	ParentIndex uint64

	// localOutputIndex is the output index of this HTLc output in the
	// commitment transaction of the local node.
	//
	// NOTE: If the output is dust from the PoV of the local commitment
	// chain, then this value will be -1.
	localOutputIndex int32

	// remoteOutputIndex is the output index of this HTLC output in the
	// commitment transaction of the remote node.
	//
	// NOTE: If the output is dust from the PoV of the remote commitment
	// chain, then this value will be -1.
	remoteOutputIndex int32

	// sig is the signature for the second-level HTLC transaction that
	// spends the version of this HTLC on the commitment transaction of the
	// local node. This signature is generated by the remote node and
	// stored by the local node in the case that local node needs to
	// broadcast their commitment transaction.
	sig *btcec.Signature

	// addCommitHeight[Remote|Local] encodes the height of the commitment
	// which included this HTLC on either the remote or local commitment
	// chain. This value is used to determine when an HTLC is fully
	// "locked-in".
	addCommitHeightRemote uint64
	addCommitHeightLocal  uint64

	// removeCommitHeight[Remote|Local] encodes the height of the
	// commitment which removed the parent pointer of this
	// PaymentDescriptor either due to a timeout or a settle. Once both
	// these heights are below the tail of both chains, the log entries can
	// safely be removed.
	removeCommitHeightRemote uint64
	removeCommitHeightLocal  uint64

	// OnionBlob is an opaque blob which is used to complete multi-hop
	// routing.
	//
	// NOTE: Populated only on add payment descriptor entry types.
	OnionBlob []byte

	// ShaOnionBlob is a sha of the onion blob.
	//
	// NOTE: Populated only in payment descriptor with MalfromedFail type.
	ShaOnionBlob [sha256.Size]byte

	// FailReason stores the reason why a particular payment was cancelled.
	//
	// NOTE: Populate only in fail payment descriptor entry types.
	FailReason []byte

	// FailCode stores the code why a particular payment was cancelled.
	//
	// NOTE: Populated only in payment descriptor with MalfromedFail type.
	FailCode lnwire.FailCode

	// [our|their|]PkScript are the raw public key scripts that encodes the
	// redemption rules for this particular HTLC. These fields will only be
	// populated iff the EntryType of this PaymentDescriptor is Add.
	// ourPkScript is the ourPkScript from the context of our local
	// commitment chain. theirPkScript is the latest pkScript from the
	// context of the remote commitment chain.
	//
	// NOTE: These values may change within the logs themselves, however,
	// they'll stay consistent within the commitment chain entries
	// themselves.
	ourPkScript        []byte
	ourWitnessScript   []byte
	theirPkScript      []byte
	theirWitnessScript []byte

	// EntryType denotes the exact type of the PaymentDescriptor. In the
	// case of a Timeout, or Settle type, then the Parent field will point
	// into the log to the HTLC being modified.
	EntryType updateType

	// isForwarded denotes if an incoming HTLC has been forwarded to any
	// possible upstream peers in the route.
	isForwarded bool
}

// commitment represents a commitment to a new state within an active channel.
// New commitments can be initiated by either side. Commitments are ordered
// into a commitment chain, with one existing for both parties. Each side can
// independently extend the other side's commitment chain, up to a certain
// "revocation window", which once reached, disallows new commitments until
// the local nodes receives the revocation for the remote node's chain tail.
type commitment struct {
	// height represents the commitment height of this commitment, or the
	// update number of this commitment.
	height uint64

	// isOurs indicates whether this is the local or remote node's version
	// of the commitment.
	isOurs bool

	// [our|their]MessageIndex are indexes into the HTLC log, up to which
	// this commitment transaction includes. These indexes allow both sides
	// to independently, and concurrent send create new commitments. Each
	// new commitment sent to the remote party includes an index in the
	// shared log which details which of their updates we're including in
	// this new commitment.
	ourMessageIndex   uint64
	theirMessageIndex uint64

	// [our|their]HtlcIndex are the current running counters for the HTLC's
	// offered by either party. This value is incremented each time a party
	// offers a new HTLC. The log update methods that consume HTLC's will
	// reference these counters, rather than the running cumulative message
	// counters.
	ourHtlcIndex   uint64
	theirHtlcIndex uint64

	// txn is the commitment transaction generated by including any HTLC
	// updates whose index are below the two indexes listed above. If this
	// commitment is being added to the remote chain, then this txn is
	// their version of the commitment transactions. If the local commit
	// chain is being modified, the opposite is true.
	txn *wire.MsgTx

	// sig is a signature for the above commitment transaction.
	sig []byte

	// [our|their]Balance represents the settled balances at this point
	// within the commitment chain. This balance is computed by properly
	// evaluating all the add/remove/settle log entries before the listed
	// indexes.
	ourBalance   lnwire.MilliSatoshi
	theirBalance lnwire.MilliSatoshi

	// fee is the amount that will be paid as fees for this commitment
	// transaction. The fee is recorded here so that it can be added back
	// and recalculated for each new update to the channel state.
	fee btcutil.Amount

	// feePerKw is the fee per kw used to calculate this commitment
	// transaction's fee.
	feePerKw btcutil.Amount

	// dustLimit is the limit on the commitment transaction such that no
	// output values should be below this amount.
	dustLimit btcutil.Amount

	// outgoingHTLCs is a slice of all the outgoing HTLC's (from our PoV)
	// on this commitment transaction.
	outgoingHTLCs []PaymentDescriptor

	// incomingHTLCs is a slice of all the incoming HTLC's (from our PoV)
	// on this commitment transaction.
	incomingHTLCs []PaymentDescriptor

	// [outgoing|incoming]HTLCIndex is an index that maps an output index
	// on the commitment transaction to the payment descriptor that
	// represents the HTLC output.
	//
	// NOTE: that these fields are only populated if this commitment state
	// belongs to the local node. These maps are used when validating any
	// HTLC signatures which are part of the local commitment state. We use
	// this map in order to locate the details needed to validate an HTLC
	// signature while iterating of the outputs in the local commitment
	// view.
	outgoingHTLCIndex map[int32]*PaymentDescriptor
	incomingHTLCIndex map[int32]*PaymentDescriptor
}

// locateOutputIndex is a small helper function to locate the output index of a
// particular HTLC within the current commitment transaction. The duplicate map
// massed in is to be retained for each output within the commitment
// transition.  This ensures that we don't assign multiple HTLC's to the same
// index within the commitment transaction.
func locateOutputIndex(p *PaymentDescriptor, tx *wire.MsgTx, ourCommit bool,
	dups map[PaymentHash][]int32) (int32, error) {

	// Checks to see if element (e) exists in slice (s).
	contains := func(s []int32, e int32) bool {
		for _, a := range s {
			if a == e {
				return true
			}
		}
		return false
	}

	// If this their commitment transaction, we'll be trying to locate
	// their pkScripts, otherwise we'll be looking for ours. This is
	// required as the commitment states are asymmetric in order to ascribe
	// blame in the case of a contract breach.
	pkScript := p.theirPkScript
	if ourCommit {
		pkScript = p.ourPkScript
	}

	for i, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) &&
			txOut.Value == int64(p.Amount.ToSatoshis()) {

			// If this payment hash and index has already been
			// found, then we'll continue in order to avoid any
			// duplicate indexes.
			if contains(dups[p.RHash], int32(i)) {
				continue
			}

			idx := int32(i)
			dups[p.RHash] = append(dups[p.RHash], idx)
			return idx, nil
		}
	}

	return 0, fmt.Errorf("unable to find htlc: script=%x, value=%v",
		pkScript, p.Amount)
}

// populateHtlcIndexes modifies the set of HTLC's locked-into the target view
// to have full indexing information populated. This information is required as
// we need to keep track of the indexes of each HTLC in order to properly write
// the current state to disk, and also to locate the PaymentDescriptor
// corresponding to HTLC outputs in the commitment transaction.
func (c *commitment) populateHtlcIndexes() error {
	// First, we'll set up some state to allow us to locate the output
	// index of the all the HTLC's within the commitment transaction. We
	// must keep this index so we can validate the HTLC signatures sent to
	// us.
	dups := make(map[PaymentHash][]int32)
	c.outgoingHTLCIndex = make(map[int32]*PaymentDescriptor)
	c.incomingHTLCIndex = make(map[int32]*PaymentDescriptor)

	// populateIndex is a helper function that populates the necessary
	// indexes within the commitment view for a particular HTLC.
	populateIndex := func(htlc *PaymentDescriptor, incoming bool) error {
		isDust := htlcIsDust(incoming, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit)

		var err error
		switch {

		// If this is our commitment transaction, and this is a dust
		// output then we mark it as such using a -1 index.
		case c.isOurs && isDust:
			htlc.localOutputIndex = -1

		// If this is the commitment transaction of the remote party,
		// and this is a dust output then we mark it as such using a -1
		// index.
		case !c.isOurs && isDust:
			htlc.remoteOutputIndex = -1

		// If this is our commitment transaction, then we'll need to
		// locate the output and the index so we can verify an HTLC
		// signatures.
		case c.isOurs:
			htlc.localOutputIndex, err = locateOutputIndex(
				htlc, c.txn, c.isOurs, dups,
			)
			if err != nil {
				return err
			}

			// As this is our commitment transactions, we need to
			// keep track of the locations of each output on the
			// transaction so we can verify any HTLC signatures
			// sent to us after we construct the HTLC view.
			if incoming {
				c.incomingHTLCIndex[htlc.localOutputIndex] = htlc
			} else {
				c.outgoingHTLCIndex[htlc.localOutputIndex] = htlc
			}

		// Otherwise, this is there remote party's commitment
		// transaction and we only need to populate the remote output
		// index within the HTLC index.
		case !c.isOurs:
			htlc.remoteOutputIndex, err = locateOutputIndex(
				htlc, c.txn, c.isOurs, dups,
			)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("invalid commitment configuration")
		}

		return nil
	}

	// Finally, we'll need to locate the index within the commitment
	// transaction of all the HTLC outputs. This index will be required
	// later when we write the commitment state to disk, and also when
	// generating signatures for each of the HTLC transactions.
	for i := 0; i < len(c.outgoingHTLCs); i++ {
		htlc := &c.outgoingHTLCs[i]
		if err := populateIndex(htlc, false); err != nil {
			return nil
		}
	}
	for i := 0; i < len(c.incomingHTLCs); i++ {
		htlc := &c.incomingHTLCs[i]
		if err := populateIndex(htlc, true); err != nil {
			return nil
		}
	}

	return nil
}

// toDiskCommit converts the target commitment into a format suitable to be
// written to disk after an accepted state transition.
func (c *commitment) toDiskCommit(ourCommit bool) *channeldb.ChannelCommitment {
	numHtlcs := len(c.outgoingHTLCs) + len(c.incomingHTLCs)

	commit := &channeldb.ChannelCommitment{
		CommitHeight:    c.height,
		LocalLogIndex:   c.ourMessageIndex,
		LocalHtlcIndex:  c.ourHtlcIndex,
		RemoteLogIndex:  c.theirMessageIndex,
		RemoteHtlcIndex: c.theirHtlcIndex,
		LocalBalance:    c.ourBalance,
		RemoteBalance:   c.theirBalance,
		CommitFee:       c.fee,
		FeePerKw:        c.feePerKw,
		CommitTx:        c.txn,
		CommitSig:       c.sig,
		Htlcs:           make([]channeldb.HTLC, 0, numHtlcs),
	}

	for _, htlc := range c.outgoingHTLCs {
		outputIndex := htlc.localOutputIndex
		if !ourCommit {
			outputIndex = htlc.remoteOutputIndex
		}

		h := channeldb.HTLC{
			RHash:         htlc.RHash,
			Amt:           htlc.Amount,
			RefundTimeout: htlc.Timeout,
			OutputIndex:   outputIndex,
			HtlcIndex:     htlc.HtlcIndex,
			LogIndex:      htlc.LogIndex,
			Incoming:      false,
		}
		h.OnionBlob = make([]byte, len(htlc.OnionBlob))
		copy(h.OnionBlob[:], htlc.OnionBlob)

		if ourCommit && htlc.sig != nil {
			h.Signature = htlc.sig.Serialize()
		}

		commit.Htlcs = append(commit.Htlcs, h)
	}

	for _, htlc := range c.incomingHTLCs {
		outputIndex := htlc.localOutputIndex
		if !ourCommit {
			outputIndex = htlc.remoteOutputIndex
		}

		h := channeldb.HTLC{
			RHash:         htlc.RHash,
			Amt:           htlc.Amount,
			RefundTimeout: htlc.Timeout,
			OutputIndex:   outputIndex,
			HtlcIndex:     htlc.HtlcIndex,
			LogIndex:      htlc.LogIndex,
			Incoming:      true,
		}
		h.OnionBlob = make([]byte, len(htlc.OnionBlob))
		copy(h.OnionBlob[:], htlc.OnionBlob)

		if ourCommit && htlc.sig != nil {
			h.Signature = htlc.sig.Serialize()
		}

		commit.Htlcs = append(commit.Htlcs, h)
	}

	return commit
}

// diskHtlcToPayDesc converts an HTLC previously written to disk within a
// commitment state to the form required to manipulate in memory within the
// commitment struct and updateLog. This function is used when we need to
// restore commitment state written do disk back into memory once we need to
// restart a channel session.
func (lc *LightningChannel) diskHtlcToPayDesc(feeRate btcutil.Amount,
	commitHeight uint64, isPendingCommit bool, htlc *channeldb.HTLC,
	localCommitKeys, remoteCommitKeys *commitmentKeyRing) (PaymentDescriptor, error) {

	// The proper pkScripts for this PaymentDescriptor must be
	// generated so we can easily locate them within the commitment
	// transaction in the future.
	var (
		ourP2WSH, theirP2WSH                 []byte
		ourWitnessScript, theirWitnessScript []byte
		pd                                   PaymentDescriptor
		err                                  error
	)

	// If the either outputs is dust from the local or remote node's
	// perspective, then we don't need to generate the scripts as we only
	// generate them in order to locate the outputs within the commitment
	// transaction. As we'll mark dust with a special output index in the
	// on-disk state snapshot.
	isDustLocal := htlcIsDust(htlc.Incoming, true, feeRate,
		htlc.Amt.ToSatoshis(), lc.channelState.LocalChanCfg.DustLimit)
	if !isDustLocal && localCommitKeys != nil {
		ourP2WSH, ourWitnessScript, err = genHtlcScript(
			htlc.Incoming, true, htlc.RefundTimeout, htlc.RHash,
			localCommitKeys)
		if err != nil {
			return pd, err
		}
	}
	isDustRemote := htlcIsDust(htlc.Incoming, false, feeRate,
		htlc.Amt.ToSatoshis(), lc.channelState.RemoteChanCfg.DustLimit)
	if !isDustRemote && remoteCommitKeys != nil {
		theirP2WSH, theirWitnessScript, err = genHtlcScript(
			htlc.Incoming, false, htlc.RefundTimeout, htlc.RHash,
			remoteCommitKeys)
		if err != nil {
			return pd, err
		}
	}

	// With the scripts reconstructed (depending on if this is our commit
	// vs theirs or a pending commit for the remote party), we can now
	// re-create the original payment descriptor.
	pd = PaymentDescriptor{
		RHash:              htlc.RHash,
		Timeout:            htlc.RefundTimeout,
		Amount:             htlc.Amt,
		EntryType:          Add,
		HtlcIndex:          htlc.HtlcIndex,
		LogIndex:           htlc.LogIndex,
		OnionBlob:          htlc.OnionBlob,
		ourPkScript:        ourP2WSH,
		ourWitnessScript:   ourWitnessScript,
		theirPkScript:      theirP2WSH,
		theirWitnessScript: theirWitnessScript,
	}

	// If this is a pending commit, then the HTLC was only included in the
	// commitment of the remote party, so we only set that commit height.
	// Otherwise, we'll set the commit height for both chains as the HTLC
	// was written to disk after it was fully locked in.
	if isPendingCommit {
		pd.addCommitHeightRemote = commitHeight
	} else {
		pd.addCommitHeightRemote = commitHeight
		pd.addCommitHeightLocal = commitHeight

	}

	return pd, nil
}

// extractPayDescs will convert all HTLC's present within a disk commit state
// to a set of incoming and outgoing payment descriptors. Once reconstructed,
// these payment descriptors can be re-inserted into the in-memory updateLog
// for each side.
func (lc *LightningChannel) extractPayDescs(commitHeight uint64,
	isPendingCommit bool, feeRate btcutil.Amount,
	htlcs []channeldb.HTLC, localCommitKeys *commitmentKeyRing,
	remoteCommitKeys *commitmentKeyRing) ([]PaymentDescriptor, []PaymentDescriptor, error) {

	var (
		incomingHtlcs []PaymentDescriptor
		outgoingHtlcs []PaymentDescriptor
	)

	// For each included HTLC within this commitment state, we'll convert
	// the disk format into our in memory PaymentDescriptor format,
	// partitioning based on if we offered or received the HTLC.
	for _, htlc := range htlcs {
		// TODO(roasbeef): set isForwarded to false for all? need to
		// persist state w.r.t to if forwarded or not, or can
		// inadvertently trigger replays

		payDesc, err := lc.diskHtlcToPayDesc(
			feeRate, commitHeight, isPendingCommit, &htlc,
			localCommitKeys, remoteCommitKeys,
		)
		if err != nil {
			return incomingHtlcs, outgoingHtlcs, err
		}

		if htlc.Incoming {
			incomingHtlcs = append(incomingHtlcs, payDesc)
		} else {
			outgoingHtlcs = append(outgoingHtlcs, payDesc)
		}
	}

	return incomingHtlcs, outgoingHtlcs, nil
}

// diskCommitToMemCommit converts tthe on-disk commitment format to our
// in-memory commitment format which is needed in order to properly resume
// channel operations after a restart.
func (lc *LightningChannel) diskCommitToMemCommit(isLocal, isPendingCommit bool,
	diskCommit *channeldb.ChannelCommitment,
	localCommitPoint, remoteCommitPoint *btcec.PublicKey) (*commitment, error) {

	// First, we'll need to re-derive the commitment key ring for each
	// party used within this particular state. If this is a pending commit
	// (we extended but weren't able to complete the commitment dance
	// before shutdown), then the localCommitPoint won't be set as we
	// haven't yet received a responding commitment from the remote party.
	var localCommitKeys, remoteCommitKeys *commitmentKeyRing
	if localCommitPoint != nil {
		localCommitKeys = deriveCommitmentKeys(localCommitPoint, true,
			lc.localChanCfg, lc.remoteChanCfg)
	}
	if remoteCommitPoint != nil {
		remoteCommitKeys = deriveCommitmentKeys(remoteCommitPoint, false,
			lc.localChanCfg, lc.remoteChanCfg)
	}

	// With the key rings re-created, we'll now convert all the on-disk
	// HTLC"s into PaymentDescriptor's so we can re-insert them into our
	// update log.
	incomingHtlcs, outgoingHtlcs, err := lc.extractPayDescs(
		diskCommit.CommitHeight, isPendingCommit,
		diskCommit.FeePerKw, diskCommit.Htlcs,
		localCommitKeys, remoteCommitKeys,
	)
	if err != nil {
		return nil, err
	}

	// With the necessary items generated, we'll now re-construct the
	// commitment state as it was originally present in memory.
	commit := &commitment{
		height:            diskCommit.CommitHeight,
		isOurs:            isLocal,
		ourBalance:        diskCommit.LocalBalance,
		theirBalance:      diskCommit.RemoteBalance,
		ourMessageIndex:   diskCommit.LocalLogIndex,
		ourHtlcIndex:      diskCommit.LocalHtlcIndex,
		theirMessageIndex: diskCommit.RemoteLogIndex,
		theirHtlcIndex:    diskCommit.RemoteHtlcIndex,
		txn:               diskCommit.CommitTx,
		sig:               diskCommit.CommitSig,
		fee:               diskCommit.CommitFee,
		feePerKw:          diskCommit.FeePerKw,
		incomingHTLCs:     incomingHtlcs,
		outgoingHTLCs:     outgoingHtlcs,
	}
	if isLocal {
		commit.dustLimit = lc.channelState.LocalChanCfg.DustLimit
	} else {
		commit.dustLimit = lc.channelState.RemoteChanCfg.DustLimit
	}

	// Finally, we'll re-populate the HTLC index for this state so we can
	// properly locate each HTLC within the commitment transaction.
	if err := commit.populateHtlcIndexes(); err != nil {
		return nil, err
	}

	return commit, nil
}

// commitmentKeyRing holds all derived keys needed to construct commitment and
// HTLC transactions. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's. Private keys associated
// with each key may belong to the commitment owner or the "other party" which
// is referred to in the field comments, regardless of which is local and which
// is remote.
type commitmentKeyRing struct {
	// commitPoint is the "per commitment point" used to derive the tweak
	// for each base point.
	commitPoint *btcec.PublicKey

	// localCommitKeyTweak is the tweak used to derive the local public key
	// from the local payment base point or the local private key from the
	// base point secret. This may be included in a SignDescriptor to
	// generate signatures for the local payment key.
	localCommitKeyTweak []byte

	// TODO(roasbeef): need delay tweak as well?

	// localHtlcKeyTweak is the teak used to derive the local HTLC key from
	// the local HTLC base point point. This value is needed in order to
	// derive the final key used within the HTLC scripts in the commitment
	// transaction.
	localHtlcKeyTweak []byte

	// localHtlcKey is the key that will be used in the "to self" clause of
	// any HTLC scripts within the commitment transaction for this key ring
	// set.
	localHtlcKey *btcec.PublicKey

	// remoteHtlcKey is the key that will be used in clauses within the
	// HTLC script that send money to the remote party.
	remoteHtlcKey *btcec.PublicKey

	// delayKey is the commitment transaction owner's key which is included
	// in HTLC success and timeout transaction scripts.
	delayKey *btcec.PublicKey

	// noDelayKey is the other party's payment key in the commitment tx.
	// This is the key used to generate the unencumbered output within the
	// commitment transaction.
	noDelayKey *btcec.PublicKey

	// revocationKey is the key that can be used by the other party to
	// redeem outputs from a revoked commitment transaction if it were to
	// be published.
	revocationKey *btcec.PublicKey
}

// deriveCommitmentKey generates a new commitment key set using the base points
// and commitment point. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's.
func deriveCommitmentKeys(commitPoint *btcec.PublicKey, isOurCommit bool,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig) *commitmentKeyRing {

	// First, we'll derive all the keys that don't depend on the context of
	// whose commitment transaction this is.
	keyRing := &commitmentKeyRing{
		commitPoint: commitPoint,

		localCommitKeyTweak: SingleTweakBytes(commitPoint,
			localChanCfg.PaymentBasePoint),
		localHtlcKeyTweak: SingleTweakBytes(commitPoint,
			localChanCfg.HtlcBasePoint),

		localHtlcKey: TweakPubKey(localChanCfg.HtlcBasePoint,
			commitPoint),
		remoteHtlcKey: TweakPubKey(remoteChanCfg.HtlcBasePoint,
			commitPoint),
	}

	// We'll now compute the delay, no delay, and revocation key based on
	// the current commitment point. All keys are tweaked each state in
	// order to ensure the keys from each state are unlinkable. To create
	// the revocation key, we take the opposite party's revocation base
	// point and combine that with the current commitment point.
	var (
		delayBasePoint      *btcec.PublicKey
		noDelayBasePoint    *btcec.PublicKey
		revocationBasePoint *btcec.PublicKey
	)
	if isOurCommit {
		delayBasePoint = localChanCfg.DelayBasePoint
		noDelayBasePoint = remoteChanCfg.PaymentBasePoint
		revocationBasePoint = remoteChanCfg.RevocationBasePoint
	} else {
		delayBasePoint = remoteChanCfg.DelayBasePoint
		noDelayBasePoint = localChanCfg.PaymentBasePoint
		revocationBasePoint = localChanCfg.RevocationBasePoint
	}

	// With the base points assigned, we can now derive the actual keys
	// using the base point, and the current commitment tweak.
	keyRing.delayKey = TweakPubKey(delayBasePoint, commitPoint)
	keyRing.noDelayKey = TweakPubKey(noDelayBasePoint, commitPoint)
	keyRing.revocationKey = DeriveRevocationPubkey(
		revocationBasePoint, commitPoint,
	)

	return keyRing
}

// commitmentChain represents a chain of unrevoked commitments. The tail of the
// chain is the latest fully signed, yet unrevoked commitment. Two chains are
// tracked, one for the local node, and another for the remote node. New
// commitments we create locally extend the remote node's chain, and vice
// versa. Commitment chains are allowed to grow to a bounded length, after
// which the tail needs to be "dropped" before new commitments can be received.
// The tail is "dropped" when the owner of the chain sends a revocation for the
// previous tail.
type commitmentChain struct {
	// commitments is a linked list of commitments to new states. New
	// commitments are added to the end of the chain with increase height.
	// Once a commitment transaction is revoked, the tail is incremented,
	// freeing up the revocation window for new commitments.
	commitments *list.List

	// startingHeight is the starting height of this commitment chain on a
	// session basis.
	startingHeight uint64
}

// newCommitmentChain creates a new commitment chain from an initial height.
func newCommitmentChain(initialHeight uint64) *commitmentChain {
	return &commitmentChain{
		commitments:    list.New(),
		startingHeight: initialHeight,
	}
}

// addCommitment extends the commitment chain by a single commitment. This
// added commitment represents a state update proposed by either party. Once
// the commitment prior to this commitment is revoked, the commitment becomes
// the new defacto state within the channel.
func (s *commitmentChain) addCommitment(c *commitment) {
	s.commitments.PushBack(c)
}

// advanceTail reduces the length of the commitment chain by one. The tail of
// the chain should be advanced once a revocation for the lowest unrevoked
// commitment in the chain is received.
func (s *commitmentChain) advanceTail() {
	s.commitments.Remove(s.commitments.Front())
}

// tip returns the latest commitment added to the chain.
func (s *commitmentChain) tip() *commitment {
	return s.commitments.Back().Value.(*commitment)
}

// tail returns the lowest unrevoked commitment transaction in the chain.
func (s *commitmentChain) tail() *commitment {
	return s.commitments.Front().Value.(*commitment)
}

// hasUnackedCommitment returns true if the commitment chain has more than one
// entry. The tail of the commitment chain has been ACKed by revoking all prior
// commitments, but any subsequent commitments have not yet been ACKed.
func (s *commitmentChain) hasUnackedCommitment() bool {
	return s.commitments.Front() != s.commitments.Back()
}

// updateLog is an append-only log that stores updates to a node's commitment
// chain. This structure can be seen as the "mempool" within Lightning where
// changes are stored before they're committed to the chain. Once an entry has
// been committed in both the local and remote commitment chain, then it can be
// removed from this log.
//
// TODO(roasbeef): create lightning package, move commitment and update to
// package?
//  * also move state machine, separate from lnwallet package
//  * possible embed updateLog within commitmentChain.
type updateLog struct {
	// logIndex is a monotonically increasing integer that tracks the total
	// number of update entries ever applied to the log. When sending new
	// commitment states, we include all updates up to this index.
	logIndex uint64

	// htlcCounter is a monotonically increasing integer that tracks the
	// total number of offered HTLC's by the owner of this update log. We
	// use a distinct index for this purpose, as update's that remove
	// entires from the log will be indexed using this counter.
	htlcCounter uint64

	// List is the updatelog itself, we embed this value so updateLog has
	// access to all the method of a list.List.
	*list.List

	// updateIndex is an index that maps a particular entries index to the
	// list element within the list.List above.
	updateIndex map[uint64]*list.Element

	// offerIndex is an index that maps the counter for offered HTLC's to
	// their list elemtn within the main list.List.
	htlcIndex map[uint64]*list.Element
}

// newUpdateLog creates a new updateLog instance.
func newUpdateLog(logIndex, htlcCounter uint64) *updateLog {
	return &updateLog{
		List:        list.New(),
		updateIndex: make(map[uint64]*list.Element),
		htlcIndex:   make(map[uint64]*list.Element),
		logIndex:    logIndex,
		htlcCounter: htlcCounter,
	}
}

// restoreHtlc will "restore" a prior HTLC to the updateLog. We say restore as
// this method is intended to be used when re-covering a prior commitment
// state. This function differs from appendHtlc in that it won't increment
// either of log's counters. If the HTLC is already present, then it is
// ignored.
func (u *updateLog) restoreHtlc(pd *PaymentDescriptor) {
	if _, ok := u.htlcIndex[pd.HtlcIndex]; ok {
		return
	}

	u.htlcIndex[pd.HtlcIndex] = u.PushBack(pd)
}

// appendUpdate appends a new update to the tip of the updateLog. The entry is
// also added to index accordingly.
func (u *updateLog) appendUpdate(pd *PaymentDescriptor) {
	u.updateIndex[u.logIndex] = u.PushBack(pd)
	u.logIndex++
}

// appendHtlc appends a new HTLC offer to the tip of the update log. The entry
// is also added to the offer index accordingly.
func (u *updateLog) appendHtlc(pd *PaymentDescriptor) {
	u.htlcIndex[u.htlcCounter] = u.PushBack(pd)
	u.htlcCounter++

	u.logIndex++
}

// lookupHtlc attempts to look up an offered HTLC according to it's offer
// index. If the entry isn't found, then a nil pointer is returned.
func (u *updateLog) lookupHtlc(i uint64) *PaymentDescriptor {
	htlc, ok := u.htlcIndex[i]
	if !ok {
		return nil
	}

	return htlc.Value.(*PaymentDescriptor)
}

// remove attempts to remove an entry from the update log. If the entry is
// found, then the entry will be removed from the update log and index.
func (u *updateLog) removeUpdate(i uint64) {
	entry := u.updateIndex[i]
	u.Remove(entry)
	delete(u.updateIndex, i)
}

// removeHtlc attempts to remove an HTLC offer form the update log. If the
// entry is found, then the entry will be removed from both the main log and
// the offer index.
func (u *updateLog) removeHtlc(i uint64) {
	entry := u.htlcIndex[i]
	u.Remove(entry)
	delete(u.htlcIndex, i)
}

// compactLogs performs garbage collection within the log removing HTLCs which
// have been removed from the point-of-view of the tail of both chains. The
// entries which timeout/settle HTLCs are also removed.
func compactLogs(ourLog, theirLog *updateLog,
	localChainTail, remoteChainTail uint64) {

	compactLog := func(logA, logB *updateLog) {
		var nextA *list.Element
		for e := logA.Front(); e != nil; e = nextA {
			// Assign next iteration element at top of loop because
			// we may remove the current element from the list,
			// which can change the iterated sequence.
			nextA = e.Next()

			htlc := e.Value.(*PaymentDescriptor)
			if htlc.EntryType == Add {
				continue
			}

			// If the HTLC hasn't yet been removed from either
			// chain, the skip it.
			if htlc.removeCommitHeightRemote == 0 ||
				htlc.removeCommitHeightLocal == 0 {
				continue
			}

			// Otherwise if the height of the tail of both chains
			// is at least the height in which the HTLC was
			// removed, then evict the settle/timeout entry along
			// with the original add entry.
			if remoteChainTail >= htlc.removeCommitHeightRemote &&
				localChainTail >= htlc.removeCommitHeightLocal {

				logA.removeUpdate(htlc.LogIndex)
				logB.removeHtlc(htlc.ParentIndex)
			}

		}
	}

	compactLog(ourLog, theirLog)
	compactLog(theirLog, ourLog)
}

// LightningChannel implements the state machine which corresponds to the
// current commitment protocol wire spec. The state machine implemented allows
// for asynchronous fully desynchronized, batched+pipelined updates to
// commitment transactions allowing for a high degree of non-blocking
// bi-directional payment throughput.
//
// In order to allow updates to be fully non-blocking, either side is able to
// create multiple new commitment states up to a pre-determined window size.
// This window size is encoded within InitialRevocationWindow. Before the start
// of a session, both side should send out revocation messages with nil
// preimages in order to populate their revocation window for the remote party.
//
// The state machine has for main methods:
//  * .SignNextCommitment()
//    * Called one one wishes to sign the next commitment, either initiating a
//      new state update, or responding to a received commitment.
//  * .ReceiveNewCommitment()
//    * Called upon receipt of a new commitment from the remote party. If the
//      new commitment is valid, then a revocation should immediately be
//      generated and sent.
//  * .RevokeCurrentCommitment()
//    * Revokes the current commitment. Should be called directly after
//      receiving a new commitment.
//  * .ReceiveRevocation()
//   * Processes a revocation from the remote party. If successful creates a
//     new defacto broadcastable state.
//
// See the individual comments within the above methods for further details.
type LightningChannel struct {
	// signer is the main signer instances that will be responsible for
	// signing any HTLC and commitment transaction generated by the state
	// machine.
	signer Signer

	// signDesc is the primary sign descriptor that is capable of signing
	// the commitment transaction that spends the multi-sig output.
	signDesc *SignDescriptor

	channelEvents chainntnfs.ChainNotifier

	status channelState

	// ChanPoint is the funding outpoint of this channel.
	ChanPoint *wire.OutPoint

	// sigPool is a pool of workers that are capable of signing and
	// validating signatures in parallel. This is utilized as an
	// optimization to void serially signing or validating the HTLC
	// signatures, of which there may be hundreds.
	sigPool *sigPool

	// feeEstimator is used to calculate the fee rate for the channel's
	// commitment and cooperative close transactions.
	feeEstimator FeeEstimator

	// Capacity is the total capacity of this channel.
	Capacity btcutil.Amount

	// stateHintObfuscator is a 48-bit state hint that's used to obfsucate
	// the current state number on the commitment transactions.
	stateHintObfuscator [StateHintSize]byte

	// currentHeight is the current height of our local commitment chain.
	// This is also the same as the number of updates to the channel we've
	// accepted.
	currentHeight uint64

	// remoteCommitChain is the remote node's commitment chain. Any new
	// commitments we initiate are added to the tip of this chain.
	remoteCommitChain *commitmentChain

	// localCommitChain is our local commitment chain. Any new commitments
	// received are added to the tip of this chain. The tail (or lowest
	// height) in this chain is our current accepted state, which we are
	// able to broadcast safely.
	localCommitChain *commitmentChain

	channelState *channeldb.OpenChannel

	localChanCfg *channeldb.ChannelConfig

	remoteChanCfg *channeldb.ChannelConfig

	// [local|remote]Log is a (mostly) append-only log storing all the HTLC
	// updates to this channel. The log is walked backwards as HTLC updates
	// are applied in order to re-construct a commitment transaction from a
	// commitment. The log is compacted once a revocation is received.
	localUpdateLog  *updateLog
	remoteUpdateLog *updateLog

	// pendingFeeUpdate is set to the fee-per-kw we last sent (if we are
	// channel initiator) or received (if non-initiator) in an update fee
	// message, which haven't yet been included in a commitment.  It will
	// be nil if no fee update is un-committed.
	pendingFeeUpdate *btcutil.Amount

	// pendingAckFeeUpdate is set to the last committed fee update which is
	// not yet ACKed. This value will be nil if a fee update hasn't been
	// initiated.
	pendingAckFeeUpdate *btcutil.Amount

	// FundingWitnessScript is the witness script for the 2-of-2 multi-sig
	// that opened the channel.
	FundingWitnessScript []byte

	fundingTxIn  wire.TxIn
	fundingP2WSH []byte

	// ForceCloseSignal is a channel that is closed to indicate that a
	// local system has initiated a force close by broadcasting the current
	// commitment transaction directly on-chain.
	ForceCloseSignal chan struct{}

	// UnilateralCloseSignal is a channel that is closed to indicate that
	// the remote party has performed a unilateral close by broadcasting
	// their version of the commitment transaction on-chain.
	UnilateralCloseSignal chan struct{}

	// UnilateralClose is a channel that will be sent upon by the close
	// observer once the unilateral close of a channel is detected.
	UnilateralClose chan *UnilateralCloseSummary

	// ContractBreach is a channel that is used to communicate the data
	// necessary to fully resolve the channel in the case that a contract
	// breach is detected. A contract breach occurs it is detected that the
	// counterparty has broadcast a prior *revoked* state.
	ContractBreach chan *BreachRetribution

	// LocalFundingKey is the public key under control by the wallet that
	// was used for the 2-of-2 funding output which created this channel.
	LocalFundingKey *btcec.PublicKey

	// RemoteFundingKey is the public key for the remote channel counter
	// party  which used for the 2-of-2 funding output which created this
	// channel.
	RemoteFundingKey *btcec.PublicKey

	sync.RWMutex

	cowg sync.WaitGroup
	wg   sync.WaitGroup

	shutdown int32
	quit     chan struct{}

	observerFin  int32
	observerQuit chan struct{}
}

// NewLightningChannel creates a new, active payment channel given an
// implementation of the chain notifier, channel database, and the current
// settled channel state. Throughout state transitions, then channel will
// automatically persist pertinent state to the database in an efficient
// manner.
func NewLightningChannel(signer Signer, events chainntnfs.ChainNotifier,
	fe FeeEstimator, state *channeldb.OpenChannel) (*LightningChannel, error) {

	localKey := state.LocalChanCfg.MultiSigKey.SerializeCompressed()
	remoteKey := state.RemoteChanCfg.MultiSigKey.SerializeCompressed()
	multiSigScript, err := genMultiSigScript(localKey, remoteKey)
	if err != nil {
		return nil, err
	}

	var stateHint [StateHintSize]byte
	if state.IsInitiator {
		stateHint = deriveStateHintObfuscator(
			state.LocalChanCfg.PaymentBasePoint,
			state.RemoteChanCfg.PaymentBasePoint,
		)
	} else {
		stateHint = deriveStateHintObfuscator(
			state.RemoteChanCfg.PaymentBasePoint,
			state.LocalChanCfg.PaymentBasePoint,
		)
	}

	localCommit := state.LocalCommitment
	remoteCommit := state.RemoteCommitment

	// First, initialize the update logs with their current counter values
	// from the local and remote commitments.
	localUpdateLog := newUpdateLog(
		localCommit.LocalLogIndex, localCommit.LocalHtlcIndex,
	)
	remoteUpdateLog := newUpdateLog(
		remoteCommit.RemoteLogIndex, remoteCommit.RemoteHtlcIndex,
	)

	lc := &LightningChannel{
		// TODO(roasbeef): tune num sig workers?
		sigPool:               newSigPool(runtime.NumCPU(), signer),
		signer:                signer,
		channelEvents:         events,
		feeEstimator:          fe,
		stateHintObfuscator:   stateHint,
		currentHeight:         localCommit.CommitHeight,
		remoteCommitChain:     newCommitmentChain(remoteCommit.CommitHeight),
		localCommitChain:      newCommitmentChain(localCommit.CommitHeight),
		channelState:          state,
		localChanCfg:          &state.LocalChanCfg,
		remoteChanCfg:         &state.RemoteChanCfg,
		localUpdateLog:        localUpdateLog,
		remoteUpdateLog:       remoteUpdateLog,
		ChanPoint:             &state.FundingOutpoint,
		Capacity:              state.Capacity,
		FundingWitnessScript:  multiSigScript,
		ForceCloseSignal:      make(chan struct{}),
		UnilateralClose:       make(chan *UnilateralCloseSummary, 1),
		UnilateralCloseSignal: make(chan struct{}),
		ContractBreach:        make(chan *BreachRetribution, 1),
		LocalFundingKey:       state.LocalChanCfg.MultiSigKey,
		RemoteFundingKey:      state.RemoteChanCfg.MultiSigKey,
		quit:                  make(chan struct{}),
		observerQuit:          make(chan struct{}),
	}

	// With the main channel struct reconstructed, we'll now restore the
	// commitment state in memory and also the update logs themselves.
	err = lc.restoreCommitState(
		&localCommit, &remoteCommit, localUpdateLog, remoteUpdateLog,
	)
	if err != nil {
		return nil, err
	}

	// Create the sign descriptor which we'll be using very frequently to
	// request a signature for the 2-of-2 multi-sig from the signer in
	// order to complete channel state transitions.
	fundingPkScript, err := witnessScriptHash(multiSigScript)
	if err != nil {
		return nil, err
	}
	lc.fundingTxIn = *wire.NewTxIn(&state.FundingOutpoint, nil, nil)
	lc.fundingP2WSH = fundingPkScript
	lc.signDesc = &SignDescriptor{
		PubKey:        lc.localChanCfg.MultiSigKey,
		WitnessScript: multiSigScript,
		Output: &wire.TxOut{
			PkScript: lc.fundingP2WSH,
			Value:    int64(lc.channelState.Capacity),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}

	// We'll only launch a close observer if the ChainNotifier
	// implementation is non-nil. Passing a nil value indicates that the
	// channel shouldn't be actively watched for.
	if lc.channelEvents != nil {
		// Register for a notification to be dispatched if the funding
		// outpoint has been spent. This indicates that either us or
		// the remote party has broadcast a commitment transaction
		// on-chain.
		fundingOut := &lc.fundingTxIn.PreviousOutPoint

		// As a height hint, we'll try to use the opening height, but
		// if the channel isn't yet open, then we'll use the height it
		// was broadcast at.
		heightHint := lc.channelState.ShortChanID.BlockHeight
		if heightHint == 0 {
			heightHint = lc.channelState.FundingBroadcastHeight
		}

		channelCloseNtfn, err := lc.channelEvents.RegisterSpendNtfn(
			fundingOut, heightHint,
		)
		if err != nil {
			return nil, err
		}

		// Launch the close observer which will vigilantly watch the
		// network for any broadcasts the current or prior commitment
		// transactions, taking action accordingly.
		lc.cowg.Add(1)
		go lc.closeObserver(channelCloseNtfn)
	}

	// Finally, we'll kick of the signature job pool to handle any upcoming
	// commitment state generation and validation.
	if err := lc.sigPool.Start(); err != nil {
		return nil, err
	}

	return lc, nil
}

// Stop gracefully shuts down any active goroutines spawned by the
// LightningChannel during regular duties.
func (lc *LightningChannel) Stop() {
	if !atomic.CompareAndSwapInt32(&lc.shutdown, 0, 1) {
		return
	}

	lc.sigPool.Stop()

	close(lc.quit)
}

// CancelObserver signals the active goroutines watching for on-chain channel
// closes that they can exit safely.
func (lc *LightningChannel) CancelObserver() {
	if !atomic.CompareAndSwapInt32(&lc.observerFin, 0, 1) {
		return
	}

	close(lc.observerQuit)
}

// WaitForClose blocks until the channel's close observer has terminated.
func (lc *LightningChannel) WaitForClose() {
	lc.cowg.Wait()
}

// ResetState resets the state of the channel back to the default state. This
// ensures that any active goroutines which need to act based on on-chain
// events do so properly.
func (lc *LightningChannel) ResetState() {
	lc.Lock()
	lc.status = channelOpen
	lc.Unlock()
}

// logUpdateToPayDesc converts a LogUpdate into a matching PaymentDescriptor
// entry that can be re-inserted into the update log. This method is used when
// we extended a state to the remote party, but the connection was obstructed
// before we could finish the commitment dance. In this case, we need to
// re-insert the original entries back into the update log so we can resume as
// if nothing happened.
func (lc *LightningChannel) logUpdateToPayDesc(logUpdate *channeldb.LogUpdate,
	remoteUpdateLog *updateLog, commitHeight uint64,
	feeRate btcutil.Amount, remoteCommitKeys *commitmentKeyRing,
	remoteDustLimit btcutil.Amount) (*PaymentDescriptor, error) {

	// Depending on the type of update message we'll map that to a distinct
	// PaymentDescriptor instance.
	var pd *PaymentDescriptor

	switch wireMsg := logUpdate.UpdateMsg.(type) {

	// For offered HTLC's, we'll map that to a PaymentDescriptor with the
	// type Add, ensuring we restore the necessary fields. From the PoV of
	// the commitment chain, this HTLC was included in the remote chain,
	// but not the local chain.
	case *lnwire.UpdateAddHTLC:
		// First, we'll map all the relevant fields in the
		// UpdateAddHTLC message to their corresponding fields in the
		// PaymentDescriptor struct. We also set addCommitHeightRemote
		// as we've included this HTLC in our local commitment chain
		// for the remote party.
		pd = &PaymentDescriptor{
			RHash:                 wireMsg.PaymentHash,
			Timeout:               wireMsg.Expiry,
			Amount:                wireMsg.Amount,
			EntryType:             Add,
			HtlcIndex:             wireMsg.ID,
			LogIndex:              logUpdate.LogIndex,
			addCommitHeightRemote: commitHeight,
		}
		pd.OnionBlob = make([]byte, len(wireMsg.OnionBlob))
		copy(pd.OnionBlob[:], wireMsg.OnionBlob[:])

		isDustRemote := htlcIsDust(false, false, feeRate,
			wireMsg.Amount.ToSatoshis(), remoteDustLimit)
		if !isDustRemote {
			theirP2WSH, theirWitnessScript, err := genHtlcScript(
				false, false, wireMsg.Expiry, wireMsg.PaymentHash,
				remoteCommitKeys)
			if err != nil {
				return nil, err
			}
			pd.theirPkScript = theirP2WSH
			pd.theirWitnessScript = theirWitnessScript
		}

	// For HTLC's we we're offered we'll fetch the original offered HTLc
	// from the remote party's update log so we can retrieve the same
	// PaymentDescriptor that SettleHTLC would produce.
	case *lnwire.UpdateFufillHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)

		pd = &PaymentDescriptor{
			Amount:                   ogHTLC.Amount,
			RPreimage:                wireMsg.PaymentPreimage,
			LogIndex:                 logUpdate.LogIndex,
			ParentIndex:              ogHTLC.HtlcIndex,
			EntryType:                Settle,
			removeCommitHeightRemote: commitHeight,
		}

	// If we sent a failure for a prior incoming HTLC, then we'll consult
	// the update log of the remote party so we can retrieve the
	// information of the original HTLC we're failing. We also set the
	// removal height for the remote commitment.
	case *lnwire.UpdateFailHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)

		pd = &PaymentDescriptor{
			Amount:                   ogHTLC.Amount,
			RHash:                    ogHTLC.RHash,
			ParentIndex:              ogHTLC.HtlcIndex,
			LogIndex:                 logUpdate.LogIndex,
			EntryType:                Fail,
			FailReason:               wireMsg.Reason[:],
			removeCommitHeightRemote: commitHeight,
		}

	// HTLC fails due to malformed onion blobs are treated the exact same
	// way as regular HTLC fails.
	case *lnwire.UpdateFailMalformedHTLC:
		ogHTLC := remoteUpdateLog.lookupHtlc(wireMsg.ID)
		// TODO(roasbeef): err if nil?

		pd = &PaymentDescriptor{
			Amount:                   ogHTLC.Amount,
			RHash:                    ogHTLC.RHash,
			ParentIndex:              ogHTLC.HtlcIndex,
			LogIndex:                 logUpdate.LogIndex,
			EntryType:                MalformedFail,
			FailCode:                 wireMsg.FailureCode,
			ShaOnionBlob:             wireMsg.ShaOnionBlob,
			removeCommitHeightRemote: commitHeight,
		}
	}

	return pd, nil
}

// restoreCommitState will restore the local commitment chain and updateLog
// state to a consistent in-memory representation of the passed dis commitment.
// This method is to be used upon reconnection to our channel counter party.
// Once the connection has been established, we'll prepare our in memory state
// to re-sync states with the remote party, and also verify/extend new proposed
// commitment states.
func (lc *LightningChannel) restoreCommitState(
	localCommitState, remoteCommitState *channeldb.ChannelCommitment,
	localUpdateLog, remoteUpdateLog *updateLog) error {

	// In order to reconstruct the pkScripts on each of the pending HTLC
	// outputs (if any) we'll need to regenerate the current revocation for
	// this current un-revoked state as well as retrieve the current
	// revocation for the remote party.
	ourRevPreImage, err := lc.channelState.RevocationProducer.AtIndex(
		lc.currentHeight,
	)
	if err != nil {
		return err
	}
	localCommitPoint := ComputeCommitmentPoint(ourRevPreImage[:])
	remoteCommitPoint := lc.channelState.RemoteCurrentRevocation

	// With the revocation state reconstructed, we can now convert the disk
	// commitment into our in-memory commitment format, inserting it into
	// the local commitment chain.
	localCommit, err := lc.diskCommitToMemCommit(
		true, false, localCommitState, localCommitPoint,
		remoteCommitPoint,
	)
	if err != nil {
		return err
	}
	lc.localCommitChain.addCommitment(localCommit)

	walletLog.Debugf("ChannelPoint(%v), starting local commitment: %v",
		lc.channelState.FundingOutpoint, newLogClosure(func() string {
			return spew.Sdump(lc.localCommitChain.tail())
		}),
	)

	// We'll also do the same for the remote commitment chain.
	remoteCommit, err := lc.diskCommitToMemCommit(
		false, false, remoteCommitState, localCommitPoint,
		remoteCommitPoint,
	)
	if err != nil {
		return err
	}
	lc.remoteCommitChain.addCommitment(remoteCommit)

	walletLog.Debugf("ChannelPoint(%v), starting remote commitment: %v",
		lc.channelState.FundingOutpoint, newLogClosure(func() string {
			return spew.Sdump(lc.remoteCommitChain.tail())
		}),
	)

	var (
		pendingRemoteCommit     *commitment
		pendingRemoteCommitDiff *channeldb.CommitDiff
		pendingRemoteKeyChain   *commitmentKeyRing
	)

	// Next, we'll check to see if we have an un-acked commitment state we
	// extended to the remote party but which was never ACK'd.
	pendingRemoteCommitDiff, err = lc.channelState.RemoteCommitChainTip()
	if err != nil && err != channeldb.ErrNoPendingCommit {
		return nil
	}

	if pendingRemoteCommitDiff != nil {
		// If we have a pending remote commitment, then we'll also
		// reconstruct the original commitment for that state,
		// inserting it into the remote party's commitment chain. We
		// don't pass our commit point as we don't have the
		// corresponding state for the local commitment chain.
		pendingCommitPoint := lc.channelState.RemoteNextRevocation
		pendingRemoteCommit, err = lc.diskCommitToMemCommit(
			false, true, &pendingRemoteCommitDiff.Commitment,
			nil, pendingCommitPoint,
		)
		if err != nil {
			return err
		}
		lc.remoteCommitChain.addCommitment(pendingRemoteCommit)

		// We'll also re-create the set of commitment keys needed to
		// fully re-derive the state.
		pendingRemoteKeyChain = deriveCommitmentKeys(
			pendingCommitPoint, false, lc.localChanCfg,
			lc.remoteChanCfg,
		)
	}

	// Finally, with the commitment states restored, we'll now restore the
	// state logs based on the current local+remote commit, and any pending
	// remote commit that exists.
	err = lc.restoreStateLogs(localCommit, remoteCommit, pendingRemoteCommit,
		pendingRemoteCommitDiff, pendingRemoteKeyChain,
	)
	if err != nil {
		return err
	}

	return nil
}

// restoreStateLogs runs through the current locked-in HTLCs from the point of
// view of the channel and insert corresponding log entries (both local and
// remote) for each HTLC read from disk. This method is required to sync the
// in-memory state of the state machine with that read from persistent storage.
func (lc *LightningChannel) restoreStateLogs(
	localCommitment, remoteCommitment, pendingRemoteCommit *commitment,
	pendingRemoteCommitDiff *channeldb.CommitDiff,
	pendingRemoteKeys *commitmentKeyRing) error {

	// For each HTLC within the local commitment, we add it to the relevant
	// update logc based on if it's incoming vs outgoing. For any incoming
	// HTLC's, we also re-add it to the rHashMap so we can quickly look it
	// up.
	for i := range localCommitment.incomingHTLCs {
		htlc := localCommitment.incomingHTLCs[i]
		lc.remoteUpdateLog.restoreHtlc(&htlc)
	}
	for i := range localCommitment.outgoingHTLCs {
		htlc := localCommitment.outgoingHTLCs[i]
		lc.localUpdateLog.restoreHtlc(&htlc)
	}

	// We'll also do the same for the HTLC"s within the remote commitment
	// party. We also insert these HTLC's as it's possible our state has
	// diverged slightly in the case of a congruent update from both sides.
	// The restoreHtlc method will de-dup the HTLC's to handle this case.
	for i := range remoteCommitment.incomingHTLCs {
		htlc := remoteCommitment.incomingHTLCs[i]
		lc.remoteUpdateLog.restoreHtlc(&htlc)
	}
	for i := range remoteCommitment.outgoingHTLCs {
		htlc := remoteCommitment.outgoingHTLCs[i]
		lc.localUpdateLog.restoreHtlc(&htlc)
	}

	// If we didn't have a dangling (un-acked) commit for the remote party,
	// then we can exit here.
	if pendingRemoteCommit == nil {
		return nil
	}

	// If we do have a dangling commitment for the remote party, then we'll
	// also restore into the log any incoming HTLC's offered by them. Any
	// outgoing HTLC's that were initially committed in this new state will
	// be restored below.
	for i := range pendingRemoteCommit.incomingHTLCs {
		htlc := pendingRemoteCommit.incomingHTLCs[i]
		lc.remoteUpdateLog.restoreHtlc(&htlc)
	}

	// We'll also update the log counters to match the latest known
	// counters in this dangling commitment. Otherwise, our updateLog would
	// have dated counters as it was initially created using their lowest
	// unrevoked commitment.
	lc.remoteUpdateLog.logIndex = pendingRemoteCommit.theirMessageIndex
	lc.remoteUpdateLog.htlcCounter = pendingRemoteCommit.theirHtlcIndex

	pendingCommit := pendingRemoteCommitDiff.Commitment
	pendingHeight := pendingCommit.CommitHeight

	// If we did have a dangling commit, then we'll examine which updates
	// we included in that state and re-insert them into our update log.
	for _, logUpdate := range pendingRemoteCommitDiff.LogUpdates {
		// If the log update is a fee update, then it doesn't need an
		// entry within the updateLog, so we'll just apply it and move
		// on.
		if feeUpdate, ok := logUpdate.UpdateMsg.(*lnwire.UpdateFee); ok {
			newFeeRate := btcutil.Amount(feeUpdate.FeePerKw)
			lc.pendingAckFeeUpdate = &newFeeRate
			continue
		}

		payDesc, err := lc.logUpdateToPayDesc(
			&logUpdate, lc.remoteUpdateLog, pendingHeight,
			pendingCommit.FeePerKw, pendingRemoteKeys,
			lc.channelState.RemoteChanCfg.DustLimit,
		)
		if err != nil {
			return err
		}

		if payDesc.EntryType == Add {
			lc.localUpdateLog.appendHtlc(payDesc)
		} else {
			lc.localUpdateLog.appendUpdate(payDesc)
		}
	}

	return nil
}

// HtlcRetribution contains all the items necessary to seep a revoked HTLC
// transaction from a revoked commitment transaction broadcast by the remote
// party.
type HtlcRetribution struct {
	// SignDesc is a design descriptor capable of generating the necessary
	// signatures to satisfy the revocation clause of the HTLC's public key
	// script.
	SignDesc SignDescriptor

	// OutPoint is the target outpoint of this HTLC pointing to the
	// breached commitment transaction.
	OutPoint wire.OutPoint

	// IsIncoming is a boolean flag that indicates whether or not this
	// HTLC was accepted from the counterparty. A false value indicates that
	// this HTLC was offered by us. This flag is used determine the exact
	// witness type should be used to sweep the output.
	IsIncoming bool
}

// BreachRetribution contains all the data necessary to bring a channel
// counterparty to justice claiming ALL lingering funds within the channel in
// the scenario that they broadcast a revoked commitment transaction. A
// BreachRetribution is created by the closeObserver if it detects an
// uncooperative close of the channel which uses a revoked commitment
// transaction. The BreachRetribution is then sent over the ContractBreach
// channel in order to allow the subscriber of the channel to dispatch justice.
type BreachRetribution struct {
	// BreachTransaction is the transaction which breached the channel
	// contract by spending from the funding multi-sig with a revoked
	// commitment transaction.
	BreachTransaction *wire.MsgTx

	// BreachHeight records the block height confirming the breach
	// transaction, used as a height hint when registering for
	// confirmations.
	BreachHeight uint32

	// ChainHash is the chain that the contract beach was identified
	// within. This is also the resident chain of the contract (the chain
	// the contract was created on).
	ChainHash chainhash.Hash

	// RevokedStateNum is the revoked state number which was broadcast.
	RevokedStateNum uint64

	// PendingHTLCs is a slice of the HTLCs which were pending at this
	// point within the channel's history transcript.
	PendingHTLCs []channeldb.HTLC

	// LocalOutputSignDesc is a SignDescriptor which is capable of
	// generating the signature necessary to sweep the output within the
	// BreachTransaction that pays directly us.
	//
	// NOTE: A nil value indicates that the local output is considered dust
	// according to the remote party's dust limit.
	LocalOutputSignDesc *SignDescriptor

	// LocalOutpoint is the outpoint of the output paying to us (the local
	// party) within the breach transaction.
	LocalOutpoint wire.OutPoint

	// RemoteOutputSignDesc is a SignDescriptor which is capable of
	// generating the signature required to claim the funds as described
	// within the revocation clause of the remote party's commitment
	// output.
	//
	// NOTE: A nil value indicates that the local output is considered dust
	// according to the remote party's dust limit.
	RemoteOutputSignDesc *SignDescriptor

	// RemoteOutpoint is the outpoint of the output paying to the remote
	// party within the breach transaction.
	RemoteOutpoint wire.OutPoint

	// HtlcRetributions is a slice of HTLC retributions for each output
	// active HTLC output within the breached commitment transaction.
	HtlcRetributions []HtlcRetribution

	// Err is used to reliably hand-off the breach retribution to the breach
	// arbiter.
	Err chan error
}

// newBreachRetribution creates a new fully populated BreachRetribution for the
// passed channel, at a particular revoked state number, and one which targets
// the passed commitment transaction.
func newBreachRetribution(chanState *channeldb.OpenChannel, stateNum uint64,
	broadcastCommitment *wire.MsgTx,
	breachHeight uint32) (*BreachRetribution, error) {

	commitHash := broadcastCommitment.TxHash()

	// Query the on-disk revocation log for the snapshot which was recorded
	// at this particular state num.
	revokedSnapshot, err := chanState.FindPreviousState(stateNum)
	if err != nil {
		return nil, err
	}

	// With the state number broadcast known, we can now derive/restore the
	// proper revocation preimage necessary to sweep the remote party's
	// output.
	revocationPreimage, err := chanState.RevocationStore.LookUp(stateNum)
	if err != nil {
		return nil, err
	}
	commitmentSecret, commitmentPoint := btcec.PrivKeyFromBytes(btcec.S256(),
		revocationPreimage[:])

	// With the commitment point generated, we can now generate the four
	// keys we'll need to reconstruct the commitment state,
	keyRing := deriveCommitmentKeys(commitmentPoint, false,
		&chanState.LocalChanCfg, &chanState.RemoteChanCfg)

	// Next, reconstruct the scripts as they were present at this state
	// number so we can have the proper witness script to sign and include
	// within the final witness.
	remoteDelay := uint32(chanState.RemoteChanCfg.CsvDelay)
	remotePkScript, err := commitScriptToSelf(remoteDelay, keyRing.delayKey,
		keyRing.revocationKey)
	if err != nil {
		return nil, err
	}
	remoteWitnessHash, err := witnessScriptHash(remotePkScript)
	if err != nil {
		return nil, err
	}
	localPkScript, err := commitScriptUnencumbered(keyRing.noDelayKey)
	if err != nil {
		return nil, err
	}

	// In order to fully populate the breach retribution struct, we'll need
	// to find the exact index of the local+remote commitment outputs.
	localOutpoint := wire.OutPoint{
		Hash: commitHash,
	}
	remoteOutpoint := wire.OutPoint{
		Hash: commitHash,
	}
	for i, txOut := range broadcastCommitment.TxOut {
		switch {
		case bytes.Equal(txOut.PkScript, localPkScript):
			localOutpoint.Index = uint32(i)
		case bytes.Equal(txOut.PkScript, remoteWitnessHash):
			remoteOutpoint.Index = uint32(i)
		}
	}

	// Conditionally instantiate a sign descriptor for each of the
	// commitment outputs. If either is considered dust using the remote
	// party's dust limit, the respective sign descriptor will be nil.
	var (
		localSignDesc  *SignDescriptor
		remoteSignDesc *SignDescriptor
	)

	// Compute the local and remote balances in satoshis.
	localAmt := revokedSnapshot.LocalBalance.ToSatoshis()
	remoteAmt := revokedSnapshot.RemoteBalance.ToSatoshis()

	// If the local balance exceeds the remote party's dust limit,
	// instantiate the local sign descriptor.
	if localAmt >= chanState.RemoteChanCfg.DustLimit {
		localSignDesc = &SignDescriptor{
			SingleTweak:   keyRing.localCommitKeyTweak,
			PubKey:        chanState.LocalChanCfg.PaymentBasePoint,
			WitnessScript: localPkScript,
			Output: &wire.TxOut{
				PkScript: localPkScript,
				Value:    int64(localAmt),
			},
			HashType: txscript.SigHashAll,
		}
	}

	// Similarly, if the remote balance exceeds the remote party's dust
	// limit, assemble the remote sign descriptor.
	if remoteAmt >= chanState.RemoteChanCfg.DustLimit {
		remoteSignDesc = &SignDescriptor{
			PubKey:        chanState.LocalChanCfg.RevocationBasePoint,
			DoubleTweak:   commitmentSecret,
			WitnessScript: remotePkScript,
			Output: &wire.TxOut{
				PkScript: remoteWitnessHash,
				Value:    int64(remoteAmt),
			},
			HashType: txscript.SigHashAll,
		}
	}

	// With the commitment outputs located, we'll now generate all the
	// retribution structs for each of the HTLC transactions active on the
	// remote commitment transaction.
	htlcRetributions := make([]HtlcRetribution, len(revokedSnapshot.Htlcs))
	for i, htlc := range revokedSnapshot.Htlcs {
		var (
			htlcScript []byte
			err        error
		)

		// If this is an incoming HTLC, then this means that they were
		// the sender of the HTLC (relative to us). So we'll
		// re-generate the sender HTLC script.
		if htlc.Incoming {
			htlcScript, err = senderHTLCScript(
				keyRing.localHtlcKey, keyRing.remoteHtlcKey,
				keyRing.revocationKey, htlc.RHash[:],
			)
			if err != nil {
				return nil, err
			}

			// Otherwise, is this was an outgoing HTLC that we sent, then
			// from the PoV of the remote commitment state, they're the
			// receiver of this HTLC.
		} else {
			htlcScript, err = receiverHTLCScript(
				htlc.RefundTimeout, keyRing.localHtlcKey,
				keyRing.remoteHtlcKey, keyRing.revocationKey,
				htlc.RHash[:],
			)
			if err != nil {
				return nil, err
			}
		}

		htlcRetributions[i] = HtlcRetribution{
			SignDesc: SignDescriptor{
				PubKey:        chanState.LocalChanCfg.RevocationBasePoint,
				DoubleTweak:   commitmentSecret,
				WitnessScript: htlcScript,
				Output: &wire.TxOut{
					Value: int64(htlc.Amt.ToSatoshis()),
				},
				HashType: txscript.SigHashAll,
			},
			OutPoint: wire.OutPoint{
				Hash:  commitHash,
				Index: uint32(htlc.OutputIndex),
			},
			IsIncoming: htlc.Incoming,
		}
	}

	// Finally, with all the necessary data constructed, we can create the
	// BreachRetribution struct which houses all the data necessary to
	// swiftly bring justice to the cheating remote party.
	return &BreachRetribution{
		ChainHash:            chanState.ChainHash,
		BreachTransaction:    broadcastCommitment,
		BreachHeight:         breachHeight,
		RevokedStateNum:      stateNum,
		PendingHTLCs:         revokedSnapshot.Htlcs,
		LocalOutpoint:        localOutpoint,
		LocalOutputSignDesc:  localSignDesc,
		RemoteOutpoint:       remoteOutpoint,
		RemoteOutputSignDesc: remoteSignDesc,
		HtlcRetributions:     htlcRetributions,
		Err:                  make(chan error, 1),
	}, nil
}

// closeObserver is a goroutine which watches the network for any spends of the
// multi-sig funding output. A spend from the multi-sig output may occur under
// the following three scenarios: a cooperative close, a unilateral close, and
// a uncooperative contract breaching close. In the case of the last scenario a
// BreachRetribution struct is created and sent over the ContractBreach channel
// notifying subscribers that the counterparty has violated the condition of
// the channel by broadcasting a revoked prior state.
//
// NOTE: This MUST be run as a goroutine.
func (lc *LightningChannel) closeObserver(channelCloseNtfn *chainntnfs.SpendEvent) {
	defer lc.cowg.Done()

	walletLog.Infof("Close observer for ChannelPoint(%v) active",
		lc.channelState.FundingOutpoint)

	var (
		commitSpend *chainntnfs.SpendDetail
		spendHeight uint32
		ok          bool
	)

	select {
	// If the daemon is shutting down, then this notification channel will
	// be closed, so check the second read-value to avoid a false positive.
	case commitSpend, ok = <-channelCloseNtfn.Spend:
		if !ok {
			return
		}

		spendHeight = uint32(commitSpend.SpendingHeight)

	// Otherwise, we've been signalled to bail out early by the
	// caller/maintainer of this channel.
	case <-lc.observerQuit:
		// As we're exiting before the spend notification has been
		// triggered, we'll cancel the notification intent so the
		// ChainNotiifer can free up the resources.
		channelCloseNtfn.Cancel()
		return
	}

	// If we've already initiated a local cooperative or unilateral close
	// locally, then we have nothing more to do.
	lc.RLock()
	if lc.status == channelClosed || lc.status == channelDispute ||
		lc.status == channelClosing {

		// TODO(roasbeef): if seq+lockno are zero, then is cooperative closure

		lc.RUnlock()
		return
	}
	lc.RUnlock()

	// Otherwise, the remote party might have broadcast a prior revoked
	// state...!!!
	commitTxBroadcast := commitSpend.SpendingTx

	// If this is our commitment transaction, then we can exit here as we
	// don't have any further processing we need to do (we can't cheat
	// ourselves :p).
	commitmentHash := lc.channelState.LocalCommitment.CommitTx.TxHash()
	isOurCommitment := commitSpend.SpenderTxHash.IsEqual(&commitmentHash)
	if isOurCommitment {
		return
	}

	lc.Lock()
	defer lc.Unlock()

	walletLog.Warnf("Unprompted commitment broadcast for ChannelPoint(%v) "+
		"detected!", lc.channelState.FundingOutpoint)

	// Decode the state hint encoded within the commitment transaction to
	// determine if this is a revoked state or not.
	obfuscator := lc.stateHintObfuscator
	broadcastStateNum := GetStateNumHint(commitTxBroadcast, obfuscator)

	remoteStateNum := lc.channelState.RemoteCommitment.CommitHeight

	// TODO(roasbeef): track heights distinctly?

	switch {
	// If state number spending transaction matches the current latest
	// state, then they've initiated a unilateral close. So we'll trigger
	// the unilateral close signal so subscribers can clean up the state as
	// necessary.
	//
	// We'll also handle the case of the remote party broadcasting their
	// commitment transaction which is one height above ours. This case can
	// arise when we initiate a state transition, but the remote party has
	// a fail crash _after_ accepting the new state, but _before_ sending
	// their signature to us.
	case broadcastStateNum >= remoteStateNum:
		walletLog.Infof("Unilateral close of ChannelPoint(%v) "+
			"detected", lc.channelState.FundingOutpoint)

		// As we've detected that the channel has been closed,
		// immediately delete the state from disk, creating a close
		// summary for future usage by related sub-systems.
		//
		// TODO(roasbeef): include HTLC's
		//  * and time-locked balance, NEED TO???
		closeSummary := channeldb.ChannelCloseSummary{
			ChanPoint:      lc.channelState.FundingOutpoint,
			ChainHash:      lc.channelState.ChainHash,
			ClosingTXID:    *commitSpend.SpenderTxHash,
			CloseHeight:    spendHeight,
			RemotePub:      lc.channelState.IdentityPub,
			Capacity:       lc.Capacity,
			SettledBalance: lc.channelState.LocalCommitment.LocalBalance.ToSatoshis(),
			CloseType:      channeldb.ForceClose,
			IsPending:      true,
		}

		if err := lc.DeleteState(&closeSummary); err != nil {
			walletLog.Errorf("unable to delete channel state: %v", err)
			return
		}

		// TODO(roasbeef): need to handle case of if >

		// First, we'll generate the commitment point and the
		// revocation point so we can re-construct the HTLC state and
		// also our payment key.
		commitPoint := lc.channelState.RemoteCurrentRevocation
		keyRing := deriveCommitmentKeys(commitPoint, false,
			lc.localChanCfg, lc.remoteChanCfg)

		// Next, we'll obtain HTLC resolutions for all the outgoing
		// HTLC's we had on their commitment transaction.
		htlcResolutions, err := extractHtlcResolutions(
			lc.channelState.LocalCommitment.FeePerKw, false,
			lc.signer, lc.channelState.LocalCommitment.Htlcs,
			keyRing, lc.localChanCfg, lc.remoteChanCfg,
			*commitSpend.SpenderTxHash)
		if err != nil {
			walletLog.Errorf("unable to create htlc "+
				"resolutions: %v", err)
			return
		}

		// Before we can generate the proper sign descriptor, we'll
		// need to locate the output index of our non-delayed output on
		// the commitment transaction.
		selfP2WKH, err := commitScriptUnencumbered(keyRing.noDelayKey)
		if err != nil {
			walletLog.Errorf("unable to create self commit "+
				"script: %v", err)
			return
		}
		var selfPoint *wire.OutPoint
		for outputIndex, txOut := range commitTxBroadcast.TxOut {
			if bytes.Equal(txOut.PkScript, selfP2WKH) {
				selfPoint = &wire.OutPoint{
					Hash:  *commitSpend.SpenderTxHash,
					Index: uint32(outputIndex),
				}
				break
			}
		}

		// With the HTLC's taken care of, we'll generate the sign
		// descriptor necessary to sweep our commitment output, but
		// only if we had a non-trimmed balance.
		var selfSignDesc *SignDescriptor
		if selfPoint != nil {
			localPayBase := lc.localChanCfg.PaymentBasePoint
			localBalance := lc.channelState.LocalCommitment.LocalBalance.ToSatoshis()
			selfSignDesc = &SignDescriptor{
				PubKey:        localPayBase,
				SingleTweak:   keyRing.localCommitKeyTweak,
				WitnessScript: selfP2WKH,
				Output: &wire.TxOut{
					Value:    int64(localBalance),
					PkScript: selfP2WKH,
				},
				HashType: txscript.SigHashAll,
			}
		}

		// We'll also send all the details necessary to re-claim funds
		// that are suspended within any contracts.
		unilateralCloseSummary := &UnilateralCloseSummary{
			SpendDetail:         commitSpend,
			ChannelCloseSummary: closeSummary,
			SelfOutPoint:        selfPoint,
			SelfOutputSignDesc:  selfSignDesc,
			MaturityDelay:       uint32(lc.remoteChanCfg.CsvDelay),
			HtlcResolutions:     htlcResolutions,
		}

		// TODO(roasbeef): send msg before writing to disk
		//  * need to ensure proper fault tolerance in all cases
		//  * get ACK from the consumer of the ntfn before writing to disk?
		//  * no harm in repeated ntfns: at least once semantics

		// Notify any subscribers that we've detected a unilateral
		// commitment transaction broadcast.
		close(lc.UnilateralCloseSignal)

		select {
		case lc.UnilateralClose <- unilateralCloseSummary:
		case <-lc.observerQuit:
			walletLog.Errorf("channel shutting down")
			return
		}

	// If the state number broadcast is lower than the remote node's
	// current un-revoked height, then THEY'RE ATTEMPTING TO VIOLATE THE
	// CONTRACT LAID OUT WITHIN THE PAYMENT CHANNEL.  Therefore we close
	// the signal indicating a revoked broadcast to allow subscribers to
	// swiftly dispatch justice!!!
	case broadcastStateNum < remoteStateNum:
		walletLog.Warnf("Remote peer has breached the channel "+
			"contract for ChannelPoint(%v). Revoked state #%v was "+
			"broadcast!!!", lc.channelState.FundingOutpoint,
			remoteStateNum)

		if err := lc.channelState.MarkBorked(); err != nil {
			walletLog.Errorf("unable to mark channel as borked: %v",
				err)
			return
		}

		// Create a new reach retribution struct which contains all the
		// data needed to swiftly bring the cheating peer to justice.
		retribution, err := newBreachRetribution(lc.channelState,
			broadcastStateNum, commitTxBroadcast, spendHeight)
		if err != nil {
			walletLog.Errorf("unable to create breach retribution: %v", err)
			return
		}

		walletLog.Debugf("Punishment breach retribution created: %v",
			spew.Sdump(retribution))

		// Finally, send the retribution struct over the contract beach
		// channel to allow the observer the use the breach retribution
		// to sweep ALL funds.
		select {
		case lc.ContractBreach <- retribution:
		case <-lc.observerQuit:
			walletLog.Errorf("channel shutting down")
			return
		}

		// Wait for the breach arbiter to ACK the handoff before marking
		// the channel as pending force closed in channeldb.
		select {
		case err := <-retribution.Err:
			// Bail if the handoff failed.
			if err != nil {
				walletLog.Errorf("unable to handoff "+
					"retribution info: %v", err)
				return
			}

		case <-lc.observerQuit:
			walletLog.Errorf("channel shutting down")
			return
		}

		// At this point, we've successfully received an ack for the
		// breach close. We now construct and persist  the close
		// summary, marking the channel as pending force closed.
		settledBalance := lc.channelState.LocalCommitment.
			LocalBalance.ToSatoshis()
		closeSummary := channeldb.ChannelCloseSummary{
			ChanPoint:      lc.channelState.FundingOutpoint,
			ChainHash:      lc.channelState.ChainHash,
			ClosingTXID:    *commitSpend.SpenderTxHash,
			CloseHeight:    spendHeight,
			RemotePub:      lc.channelState.IdentityPub,
			Capacity:       lc.Capacity,
			SettledBalance: settledBalance,
			CloseType:      channeldb.BreachClose,
			IsPending:      true,
		}

		err = lc.DeleteState(&closeSummary)
		if err != nil {
			walletLog.Errorf("unable to delete channel state: %v",
				err)
			return
		}

		walletLog.Infof("Breached channel=%v marked pending-closed",
			lc.channelState.FundingOutpoint)
	}
}

// htlcTimeoutFee returns the fee in satoshis required for an HTLC timeout
// transaction based on the current fee rate.
func htlcTimeoutFee(feePerKw btcutil.Amount) btcutil.Amount {
	return (feePerKw * HtlcTimeoutWeight) / 1000
}

// htlcSuccessFee returns the fee in satoshis required for an HTLC success
// transaction based on the current fee rate.
func htlcSuccessFee(feePerKw btcutil.Amount) btcutil.Amount {
	return (feePerKw * HtlcSuccessWeight) / 1000
}

// htlcIsDust determines if an HTLC output is dust or not depending on two
// bits: if the HTLC is incoming and if the HTLC will be placed on our
// commitment transaction, or theirs. These two pieces of information are
// require as we currently used second-level HTLC transactions ass off-chain
// covenants. Depending on the two bits, we'll either be using a timeout or
// success transaction which have different weights.
func htlcIsDust(incoming, ourCommit bool,
	feePerKw, htlcAmt, dustLimit btcutil.Amount) bool {

	// First we'll determine the fee required for this HTLC based on if this is
	// an incoming HTLC or not, and also on whose commitment transaction it
	// will be placed on.
	var htlcFee btcutil.Amount
	switch {

	// If this is an incoming HTLC on our commitment transaction, then the
	// second-level transaction will be a success transaction.
	case incoming && ourCommit:
		htlcFee = htlcSuccessFee(feePerKw)

	// If this is an incoming HTLC on their commitment transaction, then
	// we'll be using a second-level timeout transaction as they've added
	// this HTLC.
	case incoming && !ourCommit:
		htlcFee = htlcTimeoutFee(feePerKw)

	// If this is an outgoing HTLC on our commitment transaction, then
	// we'll be using a timeout transaction as we're the sender of the
	// HTLC.
	case !incoming && ourCommit:
		htlcFee = htlcTimeoutFee(feePerKw)

	// If this is an outgoing HTLC on their commitment transaction, then
	// we'll be using an HTLC success transaction as they're the receiver
	// of this HTLC.
	case !incoming && !ourCommit:
		htlcFee = htlcSuccessFee(feePerKw)
	}

	return (htlcAmt - htlcFee) < dustLimit
}

// htlcView represents the "active" HTLCs at a particular point within the
// history of the HTLC update log.
type htlcView struct {
	ourUpdates   []*PaymentDescriptor
	theirUpdates []*PaymentDescriptor
}

// fetchHTLCView returns all the candidate HTLC updates which should be
// considered for inclusion within a commitment based on the passed HTLC log
// indexes.
func (lc *LightningChannel) fetchHTLCView(theirLogIndex, ourLogIndex uint64) *htlcView {
	var ourHTLCs []*PaymentDescriptor
	for e := lc.localUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		// This HTLC is active from this point-of-view iff the log
		// index of the state update is below the specified index in
		// our update log.
		if htlc.LogIndex < ourLogIndex {
			ourHTLCs = append(ourHTLCs, htlc)
		}
	}

	var theirHTLCs []*PaymentDescriptor
	for e := lc.remoteUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		// If this is an incoming HTLC, then it is only active from
		// this point-of-view if the index of the HTLC addition in
		// their log is below the specified view index.
		if htlc.LogIndex < theirLogIndex {
			theirHTLCs = append(theirHTLCs, htlc)
		}
	}

	return &htlcView{
		ourUpdates:   ourHTLCs,
		theirUpdates: theirHTLCs,
	}
}

// fetchCommitmentView returns a populated commitment which expresses the state
// of the channel from the point of view of a local or remote chain, evaluating
// the HTLC log up to the passed indexes. This function is used to construct
// both local and remote commitment transactions in order to sign or verify new
// commitment updates. A fully populated commitment is returned which reflects
// the proper balances for both sides at this point in the commitment chain.
func (lc *LightningChannel) fetchCommitmentView(remoteChain bool,
	ourLogIndex, ourHtlcIndex, theirLogIndex, theirHtlcIndex uint64,
	keyRing *commitmentKeyRing) (*commitment, error) {

	commitChain := lc.localCommitChain
	if remoteChain {
		commitChain = lc.remoteCommitChain
	}

	ourBalance := commitChain.tip().ourBalance
	theirBalance := commitChain.tip().theirBalance

	// Add the fee from the previous commitment state back to the
	// initiator's balance, so that the fee can be recalculated and
	// re-applied in case fee estimation parameters have changed or the
	// number of outstanding HTLCs has changed.
	if lc.channelState.IsInitiator {
		ourBalance += lnwire.NewMSatFromSatoshis(commitChain.tip().fee)
	} else if !lc.channelState.IsInitiator {
		theirBalance += lnwire.NewMSatFromSatoshis(commitChain.tip().fee)
	}

	nextHeight := commitChain.tip().height + 1

	// Run through all the HTLCs that will be covered by this transaction
	// in order to update their commitment addition height, and to adjust
	// the balances on the commitment transaction accordingly.
	htlcView := lc.fetchHTLCView(theirLogIndex, ourLogIndex)
	filteredHTLCView := lc.evaluateHTLCView(htlcView, &ourBalance,
		&theirBalance, nextHeight, remoteChain)

	// Initiate feePerKw to the last committed fee for this chain as we'll
	// need this to determine which HTLC's are dust, and also the final fee
	// rate.
	feePerKw := commitChain.tail().feePerKw

	// Check if any fee updates have taken place since that last
	// commitment.
	if lc.channelState.IsInitiator {
		switch {
		// We've sent an update_fee message since our last commitment,
		// and now are now creating a commitment that reflects the new
		// fee update.
		case remoteChain && lc.pendingFeeUpdate != nil:
			feePerKw = *lc.pendingFeeUpdate

		// We've created a new commitment for the remote chain that
		// includes a fee update, and have not received a commitment
		// after the fee update has been ACKed.
		case !remoteChain && lc.pendingAckFeeUpdate != nil:
			feePerKw = *lc.pendingAckFeeUpdate
		}
	} else {
		switch {
		// We've received a fee update since the last local commitment,
		// so we'll include the fee update in the current view.
		case !remoteChain && lc.pendingFeeUpdate != nil:
			feePerKw = *lc.pendingFeeUpdate

		// Earlier we received a commitment that signed an earlier fee
		// update, and now we must ACK that update.
		case remoteChain && lc.pendingAckFeeUpdate != nil:
			feePerKw = *lc.pendingAckFeeUpdate
		}
	}

	// Determine how many current HTLCs are over the dust limit, and should
	// be counted for the purpose of fee calculation.
	var dustLimit btcutil.Amount
	if remoteChain {
		dustLimit = lc.remoteChanCfg.DustLimit
	} else {
		dustLimit = lc.localChanCfg.DustLimit
	}

	c := &commitment{
		ourBalance:        ourBalance,
		theirBalance:      theirBalance,
		ourMessageIndex:   ourLogIndex,
		ourHtlcIndex:      ourHtlcIndex,
		theirMessageIndex: theirLogIndex,
		theirHtlcIndex:    theirHtlcIndex,
		height:            nextHeight,
		feePerKw:          feePerKw,
		dustLimit:         dustLimit,
		isOurs:            !remoteChain,
	}

	// Actually generate unsigned commitment transaction for this view.
	if err := lc.createCommitmentTx(c, filteredHTLCView, keyRing); err != nil {
		return nil, err
	}

	// In order to ensure _none_ of the HTLC's associated with this new
	// commitment are mutated, we'll manually copy over each HTLC to its
	// respective slice.
	c.outgoingHTLCs = make([]PaymentDescriptor, len(filteredHTLCView.ourUpdates))
	for i, htlc := range filteredHTLCView.ourUpdates {
		c.outgoingHTLCs[i] = *htlc
	}
	c.incomingHTLCs = make([]PaymentDescriptor, len(filteredHTLCView.theirUpdates))
	for i, htlc := range filteredHTLCView.theirUpdates {
		c.incomingHTLCs[i] = *htlc
	}

	// Finally, we'll populate all the HTLC indexes so we can track the
	// locations of each HTLC in the commitment state.
	if err := c.populateHtlcIndexes(); err != nil {
		return nil, err
	}

	return c, nil
}

// createCommitmentTx generates the unsigned commitment transaction for a
// commitment view and assigns to txn field.
func (lc *LightningChannel) createCommitmentTx(c *commitment,
	filteredHTLCView *htlcView, keyRing *commitmentKeyRing) error {

	ourBalance := c.ourBalance
	theirBalance := c.theirBalance

	numHTLCs := int64(0)
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}

	// Next, we'll calculate the fee for the commitment transaction based
	// on its total weight. Once we have the total weight, we'll multiply
	// by the current fee-per-kw, then divide by 1000 to get the proper
	// fee.
	totalCommitWeight := CommitWeight + (HtlcWeight * numHTLCs)

	// With the weight known, we can now calculate the commitment fee,
	// ensuring that we account for any dust outputs trimmed above.
	commitFee := btcutil.Amount((int64(c.feePerKw) * totalCommitWeight) / 1000)

	// Currently, within the protocol, the initiator always pays the fees.
	// So we'll subtract the fee amount from the balance of the current
	// initiator.
	if lc.channelState.IsInitiator {
		ourBalance -= lnwire.NewMSatFromSatoshis(commitFee)
	} else if !lc.channelState.IsInitiator {
		theirBalance -= lnwire.NewMSatFromSatoshis(commitFee)
	}

	var (
		delay                      uint32
		delayBalance, p2wkhBalance btcutil.Amount
	)
	if c.isOurs {
		delay = uint32(lc.localChanCfg.CsvDelay)
		delayBalance = ourBalance.ToSatoshis()
		p2wkhBalance = theirBalance.ToSatoshis()
	} else {
		delay = uint32(lc.remoteChanCfg.CsvDelay)
		delayBalance = theirBalance.ToSatoshis()
		p2wkhBalance = ourBalance.ToSatoshis()
	}

	// Generate a new commitment transaction with all the latest
	// unsettled/un-timed out HTLCs.
	commitTx, err := CreateCommitTx(lc.fundingTxIn, keyRing, delay,
		delayBalance, p2wkhBalance, c.dustLimit)
	if err != nil {
		return err
	}

	// We'll now add all the HTLC outputs to the commitment transaction.
	// Each output includes an off-chain 2-of-2 covenant clause, so we'll
	// need the objective local/remote keys for this particular commitment
	// as well.
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := lc.addHTLC(commitTx, c.isOurs, false, htlc, keyRing)
		if err != nil {
			return err
		}
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := lc.addHTLC(commitTx, c.isOurs, true, htlc, keyRing)
		if err != nil {
			return err
		}
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	err = SetStateNumHint(commitTx, c.height, lc.stateHintObfuscator)
	if err != nil {
		return err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(commitTx)

	c.txn = commitTx
	c.fee = commitFee
	c.ourBalance = ourBalance
	c.theirBalance = theirBalance
	return nil
}

// evaluateHTLCView processes all update entries in both HTLC update logs,
// producing a final view which is the result of properly applying all adds,
// settles, and timeouts found in both logs. The resulting view returned
// reflects the current state of HTLCs within the remote or local commitment
// chain.
func (lc *LightningChannel) evaluateHTLCView(view *htlcView, ourBalance,
	theirBalance *lnwire.MilliSatoshi, nextHeight uint64, remoteChain bool) *htlcView {

	newView := &htlcView{}

	// We use two maps, one for the local log and one for the remote log to
	// keep track of which entries we need to skip when creating the final
	// htlc view. We skip an entry whenever we find a settle or a timeout
	// modifying an entry.
	skipUs := make(map[uint64]struct{})
	skipThem := make(map[uint64]struct{})

	// First we run through non-add entries in both logs, populating the
	// skip sets and mutating the current chain state (crediting balances,
	// etc) to reflect the settle/timeout entry encountered.
	for _, entry := range view.ourUpdates {
		if entry.EntryType == Add {
			continue
		}

		// If we're settling an inbound HTLC, and it hasn't been
		// processed yet, then increment our state tracking the total
		// number of satoshis we've received within the channel.
		if entry.EntryType == Settle && !remoteChain &&
			entry.removeCommitHeightLocal == 0 {
			lc.channelState.TotalMSatReceived += entry.Amount
		}

		addEntry := lc.remoteUpdateLog.lookupHtlc(entry.ParentIndex)

		skipThem[addEntry.HtlcIndex] = struct{}{}
		processRemoveEntry(entry, ourBalance, theirBalance,
			nextHeight, remoteChain, true)
	}
	for _, entry := range view.theirUpdates {
		if entry.EntryType == Add {
			continue
		}

		// If the remote party is settling one of our outbound HTLC's,
		// and it hasn't been processed, yet, the increment our state
		// tracking the total number of satoshis we've sent within the
		// channel.
		if entry.EntryType == Settle && !remoteChain &&
			entry.removeCommitHeightLocal == 0 {
			lc.channelState.TotalMSatSent += entry.Amount
		}

		addEntry := lc.localUpdateLog.lookupHtlc(entry.ParentIndex)

		skipUs[addEntry.HtlcIndex] = struct{}{}
		processRemoveEntry(entry, ourBalance, theirBalance,
			nextHeight, remoteChain, false)
	}

	// Next we take a second pass through all the log entries, skipping any
	// settled HTLCs, and debiting the chain state balance due to any newly
	// added HTLCs.
	for _, entry := range view.ourUpdates {
		isAdd := entry.EntryType == Add
		if _, ok := skipUs[entry.HtlcIndex]; !isAdd || ok {
			continue
		}

		processAddEntry(entry, ourBalance, theirBalance, nextHeight,
			remoteChain, false)
		newView.ourUpdates = append(newView.ourUpdates, entry)
	}
	for _, entry := range view.theirUpdates {
		isAdd := entry.EntryType == Add
		if _, ok := skipThem[entry.HtlcIndex]; !isAdd || ok {
			continue
		}

		processAddEntry(entry, ourBalance, theirBalance, nextHeight,
			remoteChain, true)
		newView.theirUpdates = append(newView.theirUpdates, entry)
	}

	return newView
}

// processAddEntry evaluates the effect of an add entry within the HTLC log.
// If the HTLC hasn't yet been committed in either chain, then the height it
// was committed is updated. Keeping track of this inclusion height allows us to
// later compact the log once the change is fully committed in both chains.
func processAddEntry(htlc *PaymentDescriptor, ourBalance, theirBalance *lnwire.MilliSatoshi,
	nextHeight uint64, remoteChain bool, isIncoming bool) {

	// If we're evaluating this entry for the remote chain (to create/view
	// a new commitment), then we'll may be updating the height this entry
	// was added to the chain. Otherwise, we may be updating the entry's
	// height w.r.t the local chain.
	var addHeight *uint64
	if remoteChain {
		addHeight = &htlc.addCommitHeightRemote
	} else {
		addHeight = &htlc.addCommitHeightLocal
	}

	if *addHeight != 0 {
		return
	}

	if isIncoming {
		// If this is a new incoming (un-committed) HTLC, then we need
		// to update their balance accordingly by subtracting the
		// amount of the HTLC that are funds pending.
		*theirBalance -= htlc.Amount
	} else {
		// Similarly, we need to debit our balance if this is an out
		// going HTLC to reflect the pending balance.
		*ourBalance -= htlc.Amount
	}

	*addHeight = nextHeight
}

// processRemoveEntry processes a log entry which settles or times out a
// previously added HTLC. If the removal entry has already been processed, it
// is skipped.
func processRemoveEntry(htlc *PaymentDescriptor, ourBalance,
	theirBalance *lnwire.MilliSatoshi, nextHeight uint64,
	remoteChain bool, isIncoming bool) {

	var removeHeight *uint64
	if remoteChain {
		removeHeight = &htlc.removeCommitHeightRemote
	} else {
		removeHeight = &htlc.removeCommitHeightLocal
	}

	// Ignore any removal entries which have already been processed.
	if *removeHeight != 0 {
		return
	}

	switch {
	// If an incoming HTLC is being settled, then this means that we've
	// received the preimage either from another subsystem, or the
	// upstream peer in the route. Therefore, we increase our balance by
	// the HTLC amount.
	case isIncoming && htlc.EntryType == Settle:
		*ourBalance += htlc.Amount

	// Otherwise, this HTLC is being failed out, therefore the value of the
	// HTLC should return to the remote party.
	case isIncoming && (htlc.EntryType == Fail || htlc.EntryType == MalformedFail):
		*theirBalance += htlc.Amount

	// If an outgoing HTLC is being settled, then this means that the
	// downstream party resented the preimage or learned of it via a
	// downstream peer. In either case, we credit their settled value with
	// the value of the HTLC.
	case !isIncoming && htlc.EntryType == Settle:
		*theirBalance += htlc.Amount

	// Otherwise, one of our outgoing HTLC's has timed out, so the value of
	// the HTLC should be returned to our settled balance.
	case !isIncoming && (htlc.EntryType == Fail || htlc.EntryType == MalformedFail):
		*ourBalance += htlc.Amount
	}

	*removeHeight = nextHeight
}

// generateRemoteHtlcSigJobs generates a series of HTLC signature jobs for the
// sig pool, along with a channel that if closed, will cancel any jobs after
// they have been submitted to the sigPool. This method is to be used when
// generating a new commitment for the remote party. The jobs generated by the
// signature can be submitted to the sigPool to generate all the signatures
// asynchronously and in parallel.
func genRemoteHtlcSigJobs(keyRing *commitmentKeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	remoteCommitView *commitment) ([]signJob, chan struct{}, error) {

	txHash := remoteCommitView.txn.TxHash()
	dustLimit := localChanCfg.DustLimit
	feePerKw := remoteCommitView.feePerKw

	// With the keys generated, we'll make a slice with enough capacity to
	// hold potentially all the HTLC's. The actual slice may be a bit
	// smaller (than its total capacity) an some HTLC's may be dust.
	numSigs := (len(remoteCommitView.incomingHTLCs) +
		len(remoteCommitView.outgoingHTLCs))
	sigBatch := make([]signJob, 0, numSigs)

	var err error
	cancelChan := make(chan struct{})

	// For ech outgoing and incoming HTLC, if the HTLC isn't considered a
	// dust output after taking into account second-level HTLC fees, then a
	// sigJob will be generated and appended to the current batch.
	for _, htlc := range remoteCommitView.incomingHTLCs {
		if htlcIsDust(true, false, feePerKw, htlc.Amount.ToSatoshis(),
			dustLimit) {
			continue
		}

		// If the HTLC isn't dust, then we'll create an empty sign job
		// to add to the batch momentarily.
		sigJob := signJob{}
		sigJob.cancel = cancelChan
		sigJob.resp = make(chan signJobResp, 1)

		// As this is an incoming HTLC and we're sinning the commitment
		// transaction of the remote node, we'll need to generate an
		// HTLC timeout transaction for them. The output of the timeout
		// transaction needs to account for fees, so we'll compute the
		// required fee and output now.
		htlcFee := htlcTimeoutFee(feePerKw)
		outputAmt := htlc.Amount.ToSatoshis() - htlcFee

		// With the fee calculate, we can properly create the HTLC
		// timeout transaction using the HTLC amount minus the fee.
		op := wire.OutPoint{
			Hash:  txHash,
			Index: uint32(htlc.remoteOutputIndex),
		}
		sigJob.tx, err = createHtlcTimeoutTx(op, outputAmt,
			htlc.Timeout, uint32(remoteChanCfg.CsvDelay),
			keyRing.revocationKey, keyRing.delayKey)
		if err != nil {
			return nil, nil, err
		}

		// Finally, we'll generate a sign descriptor to generate a
		// signature to give to the remote party for this commitment
		// transaction. Note we use the raw HTLC amount.
		sigJob.signDesc = SignDescriptor{
			PubKey:        localChanCfg.HtlcBasePoint,
			SingleTweak:   keyRing.localHtlcKeyTweak,
			WitnessScript: htlc.theirWitnessScript,
			Output: &wire.TxOut{
				Value: int64(htlc.Amount.ToSatoshis()),
			},
			HashType:   txscript.SigHashAll,
			SigHashes:  txscript.NewTxSigHashes(sigJob.tx),
			InputIndex: 0,
		}
		sigJob.outputIndex = htlc.remoteOutputIndex

		sigBatch = append(sigBatch, sigJob)
	}
	for _, htlc := range remoteCommitView.outgoingHTLCs {
		if htlcIsDust(false, false, feePerKw, htlc.Amount.ToSatoshis(),
			dustLimit) {
			continue
		}

		sigJob := signJob{}
		sigJob.cancel = cancelChan
		sigJob.resp = make(chan signJobResp, 1)

		// As this is an outgoing HTLC and we're signing the commitment
		// transaction of the remote node, we'll need to generate an
		// HTLC success transaction for them. The output of the timeout
		// transaction needs to account for fees, so we'll compute the
		// required fee and output now.
		htlcFee := htlcSuccessFee(feePerKw)
		outputAmt := htlc.Amount.ToSatoshis() - htlcFee

		// With the proper output amount calculated, we can now
		// generate the success transaction using the remote party's
		// CSV delay.
		op := wire.OutPoint{
			Hash:  txHash,
			Index: uint32(htlc.remoteOutputIndex),
		}
		sigJob.tx, err = createHtlcSuccessTx(op, outputAmt,
			uint32(remoteChanCfg.CsvDelay), keyRing.revocationKey,
			keyRing.delayKey)
		if err != nil {
			return nil, nil, err
		}

		// Finally, we'll generate a sign descriptor to generate a
		// signature to give to the remote party for this commitment
		// transaction. Note we use the raw HTLC amount.
		sigJob.signDesc = SignDescriptor{
			PubKey:        localChanCfg.HtlcBasePoint,
			SingleTweak:   keyRing.localHtlcKeyTweak,
			WitnessScript: htlc.theirWitnessScript,
			Output: &wire.TxOut{
				Value: int64(htlc.Amount.ToSatoshis()),
			},
			HashType:   txscript.SigHashAll,
			SigHashes:  txscript.NewTxSigHashes(sigJob.tx),
			InputIndex: 0,
		}
		sigJob.outputIndex = htlc.remoteOutputIndex

		sigBatch = append(sigBatch, sigJob)
	}

	return sigBatch, cancelChan, nil
}

// createCommitDiff will create a commit diff given a new pending commitment
// for the remote party and the necessary signatures for the remote party to
// validate this new state. This function is called right before sending the
// new commitment to the remote party. The commit diff returned contains all
// information necessary for retransmission.
func (lc *LightningChannel) createCommitDiff(
	newCommit *commitment, commitSig *btcec.Signature,
	htlcSigs []*btcec.Signature) (*channeldb.CommitDiff, error) {

	// First, we need to convert the funding outpoint into the ID that's
	// used on the wire to identify this channel. We'll use this shortly
	// when recording the exact CommitSig message that we'll be sending
	// out.
	chanID := lnwire.NewChanIDFromOutPoint(&lc.channelState.FundingOutpoint)

	// If we have a fee update that we're waiting on an ACK of, then we'll
	// create an entry so this is properly retransmitted. Note that we can
	// only send an UpdateFee message if we're the initiator of the
	// channel.
	var logUpdates []channeldb.LogUpdate
	if lc.channelState.IsInitiator && lc.pendingFeeUpdate != nil {
		logUpdates = append(logUpdates, channeldb.LogUpdate{
			UpdateMsg: &lnwire.UpdateFee{
				ChanID:   chanID,
				FeePerKw: uint32(*lc.pendingFeeUpdate),
			},
		})
	}

	// We'll now run through our local update log to locate the items which
	// were only just committed within this pending state. This will be the
	// set of items we need to retransmit if we reconnect and find that
	// they didn't process this new state fully.
	for e := lc.localUpdateLog.Front(); e != nil; e = e.Next() {
		pd := e.Value.(*PaymentDescriptor)

		// If this entry wasn't committed at the exact height of this
		// remote commitment, then we'll skip it as it was already
		// lingering in the log.
		if pd.addCommitHeightRemote != newCommit.height &&
			pd.removeCommitHeightRemote != newCommit.height {

			continue
		}

		// Knowing that this update is a part of this new commitment,
		// we'll create a log update and not it's index in the log so
		// we can later restore it properly if a restart occurs.
		logUpdate := channeldb.LogUpdate{
			LogIndex: pd.LogIndex,
		}

		// We'll map the type of the PaymentDescriptor to one of the
		// four messages that it corresponds to. With this set of
		// messages obtained, we can simply read from disk and re-send
		// them in the case of a needed channel sync.
		switch pd.EntryType {
		case Add:
			htlc := &lnwire.UpdateAddHTLC{
				ChanID:      chanID,
				ID:          pd.HtlcIndex,
				Amount:      pd.Amount,
				Expiry:      pd.Timeout,
				PaymentHash: pd.RHash,
			}
			copy(htlc.OnionBlob[:], pd.OnionBlob)
			logUpdate.UpdateMsg = htlc

		case Settle:
			logUpdate.UpdateMsg = &lnwire.UpdateFufillHTLC{
				ChanID:          chanID,
				ID:              pd.ParentIndex,
				PaymentPreimage: pd.RPreimage,
			}

		case Fail:
			logUpdate.UpdateMsg = &lnwire.UpdateFailHTLC{
				ChanID: chanID,
				ID:     pd.ParentIndex,
				Reason: pd.FailReason,
			}

		case MalformedFail:
			logUpdate.UpdateMsg = &lnwire.UpdateFailMalformedHTLC{
				ChanID:       chanID,
				ID:           pd.ParentIndex,
				ShaOnionBlob: pd.ShaOnionBlob,
				FailureCode:  pd.FailCode,
			}
		}

		logUpdates = append(logUpdates, logUpdate)
	}

	// With the set of log updates mapped into wire messages, we'll now
	// convert the in-memory commit into a format suitable for writing to
	// disk.
	diskCommit := newCommit.toDiskCommit(false)

	return &channeldb.CommitDiff{
		Commitment: *diskCommit,
		CommitSig: &lnwire.CommitSig{
			ChanID: lnwire.NewChanIDFromOutPoint(
				&lc.channelState.FundingOutpoint,
			),
			CommitSig: commitSig,
			HtlcSigs:  htlcSigs,
		},
		LogUpdates: logUpdates,
	}, nil
}

// SignNextCommitment signs a new commitment which includes any previous
// unsettled HTLCs, any new HTLCs, and any modifications to prior HTLCs
// committed in previous commitment updates. Signing a new commitment
// decrements the available revocation window by 1. After a successful method
// call, the remote party's commitment chain is extended by a new commitment
// which includes all updates to the HTLC log prior to this method invocation.
// The first return parameter is the signature for the commitment transaction
// itself, while the second parameter is a slice of all HTLC signatures (if
// any). The HTLC signatures are sorted according to the BIP 69 order of the
// HTLC's on the commitment transaction.
func (lc *LightningChannel) SignNextCommitment() (*btcec.Signature, []*btcec.Signature, error) {
	lc.Lock()
	defer lc.Unlock()

	// If we're awaiting for an ACK to a commitment signature, or if we
	// don't yet have the initial next revocation point of the remote
	// party, then we're unable to create new states. Each time we create a
	// new state, we consume a prior revocation point.
	commitPoint := lc.channelState.RemoteNextRevocation
	if lc.remoteCommitChain.hasUnackedCommitment() || commitPoint == nil {

		return nil, nil, ErrNoWindow
	}

	// Determine the last update on the remote log that has been locked in.
	remoteACKedIndex := lc.localCommitChain.tail().theirMessageIndex
	remoteHtlcIndex := lc.localCommitChain.tail().theirHtlcIndex

	// Before we extend this new commitment to the remote commitment chain,
	// ensure that we aren't violating any of the constraints the remote
	// party set up when we initially set up the channel. If we are, then
	// we'll abort this state transition.
	err := lc.validateCommitmentSanity(remoteACKedIndex,
		lc.localUpdateLog.logIndex, false, true, true)
	if err != nil {
		return nil, nil, err
	}

	// Grab the next commitment point for the remote party. This will be
	// used within fetchCommitmentView to derive all the keys necessary to
	// construct the commitment state.
	keyRing := deriveCommitmentKeys(commitPoint, false, lc.localChanCfg,
		lc.remoteChanCfg)

	// Create a new commitment view which will calculate the evaluated
	// state of the remote node's new commitment including our latest added
	// HTLCs. The view includes the latest balances for both sides on the
	// remote node's chain, and also update the addition height of any new
	// HTLC log entries. When we creating a new remote view, we include
	// _all_ of our changes (pending or committed) but only the remote
	// node's changes up to the last change we've ACK'd.
	newCommitView, err := lc.fetchCommitmentView(
		true, lc.localUpdateLog.logIndex, lc.localUpdateLog.htlcCounter,
		remoteACKedIndex, remoteHtlcIndex, keyRing,
	)
	if err != nil {
		return nil, nil, err
	}

	walletLog.Tracef("ChannelPoint(%v): extending remote chain to height %v, "+
		"local_log=%v, remote_log=%v",
		lc.channelState.FundingOutpoint, newCommitView.height,
		lc.localUpdateLog.logIndex, remoteACKedIndex)

	walletLog.Tracef("ChannelPoint(%v): remote chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v",
		lc.channelState.FundingOutpoint, newCommitView.ourBalance,
		newCommitView.theirBalance,
		newLogClosure(func() string {
			return spew.Sdump(newCommitView.txn)
		}),
	)

	// With the commitment view constructed, if there are any HTLC's, we'll
	// need to generate signatures of each of them for the remote party's
	// commitment state. We do so in two phases: first we generate and
	// submit the set of signature jobs to the worker pool.
	sigBatch, cancelChan, err := genRemoteHtlcSigJobs(keyRing,
		lc.localChanCfg, lc.remoteChanCfg, newCommitView,
	)
	if err != nil {
		return nil, nil, err
	}
	lc.sigPool.SubmitSignBatch(sigBatch)

	// While the jobs are being carried out, we'll Sign their version of
	// the new commitment transaction while we're waiting for the rest of
	// the HTLC signatures to be processed.
	lc.signDesc.SigHashes = txscript.NewTxSigHashes(newCommitView.txn)
	rawSig, err := lc.signer.SignOutputRaw(newCommitView.txn, lc.signDesc)
	if err != nil {
		close(cancelChan)
		return nil, nil, err
	}
	sig, err := btcec.ParseSignature(rawSig, btcec.S256())
	if err != nil {
		close(cancelChan)
		return nil, nil, err
	}

	// We'll need to send over the signatures to the remote party in the
	// order as they appear on the commitment transaction after BIP 69
	// sorting.
	sort.Slice(sigBatch, func(i, j int) bool {
		return sigBatch[i].outputIndex < sigBatch[j].outputIndex
	})

	// With the jobs sorted, we'll now iterate through all the responses to
	// gather each of the signatures in order.
	htlcSigs := make([]*btcec.Signature, 0, len(sigBatch))
	for _, htlcSigJob := range sigBatch {
		select {
		case jobResp := <-htlcSigJob.resp:
			// If an error occurred, then we'll cancel any other
			// active jobs.
			if jobResp.err != nil {
				close(cancelChan)
				return nil, nil, err
			}

			htlcSigs = append(htlcSigs, jobResp.sig)
		case <-lc.quit:
			return nil, nil, fmt.Errorf("channel shutting down")
		}
	}

	// As we're about to proposer a new commitment state for the remote
	// party, we'll write this pending state to disk before we exit, so we
	// can retransmit it if necessary.
	commitDiff, err := lc.createCommitDiff(newCommitView, sig, htlcSigs)
	if err != nil {
		return nil, nil, err
	}
	if lc.channelState.AppendRemoteCommitChain(commitDiff); err != nil {
		return nil, nil, err
	}

	// TODO(roasbeef): check that one eclair bug
	//  * need to retransmit on first state still?
	//  * after initial reconnect

	// Extend the remote commitment chain by one with the addition of our
	// latest commitment update.
	lc.remoteCommitChain.addCommitment(newCommitView)

	// If we are the channel initiator then we would have signed any sent
	// fee update at this point, so mark this update as pending ACK, and
	// set pendingFeeUpdate to nil. We can do this since we know we won't
	// sign any new commitment before receiving a RevokeAndAck, because of
	// the revocation window of 1.
	if lc.channelState.IsInitiator {
		lc.pendingAckFeeUpdate = lc.pendingFeeUpdate
		lc.pendingFeeUpdate = nil
	}

	return sig, htlcSigs, nil
}

// ProcessChanSyncMsg processes a ChannelReestablish message sent by the remote
// connection upon re establishment of our connection with them. This method
// will return a single message if we are currently out of sync, otherwise a
// nil lnwire.Message will be returned. If it is decided that our level of
// de-synchronization is irreconcilable, then an error indicating the issue
// will be returned. In this case that an error is returned, the channel should
// be force closed, as we cannot continue updates.
//
// One of two message sets will be returned:
//
//  * CommitSig+Updates: if we have a pending remote commit which they claim to
//    have not received
//  * RevokeAndAck: if we sent a revocation message that they claim to have
//    not received
func (lc *LightningChannel) ProcessChanSyncMsg(msg *lnwire.ChannelReestablish) ([]lnwire.Message, error) {

	// We owe them a commitment if they have an un-acked commitment and the
	// tip of their chain (from our Pov) is equal to what they think their
	// next commit height should be.
	remoteChainTip := lc.remoteCommitChain.tip()
	oweCommitment := (lc.remoteCommitChain.hasUnackedCommitment() &&
		msg.NextLocalCommitHeight == remoteChainTip.height)

	// We owe them a revocation if the tail of our current commitment is
	// one greater than what they _think_ our commitment tail is.
	localChainTail := lc.localCommitChain.tail()
	oweRevocation := localChainTail.height == msg.RemoteCommitTailHeight+1

	// Now we'll examine the state we have, vs what was contained in the
	// chain sync message. If we're de-synchronized, then we'll send a
	// batch of messages which when applied will kick start the chain
	// resync.
	var updates []lnwire.Message

	// If the remote party included the optional fields, then we'll verify
	// their correctness first, as it will influence our decisions below.
	hasRecoveryOptions := msg.LocalUnrevokedCommitPoint != nil
	commitSecretCorrect := true
	if hasRecoveryOptions && msg.RemoteCommitTailHeight != 0 {
		// We'll check that they've really sent a valid commit
		// secret from our shachain for our prior height, but only if
		// this isn't the first state.
		heightSecret, err := lc.channelState.RevocationProducer.AtIndex(
			msg.RemoteCommitTailHeight - 1,
		)
		if err != nil {
			return nil, err
		}
		commitSecretCorrect = bytes.Equal(
			heightSecret[:], msg.LastRemoteCommitSecret[:],
		)
	}

	// TODO(roasbeef): check validity of commitment point after the fact

	// If the commit secret they sent is incorrect then we'll fail the
	// channel as the remote node has an inconsistent state.
	if !commitSecretCorrect {
		// In this case, we'll return an error to indicate the remote
		// node sent us the wrong values. This will let the caller act
		// accordingly.
		return nil, ErrInvalidLastCommitSecret
	}

	switch {
	// If we owe the remote party a revocation message, then we'll re-send
	// the last revocation message that we sent. This will be the
	// revocation message for our prior chain tail.
	case oweRevocation:
		revocationMsg, err := lc.generateRevocation(
			localChainTail.height - 1,
		)
		if err != nil {
			return nil, err
		}
		updates = append(updates, revocationMsg)

		// Next, as a precaution, we'll check a special edge case. If
		// they initiated a state transition, we sent the revocation,
		// but died before the signature was sent. We re-transmit our
		// revocation, but also initiate a state transition to re-sync
		// them.
		if lc.localCommitChain.tip().height >
			lc.remoteCommitChain.tip().height {

			commitSig, htlcSigs, err := lc.SignNextCommitment()
			switch {

			// If we signed this state, then we'll accumulate
			// another update to send over.
			case err == nil:
				updates = append(updates, &lnwire.CommitSig{
					ChanID: lnwire.NewChanIDFromOutPoint(
						&lc.channelState.FundingOutpoint,
					),
					CommitSig: commitSig,
					HtlcSigs:  htlcSigs,
				})

			// If we get a failure due to not knowing their next
			// point, then this is fine as they'll either send
			// FundingLocked, or revoke their next state to allow
			// us to continue forwards.
			case err == ErrNoWindow:

			// Otherwise, this is an error and we'll treat it as
			// such.
			default:
				return nil, err
			}
		}

	// If we don't owe the remote party a revocation, but their value for
	// what our remote chain tail should be doesn't match up, and their
	// purported commitment secrete matches up, then we'll behind!
	case (msg.RemoteCommitTailHeight > localChainTail.height &&
		hasRecoveryOptions && commitSecretCorrect):

		// In this case, we've likely lost data and shouldn't proceed
		// with channel updates. So we'll return the appropriate error
		// to signal to the caller the current state.
		return nil, ErrCommitSyncDataLoss

	// If we don't owe them a revocation, and the height of our commitment
	// chain reported by the remote party is not equal to our chain tail,
	// then we cannot sync.
	case !oweRevocation && localChainTail.height != msg.RemoteCommitTailHeight:
		if err := lc.channelState.MarkBorked(); err != nil {
			return nil, err
		}

		return nil, ErrCannotSyncCommitChains
	}

	// If we owe them a commitment, then we'll read from disk our
	// commitment diff, so we can re-send them to the remote party.
	if oweCommitment {
		// Grab the current remote chain tip from the database. This
		// commit diff contains all the information required to re-sync
		// our states.
		commitDiff, err := lc.channelState.RemoteCommitChainTip()
		if err != nil {
			return nil, err
		}

		// Next, we'll need to send over any updates we sent as part of
		// this new proposed commitment state.
		for _, logUpdate := range commitDiff.LogUpdates {
			updates = append(updates, logUpdate.UpdateMsg)
		}

		// With the batch of updates accumulated, we'll now re-send the
		// original CommitSig message required to re-sync their remote
		// commitment chain with our local version of their chain.
		updates = append(updates, commitDiff.CommitSig)

	} else if !oweCommitment && remoteChainTip.height+1 !=
		msg.NextLocalCommitHeight {

		if err := lc.channelState.MarkBorked(); err != nil {
			return nil, err
		}

		// If we don't owe them a commitment, yet the tip of their
		// chain isn't one more than the next local commit height they
		// report, we'll fail the channel.
		return nil, ErrCannotSyncCommitChains
	}

	return updates, nil
}

// ChanSyncMsg returns the ChannelReestablish message that should be sent upon
// reconnection with the remote peer that we're maintaining this channel with.
// The information contained within this message is necessary to re-sync our
// commitment chains in the case of a last or only partially processed message.
// When the remote party receiver this message one of three things may happen:
//
//   1. We're fully synced and no messages need to be sent.
//   2. We didn't get the last CommitSig message they sent, to they'll re-send
//      it.
//   3. We didn't get the last RevokeAndAck message they sent, so they'll
//      re-send it.
func (lc *LightningChannel) ChanSyncMsg() (*lnwire.ChannelReestablish, error) {
	// The remote commitment height that we we'll send in the
	// ChannelReestablish message is our current commitment height plus
	// one. If the receiver thinks that our commitment height is actually
	// *equal* to this value, then they'll re-send the last commitment that
	// they sent but we never fully processed.
	localHeight := lc.localCommitChain.tip().height
	nextLocalCommitHeight := localHeight + 1

	// The second value we'll send is the height of the remote commitment
	// from our PoV. If the receiver thinks that their height is actually
	// *one plus* this value, then they'll re-send their last revocation.
	remoteChainTipHeight := lc.remoteCommitChain.tail().height

	// If this channel has undergone a commitment update, then in order to
	// prove to the remote party our knowledge of their prior commitment
	// state, we'll also send over the last commitment secret that the
	// remote party sent.
	var lastCommitSecret [32]byte
	if remoteChainTipHeight != 0 {
		remoteSecret, err := lc.channelState.RevocationStore.LookUp(
			remoteChainTipHeight - 1,
		)
		if err != nil {
			return nil, err
		}
		lastCommitSecret = [32]byte(*remoteSecret)
	}

	// Additionally, we'll send over the current unrevoked commitment on
	// our local commitment transaction.
	currentCommitSecret, err := lc.channelState.RevocationProducer.AtIndex(
		localHeight,
	)
	if err != nil {
		return nil, err
	}

	return &lnwire.ChannelReestablish{
		ChanID: lnwire.NewChanIDFromOutPoint(
			&lc.channelState.FundingOutpoint,
		),
		NextLocalCommitHeight:  nextLocalCommitHeight,
		RemoteCommitTailHeight: remoteChainTipHeight,
		LastRemoteCommitSecret: lastCommitSecret,
		LocalUnrevokedCommitPoint: ComputeCommitmentPoint(
			currentCommitSecret[:],
		),
	}, nil
}

// validateCommitmentSanity is used to validate that on current state the commitment
// transaction is valid in terms of propagating it over Bitcoin network, and
// also that all outputs are meet Bitcoin spec requirements and they are
// spendable.
func (lc *LightningChannel) validateCommitmentSanity(theirLogCounter,
	ourLogCounter uint64, prediction bool, local bool, remote bool) error {

	// TODO(roasbeef): verify remaining sanity requirements
	htlcCount := 0

	// If we adding or receiving the htlc we increase the number of htlcs
	// by one in order to not overflow the commitment transaction by
	// insertion.
	if prediction {
		htlcCount++
	}

	// TODO(roasbeef): call availableBalance in here re-using htlcView

	// Run through all the HTLCs that will be covered by this transaction
	// in order to calculate theirs count.
	view := lc.fetchHTLCView(theirLogCounter, ourLogCounter)

	if remote {
		for _, entry := range view.theirUpdates {
			if entry.EntryType == Add {
				htlcCount++
			}
		}
		for _, entry := range view.ourUpdates {
			if entry.EntryType != Add {
				htlcCount--
			}
		}
	}

	if local {
		for _, entry := range view.ourUpdates {
			if entry.EntryType == Add {
				htlcCount++
			}
		}
		for _, entry := range view.theirUpdates {
			if entry.EntryType != Add {
				htlcCount--
			}
		}
	}

	// If we're validating the commitment sanity for HTLC _log_ update by a
	// particular side, then we'll only consider half of the available HTLC
	// bandwidth. However, if we're validating the _creation_ of a new
	// commitment state, then we'll use the full value as the sum of the
	// contribution of both sides shouldn't exceed the max number.
	var maxHTLCNumber int
	if local && remote {
		maxHTLCNumber = MaxHTLCNumber
	} else {
		maxHTLCNumber = MaxHTLCNumber / 2
	}

	if htlcCount > maxHTLCNumber {
		return ErrMaxHTLCNumber
	}

	return nil
}

// genHtlcSigValidationJobs generates a series of signatures verification jobs
// meant to verify all the signatures for HTLC's attached to a newly created
// commitment state. The jobs generated are fully populated, and can be sent
// directly into the pool of workers.
func genHtlcSigValidationJobs(localCommitmentView *commitment,
	keyRing *commitmentKeyRing, htlcSigs []*btcec.Signature,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig) []verifyJob {

	// If this new commitment state doesn't have any HTLC's that are to be
	// signed, then we'll return a nil slice.
	if len(htlcSigs) == 0 {
		return nil
	}

	txHash := localCommitmentView.txn.TxHash()
	feePerKw := localCommitmentView.feePerKw

	// With the required state generated, we'll create a slice with large
	// enough capacity to hold verification jobs for all HTLC's in this
	// view. In the case that we have some dust outputs, then the actual
	// length will be smaller than the total capacity.
	numHtlcs := (len(localCommitmentView.incomingHTLCs) +
		len(localCommitmentView.outgoingHTLCs))
	verifyJobs := make([]verifyJob, 0, numHtlcs)

	// We'll iterate through each output in the commitment transaction,
	// populating the sigHash closure function if it's detected to be an
	// HLTC output. Given the sighash, and the signing key, we'll be able
	// to validate each signature within the worker pool.
	i := 0
	for index := range localCommitmentView.txn.TxOut {
		var sigHash func() ([]byte, error)

		outputIndex := int32(index)
		switch {

		// If this output index is found within the incoming HTLC index,
		// then this means that we need to generate an HTLC success
		// transaction in order to validate the signature.
		case localCommitmentView.incomingHTLCIndex[outputIndex] != nil:
			htlc := localCommitmentView.incomingHTLCIndex[outputIndex]

			sigHash = func() ([]byte, error) {
				op := wire.OutPoint{
					Hash:  txHash,
					Index: uint32(htlc.localOutputIndex),
				}

				htlcFee := htlcSuccessFee(feePerKw)
				outputAmt := htlc.Amount.ToSatoshis() - htlcFee

				successTx, err := createHtlcSuccessTx(op,
					outputAmt, uint32(localChanCfg.CsvDelay),
					keyRing.revocationKey, keyRing.delayKey)
				if err != nil {
					return nil, err
				}

				hashCache := txscript.NewTxSigHashes(successTx)
				sigHash, err := txscript.CalcWitnessSigHash(
					htlc.ourWitnessScript, hashCache,
					txscript.SigHashAll, successTx, 0,
					int64(htlc.Amount.ToSatoshis()),
				)
				if err != nil {
					return nil, err
				}

				return sigHash, nil
			}

			// With the sighash generated, we'll also store the
			// signature so it can be written to disk if this state
			// is valid.
			htlc.sig = htlcSigs[i]

		// Otherwise, if this is an outgoing HTLC, then we'll need to
		// generate a timeout transaction so we can verify the
		// signature presented.
		case localCommitmentView.outgoingHTLCIndex[outputIndex] != nil:
			htlc := localCommitmentView.outgoingHTLCIndex[outputIndex]

			sigHash = func() ([]byte, error) {
				op := wire.OutPoint{
					Hash:  txHash,
					Index: uint32(htlc.localOutputIndex),
				}

				htlcFee := htlcTimeoutFee(feePerKw)
				outputAmt := htlc.Amount.ToSatoshis() - htlcFee

				timeoutTx, err := createHtlcTimeoutTx(op,
					outputAmt, htlc.Timeout,
					uint32(localChanCfg.CsvDelay),
					keyRing.revocationKey, keyRing.delayKey,
				)
				if err != nil {
					return nil, err
				}

				hashCache := txscript.NewTxSigHashes(timeoutTx)
				sigHash, err := txscript.CalcWitnessSigHash(
					htlc.ourWitnessScript, hashCache,
					txscript.SigHashAll, timeoutTx, 0,
					int64(htlc.Amount.ToSatoshis()),
				)
				if err != nil {
					return nil, err
				}

				return sigHash, nil
			}

			// With the sighash generated, we'll also store the
			// signature so it can be written to disk if this state
			// is valid.
			htlc.sig = htlcSigs[i]

		default:
			continue
		}

		verifyJobs = append(verifyJobs, verifyJob{
			pubKey:  keyRing.remoteHtlcKey,
			sig:     htlcSigs[i],
			sigHash: sigHash,
		})

		i++
	}

	return verifyJobs
}

// InvalidCommitSigError is a struct that implements the error interface to
// report a failure to validation a commitment signature for a remote peer.
// We'll use the items in this struct to generate a rich error message for the
// remote peer when we receive an invalid signature from it. Doing so can
// greatly aide in debugging cross implementation issues.
type InvalidCommitSigError struct {
	commitHeight uint64

	commitSig []byte

	sigHash []byte

	commitTx []byte
}

// Error returns a detailed error string including the exact transaction that
// caused an invalid commitment signature.
func (i *InvalidCommitSigError) Error() string {
	return fmt.Sprintf("rejected commitment: commit_height=%v, "+
		"invalid_sig=%x, commit_tx=%x, sig_hash=%x", i.commitHeight,
		i.commitSig[:], i.commitTx, i.sigHash[:])
}

// A compile time flag to ensure that InvalidCommitSigError implements the
// error interface.
var _ error = (*InvalidCommitSigError)(nil)

// ReceiveNewCommitment process a signature for a new commitment state sent by
// the remote party. This method should be called in response to the
// remote party initiating a new change, or when the remote party sends a
// signature fully accepting a new state we've initiated. If we are able to
// successfully validate the signature, then the generated commitment is added
// to our local commitment chain. Once we send a revocation for our prior
// state, then this newly added commitment becomes our current accepted channel
// state.
func (lc *LightningChannel) ReceiveNewCommitment(commitSig *btcec.Signature,
	htlcSigs []*btcec.Signature) error {

	lc.Lock()
	defer lc.Unlock()

	// Determine the last update on the local log that has been locked in.
	localACKedIndex := lc.remoteCommitChain.tail().ourMessageIndex
	localHtlcIndex := lc.remoteCommitChain.tail().ourHtlcIndex

	// Ensure that this new local update from the remote node respects all
	// the constraints we specified during initial channel setup. If not,
	// then we'll abort the channel as they've violated our constraints.
	err := lc.validateCommitmentSanity(lc.remoteUpdateLog.logIndex,
		localACKedIndex, false, true, true)
	if err != nil {
		return err
	}

	// We're receiving a new commitment which attempts to extend our local
	// commitment chain height by one, so fetch the proper commitment point
	// as this will be needed to derive the keys required to construct the
	// commitment.
	nextHeight := lc.currentHeight + 1
	commitSecret, err := lc.channelState.RevocationProducer.AtIndex(nextHeight)
	if err != nil {
		return err
	}
	commitPoint := ComputeCommitmentPoint(commitSecret[:])
	keyRing := deriveCommitmentKeys(commitPoint, true, lc.localChanCfg,
		lc.remoteChanCfg)

	// With the current commitment point re-calculated, construct the new
	// commitment view which includes all the entries we know of in their
	// HTLC log, and up to ourLogIndex in our HTLC log.
	localCommitmentView, err := lc.fetchCommitmentView(
		false, localACKedIndex, localHtlcIndex,
		lc.remoteUpdateLog.logIndex, lc.remoteUpdateLog.htlcCounter,
		keyRing,
	)
	if err != nil {
		return err
	}

	walletLog.Tracef("ChannelPoint(%v): extending local chain to height %v, "+
		"local_log=%v, remote_log=%v",
		lc.channelState.FundingOutpoint, localCommitmentView.height,
		localACKedIndex, lc.remoteUpdateLog.logIndex)

	walletLog.Tracef("ChannelPoint(%v): local chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v", lc.channelState.FundingOutpoint,
		localCommitmentView.ourBalance, localCommitmentView.theirBalance,
		newLogClosure(func() string {
			return spew.Sdump(localCommitmentView.txn)
		}),
	)

	// Construct the sighash of the commitment transaction corresponding to
	// this newly proposed state update.
	localCommitTx := localCommitmentView.txn
	multiSigScript := lc.FundingWitnessScript
	hashCache := txscript.NewTxSigHashes(localCommitTx)
	sigHash, err := txscript.CalcWitnessSigHash(multiSigScript, hashCache,
		txscript.SigHashAll, localCommitTx, 0,
		int64(lc.channelState.Capacity))
	if err != nil {
		// TODO(roasbeef): fetchview has already mutated the HTLCs...
		//  * need to either roll-back, or make pure
		return err
	}

	// As an optimization, we'll generate a series of jobs for the worker
	// pool to verify each of the HTLc signatures presented. Once
	// generated, we'll submit these jobs to the worker pool.
	verifyJobs := genHtlcSigValidationJobs(localCommitmentView,
		keyRing, htlcSigs, lc.localChanCfg, lc.remoteChanCfg)
	cancelChan := make(chan struct{})
	verifyResps := lc.sigPool.SubmitVerifyBatch(verifyJobs, cancelChan)

	// While the HTLC verification jobs are proceeding asynchronously,
	// we'll ensure that the newly constructed commitment state has a valid
	// signature.
	verifyKey := btcec.PublicKey{
		X:     lc.remoteChanCfg.MultiSigKey.X,
		Y:     lc.remoteChanCfg.MultiSigKey.Y,
		Curve: btcec.S256(),
	}
	if !commitSig.Verify(sigHash, &verifyKey) {
		close(cancelChan)

		// If we fail to validate their commitment signature, we'll
		// generate a special error to send over the protocol. We'll
		// include the exact signature and commitment we failed to
		// verify against in order to aide debugging.
		var txBytes bytes.Buffer
		localCommitTx.Serialize(&txBytes)
		return &InvalidCommitSigError{
			commitHeight: nextHeight,
			commitSig:    commitSig.Serialize(),
			sigHash:      sigHash,
			commitTx:     txBytes.Bytes(),
		}
	}

	// With the primary commitment transaction validated, we'll check each
	// of the HTLC validation jobs.
	for i := 0; i < len(verifyJobs); i++ {
		// In the case that a single signature is invalid, we'll exit
		// early and cancel all the outstanding verification jobs.
		select {
		case err := <-verifyResps:
			if err != nil {
				close(cancelChan)
				return fmt.Errorf("invalid htlc signature: %v", err)
			}
		case <-lc.quit:
			return fmt.Errorf("channel shutting down")
		}
	}

	// The signature checks out, so we can now add the new commitment to
	// our local commitment chain.
	localCommitmentView.sig = commitSig.Serialize()
	lc.localCommitChain.addCommitment(localCommitmentView)

	// If we are not channel initiator, then the commitment just received
	// would've signed any received fee update since last commitment. Mark
	// any such fee update as pending ACK (so we remember to ACK it on our
	// next commitment), and set pendingFeeUpdate to nil. We can do this
	// since we won't receive any new commitment before ACKing.
	if !lc.channelState.IsInitiator {
		lc.pendingAckFeeUpdate = lc.pendingFeeUpdate
		lc.pendingFeeUpdate = nil
	}

	return nil
}

// FullySynced returns a boolean value reflecting if both commitment chains
// (remote+local) are fully in sync. Both commitment chains are fully in sync
// if the tip of each chain includes the latest committed changes from both
// sides.
func (lc *LightningChannel) FullySynced() bool {
	lc.RLock()
	defer lc.RUnlock()

	lastLocalCommit := lc.localCommitChain.tip()
	lastRemoteCommit := lc.remoteCommitChain.tip()

	oweCommitment := lastLocalCommit.height > lastRemoteCommit.height

	localUpdatesSynced := (lastLocalCommit.ourMessageIndex ==
		lastRemoteCommit.ourMessageIndex)

	remoteUpdatesSynced := (lastLocalCommit.theirMessageIndex ==
		lastRemoteCommit.theirMessageIndex)

	return !oweCommitment && localUpdatesSynced && remoteUpdatesSynced
}

// RevokeCurrentCommitment revokes the next lowest unrevoked commitment
// transaction in the local commitment chain. As a result the edge of our
// revocation window is extended by one, and the tail of our local commitment
// chain is advanced by a single commitment. This now lowest unrevoked
// commitment becomes our currently accepted state within the channel.
func (lc *LightningChannel) RevokeCurrentCommitment() (*lnwire.RevokeAndAck, error) {
	lc.Lock()
	defer lc.Unlock()

	revocationMsg, err := lc.generateRevocation(lc.currentHeight)
	if err != nil {
		return nil, err
	}

	walletLog.Tracef("ChannelPoint(%v): revoking height=%v, now at height=%v",
		lc.channelState.FundingOutpoint, lc.localCommitChain.tail().height,
		lc.currentHeight+1)

	// Advance our tail, as we've revoked our previous state.
	lc.localCommitChain.advanceTail()
	lc.currentHeight++

	// Additionally, generate a channel delta for this state transition for
	// persistent storage.
	chainTail := lc.localCommitChain.tail()
	newCommitment := chainTail.toDiskCommit(true)
	err = lc.channelState.UpdateCommitment(newCommitment)
	if err != nil {
		return nil, err
	}

	walletLog.Tracef("ChannelPoint(%v): state transition accepted: "+
		"our_balance=%v, their_balance=%v",
		lc.channelState.FundingOutpoint, chainTail.ourBalance,
		chainTail.theirBalance)

	revocationMsg.ChanID = lnwire.NewChanIDFromOutPoint(
		&lc.channelState.FundingOutpoint,
	)

	return revocationMsg, nil
}

// ReceiveRevocation processes a revocation sent by the remote party for the
// lowest unrevoked commitment within their commitment chain. We receive a
// revocation either during the initial session negotiation wherein revocation
// windows are extended, or in response to a state update that we initiate. If
// successful, then the remote commitment chain is advanced by a single
// commitment, and a log compaction is attempted. In addition, a slice of
// HTLC's which can be forwarded upstream are returned.
func (lc *LightningChannel) ReceiveRevocation(revMsg *lnwire.RevokeAndAck) ([]*PaymentDescriptor, error) {
	lc.Lock()
	defer lc.Unlock()

	// Ensure that the new pre-image can be placed in preimage store.
	store := lc.channelState.RevocationStore
	revocation, err := chainhash.NewHash(revMsg.Revocation[:])
	if err != nil {
		return nil, err
	}
	if err := store.AddNextEntry(revocation); err != nil {
		return nil, err
	}

	// Verify that if we use the commitment point computed based off of the
	// revealed secret to derive a revocation key with our revocation base
	// point, then it matches the current revocation of the remote party.
	currentCommitPoint := lc.channelState.RemoteCurrentRevocation
	derivedCommitPoint := ComputeCommitmentPoint(revMsg.Revocation[:])
	if !derivedCommitPoint.IsEqual(currentCommitPoint) {
		return nil, fmt.Errorf("revocation key mismatch")
	}

	// Now that we've verified that the prior commitment has been properly
	// revoked, we'll advance the revocation state we track for the remote
	// party: the new current revocation is what was previously the next
	// revocation, and the new next revocation is set to the key included
	// in the message.
	lc.channelState.RemoteCurrentRevocation = lc.channelState.RemoteNextRevocation
	lc.channelState.RemoteNextRevocation = revMsg.NextRevocationKey

	walletLog.Tracef("ChannelPoint(%v): remote party accepted state transition, "+
		"revoked height %v, now at %v", lc.channelState.FundingOutpoint,
		lc.remoteCommitChain.tail().height,
		lc.remoteCommitChain.tail().height+1)

	// At this point, the revocation has been accepted, and we've rotated
	// the current revocation key+hash for the remote party. Therefore we
	// sync now to ensure the revocation producer state is consistent with
	// the current commitment height and also to advance the on-disk
	// commitment chain.
	if err := lc.channelState.AdvanceCommitChainTail(); err != nil {
		return nil, err
	}

	// Since they revoked the current lowest height in their commitment
	// chain, we can advance their chain by a single commitment.
	lc.remoteCommitChain.advanceTail()

	remoteChainTail := lc.remoteCommitChain.tail().height
	localChainTail := lc.localCommitChain.tail().height

	// Now that we've verified the revocation update the state of the HTLC
	// log as we may be able to prune portions of it now, and update their
	// balance.
	var htlcsToForward []*PaymentDescriptor
	for e := lc.remoteUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		if htlc.isForwarded {
			continue
		}

		uncomitted := (htlc.addCommitHeightRemote == 0 ||
			htlc.addCommitHeightLocal == 0)
		if htlc.EntryType == Add && uncomitted {
			continue
		}

		// We'll only forward any new HTLC additions iff, it's "freshly
		// locked in". Meaning that the HTLC was only *just* considered
		// locked-in at this new state. By doing this we ensure that we
		// don't re-forward any already processed HTLC's after a
		// restart.
		if htlc.EntryType == Add &&
			remoteChainTail == htlc.addCommitHeightRemote &&
			localChainTail >= htlc.addCommitHeightLocal {

			htlc.isForwarded = true
			htlcsToForward = append(htlcsToForward, htlc)
			continue
		}

		if htlc.EntryType != Add &&
			remoteChainTail >= htlc.removeCommitHeightRemote &&
			localChainTail >= htlc.removeCommitHeightLocal {

			htlc.isForwarded = true
			htlcsToForward = append(htlcsToForward, htlc)
			continue
		}
	}

	// As we've just completed a new state transition, attempt to see if we
	// can remove any entries from the update log which have been removed
	// from the PoV of both commitment chains.
	compactLogs(lc.localUpdateLog, lc.remoteUpdateLog,
		localChainTail, remoteChainTail)

	return htlcsToForward, nil
}

// NextRevocationKey returns the commitment point for the _next_ commitment
// height. The pubkey returned by this function is required by the remote party
// along with their revocation base to to extend our commitment chain with a
// new commitment.
func (lc *LightningChannel) NextRevocationKey() (*btcec.PublicKey, error) {
	lc.RLock()
	defer lc.RUnlock()

	nextHeight := lc.currentHeight + 1
	revocation, err := lc.channelState.RevocationProducer.AtIndex(nextHeight)
	if err != nil {
		return nil, err
	}

	return ComputeCommitmentPoint(revocation[:]), nil
}

// InitNextRevocation inserts the passed commitment point as the _next_
// revocation to be used when creating a new commitment state for the remote
// party. This function MUST be called before the channel can accept or propose
// any new states.
func (lc *LightningChannel) InitNextRevocation(revKey *btcec.PublicKey) error {
	lc.Lock()
	defer lc.Unlock()

	return lc.channelState.InsertNextRevocation(revKey)
}

// AddHTLC adds an HTLC to the state machine's local update log. This method
// should be called when preparing to send an outgoing HTLC.
func (lc *LightningChannel) AddHTLC(htlc *lnwire.UpdateAddHTLC) (uint64, error) {
	lc.Lock()
	defer lc.Unlock()

	if err := lc.validateCommitmentSanity(lc.remoteUpdateLog.logIndex,
		lc.localUpdateLog.logIndex, true, true, false); err != nil {
		return 0, err
	}

	// To ensure that we can actually fully accept this new HTLC, we'll
	// calculate the current available bandwidth, and subtract the value of
	// the HTLC from it.
	initialBalance, _ := lc.availableBalance()
	availableBalance := initialBalance
	availableBalance -= htlc.Amount

	feePerKw := lc.channelState.LocalCommitment.FeePerKw
	dustLimit := lc.channelState.LocalChanCfg.DustLimit
	htlcIsDust := htlcIsDust(
		false, true, feePerKw, htlc.Amount.ToSatoshis(), dustLimit,
	)

	// If this HTLC is not dust, and we're the initiator, then we'll also
	// subtract the amount we'll need to pay in fees for this HTLC.
	if !htlcIsDust && lc.channelState.IsInitiator {
		htlcFee := lnwire.NewMSatFromSatoshis(
			btcutil.Amount((int64(feePerKw) * HtlcWeight) / 1000),
		)
		availableBalance -= htlcFee
	}

	// If this value is negative, then we can't accept the HTLC, so we'll
	// reject it with an error.
	if availableBalance < 0 {
		// TODO(roasbeef): also needs to respect reservation
		//  * expand to add context err msg
		walletLog.Errorf("Unable to carry added HTLC: amt=%v, bal=%v",
			htlc.Amount, availableBalance)
		return 0, ErrInsufficientBalance
	}

	pd := &PaymentDescriptor{
		EntryType: Add,
		RHash:     PaymentHash(htlc.PaymentHash),
		Timeout:   htlc.Expiry,
		Amount:    htlc.Amount,
		LogIndex:  lc.localUpdateLog.logIndex,
		HtlcIndex: lc.localUpdateLog.htlcCounter,
		OnionBlob: htlc.OnionBlob[:],
	}

	lc.localUpdateLog.appendHtlc(pd)

	return pd.HtlcIndex, nil
}

// ReceiveHTLC adds an HTLC to the state machine's remote update log. This
// method should be called in response to receiving a new HTLC from the remote
// party.
func (lc *LightningChannel) ReceiveHTLC(htlc *lnwire.UpdateAddHTLC) (uint64, error) {
	lc.Lock()
	defer lc.Unlock()

	if htlc.ID != lc.remoteUpdateLog.htlcCounter {
		return 0, fmt.Errorf("ID %d on HTLC add does not match expected next "+
			"ID %d", htlc.ID, lc.remoteUpdateLog.htlcCounter)
	}

	if err := lc.validateCommitmentSanity(lc.remoteUpdateLog.logIndex,
		lc.localUpdateLog.logIndex, true, false, true); err != nil {
		return 0, err
	}

	pd := &PaymentDescriptor{
		EntryType: Add,
		RHash:     PaymentHash(htlc.PaymentHash),
		Timeout:   htlc.Expiry,
		Amount:    htlc.Amount,
		LogIndex:  lc.remoteUpdateLog.logIndex,
		HtlcIndex: lc.remoteUpdateLog.htlcCounter,
		OnionBlob: htlc.OnionBlob[:],
	}

	lc.remoteUpdateLog.appendHtlc(pd)

	return pd.HtlcIndex, nil
}

// SettleHTLC attempts to settle an existing outstanding received HTLC. The
// remote log index of the HTLC settled is returned in order to facilitate
// creating the corresponding wire message. In the case the supplied preimage
// is invalid, an error is returned. Additionally, the value of the settled
// HTLC is also returned.
func (lc *LightningChannel) SettleHTLC(preimage [32]byte, htlcIndex uint64,
) error {

	lc.Lock()
	defer lc.Unlock()

	htlc := lc.remoteUpdateLog.lookupHtlc(htlcIndex)
	if htlc == nil {
		return fmt.Errorf("No HTLC with ID %d in channel %v", htlcIndex,
			lc.channelState.ShortChanID)
	}

	if htlc.RHash != sha256.Sum256(preimage[:]) {
		return fmt.Errorf("Invalid payment preimage %x for hash %x",
			preimage[:], htlc.RHash[:])
	}

	pd := &PaymentDescriptor{
		Amount:      htlc.Amount,
		RPreimage:   preimage,
		LogIndex:    lc.localUpdateLog.logIndex,
		ParentIndex: htlcIndex,
		EntryType:   Settle,
	}

	lc.localUpdateLog.appendUpdate(pd)

	return nil
}

// ReceiveHTLCSettle attempts to settle an existing outgoing HTLC indexed by an
// index into the local log. If the specified index doesn't exist within the
// log, and error is returned. Similarly if the preimage is invalid w.r.t to
// the referenced of then a distinct error is returned.
func (lc *LightningChannel) ReceiveHTLCSettle(preimage [32]byte, htlcIndex uint64) error {
	lc.Lock()
	defer lc.Unlock()

	htlc := lc.localUpdateLog.lookupHtlc(htlcIndex)
	if htlc == nil {
		return fmt.Errorf("No HTLC with ID %d in channel %v", htlcIndex,
			lc.channelState.ShortChanID)
	}

	if htlc.RHash != sha256.Sum256(preimage[:]) {
		return fmt.Errorf("Invalid payment preimage %x for hash %x",
			preimage[:], htlc.RHash[:])
	}

	pd := &PaymentDescriptor{
		Amount:      htlc.Amount,
		RPreimage:   preimage,
		ParentIndex: htlc.HtlcIndex,
		RHash:       htlc.RHash,
		LogIndex:    lc.remoteUpdateLog.logIndex,
		EntryType:   Settle,
	}

	lc.remoteUpdateLog.appendUpdate(pd)
	return nil
}

// FailHTLC attempts to fail a targeted HTLC by its payment hash, inserting an
// entry which will remove the target log entry within the next commitment
// update. This method is intended to be called in order to cancel in
// _incoming_ HTLC.
func (lc *LightningChannel) FailHTLC(htlcIndex uint64, reason []byte) error {
	lc.Lock()
	defer lc.Unlock()

	htlc := lc.remoteUpdateLog.lookupHtlc(htlcIndex)
	if htlc == nil {
		return fmt.Errorf("No HTLC with ID %d in channel %v", htlcIndex,
			lc.channelState.ShortChanID)
	}

	pd := &PaymentDescriptor{
		Amount:      htlc.Amount,
		RHash:       htlc.RHash,
		ParentIndex: htlcIndex,
		LogIndex:    lc.localUpdateLog.logIndex,
		EntryType:   Fail,
		FailReason:  reason,
	}

	lc.localUpdateLog.appendUpdate(pd)

	return nil
}

// MalformedFailHTLC attempts to fail a targeted HTLC by its payment hash,
// inserting an entry which will remove the target log entry within the next
// commitment update. This method is intended to be called in order to cancel
// in _incoming_ HTLC.
func (lc *LightningChannel) MalformedFailHTLC(htlcIndex uint64,
	failCode lnwire.FailCode, shaOnionBlob [sha256.Size]byte) error {

	lc.Lock()
	defer lc.Unlock()

	htlc := lc.remoteUpdateLog.lookupHtlc(htlcIndex)
	if htlc == nil {
		return fmt.Errorf("No HTLC with ID %d in channel %v", htlcIndex,
			lc.channelState.ShortChanID)
	}

	pd := &PaymentDescriptor{
		Amount:       htlc.Amount,
		RHash:        htlc.RHash,
		ParentIndex:  htlcIndex,
		LogIndex:     lc.localUpdateLog.logIndex,
		EntryType:    MalformedFail,
		FailCode:     failCode,
		ShaOnionBlob: shaOnionBlob,
	}

	lc.localUpdateLog.appendUpdate(pd)

	return nil
}

// ReceiveFailHTLC attempts to cancel a targeted HTLC by its log index,
// inserting an entry which will remove the target log entry within the next
// commitment update. This method should be called in response to the upstream
// party cancelling an outgoing HTLC. The value of the failed HTLC is returned
// along with an error indicating success.
func (lc *LightningChannel) ReceiveFailHTLC(htlcIndex uint64, reason []byte,
) error {

	lc.Lock()
	defer lc.Unlock()

	htlc := lc.localUpdateLog.lookupHtlc(htlcIndex)
	if htlc == nil {
		return fmt.Errorf("No HTLC with ID %d in channel %v", htlcIndex,
			lc.channelState.ShortChanID)
	}

	pd := &PaymentDescriptor{
		Amount:      htlc.Amount,
		RHash:       htlc.RHash,
		ParentIndex: htlc.HtlcIndex,
		LogIndex:    lc.remoteUpdateLog.logIndex,
		EntryType:   Fail,
		FailReason:  reason,
	}

	lc.remoteUpdateLog.appendUpdate(pd)

	return nil
}

// ChannelPoint returns the outpoint of the original funding transaction which
// created this active channel. This outpoint is used throughout various
// subsystems to uniquely identify an open channel.
func (lc *LightningChannel) ChannelPoint() *wire.OutPoint {
	lc.RLock()
	defer lc.RUnlock()

	return &lc.channelState.FundingOutpoint
}

// ShortChanID returns the short channel ID for the channel. The short channel
// ID encodes the exact location in the main chain that the original
// funding output can be found.
func (lc *LightningChannel) ShortChanID() lnwire.ShortChannelID {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.ShortChanID
}

// genHtlcScript generates the proper P2WSH public key scripts for the HTLC
// output modified by two-bits denoting if this is an incoming HTLC, and if the
// HTLC is being applied to their commitment transaction or ours.
func genHtlcScript(isIncoming, ourCommit bool, timeout uint32, rHash [32]byte,
	keyRing *commitmentKeyRing) ([]byte, []byte, error) {

	var (
		witnessScript []byte
		err           error
	)

	// Generate the proper redeem scripts for the HTLC output modified by
	// two-bits denoting if this is an incoming HTLC, and if the HTLC is
	// being applied to their commitment transaction or ours.
	switch {
	// The HTLC is paying to us, and being applied to our commitment
	// transaction. So we need to use the receiver's version of HTLC the
	// script.
	case isIncoming && ourCommit:
		witnessScript, err = receiverHTLCScript(timeout,
			keyRing.remoteHtlcKey, keyRing.localHtlcKey,
			keyRing.revocationKey, rHash[:])

	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && !ourCommit:
		witnessScript, err = senderHTLCScript(keyRing.remoteHtlcKey,
			keyRing.localHtlcKey, keyRing.revocationKey, rHash[:])

	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && ourCommit:
		witnessScript, err = senderHTLCScript(keyRing.localHtlcKey,
			keyRing.remoteHtlcKey, keyRing.revocationKey, rHash[:])

	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && !ourCommit:
		witnessScript, err = receiverHTLCScript(timeout, keyRing.localHtlcKey,
			keyRing.remoteHtlcKey, keyRing.revocationKey, rHash[:])
	}
	if err != nil {
		return nil, nil, err
	}

	// Now that we have the redeem scripts, create the P2WSH public key
	// script for the output itself.
	htlcP2WSH, err := witnessScriptHash(witnessScript)
	if err != nil {
		return nil, nil, err
	}

	return htlcP2WSH, witnessScript, nil
}

// addHTLC adds a new HTLC to the passed commitment transaction. One of four
// full scripts will be generated for the HTLC output depending on if the HTLC
// is incoming and if it's being applied to our commitment transaction or that
// of the remote node's. Additionally, in order to be able to efficiently
// locate the added HTLC on the commitment transaction from the
// PaymentDescriptor that generated it, the generated script is stored within
// the descriptor itself.
func (lc *LightningChannel) addHTLC(commitTx *wire.MsgTx, ourCommit bool,
	isIncoming bool, paymentDesc *PaymentDescriptor,
	keyRing *commitmentKeyRing) error {

	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	p2wsh, witnessScript, err := genHtlcScript(isIncoming, ourCommit,
		timeout, rHash, keyRing)
	if err != nil {
		return err
	}

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Amount.ToSatoshis())
	commitTx.AddTxOut(wire.NewTxOut(amountPending, p2wsh))

	// Store the pkScript of this particular PaymentDescriptor so we can
	// quickly locate it within the commitment transaction later.
	if ourCommit {
		paymentDesc.ourPkScript = p2wsh
		paymentDesc.ourWitnessScript = witnessScript
	} else {
		paymentDesc.theirPkScript = p2wsh
		paymentDesc.theirWitnessScript = witnessScript
	}

	return nil
}

// getSignedCommitTx function take the latest commitment transaction and
// populate it with witness data.
func (lc *LightningChannel) getSignedCommitTx() (*wire.MsgTx, error) {
	// Fetch the current commitment transaction, along with their signature
	// for the transaction.
	localCommit := lc.channelState.LocalCommitment
	commitTx := localCommit.CommitTx
	theirSig := append(localCommit.CommitSig, byte(txscript.SigHashAll))

	// With this, we then generate the full witness so the caller can
	// broadcast a fully signed transaction.
	lc.signDesc.SigHashes = txscript.NewTxSigHashes(commitTx)
	ourSigRaw, err := lc.signer.SignOutputRaw(commitTx, lc.signDesc)
	if err != nil {
		return nil, err
	}

	ourSig := append(ourSigRaw, byte(txscript.SigHashAll))

	// With the final signature generated, create the witness stack
	// required to spend from the multi-sig output.
	ourKey := lc.localChanCfg.MultiSigKey.SerializeCompressed()
	theirKey := lc.remoteChanCfg.MultiSigKey.SerializeCompressed()

	commitTx.TxIn[0].Witness = SpendMultiSig(
		lc.FundingWitnessScript, ourKey,
		ourSig, theirKey, theirSig,
	)

	return commitTx, nil
}

// UnilateralCloseSummary describes the details of a detected unilateral
// channel closure. This includes the information about with which
// transactions, and block the channel was unilaterally closed, as well as
// summarization details concerning the _state_ of the channel at the point of
// channel closure. Additionally, if we had a commitment output above dust on
// the remote party's commitment transaction, the necessary a SignDescriptor
// with the material necessary to seep the output are returned. Finally, if we
// had any outgoing HTLC's within the commitment transaction, then an
// OutgoingHtlcResolution for each output will included.
type UnilateralCloseSummary struct {
	// SpendDetail is a struct that describes how and when the commitment
	// output was spent.
	*chainntnfs.SpendDetail

	// ChannelCloseSummary is a struct describing the final state of the
	// channel and in which state is was closed.
	channeldb.ChannelCloseSummary

	// SelfOutPoint is the full outpoint that points to our non-delayed
	// pay-to-self output within the commitment transaction of the remote
	// party.
	SelfOutPoint *wire.OutPoint

	// SelfOutputSignDesc is a fully populated sign descriptor capable of
	// generating a valid signature to sweep the output paying to us
	SelfOutputSignDesc *SignDescriptor

	// MaturityDelay is the relative time-lock, in blocks for all outputs
	// that pay to the local party within the broadcast commitment
	// transaction.
	MaturityDelay uint32

	// HtlcResolutions is a slice of HTLC resolutions which allows the
	// local node to sweep any outgoing HTLC"s after the timeout period has
	// passed.
	HtlcResolutions []OutgoingHtlcResolution
}

// OutgoingHtlcResolution houses the information necessary to sweep any outgoing
// HTLC's after their contract has expired. This struct will be needed in one
// of two cases: the local party force closes the commitment transaction or the
// remote party unilaterally closes with their version of the commitment
// transaction.
type OutgoingHtlcResolution struct {
	// Expiry the absolute timeout of the HTLC. This value is expressed in
	// block height, meaning after this height the HLTC can be swept.
	Expiry uint32

	// SignedTimeoutTx is the fully signed HTLC timeout transaction. This
	// must be broadcast immediately after timeout has passed. Once this
	// has been confirmed, the HTLC output will transition into the
	// delay+claim state.
	SignedTimeoutTx *wire.MsgTx

	// SweepSignDesc is a sign descriptor that has been populated with the
	// necessary items required to spend the sole output of the above
	// transaction.
	SweepSignDesc SignDescriptor
}

// newHtlcResolution generates a new HTLC resolution capable of allowing the
// caller to sweep an outgoing HTLC present on either their, or the remote
// party's commitment transaction.
func newHtlcResolution(signer Signer, localChanCfg *channeldb.ChannelConfig,
	commitHash chainhash.Hash, htlc *channeldb.HTLC, keyRing *commitmentKeyRing,
	feePewKw, dustLimit btcutil.Amount, csvDelay uint32,
) (*OutgoingHtlcResolution, error) {

	op := wire.OutPoint{
		Hash:  commitHash,
		Index: uint32(htlc.OutputIndex),
	}

	// In order to properly reconstruct the HTLC transaction, we'll need to
	// re-calculate the fee required at this state, so we can add the
	// correct output value amount to the transaction.
	htlcFee := htlcTimeoutFee(feePewKw)
	secondLevelOutputAmt := htlc.Amt.ToSatoshis() - htlcFee

	// With the fee calculated, re-construct the second level timeout
	// transaction.
	timeoutTx, err := createHtlcTimeoutTx(
		op, secondLevelOutputAmt, htlc.RefundTimeout, csvDelay,
		keyRing.revocationKey, keyRing.delayKey,
	)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): branch based on local vs remote commit

	// With the transaction created, we can generate a sign descriptor
	// that's capable of generating the signature required to spend the
	// HTLC output using the timeout transaction.
	htlcCreationScript, err := senderHTLCScript(keyRing.localHtlcKey,
		keyRing.remoteHtlcKey, keyRing.revocationKey, htlc.RHash[:])
	if err != nil {
		return nil, err
	}
	timeoutSignDesc := SignDescriptor{
		PubKey:        localChanCfg.HtlcBasePoint,
		SingleTweak:   keyRing.localHtlcKeyTweak,
		WitnessScript: htlcCreationScript,
		Output: &wire.TxOut{
			Value: int64(htlc.Amt.ToSatoshis()),
		},
		HashType:   txscript.SigHashAll,
		SigHashes:  txscript.NewTxSigHashes(timeoutTx),
		InputIndex: 0,
	}

	// With the sign desc created, we can now construct the full witness
	// for the timeout transaction, and populate it as well.
	timeoutWitness, err := senderHtlcSpendTimeout(
		htlc.Signature, signer, &timeoutSignDesc, timeoutTx)
	if err != nil {
		return nil, err
	}
	timeoutTx.TxIn[0].Witness = timeoutWitness

	// Finally, we'll generate the script output that the timeout
	// transaction creates so we can generate the signDesc required to
	// complete the claim process after a delay period.
	htlcSweepScript, err := secondLevelHtlcScript(
		keyRing.revocationKey, keyRing.delayKey, csvDelay,
	)
	if err != nil {
		return nil, err
	}
	htlcScriptHash, err := witnessScriptHash(htlcSweepScript)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): signing with the delay key is wrong for remote
	// commitments
	//  * would instead be signing with local htlc key
	localDelayTweak := SingleTweakBytes(keyRing.commitPoint,
		localChanCfg.DelayBasePoint)
	return &OutgoingHtlcResolution{
		Expiry:          htlc.RefundTimeout,
		SignedTimeoutTx: timeoutTx,
		SweepSignDesc: SignDescriptor{
			PubKey:        localChanCfg.DelayBasePoint,
			SingleTweak:   localDelayTweak,
			WitnessScript: htlcSweepScript,
			Output: &wire.TxOut{
				PkScript: htlcScriptHash,
				Value:    int64(secondLevelOutputAmt),
			},
			HashType: txscript.SigHashAll,
		},
	}, nil
}

// extractHtlcResolutions creates a series of outgoing HTLC resolutions, and
// the local key used when generating the HTLC scrips. This function is to be
// used in two cases: force close, or a unilateral close.
func extractHtlcResolutions(feePerKw btcutil.Amount, ourCommit bool,
	signer Signer, htlcs []channeldb.HTLC, keyRing *commitmentKeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	commitHash chainhash.Hash) ([]OutgoingHtlcResolution, error) {

	dustLimit := remoteChanCfg.DustLimit
	csvDelay := remoteChanCfg.CsvDelay
	if ourCommit {
		dustLimit = localChanCfg.DustLimit
		csvDelay = localChanCfg.CsvDelay
	}

	htlcResolutions := make([]OutgoingHtlcResolution, 0, len(htlcs))
	for _, htlc := range htlcs {
		// Skip any incoming HTLC's, as unless we have the pre-image to
		// spend them, they'll eventually be swept by the party that
		// offered the HTLC after the timeout.
		if htlc.Incoming {
			continue
		}

		// We'll also skip any HTLC's which were dust on the commitment
		// transaction, as these don't have a corresponding output
		// within the commitment transaction.
		if htlcIsDust(htlc.Incoming, ourCommit, feePerKw,
			htlc.Amt.ToSatoshis(), dustLimit) {
			continue
		}

		ohr, err := newHtlcResolution(
			signer, localChanCfg, commitHash, &htlc, keyRing,
			feePerKw, dustLimit, uint32(csvDelay),
		)
		if err != nil {
			return nil, err
		}

		// TODO(roasbeef): needs to point to proper amount including
		htlcResolutions = append(htlcResolutions, *ohr)
	}

	return htlcResolutions, nil
}

// ForceCloseSummary describes the final commitment state before the channel is
// locked-down to initiate a force closure by broadcasting the latest state
// on-chain. The summary includes all the information required to claim all
// rightfully owned outputs.
type ForceCloseSummary struct {
	// ChanPoint is the outpoint that created the channel which has been
	// force closed.
	ChanPoint wire.OutPoint

	// SelfOutpoint is the output created by the above close tx which is
	// spendable by us after a relative time delay.
	SelfOutpoint wire.OutPoint

	// CloseTx is the transaction which closed the channel on-chain. If we
	// initiate the force close, then this'll be our latest commitment
	// state. Otherwise, this'll be the state that the remote peer
	// broadcasted on-chain.
	CloseTx *wire.MsgTx

	// SelfOutputSignDesc is a fully populated sign descriptor capable of
	// generating a valid signature to sweep the self output.
	//
	// NOTE: If the commitment delivery output of the force closing party
	// is below the dust limit, then this will be nil.
	SelfOutputSignDesc *SignDescriptor

	// SelfOutputMaturity is the relative maturity period before the above
	// output can be claimed.
	SelfOutputMaturity uint32

	// HtlcResolutions is a slice of HTLC resolutions which allows the
	// local node to sweep any outgoing HTLC"s after the timeout period has
	// passed.
	HtlcResolutions []OutgoingHtlcResolution
}

// ForceClose executes a unilateral closure of the transaction at the current
// lowest commitment height of the channel. Following a force closure, all
// state transitions, or modifications to the state update logs will be
// rejected. Additionally, this function also returns a ForceCloseSummary which
// includes the necessary details required to sweep all the time-locked within
// the commitment transaction.
//
// TODO(roasbeef): all methods need to abort if in dispute state
// TODO(roasbeef): method to generate CloseSummaries for when the remote peer
// does a unilateral close
func (lc *LightningChannel) ForceClose() (*ForceCloseSummary, error) {
	lc.Lock()
	defer lc.Unlock()

	// Set the channel state to indicate that the channel is now in a
	// contested state.
	lc.status = channelDispute

	commitTx, err := lc.getSignedCommitTx()
	if err != nil {
		return nil, err
	}

	// Re-derive the original pkScript for to-self output within the
	// commitment transaction. We'll need this to find the corresponding
	// output in the commitment transaction and potentially for creating
	// the sign descriptor.
	csvTimeout := uint32(lc.localChanCfg.CsvDelay)
	unusedRevocation, err := lc.channelState.RevocationProducer.AtIndex(
		lc.currentHeight,
	)
	if err != nil {
		return nil, err
	}
	commitPoint := ComputeCommitmentPoint(unusedRevocation[:])
	keyRing := deriveCommitmentKeys(commitPoint, true, lc.localChanCfg,
		lc.remoteChanCfg)
	selfScript, err := commitScriptToSelf(csvTimeout, keyRing.delayKey,
		keyRing.revocationKey)
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := witnessScriptHash(selfScript)
	if err != nil {
		return nil, err
	}

	// Locate the output index of the delayed commitment output back to us.
	// We'll return the details of this output to the caller so they can
	// sweep it once it's mature.
	var (
		delayIndex   uint32
		delayScript  []byte
		selfSignDesc *SignDescriptor
	)
	for i, txOut := range commitTx.TxOut {
		if !bytes.Equal(payToUsScriptHash, txOut.PkScript) {
			continue
		}

		delayIndex = uint32(i)
		delayScript = txOut.PkScript
		break
	}

	localCommitment := lc.channelState.LocalCommitment

	// With the necessary information gathered above, create a new sign
	// descriptor which is capable of generating the signature the caller
	// needs to sweep this output. The hash cache, and input index are not
	// set as the caller will decide these values once sweeping the output.
	// If the output is non-existent (dust), have the sign descriptor be
	// nil.
	if len(delayScript) != 0 {
		singleTweak := SingleTweakBytes(commitPoint,
			lc.localChanCfg.DelayBasePoint)
		localBalance := localCommitment.LocalBalance
		selfSignDesc = &SignDescriptor{
			PubKey:        lc.localChanCfg.DelayBasePoint,
			SingleTweak:   singleTweak,
			WitnessScript: selfScript,
			Output: &wire.TxOut{
				PkScript: delayScript,
				Value:    int64(localBalance.ToSatoshis()),
			},
			HashType: txscript.SigHashAll,
		}
	}

	// Once the delay output has been found (if it exists), then we'll also
	// need to create a series of sign descriptors for any lingering
	// outgoing HTLC's that we'll need to claim as well.
	txHash := commitTx.TxHash()
	htlcResolutions, err := extractHtlcResolutions(
		localCommitment.FeePerKw, true, lc.signer, localCommitment.Htlcs,
		keyRing, lc.localChanCfg, lc.remoteChanCfg, txHash)
	if err != nil {
		return nil, err
	}

	// Finally, close the channel force close signal which notifies any
	// subscribers that the channel has now been forcibly closed. This
	// allows callers to begin to carry out any post channel closure
	// activities.
	close(lc.ForceCloseSignal)

	return &ForceCloseSummary{
		ChanPoint: lc.channelState.FundingOutpoint,
		SelfOutpoint: wire.OutPoint{
			Hash:  commitTx.TxHash(),
			Index: delayIndex,
		},
		CloseTx:            commitTx,
		SelfOutputSignDesc: selfSignDesc,
		SelfOutputMaturity: csvTimeout,
		HtlcResolutions:    htlcResolutions,
	}, nil
}

// CreateCloseProposal is used by both parties in a cooperative channel close
// workflow to generate proposed close transactions and signatures. This method
// should only be executed once all pending HTLCs (if any) on the channel have
// been cleared/removed. Upon completion, the source channel will shift into
// the "closing" state, which indicates that all incoming/outgoing HTLC
// requests should be rejected. A signature for the closing transaction is
// returned.
//
// TODO(roasbeef): caller should initiate signal to reject all incoming HTLCs,
// settle any in flight.
func (lc *LightningChannel) CreateCloseProposal(proposedFee btcutil.Amount,
	localDeliveryScript, remoteDeliveryScript []byte) ([]byte, error) {

	lc.Lock()
	defer lc.Unlock()

	// If we've already closed the channel, then ignore this request.
	if lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, ErrChanClosing
	}

	// Subtract the proposed fee from the appropriate balance, taking care
	// not to persist the adjusted balance, as the feeRate may change
	// during the channel closing process.
	localCommit := lc.channelState.LocalCommitment
	ourBalance := localCommit.LocalBalance.ToSatoshis()
	theirBalance := localCommit.RemoteBalance.ToSatoshis()

	// We'll make sure we account for the complete balance by adding the
	// current dangling commitment fee to the balance of the initiator.
	commitFee := localCommit.CommitFee
	if lc.channelState.IsInitiator {
		ourBalance = ourBalance - proposedFee + commitFee
	} else {
		theirBalance = theirBalance - proposedFee + commitFee
	}

	closeTx := CreateCooperativeCloseTx(lc.fundingTxIn,
		lc.localChanCfg.DustLimit, lc.remoteChanCfg.DustLimit,
		ourBalance, theirBalance, localDeliveryScript,
		remoteDeliveryScript, lc.channelState.IsInitiator)

	// Ensure that the transaction doesn't explicitly violate any
	// consensus rules such as being too big, or having any value with a
	// negative output.
	tx := btcutil.NewTx(closeTx)
	if err := blockchain.CheckTransactionSanity(tx); err != nil {
		return nil, err
	}

	// Finally, sign the completed cooperative closure transaction. As the
	// initiator we'll simply send our signature over to the remote party,
	// using the generated txid to be notified once the closure transaction
	// has been confirmed.
	lc.signDesc.SigHashes = txscript.NewTxSigHashes(closeTx)
	sig, err := lc.signer.SignOutputRaw(closeTx, lc.signDesc)
	if err != nil {
		return nil, err
	}

	// As everything checks out, indicate in the channel status that a
	// channel closure has been initiated.
	lc.status = channelClosing

	return sig, nil
}

// CompleteCooperativeClose completes the cooperative closure of the target
// active lightning channel. A fully signed closure transaction as well as the
// signature itself are returned. Additionally, we also return our final
// settled balance, which reflects any fees we may have paid.
//
// NOTE: The passed local and remote sigs are expected to be fully complete
// signatures including the proper sighash byte.
func (lc *LightningChannel) CompleteCooperativeClose(localSig, remoteSig []byte,
	localDeliveryScript, remoteDeliveryScript []byte,
	proposedFee btcutil.Amount) (*wire.MsgTx, btcutil.Amount, error) {

	lc.Lock()
	defer lc.Unlock()

	// If the channel is already closed, then ignore this request.
	if lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, 0, ErrChanClosing
	}

	// Subtract the proposed fee from the appropriate balance, taking care
	// not to persist the adjusted balance, as the feeRate may change
	// during the channel closing process.
	localCommit := lc.channelState.LocalCommitment
	ourBalance := localCommit.LocalBalance.ToSatoshis()
	theirBalance := localCommit.RemoteBalance.ToSatoshis()

	// We'll make sure we account for the complete balance by adding the
	// current dangling commitment fee to the balance of the initiator.
	commitFee := localCommit.CommitFee
	if lc.channelState.IsInitiator {
		ourBalance = ourBalance - proposedFee + commitFee
	} else {
		theirBalance = theirBalance - proposedFee + commitFee
	}

	// Create the transaction used to return the current settled balance
	// on this active channel back to both parties. In this current model,
	// the initiator pays full fees for the cooperative close transaction.
	closeTx := CreateCooperativeCloseTx(lc.fundingTxIn,
		lc.localChanCfg.DustLimit, lc.remoteChanCfg.DustLimit,
		ourBalance, theirBalance, localDeliveryScript,
		remoteDeliveryScript, lc.channelState.IsInitiator)

	// Ensure that the transaction doesn't explicitly validate any
	// consensus rules such as being too big, or having any value with a
	// negative output.
	tx := btcutil.NewTx(closeTx)
	if err := blockchain.CheckTransactionSanity(tx); err != nil {
		return nil, 0, err
	}
	hashCache := txscript.NewTxSigHashes(closeTx)

	// Finally, construct the witness stack minding the order of the
	// pubkeys+sigs on the stack.
	ourKey := lc.localChanCfg.MultiSigKey.SerializeCompressed()
	theirKey := lc.remoteChanCfg.MultiSigKey.SerializeCompressed()
	witness := SpendMultiSig(lc.signDesc.WitnessScript, ourKey,
		localSig, theirKey, remoteSig)
	closeTx.TxIn[0].Witness = witness

	// Validate the finalized transaction to ensure the output script is
	// properly met, and that the remote peer supplied a valid signature.
	vm, err := txscript.NewEngine(lc.fundingP2WSH, closeTx, 0,
		txscript.StandardVerifyFlags, nil, hashCache,
		int64(lc.channelState.Capacity))
	if err != nil {
		return nil, 0, err
	}
	if err := vm.Execute(); err != nil {
		return nil, 0, err
	}

	// As the transaction is sane, and the scripts are valid we'll mark the
	// channel now as closed as the closure transaction should get into the
	// chain in a timely manner and possibly be re-broadcast by the wallet.
	lc.status = channelClosed

	return closeTx, ourBalance, nil
}

// DeleteState deletes all state concerning the channel from the underlying
// database, only leaving a small summary describing metadata of the
// channel's lifetime.
func (lc *LightningChannel) DeleteState(c *channeldb.ChannelCloseSummary) error {
	return lc.channelState.CloseChannel(c)
}

// AvailableBalance returns the current available balance within the channel.
// By available balance, we mean that if at this very instance s new commitment
// were to be created which evals all the log entries, what would our available
// balance me. This method is useful when deciding if a given channel can
// accept an HTLC in the multi-hop forwarding scenario.
func (lc *LightningChannel) AvailableBalance() lnwire.MilliSatoshi {
	lc.RLock()
	defer lc.RUnlock()

	bal, _ := lc.availableBalance()
	return bal
}

// availableBalance is the private, non mutexed version of AvailableBalance.
// This method is provided so methods that already hold the lock can access
// this method. Additionally, the total weight of the next to be created
// commitment is returned for accounting purposes.
func (lc *LightningChannel) availableBalance() (lnwire.MilliSatoshi, int64) {
	// First, we'll grab the current local balance. If we're the initiator
	// of the channel then we paid the fees on the last commitment state,
	// so we'll re-apply those.
	settledBalance := lc.channelState.LocalCommitment.LocalBalance
	if lc.channelState.IsInitiator {
		settledBalance += lnwire.NewMSatFromSatoshis(
			lc.localCommitChain.tip().fee,
		)
	}

	// Next we'll grab the current set of log updates that are still active
	// and haven't been garbage collected.
	htlcView := lc.fetchHTLCView(lc.remoteUpdateLog.logIndex,
		lc.localUpdateLog.logIndex)
	feePerKw := lc.channelState.LocalCommitment.FeePerKw
	dustLimit := lc.channelState.LocalChanCfg.DustLimit

	// We'll now re-compute the current weight of all the active HTLC's. We
	// make sure to skip any HTLC's that would be dust on our version of
	// the commitment transaction.
	var totalHtlcWeight int64
	for _, htlc := range lc.channelState.LocalCommitment.Htlcs {
		if htlcIsDust(false, true, feePerKw, htlc.Amt.ToSatoshis(),
			dustLimit) {
			continue
		}

		totalHtlcWeight += HtlcWeight
	}

	// Next we'll run through our set of updates and modify the
	// settledBalance and totalHtlcWeight fields accordingly.
	for _, entry := range htlcView.ourUpdates {
		switch {

		// For any new HTLC's added as a part of this state, we'll
		// subtract the total balance, and tally the weight increase if
		// it isn't dust.
		case entry.EntryType == Add && entry.addCommitHeightLocal == 0:
			settledBalance -= entry.Amount

			if htlcIsDust(false, true, feePerKw, entry.Amount.ToSatoshis(),
				dustLimit) {
				continue

			}

			totalHtlcWeight += HtlcWeight

		// For any new HTLC's we newly settled as part of this state,
		// we'll subtract the HTLC weight and increase our balance
		// accordingly.
		case entry.EntryType == Settle && entry.removeCommitHeightLocal == 0:
			totalHtlcWeight -= HtlcWeight

			settledBalance += entry.Amount

		// For any new fails added as a part of this state, we'll
		// subtract the weight of the HTLC we're failing.
		case entry.EntryType == Fail && entry.removeCommitHeightLocal == 0:
			fallthrough
		case entry.EntryType == MalformedFail && entry.removeCommitHeightLocal == 0:
			totalHtlcWeight -= HtlcWeight
		}
	}
	for _, entry := range htlcView.theirUpdates {
		switch {
		// If the remote party has an HTLC that will be included as
		// part of this state, then we'll account for the additional
		// weight of the HTLC.
		case entry.EntryType == Add && entry.addCommitHeightLocal == 0:
			if htlcIsDust(true, true, feePerKw, entry.Amount.ToSatoshis(),
				dustLimit) {
				continue

			}

			totalHtlcWeight += HtlcWeight

		// If the remote party is settling one of our HTLC's for the
		// first time as part of this state, then we'll subtract the
		// weight of the HTLC.
		case entry.EntryType == Settle && entry.removeCommitHeightLocal == 0:
			totalHtlcWeight -= HtlcWeight

		// For any HTLC's that they're failing as a part of the next,
		// state, we'll subtract the weight of the HTLC and also credit
		// ourselves back the value of the HTLC.
		case entry.EntryType == Fail && entry.removeCommitHeightLocal == 0:
			fallthrough
		case entry.EntryType == MalformedFail && entry.removeCommitHeightLocal == 0:
			totalHtlcWeight -= HtlcWeight

			settledBalance += entry.Amount
		}

	}

	// If we subtracted dust HTLC's, then we'll need to reset the weight of
	// the HTLCs back to zero.
	if totalHtlcWeight < 0 {
		totalHtlcWeight = 0
	}

	// If we're the initiator then we need to pay fees for this state, so
	// taking into account the number of active HTLC's we'll calculate the
	// fee that must be paid.
	totalCommitWeight := CommitWeight + totalHtlcWeight
	if lc.channelState.IsInitiator {
		additionalFee := lnwire.NewMSatFromSatoshis(
			btcutil.Amount((int64(feePerKw) * totalCommitWeight) / 1000),
		)

		settledBalance -= additionalFee
	}

	return settledBalance, totalCommitWeight
}

// StateSnapshot returns a snapshot of the current fully committed state within
// the channel.
func (lc *LightningChannel) StateSnapshot() *channeldb.ChannelSnapshot {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.Snapshot()
}

// validateFeeRate ensures that if the passed fee is applied to the channel,
// and a new commitment is created (which evaluates this fee), then the
// initiator of the channel does not dip below their reserve.
func (lc *LightningChannel) validateFeeRate(feePerKw btcutil.Amount) error {
	// We'll ensure that we can accommodate this new fee change, yet still
	// be above our reserve balance. Otherwise, we'll reject the fee
	// update.
	availableBalance, txWeight := lc.availableBalance()

	// Using the weight of the commitment transaction if we were to create
	// a commitment now, we'll compute our remaining balance if we apply
	// this new fee update.
	newFee := lnwire.NewMSatFromSatoshis(
		btcutil.Amount((int64(feePerKw) * txWeight) / 1000),
	)
	balanceAfterFee := availableBalance - newFee

	// If this new balance is below our reserve, then we can't accommodate
	// the fee change, so we'll reject it.
	if balanceAfterFee.ToSatoshis() < lc.channelState.LocalChanCfg.ChanReserve {
		return fmt.Errorf("cannot apply fee_update=%v sat/kw, "+
			"insufficient balance: start=%v, end=%v",
			int64(feePerKw),
			availableBalance, balanceAfterFee)
	}

	// TODO(halseth): should fail if fee update is unreasonable,
	// as specified in BOLT#2.
	//  * COMMENT(roasbeef): can cross-check with our ideal fee rate

	return nil
}

// UpdateFee initiates a fee update for this channel. Must only be called by
// the channel initiator, and must be called before sending update_fee to
// the remote.
func (lc *LightningChannel) UpdateFee(feePerKw btcutil.Amount) error {
	lc.Lock()
	defer lc.Unlock()

	// Only initiator can send fee update, so trying to send one as
	// non-initiator will fail.
	if !lc.channelState.IsInitiator {
		return fmt.Errorf("local fee update as non-initiator")
	}

	// Ensure that the passed fee rate meets our current requirements.
	if err := lc.validateFeeRate(feePerKw); err != nil {
		return err
	}

	lc.pendingFeeUpdate = &feePerKw

	return nil
}

// ReceiveUpdateFee handles an updated fee sent from remote. This method will
// return an error if called as channel initiator.
func (lc *LightningChannel) ReceiveUpdateFee(feePerKw btcutil.Amount) error {
	lc.Lock()
	defer lc.Unlock()

	// Only initiator can send fee update, and we must fail if we receive
	// fee update as initiator
	if lc.channelState.IsInitiator {
		return fmt.Errorf("received fee update as initiator")
	}

	// TODO(roasbeef): or just modify to use the other balance?

	lc.pendingFeeUpdate = &feePerKw

	return nil
}

// generateRevocation generates the revocation message for a given height.
func (lc *LightningChannel) generateRevocation(height uint64) (*lnwire.RevokeAndAck,
	error) {

	// Now that we've accept a new state transition, we send the remote
	// party the revocation for our current commitment state.
	revocationMsg := &lnwire.RevokeAndAck{}
	commitSecret, err := lc.channelState.RevocationProducer.AtIndex(height)
	if err != nil {
		return nil, err
	}
	copy(revocationMsg.Revocation[:], commitSecret[:])

	// Along with this revocation, we'll also send the _next_ commitment
	// point that the remote party should use to create our next commitment
	// transaction. We use a +2 here as we already gave them a look ahead
	// of size one after the FundingLocked message was sent:
	//
	// 0: current revocation, 1: their "next" revocation, 2: this revocation
	//
	// We're revoking the current revocation. Once they receive this
	// message they'll set the "current" revocation for us to their stored
	// "next" revocation, and this revocation will become their new "next"
	// revocation.
	//
	// Put simply in the window slides to the left by one.
	nextCommitSecret, err := lc.channelState.RevocationProducer.AtIndex(
		height + 2,
	)
	if err != nil {
		return nil, err
	}

	revocationMsg.NextRevocationKey = ComputeCommitmentPoint(nextCommitSecret[:])
	revocationMsg.ChanID = lnwire.NewChanIDFromOutPoint(
		&lc.channelState.FundingOutpoint)

	return revocationMsg, nil
}

// CreateCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one paying
// to the "owner" of the commitment transaction which can be spent after a
// relative block delay or revocation event, and the other paying the
// counterparty within the channel, which can be spent immediately.
func CreateCommitTx(fundingOutput wire.TxIn,
	keyRing *commitmentKeyRing, csvTimeout uint32,
	amountToSelf, amountToThem, dustLimit btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	ourRedeemScript, err := commitScriptToSelf(csvTimeout, keyRing.delayKey,
		keyRing.revocationKey)
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := witnessScriptHash(ourRedeemScript)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to them. This is just a regular
	// P2WPKH output, without any added CSV delay.
	theirWitnessKeyHash, err := commitScriptUnencumbered(keyRing.noDelayKey)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself. We use a transaction version of 2 since CSV
	// will fail unless the tx version is >= 2.
	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&fundingOutput)

	// Avoid creating dust outputs within the commitment transaction.
	if amountToSelf >= dustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: payToUsScriptHash,
			Value:    int64(amountToSelf),
		})
	}
	if amountToThem >= dustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: theirWitnessKeyHash,
			Value:    int64(amountToThem),
		})
	}

	return commitTx, nil
}

// CreateCooperativeCloseTx creates a transaction which if signed by both
// parties, then broadcast cooperatively closes an active channel. The creation
// of the closure transaction is modified by a boolean indicating if the party
// constructing the channel is the initiator of the closure. Currently it is
// expected that the initiator pays the transaction fees for the closing
// transaction in full.
func CreateCooperativeCloseTx(fundingTxIn wire.TxIn,
	localDust, remoteDust, ourBalance, theirBalance btcutil.Amount,
	ourDeliveryScript, theirDeliveryScript []byte,
	initiator bool) *wire.MsgTx {

	// Construct the transaction to perform a cooperative closure of the
	// channel. In the event that one side doesn't have any settled funds
	// within the channel then a refund output for that particular side can
	// be omitted.
	closeTx := wire.NewMsgTx(2)
	closeTx.AddTxIn(&fundingTxIn)

	// Create both cooperative closure outputs, properly respecting the
	// dust limits of both parties.
	if ourBalance >= localDust {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: ourDeliveryScript,
			Value:    int64(ourBalance),
		})
	}
	if theirBalance >= remoteDust {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: theirDeliveryScript,
			Value:    int64(theirBalance),
		})
	}

	txsort.InPlaceSort(closeTx)

	return closeTx
}

// CalcFee returns the commitment fee to use for the given
// fee rate (fee-per-kw).
func (lc *LightningChannel) CalcFee(feeRate uint64) uint64 {
	return (feeRate * uint64(CommitWeight)) / 1000
}

// RemoteNextRevocation returns the channelState's RemoteNextRevocation.
func (lc *LightningChannel) RemoteNextRevocation() *btcec.PublicKey {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.RemoteNextRevocation
}

// IsInitiator returns true if we were the ones that initiated the funding
// workflow which led to the creation of this channel. Otherwise, it returns
// false.
func (lc *LightningChannel) IsInitiator() bool {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.IsInitiator
}

// CommitFeeRate returns the current fee rate of the commitment transaction in
// units of sat-per-kw.
func (lc *LightningChannel) CommitFeeRate() btcutil.Amount {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.LocalCommitment.FeePerKw
}

// IsPending returns true if the channel's funding transaction has been fully
// confirmed, and false otherwise.
func (lc *LightningChannel) IsPending() bool {
	lc.RLock()
	defer lc.RUnlock()

	return lc.channelState.IsPending
}

// State provides access to the channel's internal state for testing.
func (lc *LightningChannel) State() *channeldb.OpenChannel {
	return lc.channelState
}

// ObserverQuit returns the quit channel used to coordinate the shutdown of the
// close observer.
func (lc *LightningChannel) ObserverQuit() chan struct{} {
	return lc.observerQuit
}
