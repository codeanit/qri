package p2p

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/qri-io/cafs/ipfs"
	"github.com/qri-io/ioes"
	"github.com/qri-io/qri/config"
	"github.com/qri-io/qri/p2p/test"
	"github.com/qri-io/qri/repo"

	net "gx/ipfs/QmPjvxTpVH8qJyQDnxnsxF9kv9jezKD1kozz1hs3fCGsNh/go-libp2p-net"
	libp2p "gx/ipfs/QmY51bqSM5XgxQZqsBrQcRkKTnCb8EKpJpR9K6Qax7Njco/go-libp2p"
	discovery "gx/ipfs/QmY51bqSM5XgxQZqsBrQcRkKTnCb8EKpJpR9K6Qax7Njco/go-libp2p/p2p/discovery"
	connmgr "gx/ipfs/QmYAL9JsqVVPFWwM1ZzHNsofmTzRYQHJ2KqQaBmFJjJsNx/go-libp2p-connmgr"
	ma "gx/ipfs/QmYmsdtJ3HsodkePE3eU3TsCaP2YvPZJ4LoXnNkDE5Tpt7/go-multiaddr"
	pstore "gx/ipfs/QmZR2XWVVBCtbgBWnQhWk2xcQfaR3W8faQPriAiaaj7rsr/go-libp2p-peerstore"
	host "gx/ipfs/Qmb8T6YBBsjYsVGfrihQLfCJveczZnneSBqBKkYEBWDjge/go-libp2p-host"
	peer "gx/ipfs/QmdVrMn1LhB4ybb8hMVaMLXnA8XRSewMnK6YqXKXoTcRvN/go-libp2p-peer"
	crypto "gx/ipfs/Qme1knMqwt1hKZbc1BmQFmnm9f36nyQGwXxPGVpVJ9rMK5/go-libp2p-crypto"
	core "gx/ipfs/QmebqVUQQqQFhg74FtQFszUJo22Vpr3e8qBAkvvV4ho9HH/go-ipfs/core"
)

// QriNode encapsulates a qri peer-2-peer node
type QriNode struct {
	// ID is the node's identifier both locally & on the network
	// Identity has a relationship to privateKey (hash of PublicKey)
	ID peer.ID
	// private key for encrypted communication & verifying identity
	privateKey crypto.PrivKey

	cfg            *config.P2P
	registryConfig *config.Registry

	// base context for this node
	ctx context.Context

	// Online indicates weather this is node is connected to the p2p network
	Online bool
	// Host for p2p connections. can be provided by an ipfs node
	Host host.Host
	// Discovery service, can be provided by an ipfs node
	Discovery discovery.Service

	// Repo is a repository of this node's qri data
	// note that repo's are built upon a cafs.Filestore, which
	// may contain a reference to a functioning IPFS node. In that case
	// QriNode should piggyback non-qri-specific p2p functionality on the
	// ipfs node provided by repo
	Repo repo.Repo

	// handlers maps this nodes registered handlers. This works in a way
	// similary to a router in traditional client/server models, but messages
	// are flying around all over the place instead of a
	// request/response pattern
	handlers map[MsgType]HandlerFunc

	// msgState keeps a "scratch pad" of message IDS & timeouts
	msgState *sync.Map
	// msgChan provides a channel of received messages for others to tune into
	msgChan chan Message
	// receivers is a list of anyone who wants to be notifed on new
	// message arrival
	receivers []chan Message

	// node keeps a set of IOStreams for "node local" io, often to the
	// command line, to give feedback to the user. These may be piped to
	// local http handlers/websockets/stdio, but these streams are meant for
	// local feedback as opposed to p2p connections
	LocalStreams ioes.IOStreams
}

// Assert that conversions needed by the tests are valid.
var _ p2ptest.TestablePeerNode = (*QriNode)(nil)
var _ p2ptest.NodeMakerFunc = NewTestableQriNode

// NewTestableQriNode creates a new node, as a TestablePeerNode, usable by testing utilities.
func NewTestableQriNode(r repo.Repo, cfg *config.Config) (p2ptest.TestablePeerNode, error) {
	return NewQriNode(r, cfg)
}

// NewQriNode creates a new node from a configuration. To get a fully connected
// node that's searching for peers call:
// n, _ := NewQriNode
// n.Connect()
// n.StartOnlineServices()
func NewQriNode(r repo.Repo, cfg *config.Config) (node *QriNode, err error) {
	p2pconf := cfg.P2P
	pid, err := p2pconf.DecodePeerID()
	if err != nil {
		return nil, fmt.Errorf("error decoding peer id: %s", err.Error())
	}

	node = &QriNode{
		ID:             pid,
		cfg:            p2pconf,
		registryConfig: cfg.Registry,
		Repo:           r,
		ctx:            context.Background(),
		msgState:       &sync.Map{},
		msgChan:        make(chan Message),
		// Make sure we always have proper IOStreams, this can be set
		// later
		LocalStreams: ioes.NewDiscardIOStreams(),
	}
	node.handlers = MakeHandlers(node)

	return node, nil
}

