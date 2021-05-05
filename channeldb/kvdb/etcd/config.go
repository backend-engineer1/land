package etcd

// Config holds etcd configuration alongside with configuration related to our higher level interface.
type Config struct {
	Embedded bool `long:"embedded" description:"Use embedded etcd instance instead of the external one. Note: use for testing only."`

	EmbeddedClientPort uint16 `long:"embedded_client_port" description:"Client port to use for the embedded instance. Note: use for testing only."`

	EmbeddedPeerPort uint16 `long:"embedded_peer_port" description:"Peer port to use for the embedded instance. Note: use for testing only."`

	Host string `long:"host" description:"Etcd database host."`

	User string `long:"user" description:"Etcd database user."`

	Pass string `long:"pass" description:"Password for the database user."`

	Namespace string `long:"namespace" description:"The etcd namespace to use."`

	DisableTLS bool `long:"disabletls" description:"Disable TLS for etcd connection. Caution: use for development only."`

	CertFile string `long:"cert_file" description:"Path to the TLS certificate for etcd RPC."`

	KeyFile string `long:"key_file" description:"Path to the TLS private key for etcd RPC."`

	InsecureSkipVerify bool `long:"insecure_skip_verify" description:"Whether we intend to skip TLS verification"`

	CollectStats bool `long:"collect_stats" description:"Whether to collect etcd commit stats."`
}
