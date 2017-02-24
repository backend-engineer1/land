package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

// FundingLocked is the message that both parties to a new channel creation
// send once they have observed the funding transaction being confirmed on
// the blockchain. FundingLocked contains the signatures necessary for the
// channel participants to advertise the existence of the channel to the
// rest of the network.
type FundingLocked struct {
	// ChannelOutpoint is the outpoint of the channel's funding
	// transaction. This can be used to query for the channel in the
	// database.
	ChannelOutpoint wire.OutPoint

	// ChannelId serves to uniquely identify the channel created by the
	// current channel funding workflow.
	ChannelID ChannelID

	// NextPerCommitmentPoint is the secret that can be used to revoke
	// the next commitment transaction for the channel.
	NextPerCommitmentPoint *btcec.PublicKey
}

// NewFundingLocked creates a new FundingLocked message, populating it with
// the necessary IDs and revocation secret..
func NewFundingLocked(op wire.OutPoint, cid ChannelID,
	npcp *btcec.PublicKey) *FundingLocked {
	return &FundingLocked{
		ChannelOutpoint:        op,
		ChannelID:              cid,
		NextPerCommitmentPoint: npcp,
	}
}

// A compile time check to ensure FundingLocked implements the
// lnwire.Message interface.
var _ Message = (*FundingLocked)(nil)

// Decode deserializes the serialized FundingLocked message stored in the passed
// io.Reader into the target FundingLocked using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) Decode(r io.Reader, pver uint32) error {
	err := readElements(r,
		&c.ChannelOutpoint,
		&c.ChannelID,
		&c.NextPerCommitmentPoint)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target FundingLocked message into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelOutpoint,
		c.ChannelID,
		c.NextPerCommitmentPoint)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the uint32 code which uniquely identifies this message as a
// FundingLocked message on the wire.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) Command() uint32 {
	return CmdFundingLocked
}

// MaxPayloadLength returns the maximum allowed payload length for a
// FundingLocked message. This is calculated by summing the max length of all
// the fields within a FundingLocked message.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) MaxPayloadLength(uint32) uint32 {
	var length uint32

	// ChannelOutpoint - 36 bytes
	length += 36

	// ChannelID - 8 bytes
	length += 8

	// NextPerCommitmentPoint - 33 bytes
	length += 33

	return length
}

// Validate examines each populated field within the FundingLocked message for
// field sanity. For example, signature fields MUST NOT be nil.
//
// This is part of the lnwire.Message interface.
func (c *FundingLocked) Validate() error {
	if c.NextPerCommitmentPoint == nil {
		return fmt.Errorf("The next per commitment point must be non-nil.")
	}

	// We're good!
	return nil
}
