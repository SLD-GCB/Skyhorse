package skyhorsebox

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"

	core "tianma/boxapi"
)

// RegisterOutbound adds the protocol to an outbound registry.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[OutboundOptions](registry, Type, NewOutbound)
}

// Outbound is a SkyHorse client node inside the kernel.
type Outbound struct {
	outbound.Adapter
	h3          []*core.H3Client
	h3Load      []atomic.Int64
	h3once      sync.Once
	h3mu        sync.Mutex
	h3Cfg       core.Config
	h3Addrs     []string
	h3Next      int
	h3Rotate    time.Time
	h3Every     time.Duration
	h3Max       int
	h3Limit     int
	h3Bytes     []atomic.Int64
	h3Bad       []atomic.Bool
	h3Rate      []float64
	h3Sample    time.Time
	h3Full      int
	udpSessions atomic.Int64
}

// NewOutbound builds the client without dialing anything.
func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options OutboundOptions) (adapter.Outbound, error) {
	if options.Server == "" || options.ServerPort == 0 {
		return nil, E.New("skyhorse: server and server_port are required")
	}

	serverName := options.Server
	pin := ""
	if options.TLS != nil {
		if options.TLS.ServerName != "" {
			serverName = options.TLS.ServerName
		}
		pin = pinHex(options.TLS)
	}

	servers := make([]string, 0, len(options.ServerPorts))
	for _, port := range options.ServerPorts {
		if port != "" {
			servers = append(servers, net.JoinHostPort(options.Server, port))
		}
	}

	cfg := core.Config{
		Server:                  options.Server,
		ServerPort:              int(options.ServerPort),
		Servers:                 servers,
		ServerName:              serverName,
		Pin:                     pin,
		UUID:                    options.UUID,
		Password:                options.Password,
		ObfsPassword:            options.ObfsPassword,
		ObfsKeys:                options.ObfsKeys,
		NodeID:                  options.NodeID,
		Insecure:                options.TLS != nil && options.TLS.Insecure,
		ObfsRotationSeconds:     options.ObfsRotationSeconds,
		ObfsToleranceSeconds:    options.ObfsToleranceSeconds,
		UpMbps:                  options.UpMbps,
		DownMbps:                options.DownMbps,
		BBRProfile:              options.BBRProfile,
		DisableLossCompensation: options.DisableLossCompensation,
		Connections:             options.Connections,
		MaxStreamsPerConnection: options.MaxStreamsPerConnection,
		PortRotateSeconds:       options.PortRotateSeconds,
	}
	count := options.Connections
	if count <= 0 {
		count = 4
	}
	maxConnections := options.MaxConnections
	if maxConnections <= 0 {
		maxConnections = 16
	}
	if maxConnections < count {
		maxConnections = count
	}
	streamLimit := options.MaxStreamsPerConnection
	if streamLimit <= 0 {
		streamLimit = 32
	}
	rotateSeconds := options.PortRotateSeconds
	if rotateSeconds <= 0 {
		rotateSeconds = 300
	}
	addrs := servers
	if len(addrs) == 0 {
		addrs = []string{net.JoinHostPort(options.Server, fmt.Sprint(options.ServerPort))}
	}
	h3 := make([]*core.H3Client, 0, count)
	for index := 0; index < count; index++ {
		instanceCfg := cfg
		instanceCfg.Servers = []string{addrs[index%len(addrs)]}
		instance, err := core.NewH3(instanceCfg)
		if err != nil {
			return nil, E.Cause(err, "skyhorse: h3 client")
		}
		h3 = append(h3, instance)
	}
	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(Type, tag,
			[]string{"tcp", "udp"}, options.DialerOptions),
		h3:       h3,
		h3Load:   make([]atomic.Int64, count),
		h3Cfg:    cfg,
		h3Addrs:  addrs,
		h3Next:   count,
		h3Rotate: time.Now().Add(time.Duration(rotateSeconds) * time.Second),
		h3Every:  time.Duration(rotateSeconds) * time.Second,
		h3Max:    maxConnections,
		h3Limit:  streamLimit,
		h3Bytes:  make([]atomic.Int64, count),
		h3Bad:    make([]atomic.Bool, count),
		h3Rate:   make([]float64, count),
		h3Sample: time.Now(),
	}, nil
}

// DialContext opens one relay through the tunnel.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	index := o.pickH3()
	o.h3Load[index].Add(1)
	conn, err := o.h3[index].Dial(ctx, destination.String())
	if err != nil {
		o.h3Bad[index].Store(true)
		o.h3Load[index].Add(-1)
		o.replaceH3IfIdle(index)
		return nil, err
	}
	o.h3Bad[index].Store(false)
	return &h3Conn{Conn: conn, index: index, outbound: o}, nil
}

// ListenPacket opens one dedicated QUIC connection per UDP session. Sharing a
// single SOCKS5 UDP association between sessions would make concurrent
// DATAGRAM reads race with each other.
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if o.udpSessions.Add(1) > int64(o.h3Max) {
		o.udpSessions.Add(-1)
		return nil, E.New("skyhorse: too many udp sessions")
	}
	client, err := o.newUDPClient()
	if err != nil {
		o.udpSessions.Add(-1)
		return nil, err
	}
	conn, err := client.DialUDP(ctx, destination.String())
	if err != nil {
		_ = client.Close()
		o.udpSessions.Add(-1)
		return nil, err
	}
	return &h3PacketConn{PacketConn: conn, client: client, outbound: o}, nil
}

