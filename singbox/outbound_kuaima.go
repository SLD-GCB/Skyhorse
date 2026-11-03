package skyhorsebox

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"skyhorse.local/kuaima/pkg/client"
	"skyhorse.local/kuaima/pkg/gate"
	"skyhorse.local/kuaima/pkg/obfs"
)

// RegisterKuaimaOutbound adds the Kuaima client to a registry.
func RegisterKuaimaOutbound(registry *outbound.Registry) {
	outbound.Register[KuaimaOutboundOptions](registry, TypeFastHorse, NewKuaimaOutbound)
}

// KuaimaOutbound is a Kuaima client node.
type KuaimaOutbound struct {
	outbound.Adapter
	clients    []*client.Client
	inflight   []atomic.Int64
	counter    atomic.Uint64
	maxStreams int64
}

// NewKuaimaOutbound builds the client without dialing.
func NewKuaimaOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options KuaimaOutboundOptions) (adapter.Outbound, error) {
	if options.Server == "" || options.ServerPort == 0 {
		return nil, E.New("kuaima: server and server_port are required")
	}
	if options.TLS == nil || !options.TLS.Enabled {
		return nil, E.New("kuaima: tls is required")
	}
	serverName := options.Server
	if options.TLS.ServerName != "" {
		serverName = options.TLS.ServerName
	}
	certPEM := ""
	if options.TLS.CertificatePath != "" {
		data, err := os.ReadFile(options.TLS.CertificatePath)
		if err != nil {
			return nil, E.Cause(err, "kuaima: read certificate")
		}
		certPEM = string(data)
	}
	if certPEM == "" && !options.TLS.Insecure {
		return nil, E.New("kuaima: tls.certificate_path or tls.insecure is required")
	}
	obfsConfig := obfs.Config{
		Password:  options.Obfs.Password,
		KeyID:     options.Obfs.KeyID,
		Rotation:  options.Obfs.Rotation.Build(),
		Tolerance: options.Obfs.Tolerance.Build(),
		PadTo:     options.Obfs.PadTo,
	}
	gateConfig := gate.Config{Password: options.Obfs.Password, NodeID: options.Gate.NodeID}
	ports := []uint16{options.ServerPort}
	if len(options.ServerPorts) > 0 {
		parsed, err := kuaimaPorts(strings.Join(options.ServerPorts, ","), options.ServerPort)
		if err != nil {
			return nil, err
		}
		ports = parsed
	}
	connections := options.Connections
	if connections <= 0 {
		connections = 1
	}
	outboundNode := &KuaimaOutbound{
		Adapter: outbound.NewAdapterWithDialerOptions(TypeFastHorse, tag, []string{N.NetworkTCP, N.NetworkUDP}, options.DialerOptions),
	}
	maxStreams := options.MaxStreamsPerConnection
	if maxStreams <= 0 {
		maxStreams = 32
	}
	outboundNode.maxStreams = int64(maxStreams)
	for i := 0; i < connections; i++ {
		instance, err := client.New(ctx, client.Config{
			Server:         options.Server,
			Port:           ports[i%len(ports)],
			ServerName:     serverName,
			Username:       options.Username,
			Password:       options.Password,
			CertificatePEM: certPEM,
			Insecure:       options.TLS.Insecure,
			Obfs:           obfsConfig,
			Gate:           gateConfig,
		})
		if err != nil {
			return nil, E.Cause(err, "kuaima: client")
		}
		outboundNode.clients = append(outboundNode.clients, instance)
		outboundNode.inflight = append(outboundNode.inflight, atomic.Int64{})
	}
	return outboundNode, nil
}

func (o *KuaimaOutbound) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	for _, instance := range o.clients {
		if err := instance.Start(); err != nil {
			return err
		}
		scope.Add(instance.Close)
	}
	return nil
}

func (o *KuaimaOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) == N.NetworkUDP {
		return o.dialUDP(ctx, destination)
	}
	if N.NetworkName(network) != N.NetworkTCP {
		return nil, E.New("kuaima: only tcp/udp is supported")
	}
	attempts := len(o.clients)
	if attempts == 0 {
		return nil, E.New("kuaima: no connections")
	}
	lastErr := E.New("kuaima: no connection")
	for i := 0; i < attempts; i++ {
		index := o.pick()
		o.inflight[index].Add(1)
		conn, err := o.clients[index].DialContext(ctx, destination.String())
		if err != nil {
			o.inflight[index].Add(-1)
			lastErr = err
			continue
		}
		return &kuaimaConn{Conn: conn, index: index, outbound: o}, nil
	}
	return nil, lastErr
}

func (o *KuaimaOutbound) pick() int {
	best := -1
	bestValue := int64(0)
	// 优先选还没到流上限的连接，把流摊开，减少单 TCP 队头阻塞。
	for i := range o.inflight {
		value := o.inflight[i].Load()
		if value >= o.maxStreams {
			continue
		}
		if best < 0 || value < bestValue {
			best = i
			bestValue = value
		}
	}
	if best >= 0 {
		return best
	}
	for i := range o.inflight {
		value := o.inflight[i].Load()
		if best < 0 || value < bestValue {
			best = i
			bestValue = value
		}
	}
	return best
}

func (o *KuaimaOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	pc, index, err := o.openUDP(ctx, destination)
	if err != nil {
		return nil, err
	}
	return &kuaimaPacketConn{PacketConn: pc, index: index, outbound: o}, nil
}

func (o *KuaimaOutbound) dialUDP(ctx context.Context, destination M.Socksaddr) (net.Conn, error) {
	pc, index, err := o.openUDP(ctx, destination)
	if err != nil {
		return nil, err
	}
	return &kuaimaUDPConn{PacketConn: pc, index: index, outbound: o}, nil
}

func (o *KuaimaOutbound) openUDP(ctx context.Context, destination M.Socksaddr) (net.PacketConn, int, error) {
	attempts := len(o.clients)
	if attempts == 0 {
		return nil, 0, E.New("kuaima: no connections")
	}
	lastErr := E.New("kuaima: no connection")
	for i := 0; i < attempts; i++ {
		index := o.pick()
		o.inflight[index].Add(1)
		pc, err := o.clients[index].DialUDP(ctx, destination.String())
		if err != nil {
			o.inflight[index].Add(-1)
			lastErr = err
			continue
		}
		return pc, index, nil
	}
	return nil, 0, lastErr
}

func (o *KuaimaOutbound) Close() error {
	for _, instance := range o.clients {
		_ = instance.Close()
	}
	return nil
}

type kuaimaConn struct {
	net.Conn
	index    int
	outbound *KuaimaOutbound
	once     sync.Once
}

func (c *kuaimaConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.outbound.inflight[c.index].Add(-1) })
	return err
}

type kuaimaUDPConn struct {
	net.PacketConn
	index    int
	outbound *KuaimaOutbound
	once     sync.Once
}

func (c *kuaimaUDPConn) Read(p []byte) (int, error)  { return c.PacketConn.(net.Conn).Read(p) }
func (c *kuaimaUDPConn) Write(p []byte) (int, error) { return c.PacketConn.(net.Conn).Write(p) }
func (c *kuaimaUDPConn) RemoteAddr() net.Addr        { return c.PacketConn.(net.Conn).RemoteAddr() }
func (c *kuaimaUDPConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(func() { c.outbound.inflight[c.index].Add(-1) })
	return err
}

type kuaimaPacketConn struct {
	net.PacketConn
	index    int
	outbound *KuaimaOutbound
	once     sync.Once
}

func (c *kuaimaPacketConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(func() { c.outbound.inflight[c.index].Add(-1) })
	return err
}
