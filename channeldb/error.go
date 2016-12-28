package channeldb

import "fmt"

var (
	ErrNoChanDBExists    = fmt.Errorf("channel db has not yet been created")
	ErrLinkNodesNotFound = fmt.Errorf("no link nodes exist")

	ErrNoActiveChannels = fmt.Errorf("no active channels exist")
	ErrChannelNoExist   = fmt.Errorf("this channel does not exist")
	ErrNoPastDeltas     = fmt.Errorf("channel has no recorded deltas")

	ErrInvoiceNotFound   = fmt.Errorf("unable to locate invoice")
	ErrNoInvoicesCreated = fmt.Errorf("there are no existing invoices")
	ErrDuplicateInvoice  = fmt.Errorf("invoice with payment hash already exists")

	ErrNoPaymentsCreated = fmt.Errorf("there are no existing payments")

	ErrNodeNotFound = fmt.Errorf("link node with target identity not found")
	ErrMetaNotFound = fmt.Errorf("unable to locate meta information")

	ErrGraphNotFound      = fmt.Errorf("graph bucket not initialized")
	ErrGraphNodesNotFound = fmt.Errorf("no graph nodes exist")
	ErrGraphNoEdgesFound  = fmt.Errorf("no graph edges exist")
	ErrGraphNodeNotFound  = fmt.Errorf("unable to find node")

	ErrEdgeNotFound = fmt.Errorf("edge for chanID not found")

	ErrNodeAliasNotFound = fmt.Errorf("alias for node not found")

	ErrSourceNodeNotSet = fmt.Errorf("source node does not exist")
)
