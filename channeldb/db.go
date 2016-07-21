package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/wire"
)

const (
	dbName = "channel.db"
)

var (
	// Big endian is the preferred byte order, due to cursor scans over integer
	// keys iterating in order.
	byteOrder = binary.BigEndian
)

var bufPool = &sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// EncryptorDecryptor...
// TODO(roasbeef): ability to rotate EncryptorDecryptor's across DB
type EncryptorDecryptor interface {
	Encrypt(in []byte) ([]byte, error)
	Decrypt(in []byte) ([]byte, error)
	OverheadSize() uint32
}

// DB is the primary datastore for the LND daemon. The database stores
// information related to nodes, routing data, open/closed channels, fee
// schedules, and reputation data.
type DB struct {
	store *bolt.DB

	netParams *chaincfg.Params

	cryptoSystem EncryptorDecryptor
}

// Open opens an existing channeldb created under the passed namespace with
// sensitive data encrypted by the passed EncryptorDecryptor implementation.
// TODO(roasbeef): versioning?
func Open(dbPath string, netParams *chaincfg.Params) (*DB, error) {
	path := filepath.Join(dbPath, dbName)

	if !fileExists(path) {
		if err := createChannelDB(dbPath); err != nil {
			return nil, err
		}
	}

	bdb, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	return &DB{store: bdb, netParams: netParams}, nil
}

// RegisterCryptoSystem registers an implementation of the EncryptorDecryptor
// interface for use within the database to encrypt/decrypt sensitive data.
func (d *DB) RegisterCryptoSystem(ed EncryptorDecryptor) {
	d.cryptoSystem = ed
}

// Wipe completely deletes all saved state within all used buckets within the
// database. The deletion is done in a single transaction, therefore this
// operation is fully atomic.
func (d *DB) Wipe() error {
	return d.store.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket(openChannelBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(closedChannelBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		return nil
	})
}

// Close terminates the underlying database handle manually.
func (d *DB) Close() error {
	return d.store.Close()
}

// createChannelDB creates and initializes a fresh version of channeldb. In
// the case that the target path has not yet been created or doesn't yet exist,
// then the path is created. Additionally, all required top-level buckets used
// within the database are created.
func createChannelDB(dbPath string) error {
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return err
		}
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return err
	}

	err = bdb.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucket(openChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(closedChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(channelLogBucket); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to create new channeldb")
	}

	return bdb.Close()
}

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// FetchOpenChannel returns all stored currently active/open channels
// associated with the target nodeID. In the case that no active channels are
// known to have been created with this node, then a zero-length slice is
// returned.
func (d *DB) FetchOpenChannels(nodeID *wire.ShaHash) ([]*OpenChannel, error) {
	var channels []*OpenChannel
	err := d.store.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return nil
		}

		// Within this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		nodeChanBucket := openChanBucket.Bucket(nodeID[:])
		if nodeChanBucket == nil {
			return nil
		}

		// Once we have the node's channel bucket, iterate through each
		// item in the inner chan ID bucket. This bucket acts as an
		// index for all channels we currently have open with this node.
		nodeChanIDBucket := nodeChanBucket.Bucket(chanIDBucket[:])
		err := nodeChanIDBucket.ForEach(func(k, v []byte) error {
			outBytes := bytes.NewReader(k)
			chanID := &wire.OutPoint{}
			if err := readOutpoint(outBytes, chanID); err != nil {
				return err
			}

			oChannel, err := fetchOpenChannel(openChanBucket,
				nodeChanBucket, chanID, d.cryptoSystem)
			if err != nil {
				return err
			}
			oChannel.Db = d

			channels = append(channels, oChannel)
			return nil
		})
		if err != nil {
			return err
		}

		return nil
	})

	return channels, err
}
