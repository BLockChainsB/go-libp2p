package relay_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	config "github.com/libp2p/go-libp2p/config"
	relay "github.com/libp2p/go-libp2p/p2p/host/relay"

	ggio "github.com/gogo/protobuf/io"
	cid "github.com/ipfs/go-cid"
	autonat "github.com/libp2p/go-libp2p-autonat"
	autonatpb "github.com/libp2p/go-libp2p-autonat/pb"
	circuit "github.com/libp2p/go-libp2p-circuit"
	host "github.com/libp2p/go-libp2p-host"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	routing "github.com/libp2p/go-libp2p-routing"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr-net"
)

// test specific parameters
func init() {
	autonat.AutoNATIdentifyDelay = 10 * time.Millisecond
	autonat.AutoNATBootDelay = 1 * time.Second
	relay.BootDelay = 1 * time.Second
	manet.Private4 = []*net.IPNet{}
}

// mock routing
type mockRoutingTable struct {
	mx        sync.Mutex
	providers map[string]map[peer.ID]pstore.PeerInfo
}

type mockRouting struct {
	h   host.Host
	tab *mockRoutingTable
}

func newMockRoutingTable() *mockRoutingTable {
	return &mockRoutingTable{providers: make(map[string]map[peer.ID]pstore.PeerInfo)}
}

func newMockRouting(h host.Host, tab *mockRoutingTable) *mockRouting {
	return &mockRouting{h: h, tab: tab}
}

func (m *mockRouting) FindPeer(ctx context.Context, p peer.ID) (pstore.PeerInfo, error) {
	return pstore.PeerInfo{}, routing.ErrNotFound
}

func (m *mockRouting) Provide(ctx context.Context, cid cid.Cid, bcast bool) error {
	m.tab.mx.Lock()
	defer m.tab.mx.Unlock()

	pmap, ok := m.tab.providers[cid.String()]
	if !ok {
		pmap = make(map[peer.ID]pstore.PeerInfo)
		m.tab.providers[cid.String()] = pmap
	}

	pmap[m.h.ID()] = pstore.PeerInfo{ID: m.h.ID(), Addrs: m.h.Addrs()}

	return nil
}

func (m *mockRouting) FindProvidersAsync(ctx context.Context, cid cid.Cid, limit int) <-chan pstore.PeerInfo {
	ch := make(chan pstore.PeerInfo)
	go func() {
		defer close(ch)
		m.tab.mx.Lock()
		defer m.tab.mx.Unlock()

		pmap, ok := m.tab.providers[cid.String()]
		if !ok {
			return
		}

		for _, pi := range pmap {
			select {
			case ch <- pi:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch
}

// mock autonat
func makeAutoNATServicePrivate(ctx context.Context, t *testing.T) host.Host {
	h, err := libp2p.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.SetStreamHandler(autonat.AutoNATProto, sayAutoNATPrivate)
	return h
}

func sayAutoNATPrivate(s inet.Stream) {
	defer s.Close()
	w := ggio.NewDelimitedWriter(s)
	res := autonatpb.Message{
		Type:         autonatpb.Message_DIAL_RESPONSE.Enum(),
		DialResponse: newDialResponseError(autonatpb.Message_E_DIAL_ERROR, "no dialable addresses"),
	}
	w.WriteMsg(&res)
}

func newDialResponseError(status autonatpb.Message_ResponseStatus, text string) *autonatpb.Message_DialResponse {
	dr := new(autonatpb.Message_DialResponse)
	dr.Status = status.Enum()
	dr.StatusText = &text
	return dr
}

// connector
func connect(t *testing.T, a, b host.Host) {
	pinfo := pstore.PeerInfo{ID: a.ID(), Addrs: a.Addrs()}
	err := b.Connect(context.Background(), pinfo)
	if err != nil {
		t.Fatal(err)
	}
}

// and the actual test!
func TestAutoRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mtab := newMockRoutingTable()
	makeRouting := func(h host.Host) (config.Routing, error) {
		mr := newMockRouting(h, mtab)
		return mr, nil
	}

	h1 := makeAutoNATServicePrivate(ctx, t)
	_, err := libp2p.New(ctx, libp2p.EnableRelay(circuit.OptHop), libp2p.Routing(makeRouting))
	if err != nil {
		t.Fatal(err)
	}
	h3, err := libp2p.New(ctx, libp2p.EnableRelay(), libp2p.Routing(makeRouting))
	if err != nil {
		t.Fatal(err)
	}
	h4, err := libp2p.New(ctx, libp2p.EnableRelay())

	connect(t, h1, h3)
	time.Sleep(3 * time.Second)

	// verify that we advertise relay addrs
	haveRelay := false
	for _, addr := range h3.Addrs() {
		_, err := addr.ValueForProtocol(circuit.P_CIRCUIT)
		if err == nil {
			haveRelay = true
			break
		}
	}

	if !haveRelay {
		t.Fatal("No relay addrs advertised")
	}

	// check that we can connect through the relay
	var raddrs []ma.Multiaddr
	for _, addr := range h3.Addrs() {
		_, err := addr.ValueForProtocol(circuit.P_CIRCUIT)
		if err == nil {
			raddrs = append(raddrs, addr)
		}
	}

	err = h4.Connect(ctx, pstore.PeerInfo{h3.ID(), raddrs})
	if err != nil {
		t.Fatal(err)
	}
}