// Connect allocates all networking structs to enable QriNode to communicate
// over
func (n *QriNode) Connect() (err error) {
	if !n.cfg.Enabled {
		return fmt.Errorf("p2p connection is disabled")
	}

	if !n.Online {
		// If the underlying content-addressed-filestore is an ipfs
		// node, it has built-in p2p, overlay the qri protocol
		// on the ipfs node's p2p connections.
		if ipfsfs, ok := n.Repo.Store().(*ipfs_filestore.Filestore); ok {
			if !ipfsfs.Online() {
				if err := ipfsfs.GoOnline(); err != nil {
					return err
				}
			}

			ipfsnode := ipfsfs.Node()
			if ipfsnode.PeerHost != nil {
				n.Host = ipfsnode.PeerHost
				// fmt.Println("ipfs host muxer:")
				// ipfsnode.PeerHost.Mux().Ls(os.Stderr)
			}

			if ipfsnode.Discovery != nil {
				n.Discovery = ipfsnode.Discovery
			}
		} else if n.Host == nil {
			ps := pstore.NewPeerstore()
			n.Host, err = makeBasicHost(n.ctx, ps, n.cfg)
			if err != nil {
				return fmt.Errorf("error creating host: %s", err.Error())
			}
		}

		// add multistream handler for qri protocol to the host
		// for more info on multistreams check github.com/multformats/go-multistream
		n.Host.SetStreamHandler(QriProtocolID, n.QriStreamHandler)

		p, err := n.Repo.Profile()
		if err != nil {
			log.Errorf("error getting repo profile: %s\n", err.Error())
			return err
		}
		p.PeerIDs = []peer.ID{n.Host.ID()}
		// add listen addresses to profile store
		// if addrs, err := node.ListenAddresses(); err == nil {
		// 	if p.Addresses == nil {
		// 		p.Addresses = []string{fmt.Sprintf("/ipfs/%s", node.Host.ID().Pretty())}
		// 	}
		// }

		// update profile with our p2p addresses
		if err := n.Repo.SetProfile(p); err != nil {
			return err
		}

		n.Online = true
		go n.echoMessages()
	}
	return nil
}

// StartOnlineServices bootstraps the node to qri & IPFS networks
// and begins NAT discovery
func (n *QriNode) StartOnlineServices(bootstrapped func(string)) error {
	if !n.Online {
		return nil
	}

	bsPeers := make(chan pstore.PeerInfo, len(n.cfg.BootstrapAddrs))
	// need a call here to ensure boostrapped is called at least once
	// TODO - this is an "original node" problem probably solved by being able
	// to start a node with *no* qri peers specified.
	defer bootstrapped("")

	go func() {
		pInfo := <-bsPeers
		bootstrapped(pInfo.ID.Pretty())

		if err := n.AnnounceConnected(); err != nil {
			log.Infof("error announcing connected: %s", err.Error())
		}
	}()

	return n.StartDiscovery(bsPeers)
}

// ReceiveMessages adds a listener for newly received messages
func (n *QriNode) ReceiveMessages() chan Message {
	r := make(chan Message)
	n.receivers = append(n.receivers, r)
	return r
}

func (n *QriNode) echoMessages() {
	for {
		msg := <-n.msgChan
		for _, r := range n.receivers {
			r <- msg
		}
	}
}

// IPFSNode returns the underlying IPFS node if this Qri Node is running on IPFS
func (n *QriNode) IPFSNode() (*core.IpfsNode, error) {
	if ipfsfs, ok := n.Repo.Store().(*ipfs_filestore.Filestore); ok {
		return ipfsfs.Node(), nil
	}
	return nil, fmt.Errorf("not using IPFS")
}

// ListenAddresses gives the listening addresses of this node on the p2p network as
// a slice of strings
func (n *QriNode) ListenAddresses() ([]string, error) {
	maddrs := n.EncapsulatedAddresses()
	addrs := make([]string, len(maddrs))
	for i, maddr := range maddrs {
		addrs[i] = maddr.String()
	}
	return addrs, nil
}

// EncapsulatedAddresses returns a slice of full multaddrs for this node
func (n *QriNode) EncapsulatedAddresses() []ma.Multiaddr {
	// Build host multiaddress
	hostAddr, err := ma.NewMultiaddr(fmt.Sprintf("/ipfs/%s", n.Host.ID().Pretty()))
	if err != nil {
		fmt.Println(err.Error())
		return nil
	}

	res := make([]ma.Multiaddr, len(n.Host.Addrs()))
	for i, a := range n.Host.Addrs() {
		res[i] = a.Encapsulate(hostAddr)
	}

	return res
}

// Context returns this node's context
func (n *QriNode) Context() context.Context {
	if n.ctx == nil {
		n.ctx = context.Background()
	}
	return n.ctx
}

// RegistryLocation returns the location/uri of the registry associated with this node
func (n *QriNode) RegistryLocation() string {
	if n.registryConfig == nil {
		return ""
	}
	return n.registryConfig.Location
}

