package lnwire

import (
	"bytes"
	"io"
	"net"

	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/btcec"
)

var (
	startPort    uint16 = 1024
	endPort      uint16 = 49151
	aliasSpecLen        = 21
)

// RGB is used to represent the color.
type RGB struct {
	red   uint8
	green uint8
	blue  uint8
}

// Alias a hex encoded UTF-8 string that may be displayed as an alternative to
// the node's ID. Notice that aliases are not unique and may be freely chosen
// by the node operators.
type Alias struct {
	data     [32]byte
	aliasLen int
}

// NewAlias create the alias from string and also checks spec requirements.
func NewAlias(s string) (Alias, error) {
	data := []byte(s)
	return newAlias(data)
}

func newAlias(data []byte) (Alias, error) {
	var a [32]byte

	rawAlias := data
	if len(data) > aliasSpecLen {
		rawAlias = data[:aliasSpecLen]
	}

	aliasEnd := len(rawAlias)
	for rawAlias[aliasEnd-1] == 0 && aliasEnd > 0 {
		aliasEnd--
	}

	copy(a[:aliasEnd], rawAlias[:aliasEnd])

	return Alias{
		data:     a,
		aliasLen: aliasEnd,
	}, nil
}

func (a *Alias) String() string {
	return string(a.data[:a.aliasLen])
}

// Validate check that alias data length is lower than spec size.
func (a *Alias) Validate() error {
	nonzero := len(a.data)
	for a.data[nonzero-1] == 0 && nonzero > 0 {
		nonzero--
	}

	if nonzero > aliasSpecLen {
		return errors.New("alias should be less then 21 bytes")
	}

	return nil
}

// NodeAnnouncement message is used to announce the presence of a Lightning
// node and also to signal that the node is accepting incoming connections.
// Each NodeAnnouncement authenticating the advertised information within the
// announcement via a signature using the advertised node pubkey.
type NodeAnnouncement struct {
	// Signature is used to prove the ownership of node id.
	Signature *btcec.Signature

	// Timestamp allows ordering in the case of multiple
	// announcements.
	Timestamp uint32

	// NodeID is a public key which is used as node identification.
	NodeID *btcec.PublicKey

	// RGBColor is used to customize their node's appearance in
	// maps and graphs
	RGBColor RGB

	// Alias is used to customize their node's appearance in maps and graphs
	Alias Alias

	// Address includes two specification fields: 'ipv6' and 'port' on which
	// the node is accepting incoming connections.
	Addresses []net.Addr
}

// A compile time check to ensure NodeAnnouncement implements the
// lnwire.Message interface.
var _ Message = (*NodeAnnouncement)(nil)

// Validate performs any necessary sanity checks to ensure all fields present
// on the NodeAnnouncement are valid.
//
// This is part of the lnwire.Message interface.
func (a *NodeAnnouncement) Validate() error {
	// TODO(roasbeef): move validation to discovery service
	return nil
}

// Decode deserializes a serialized NodeAnnouncement stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *NodeAnnouncement) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&a.Signature,
		&a.Timestamp,
		&a.NodeID,
		&a.RGBColor,
		&a.Alias,
		&a.Addresses,
	)
}

// Encode serializes the target NodeAnnouncement into the passed io.Writer
// observing the protocol version specified.
//
func (a *NodeAnnouncement) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.Signature,
		a.Timestamp,
		a.NodeID,
		a.RGBColor,
		a.Alias,
		a.Addresses,
	)
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *NodeAnnouncement) Command() uint32 {
	return CmdNodeAnnoucmentMessage
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *NodeAnnouncement) MaxPayloadLength(pver uint32) uint32 {
	// Signature - 64 bytes
	// Timestamp - 4 bytes
	// NodeID - 33 bytes
	// RGBColor - 3 bytes
	// Alias - 32 bytes
	// NumAddresses - 2 bytes
	// AddressDescriptor - 1 byte
	// Ipv4 - 4 bytes (optional)
	// Ipv6 - 16 bytes (optional)
	// Port - 2 bytes (optional)

	// Base size, 140, but can be variable due to multiple addresses
	return 8192
}

// DataToSign returns the part of the message that should be signed.
func (a *NodeAnnouncement) DataToSign() ([]byte, error) {

	// We should not include the signatures itself.
	var w bytes.Buffer
	err := writeElements(&w,
		a.Timestamp,
		a.NodeID,
		a.RGBColor,
		a.Alias,
		a.Addresses,
	)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}
