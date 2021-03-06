package deks

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/simia-tech/conflux/recon"
	"github.com/simia-tech/errx"
	redisserver "github.com/tidwall/redcon"
)

const (
	cmdHelp         = "help"
	cmdQuit         = "quit"
	cmdPing         = "ping"
	cmdSet          = "set"
	cmdGet          = "get"
	cmdDelete       = "del"
	cmdKeys         = "keys"
	cmdPeerAdd      = "padd"
	cmdPeerRemove   = "pdel"
	cmdPeerList     = "plist"
	cmdTidy         = "tidy"
	cmdSetContainer = "cset"        // hidden
	cmdGetContainer = "cget"        // hidden
	cmdReconcilate  = "reconcilate" // hidden

	help = `Supported commands:
help                                            - prints this help message
set <key> <value>                               - sets <value> at <key>
get <key>                                       - returns value at <key>
del <key>                                       - removes value at <key>
keys                                            - returns all keys
padd <url> <ping interval> <reconnect interval> - adds a peer with <url>
pdel <url>                                      - removes the peer with <url>
plist                                           - returns all peer urls
tidy                                            - cleans up the store
quit                                            - closes the connection
`
)

// Server defines a server.
type Server struct {
	store        *Store
	listener     net.Listener
	metric       Metric
	peer         *recon.Peer
	streams      map[string]*stream
	streamsMutex sync.RWMutex
}

// NewServer returns a new server.
func NewServer(store *Store, listenURL string, m Metric) (*Server, error) {
	network, address, err := parseURL(listenURL)
	if err != nil {
		return nil, errx.Annotatef(err, "parse listen url [%s]", listenURL)
	}

	l, err := net.Listen(network, address)
	if err != nil {
		return nil, errx.Annotatef(err, "listen [%s %s]", network, address)
	}

	settings := recon.DefaultSettings()
	peer := recon.NewPeer(settings, store.State().prefixTree())

	s := &Server{
		store:    store,
		listener: l,
		metric:   m,
		peer:     peer,
		streams:  make(map[string]*stream, 0),
	}
	store.updateFn = s.update
	go s.acceptLoop()
	return s, nil
}

// ListenURL returns the url of the listener.
func (s *Server) ListenURL() string {
	addr := s.listener.Addr()
	return fmt.Sprintf("%s://%s", addr.Network(), addr.String())
}

// Close tears down the node.
func (s *Server) Close() error {
	for _, stream := range s.streams {
		stream.close()
	}
	if err := s.listener.Close(); err != nil {
		if isClosedNetworkError(err) {
			return nil
		}
		return errx.Annotatef(err, "close listener")
	}
	return nil
}

// AddPeer adds another node as a target for updates.
func (s *Server) AddPeer(
	peerURL string,
	peerPingInterval time.Duration,
	peerReconnectInterval time.Duration,
) error {
	s.streamsMutex.Lock()
	if _, ok := s.streams[peerURL]; ok {
		s.streamsMutex.Unlock()
		return errx.AlreadyExistsf("peer with url [%s] already exists", peerURL)
	}
	s.streams[peerURL] = newStream(peerURL, peerPingInterval, peerReconnectInterval, s.metric)
	s.streamsMutex.Unlock()
	return nil
}

// RemovePeer removes the peer with the provided url.
func (s *Server) RemovePeer(peerURL string) error {
	s.streamsMutex.Lock()
	stream, ok := s.streams[peerURL]
	if !ok {
		s.streamsMutex.Unlock()
		return errx.NotFoundf("no peer with url [%s] found", peerURL)
	}
	stream.close()
	delete(s.streams, peerURL)
	s.streamsMutex.Unlock()
	return nil
}

// PeerURLs returns a slice of strings containing all peers urls.
func (s *Server) PeerURLs() []string {
	result := []string{}
	s.streamsMutex.RLock()
	for peerURL := range s.streams {
		result = append(result, peerURL)
	}
	s.streamsMutex.RUnlock()
	return result
}

// Reconcilate performs a reconsiliation with the node at the provided address.
func (s *Server) Reconcilate(url string) (int, error) {
	conn, err := Dial(url)
	if err != nil {
		return 0, errx.Annotatef(err, "dial [%s]", url)
	}
	defer conn.Close()

	netConn, err := conn.Reconsilate()
	if err != nil {
		return 0, errx.Annotatef(err, "reconcilate")
	}

	keyHashes, _, err := s.peer.Reconcilate(netConn, 100)
	if err != nil {
		return 0, errx.Annotatef(err, "reconcilate")
	}

	payloadConn, err := Dial(url)
	if err != nil {
		return 0, errx.Annotatef(err, "dial [%s]", url)
	}
	defer payloadConn.Close()

	for _, keyHash := range keyHashes {
		kh := newKeyHash(keyHash)
		c, err := payloadConn.getContainer(kh)
		if err != nil {
			return 0, errx.Annotatef(err, "get container")
		}
		if err := s.store.setContainer(kh, c); err != nil {
			return 0, errx.Annotatef(err, "set container")
		}
	}

	return len(keyHashes), nil
}

func (s *Server) acceptLoop() {
	done := false
	var err error
	for !done {
		done, err = s.accept()
		if err != nil {
			log.Printf("accept loop: %v", err)
			done = true
		}
	}
}

