package elkrem

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/roasbeef/btcd/wire"
)

/* Serialization and Deserialization methods for the Elkrem structs.
Senders turn into 41 byte long slices.  Receivers are variable length,
with 41 bytes for each stored hash, up to a maximum of 48.  Receivers are
prepended with the total number of hashes, so the total max size is 1969 bytes.
*/

// ToBytes turns the Elkrem Receiver into a bunch of bytes in a slice.
// first the number of nodes (1 byte), then a series of 41 byte long
// serialized nodes, which are 1 byte height, 8 byte index, 32 byte hash.
func (e *ElkremReceiver) ToBytes() ([]byte, error) {
	numOfNodes := uint8(len(e.s))
	// 0 element receiver also OK.  Just an empty slice.
	if numOfNodes == 0 {
		return nil, nil
	}
	if numOfNodes > maxHeight+1 {
		return nil, fmt.Errorf("Broken ElkremReceiver has %d nodes, max 64",
			len(e.s))
	}
	var buf bytes.Buffer // create buffer

	// write number of nodes (1 byte)
	err := binary.Write(&buf, binary.BigEndian, numOfNodes)
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
		if node.sha == nil {
			return nil, fmt.Errorf("node %d has nil hash", node.i)
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
	if buf.Len() != (int(numOfNodes)*41)+1 {
		return nil, fmt.Errorf("Somehow made wrong size buf, got %d expect %d",
			buf.Len(), (numOfNodes*41)+1)
	}
	return buf.Bytes(), nil
}

func ElkremReceiverFromBytes(b []byte) (*ElkremReceiver, error) {
	var e ElkremReceiver
	if len(b) == 0 { // empty receiver, which is OK
		return &e, nil
	}
	buf := bytes.NewBuffer(b)
	// read 1 byte number of nodes stored in receiver
	numOfNodes, err := buf.ReadByte()
	if err != nil {
		return nil, err
	}
	if numOfNodes < 1 || numOfNodes > maxHeight+1 {
		return nil, fmt.Errorf("Read invalid number of nodes: %d", numOfNodes)
	}
	if buf.Len() != (int(numOfNodes) * 41) {
		return nil, fmt.Errorf("Remaining buf wrong size, expect %d got %d",
			(numOfNodes * 41), buf.Len())
	}

	e.s = make([]ElkremNode, numOfNodes)

	for j, _ := range e.s {
		e.s[j].sha = new(wire.ShaHash)
		// read 1 byte height
		err := binary.Read(buf, binary.BigEndian, &e.s[j].h)
		if err != nil {
			return nil, err
		}
		// read 8 byte index
		err = binary.Read(buf, binary.BigEndian, &e.s[j].i)
		if err != nil {
			return nil, err
		}
		// read 32 byte sha hash
		err = e.s[j].sha.SetBytes(buf.Next(32))
		if err != nil {
			return nil, err
		}
		// sanity check.  Note that this doesn't check that index and height
		// match.  Could add that but it's slow.
		if e.s[j].h > maxHeight { // check for super high nodes
			return nil, fmt.Errorf("Read invalid node height %d", e.s[j].h)
		}
		if e.s[j].i > maxIndex { // check for index higher than height allows
			return nil, fmt.Errorf("Node claims index %d; %d max at height %d",
				e.s[j].i, maxIndex, e.s[j].h)
		}

		if j > 0 { // check that node heights are descending
			if e.s[j-1].h < e.s[j].h {
				return nil, fmt.Errorf("Node heights out of order")
			}
		}
	}
	return &e, nil
}

// ToBytes returns the root of the elkrem sender tree as a byte slice. This
// function is in place to allow one to export the root of the tree. However,
// node that if one uses a deterministic procedure to generate the root, then
// serialization isn't necessary as it can simply be re-derived on the fly.
func (e *ElkremSender) ToBytes() []byte {
	return e.root[:]
}
