package tcp

import (
	"strings"
	"sync"
	"time"

	"github.com/marten-seemann/tcp"
	"github.com/mikioh/tcpinfo"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	newConns    *prometheus.CounterVec
	closedConns *prometheus.CounterVec
)

var collector *aggregatingCollector

func init() {
	collector = newAggregatingCollector()
	prometheus.MustRegister(collector)

	const direction = "direction"

	newConns = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tcp_connections_new_total",
			Help: "TCP new connections",
		},
		[]string{direction},
	)
	prometheus.MustRegister(newConns)
	closedConns = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tcp_connections_closed_total",
			Help: "TCP connections closed",
		},
		[]string{direction},
	)
	prometheus.MustRegister(closedConns)
}

type aggregatingCollector struct {
	mutex sync.Mutex

	highestID     uint64
	conns         map[uint64] /* id */ *tracingConn
	rtts          prometheus.Histogram
	connDurations prometheus.Histogram
}

var _ prometheus.Collector = &aggregatingCollector{}

func newAggregatingCollector() *aggregatingCollector {
	return &aggregatingCollector{
		conns: make(map[uint64]*tracingConn),
		rtts: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "tcp_rtt",
			Help:    "TCP round trip time",
			Buckets: prometheus.ExponentialBuckets(0.001, 1.25, 40), // 1ms to ~6000ms
		}),
		connDurations: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "tcp_connection_duration",
			Help:    "TCP Connection Duration",
			Buckets: prometheus.ExponentialBuckets(1, 1.5, 40), // 1s to ~12 weeks
		}),
	}
}

func (c *aggregatingCollector) AddConn(t *tracingConn) uint64 {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.highestID++
	c.conns[c.highestID] = t
	return c.highestID
}

func (c *aggregatingCollector) removeConn(id uint64) {
	delete(c.conns, id)
}

func (c *aggregatingCollector) Describe(descs chan<- *prometheus.Desc) {
	descs <- c.rtts.Desc()
	descs <- c.connDurations.Desc()
}

func (c *aggregatingCollector) Collect(metrics chan<- prometheus.Metric) {
	now := time.Now()
	c.mutex.Lock()
	for _, conn := range c.conns {
		info, err := conn.getTCPInfo()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				c.closedConn(conn)
				continue
			}
			log.Errorf("Failed to get TCP info: %s", err)
			continue
		}
		c.rtts.Observe(info.RTT.Seconds())
		c.connDurations.Observe(now.Sub(conn.startTime).Seconds())
		if info.State == tcpinfo.Closed {
			c.closedConn(conn)
		}
	}
	c.mutex.Unlock()
	metrics <- c.rtts
	metrics <- c.connDurations
}

func (c *aggregatingCollector) closedConn(conn *tracingConn) {
	collector.removeConn(conn.id)
	closedConns.WithLabelValues(conn.getDirection()).Inc()
}

type tracingConn struct {
	id uint64

	startTime time.Time
	isClient  bool

	manet.Conn
	tcpConn *tcp.Conn
}

func newTracingConn(c manet.Conn, isClient bool) (*tracingConn, error) {
	conn, err := tcp.NewConn(c)
	if err != nil {
		return nil, err
	}
	tc := &tracingConn{
		startTime: time.Now(),
		isClient:  isClient,
		Conn:      c,
		tcpConn:   conn,
	}
	tc.id = collector.AddConn(tc)
	newConns.WithLabelValues(tc.getDirection()).Inc()
	return tc, nil
}

func (c *tracingConn) getDirection() string {
	if c.isClient {
		return "outgoing"
	}
	return "incoming"
}

func (c *tracingConn) Close() error {
	return c.Conn.Close()
}

func (c *tracingConn) getTCPInfo() (*tcpinfo.Info, error) {
	var o tcpinfo.Info
	var b [256]byte
	i, err := c.tcpConn.Option(o.Level(), o.Name(), b[:])
	if err != nil {
		return nil, err
	}
	info := i.(*tcpinfo.Info)
	return info, nil
}

type tracingListener struct {
	manet.Listener
}

func (l *tracingListener) Accept() (manet.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newTracingConn(conn, false)
}