func (o *Outbound) newUDPClient() (*core.H3Client, error) {
	o.h3mu.Lock()
	defer o.h3mu.Unlock()
	o.h3Next++
	address := o.h3Addrs[o.h3Next%len(o.h3Addrs)]
	cfg := o.h3Cfg
	cfg.Servers = []string{address}
	return core.NewH3(cfg)
}

func (o *Outbound) pickH3() int {
	o.h3mu.Lock()
	o.sampleH3Locked(time.Now())
	if o.h3Every > 0 && time.Now().After(o.h3Rotate) {
		o.h3Rotate = time.Now().Add(o.h3Every)
		// Rotation only affects new connections. Existing connections are
		// never closed here; the user owns their lifetime.
		if len(o.h3) < o.h3Max {
			o.h3Next++
			address := o.h3Addrs[o.h3Next%len(o.h3Addrs)]
			cfg := o.h3Cfg
			cfg.Servers = []string{address}
			if instance, err := core.NewH3(cfg); err == nil {
				o.h3 = append(o.h3, instance)
				o.h3Load = append(o.h3Load, atomic.Int64{})
				o.h3Bytes = append(o.h3Bytes, atomic.Int64{})
				o.h3Bad = append(o.h3Bad, atomic.Bool{})
				o.h3Rate = append(o.h3Rate, 0)
			}
		}
	}
	full := true
	for index, load := range o.h3Load {
		if o.h3Bad[index].Load() {
			continue
		}
		if load.Load() < int64(o.h3Limit) {
			full = false
			break
		}
	}
	if full && len(o.h3) < o.h3Max {
		o.h3Full++
	} else {
		o.h3Full = 0
	}
	if o.h3Full >= 2 && len(o.h3) < o.h3Max {
		o.h3Full = 0
		o.h3Next++
		address := o.h3Addrs[o.h3Next%len(o.h3Addrs)]
		cfg := o.h3Cfg
		cfg.Servers = []string{address}
		if instance, err := core.NewH3(cfg); err == nil {
			o.h3 = append(o.h3, instance)
			o.h3Load = append(o.h3Load, atomic.Int64{})
			o.h3Bytes = append(o.h3Bytes, atomic.Int64{})
			o.h3Bad = append(o.h3Bad, atomic.Bool{})
			o.h3Rate = append(o.h3Rate, 0)
		}
	}
	o.h3mu.Unlock()
	best := -1
	for index := range o.h3Load {
		if o.h3Bad[index].Load() {
			continue
		}
		if best < 0 || o.h3Load[index].Load() < o.h3Load[best].Load() {
			best = index
		}
	}
	if best < 0 {
		best = 0
	}
	return best
}

func (o *Outbound) replaceH3(index int) {
	o.h3mu.Lock()
	defer o.h3mu.Unlock()
	o.replaceH3Locked(index)
}

// replaceH3IfIdle only replaces a connection after its last stream is gone.
// A single failed stream must never close the other streams on the same H3
// connection.
func (o *Outbound) replaceH3IfIdle(index int) {
	o.h3mu.Lock()
	defer o.h3mu.Unlock()
	if o.h3Load[index].Load() != 0 {
		return
	}
	o.replaceH3Locked(index)
}

func (o *Outbound) replaceH3Locked(index int) {
	o.h3Next++
	address := o.h3Addrs[o.h3Next%len(o.h3Addrs)]
	cfg := o.h3Cfg
	cfg.Servers = []string{address}
	instance, err := core.NewH3(cfg)
	if err != nil {
		return
	}
	old := o.h3[index]
	o.h3[index] = instance
	o.h3Bytes[index].Store(0)
	o.h3Rate[index] = 0
	if old != nil {
		_ = old.Close()
	}
}

func (o *Outbound) sampleH3Locked(now time.Time) {
	if o.h3Sample.IsZero() {
		o.h3Sample = now
		return
	}
	elapsed := now.Sub(o.h3Sample).Seconds()
	if elapsed < 5 {
		return
	}
	o.h3Sample = now
	total := 0.0
	active := 0
	for index := range o.h3 {
		bytes := o.h3Bytes[index].Swap(0)
		rate := float64(bytes) / elapsed
		o.h3Rate[index] = o.h3Rate[index]*0.7 + rate*0.3
		if o.h3Rate[index] > 0 {
			total += o.h3Rate[index]
			active++
		}
	}
	if active == 0 {
		return
	}
}

type h3Conn struct {
	net.Conn
	index    int
	outbound *Outbound
	once     sync.Once
}

func (c *h3Conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.outbound.h3Bytes[c.index].Add(int64(n))
	return n, err
}

func (c *h3Conn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.outbound.h3Bytes[c.index].Add(int64(n))
	return n, err
}

func (c *h3Conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.outbound.h3Load[c.index].Add(-1) })
	return err
}

type h3PacketConn struct {
	net.PacketConn
	client   *core.H3Client
	outbound *Outbound
	once     sync.Once
}

func (c *h3PacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	return c.PacketConn.ReadFrom(p)
}

func (c *h3PacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return c.PacketConn.WriteTo(p, addr)
}

func (c *h3PacketConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(func() {
		if c.client != nil {
			_ = c.client.Close()
		}
		c.outbound.udpSessions.Add(-1)
	})
	return err
}

// Close releases the session.
func (o *Outbound) Close() error { return nil }
