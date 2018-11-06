package edkvs

import (
	"log"
	"net"
	"strings"

	"github.com/simia-tech/conflux/recon"
	"github.com/simia-tech/errx"
	redisserver "github.com/tidwall/redcon"
)

const (
	cmdGetItem     = "iget"
	cmdReconcilate = "reconcilate"
)

type Node struct {
	store    *Store
	listener net.Listener
	peer     *recon.Peer
}

func NewNode(store *Store, network, address string) (*Node, error) {
	l, err := net.Listen(network, address)
	if err != nil {
		return nil, errx.Annotatef(err, "listen [%s %s]", network, address)
	}
	// log.Printf("node is listening at [%s %s]", l.Addr().Network(), l.Addr().String())

	settings := recon.DefaultSettings()
	peer := recon.NewPeer(settings, store.State().prefixTree())

	n := &Node{
		store:    store,
		listener: l,
		peer:     peer,
	}
	go n.acceptLoop()
	return n, nil
}

func (n *Node) Addr() net.Addr {
	return n.listener.Addr()
}

func (n *Node) Close() error {
	return n.listener.Close()
}

func (n *Node) Reconcilate(network, address string) (int, error) {
	conn, err := Dial(network, address)
	if err != nil {
		return 0, errx.Annotatef(err, "dial [%s %s]", network, address)
	}
	defer conn.Close()

	netConn, err := conn.Reconsilate()
	if err != nil {
		return 0, errx.Annotatef(err, "reconcilate")
	}

	keyHashes, _, err := n.peer.Reconcilate(netConn, 100)
	if err != nil {
		return 0, errx.Annotatef(err, "reconcilate")
	}

	payloadConn, err := Dial(network, address)
	if err != nil {
		return 0, errx.Annotatef(err, "dial [%s %s]", network, address)
	}
	defer payloadConn.Close()

	for _, keyHash := range keyHashes {
		kh := newKeyHash(keyHash)
		item, err := payloadConn.getItem(kh)
		if err != nil {
			return 0, errx.Annotatef(err, "get item")
		}
		if err := n.store.setItem(kh, item); err != nil {
			return 0, errx.Annotatef(err, "set item")
		}
	}

	return len(keyHashes), nil
}

func (n *Node) acceptLoop() {
	done := false
	err := error(nil)
	for !done {
		done, err = n.accept()
		if err != nil {
			log.Printf("accept loop: %v", err)
			done = true
		}
	}
}

func (n *Node) accept() (bool, error) {
	conn, err := n.listener.Accept()
	if err != nil {
		if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "use of closed network connection" {
			return true, nil
		}
		return true, errx.Annotatef(err, "accept")
	}

	go func() {
		if err := n.handleConn(conn); err != nil {
			log.Printf("conn %s: %v", conn.RemoteAddr(), err)
		}
	}()

	return false, nil
}

func (n *Node) handleConn(conn net.Conn) error {
	r := redisserver.NewReader(conn)
	w := redisserver.NewWriter(conn)

	cmd, err := r.ReadCommand()
	if err != nil {
		return errx.Annotatef(err, "read command")
	}

	switch strings.ToLower(string(cmd.Args[0])) {
	case cmdGetItem:
		kh := keyHash{}
		copy(kh[:], cmd.Args[1][:keyHashSize])
		item, err := n.store.getItem(kh)
		if err != nil {
			return errx.Annotatef(err, "get item [%s]", kh)
		}
		w.WriteBulk(item)
		if err := w.Flush(); err != nil {
			return errx.Annotatef(err, "flush")
		}
	case cmdReconcilate:
		w.WriteString("OK")
		if err := w.Flush(); err != nil {
			return errx.Annotatef(err, "flush")
		}
		if err := n.peer.Accept(conn); err != nil {
			return errx.Annotatef(err, "recon accept")
		}
	}

	return nil
}