// setRegistryLocation sets the registry location/uri of the registyr associated with this node, convenience function for testing purposes
func (n *QriNode) setRegistryLocation(loc string) {
	if n.registryConfig == nil {
		n.registryConfig = &config.Registry{Location: loc}
		return
	}
	n.registryConfig.Location = loc
}

// TODO - finish. We need a proper termination & cleanup process
// func (n *QriNode) Close() error {
// 	if node, err := n.IPFSNode(); err == nil {
// 		return node.Close()
// 	}
// }

// makeBasicHost creates a LibP2P host from a NodeCfg
func makeBasicHost(ctx context.Context, ps pstore.Peerstore, p2pconf *config.P2P) (host.Host, error) {
	pk, err := p2pconf.DecodePrivateKey()
	if err != nil {
		return nil, err
	}

	pid, err := p2pconf.DecodePeerID()
	if err != nil {
		return nil, err
	}

	ps.AddPrivKey(pid, pk)
	ps.AddPubKey(pid, pk.GetPublic())

	opts := []libp2p.Option{
		libp2p.ListenAddrs(p2pconf.Addrs...),
		libp2p.Identity(pk),
		libp2p.Peerstore(ps),
	}

	// Let's talk about these options a bit. Most of the time, we will never
	// follow the code path that takes us to makeBasicHost. Usually, we will be
	// using the Host that comes with the ipfs node. But, let's say we want to not
	// use that ipfs host, or, we are in a testing situation, we will need to
	// create our own host. If we do not explicitly pass the host the options
	// for a ConnManager, it will use the NullConnManager, which doesn't actually
	// tag or manage any conns.
	// So instead, we pass in the libp2p basic ConnManager:
	opts = append(opts, libp2p.ConnectionManager(connmgr.NewConnManager(1000, 0, time.Millisecond)))

	return libp2p.New(ctx, opts...)
}

// SendMessage opens a stream & sends a message from p to one ore more peerIDs
func (n *QriNode) SendMessage(msg Message, replies chan Message, pids ...peer.ID) error {
	for _, peerID := range pids {
		if peerID == n.ID {
			// can't send messages to yourself, silly
			continue
		}

		s, err := n.Host.NewStream(n.Context(), peerID, QriProtocolID)
		if err != nil {
			return fmt.Errorf("error opening stream: %s", err.Error())
		}
		defer s.Close()

		ws := WrapStream(s)
		go n.handleStream(ws, replies)
		if err := ws.sendMessage(msg); err != nil {
			return err
		}
	}

	return nil
}

// QriStreamHandler is the handler we register with the multistream muxer
func (n *QriNode) QriStreamHandler(s net.Stream) {
	// defer s.Close()
	n.handleStream(WrapStream(s), nil)
}

// handleStream is a for loop which receives and handles messages
// When Message.HangUp is true, it exits. This will close the stream
// on one of the sides. The other side's receiveMessage() will error
// with EOF, thus also breaking out from the loop.
func (n *QriNode) handleStream(ws *WrappedStream, replies chan Message) {
	for {
		// Loop forever, receiving messages until the other end hangs up
		// or something goes wrong
		msg, err := ws.receiveMessage()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			log.Debugf("error receiving message: %s", err.Error())
			break
		}

		if replies != nil {
			go func() { replies <- msg }()
		}
		go func() {
			n.msgChan <- msg
		}()

		handler, ok := n.handlers[msg.Type]
		if !ok {
			log.Infof("peer %s sent unrecognized message type '%s', hanging up", n.ID, msg.Type)
			break
		}

		if hangup := handler(ws, msg); hangup {
			break
		}
	}

	ws.stream.Close()
}

// Keys returns the KeyBook for the node.
func (n *QriNode) Keys() pstore.KeyBook {
	return n.Host.Peerstore()
}

// Addrs returns the AddrBook for the node.
func (n *QriNode) Addrs() pstore.AddrBook {
	return n.Host.Peerstore()
}

// SimplePeerInfo returns a PeerInfo with just the ID and Addresses.
func (n *QriNode) SimplePeerInfo() pstore.PeerInfo {
	return pstore.PeerInfo{
		ID:    n.Host.ID(),
		Addrs: n.Host.Addrs(),
	}
}

// AddPeer adds a Qri peer to this node.
func (n *QriNode) AddPeer(peer pstore.PeerInfo) error {
	return n.AddQriPeer(peer)
}

// HostNetwork returns the Host's Network for the node.
func (n *QriNode) HostNetwork() net.Network {
	return n.Host.Network()
}

// MakeHandlers generates a map of MsgTypes to their corresponding handler functions
func MakeHandlers(n *QriNode) map[MsgType]HandlerFunc {
	return map[MsgType]HandlerFunc{
		MtPing:              n.handlePing,
		MtProfile:           n.handleProfile,
		MtDatasetInfo:       n.handleDataset,
		MtDatasets:          n.handleDatasetsList,
		MtEvents:            n.handleEvents,
		MtConnected:         n.handleConnected,
		MtResolveDatasetRef: n.handleResolveDatasetRef,
		MtDatasetLog:        n.handleDatasetLog,
		MtQriPeers:          n.handleQriPeers,
	}
}
