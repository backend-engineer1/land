package elkrem

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/roasbeef/btcd/wire"
)

/* Serialization and Deserialization methods for the Elkrem structs.
Senders turn into 41 byte long slices.  Receivers are variable length,
with 41 bytes for each stored hash, up to a maximum of 64.  Receivers are
prepended with the total number of hashes, so the total max size is 2625 bytes.
*/

// ToBytes turns the Elkrem Sender into a 41 byte slice:
// first the tree height (1 byte), then 8 byte index of last sent,
// then the 32 byte root sha hash.
func (e *ElkremSender) ToBytes() ([]byte, error) {
	var buf bytes.Buffer
	// write 1 byte height of tree (size of the whole sender)
	err := binary.Write(&buf, binary.BigEndian, e.treeHeight)
	if err != nil {
		return nil, err
	}
	// write 8 byte index of current sha (last sent)
	err = binary.Write(&buf, binary.BigEndian, e.current)
	if err != nil {
		return nil, err
	}
	// write 32 byte sha hash
	n, err := buf.Write(e.root.Bytes())
	if err != nil {
		return nil, err
	}
	if n != 32 {
		return nil, fmt.Errorf("%d byte hash, expect 32", n)
	}

	return buf.Bytes(), nil
}

// ElkremSenderFromBytes turns a 41 byte slice into a sender, picking up at
// the index where it left off.
func ElkremSenderFromBytes(b []byte) (ElkremSender, error) {
	var e ElkremSender
	e.root = new(wire.ShaHash)
	buf := bytes.NewBuffer(b)
	if buf.Len() != 41 {
		return e, fmt.Errorf("Got %d bytes for sender, expect 41")
	}
	// read 1 byte height
	err := binary.Read(buf, binary.BigEndian, &e.treeHeight)
	if err != nil {
		return e, err
	}
	// read 8 byte index
	err = binary.Read(buf, binary.BigEndian, &e.current)
	if err != nil {
		return e, err
	}
	// read 32 byte sha root
	err = e.root.SetBytes(buf.Next(32))
	if err != nil {
		return e, err
	}
	if e.treeHeight < 1 || e.treeHeight > 63 { // check for super high / low tree
		return e, fmt.Errorf("Read invalid sender tree height %d", e.treeHeight)
	}
	for j := uint8(0); j <= e.treeHeight; j++ {
		e.maxIndex = e.maxIndex<<1 | 1
	}
	e.maxIndex--

	if e.current > e.maxIndex { // check for index higher than height allows
		return e, fmt.Errorf("Sender claims current %d; %d max with height %d",
			e.current, e.maxIndex, e.treeHeight)
	}
	return e, nil
}

// ToBytes turns the Elkrem Receiver into a bunch of bytes in a slice.
// first the tree height (1 byte), then number of nodes (1 byte),
// then a series of 41 byte long serialized nodes,
// which are 1 byte height, 8 byte index, 32 byte hash.
func (e *ElkremReceiver) ToBytes() ([]byte, error) {
	numOfNodes := uint8(len(e.s))
	if numOfNodes == 0 {
		return nil, fmt.Errorf("Can't serialize empty ElkremReceiver")
	}
	if numOfNodes > 64 {
		return nil, fmt.Errorf("Broken ElkremReceiver has %d nodes, max 64",
			len(e.s))
	}
	var buf bytes.Buffer // create buffer
	// write tree height (1 byte)
	err := binary.Write(&buf, binary.BigEndian, e.treeHeight)
	if err != nil {
		return nil, err
	}
	// write number of nodes (1 byte)
	err = binary.Write(&buf, binary.BigEndian, numOfNodes)
	if err != nil {
		return nil, err
	}
	for _, node := range e.s {
		// write 1 byte height
		err = binary.Write(&buf, binary.BigEndian, node.h)
		if err != nil {
			return nil, err
		}
		// write 8 byte index
		err = binary.Write(&buf, binary.BigEndian, node.i)
		if err != nil {
			return nil, err
		}
		// write 32 byte sha hash
		n, err := buf.Write(node.sha.Bytes())
		if err != nil {
			return nil, err
		}
		if n != 32 { // make sure that was 32 bytes
			return nil, fmt.Errorf("%d byte hash, expect 32", n)
		}
	}
	if buf.Len() != (int(numOfNodes)*41)+2 {
		return nil, fmt.Errorf("Somehow made wrong size buf, got %d expect %d",
			buf.Len(), (numOfNodes*41)+2)
	}
	return buf.Bytes(), nil
}

func ElkremReceiverFromBytes(b []byte) (ElkremReceiver, error) {
	var e ElkremReceiver
	var numOfNodes uint8
	buf := bytes.NewBuffer(b)
	// read 1 byte tree height
	err := binary.Read(buf, binary.BigEndian, &e.treeHeight)
	if err != nil {
		return e, err
	}
	if e.treeHeight < 1 || e.treeHeight > 63 {
		return e, fmt.Errorf("Read invalid receiver height: %d", e.treeHeight)
	}
	var max uint64 // maximum possible given height
	for j := uint8(0); j <= e.treeHeight; j++ {
		max = max<<1 | 1
	}
	max--

	// read 1 byte number of nodes stored in receiver
	err = binary.Read(buf, binary.BigEndian, &numOfNodes)
	if err != nil {
		return e, err
	}
	if numOfNodes < 1 || numOfNodes > 64 {
		return e, fmt.Errorf("Read invalid number of nodes: %d", numOfNodes)
	}

	if buf.Len() != (int(numOfNodes) * 41) {
		return e, fmt.Errorf("Remaining buf wrong size, expect %d got %d",
			(numOfNodes * 41), buf.Len())
	}

	for i := uint8(0); i < numOfNodes; i++ {
		var node ElkremNode
		node.sha = new(wire.ShaHash)
		// read 1 byte height
		err := binary.Read(buf, binary.BigEndian, &node.h)
		if err != nil {
			return e, err
		}
		// read 8 byte index
		err = binary.Read(buf, binary.BigEndian, &node.i)
		if err != nil {
			return e, err
		}
		// read 32 byte sha hash
		err = node.sha.SetBytes(buf.Next(32))
		if err != nil {
			return e, err
		}
		// sanity check.  Note that this doesn't check that index and height
		// match.  Could add that but it's slow.
		if node.h > 63 { // check for super high nodes
			return e, fmt.Errorf("Read invalid node height %d", node.h)
		}
		if node.i > max { // check for index higher than height allows
			return e, fmt.Errorf("Node claims index %d; %d max at height %d",
				node.i, max, node.h)
		}
		e.s = append(e.s, node)
		if i > 0 { // check that node heights are descending
			if e.s[i-1].h < e.s[i].h {
				return e, fmt.Errorf("Node heights out of order")
			}
		}
	}
	return e, nil
}