func (s *Server) accept() (bool, error) {
	conn, err := s.listener.Accept()
	if err != nil {
		if isClosedNetworkError(err) {
			return true, nil
		}
		return true, errx.Annotatef(err, "accept")
	}

	clientURL := urlFor(conn.RemoteAddr())

	go func() {
		if err := s.handleConn(conn); err != nil {
			log.Printf("conn %s: %v", conn.RemoteAddr(), err)
		}
		if err := conn.Close(); err != nil {
			if !isClosedNetworkError(err) {
				log.Printf("close conn %s: %v", conn.RemoteAddr(), err)
			}
		}
		s.metric.ClientDisconnected(clientURL)
	}()

	s.metric.ClientConnected(clientURL)

	return false, nil
}

func (s *Server) handleConn(conn net.Conn) error {
	r := redisserver.NewReader(conn)
	w := redisserver.NewWriter(conn)

	done := false
	for !done {
		cmd, err := r.ReadCommand()
		if err == io.EOF {
			done = true
			continue
		}
		if err != nil {
			return errx.Annotatef(err, "read command")
		}

		command := strings.ToLower(string(cmd.Args[0]))
		arguments := cmd.Args[1:]

		switch command {
		case cmdHelp:
			w.WriteBulkString(help)
		case cmdQuit:
			done = true
			w.WriteString("OK")
		case cmdPing:
			w.WriteString("OK")
		case cmdSet:
			if err := s.store.Set(arguments[0], arguments[1]); err != nil {
				return errx.Annotatef(err, "set")
			}
			w.WriteString("OK")
		case cmdGet:
			value, err := s.store.Get(arguments[0])
			if err != nil {
				return errx.Annotatef(err, "get [%s]", arguments[0])
			}
			w.WriteBulk(value)
		case cmdDelete:
			if err := s.store.Delete(arguments[0]); err != nil {
				return errx.Annotatef(err, "delete [%s]", arguments[0])
			}
			w.WriteString("OK")
		case cmdKeys:
			w.WriteArray(s.store.Len())
			s.store.Each(func(key, _ []byte) error {
				w.WriteBulk(key)
				return nil
			})
		case cmdPeerAdd:
			pingInterval, err := time.ParseDuration(string(arguments[1]))
			if err != nil {
				return errx.Annotatef(err, "parse duration [%s]", arguments[1])
			}
			reconnectInterval, err := time.ParseDuration(string(arguments[2]))
			if err != nil {
				return errx.Annotatef(err, "parse duration [%s]", arguments[2])
			}
			if err := s.AddPeer(string(arguments[0]), pingInterval, reconnectInterval); err != nil {
				return errx.Annotatef(err, "peer add [%s %s %s]", arguments[0], pingInterval, reconnectInterval)
			}
			w.WriteString("OK")
		case cmdPeerRemove:
			if err := s.RemovePeer(string(arguments[0])); err != nil {
				return errx.Annotatef(err, "peer remove [%s]", arguments[0])
			}
			w.WriteString("OK")
		case cmdPeerList:
			peerURLs := s.PeerURLs()
			w.WriteArray(len(peerURLs))
			for _, peerURL := range peerURLs {
				w.WriteString(peerURL)
			}
		case cmdTidy:
			if err := s.store.Tidy(); err != nil {
				return errx.Annotatef(err, "tidy")
			}
			w.WriteString("OK")
		case cmdSetContainer:
			kh := keyHash{}
			copy(kh[:], arguments[0][:keyHashSize])
			if err := s.store.setContainer(kh, arguments[1]); err != nil {
				return errx.Annotatef(err, "set container [%s]", kh)
			}
			w.WriteString("OK")
		case cmdGetContainer:
			kh := keyHash{}
			copy(kh[:], arguments[0][:keyHashSize])
			c, err := s.store.getContainer(kh)
			if err != nil {
				return errx.Annotatef(err, "get container [%s]", kh)
			}
			w.WriteBulk(c)
		case cmdReconcilate:
			w.WriteString("OK")
			if err := w.Flush(); err != nil {
				return errx.Annotatef(err, "flush")
			}
			if err := s.peer.Accept(conn); err != nil {
				return errx.Annotatef(err, "recon accept")
			}
			return nil // exit command loop
		default:
			w.WriteError(fmt.Sprintf("unknown command [%s]", command))
		}

		if err := w.Flush(); err != nil {
			return errx.Annotatef(err, "flush")
		}
	}

	return nil
}

func (s *Server) update(kh keyHash, container *container) {
	s.streamsMutex.RLock()
	for _, stream := range s.streams {
		stream.update(kh, container)
	}
	s.streamsMutex.RUnlock()
}

func parseURL(u string) (string, string, error) {
	url, err := url.Parse(u)
	if err != nil {
		return "", "", errx.Annotatef(err, "parse url [%s]", u)
	}
	return url.Scheme, url.Host, nil
}

func urlFor(addr net.Addr) string {
	return fmt.Sprintf("%s://%s", addr.Network(), addr.String())
}

func isClosedNetworkError(err error) bool {
	if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "use of closed network connection" {
		return true
	}
	return false
}
