package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/marvinvr/docktail/cloud/proto"
)

const (
	handshakeTimeout = 15 * time.Second
	writeWait        = 10 * time.Second
	pongWait         = 3 * proto.HeartbeatInterval * time.Second
	sendBuffer       = 256
)

const (
	minBackoff = 1 * time.Second
	maxBackoff = 60 * time.Second
)

// handlers receive cloud->agent frames decoded by the read loop. A handler runs
// ON the read loop, so anything that blocks (network I/O) must hand off to a
// goroutine.
type handlers struct {
	onHelloAck     func(proto.HelloAck)
	onConfig       func(proto.Config)
	onTailnetProbe func(proto.TailnetProbe)
}

// wsConn is a single live websocket connection with its own write pump.
type wsConn struct {
	ws  *websocket.Conn
	log zerolog.Logger

	send   chan []byte
	closed chan struct{}

	closeOnce sync.Once
	startedAt time.Time
}

// dialError carries the HTTP status of a failed upgrade so callers can decide
// between back off (5xx) and stop (401/403).
type dialError struct {
	statusCode int
	err        error
}

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// dial opens one WSS connection, authenticating with the workspace key as a
// bearer token. It does not start pumps — call run.
func dial(ctx context.Context, url, key string, logger zerolog.Logger) (*wsConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: handshakeTimeout}
	hdr := http.Header{}
	if key != "" {
		hdr.Set("Authorization", "Bearer "+key)
	}

	ws, resp, err := dialer.DialContext(ctx, url, hdr)
	if err != nil {
		if resp != nil {
			return nil, &dialError{statusCode: resp.StatusCode, err: err}
		}
		return nil, err
	}
	ws.SetReadLimit(proto.MaxCloudFrameBytes)
	return &wsConn{
		ws:        ws,
		log:       logger,
		send:      make(chan []byte, sendBuffer),
		closed:    make(chan struct{}),
		startedAt: time.Now(),
	}, nil
}

// run drives the connection's read loop, write pump, and ping heartbeat until
// ctx is cancelled, the connection errors, or close is called.
func (c *wsConn) run(ctx context.Context, h handlers) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})

	var wg sync.WaitGroup
	readErr := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		readErr <- c.readLoop(h)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writePump(ctx)
	}()

	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-readErr:
	}

	c.close()
	cancel()
	wg.Wait()

	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (c *wsConn) readLoop(h handlers) error {
	for {
		messageType, data, err := c.ws.ReadMessage()
		if err != nil {
			return err
		}
		if messageType != websocket.TextMessage {
			c.log.Warn().Int("message_type", messageType).Msg("cloud: non-text frame ignored")
			continue
		}
		var env proto.Envelope
		if derr := json.Unmarshal(data, &env); derr != nil {
			c.log.Warn().Err(derr).Msg("cloud: undecodable frame, ignoring")
			continue
		}
		switch env.Type {
		case proto.TypeHelloAck:
			var ack proto.HelloAck
			if env.Decode(&ack) == nil && h.onHelloAck != nil {
				h.onHelloAck(ack)
			}
		case proto.TypeConfig:
			var cfg proto.Config
			if env.Decode(&cfg) == nil && h.onConfig != nil {
				h.onConfig(cfg)
			}
		case proto.TypeTailnetProbe:
			var probe proto.TailnetProbe
			if derr := env.Decode(&probe); derr != nil {
				c.log.Warn().Err(derr).Msg("cloud: undecodable tailnet_probe, ignoring")
				continue
			}
			if h.onTailnetProbe != nil {
				h.onTailnetProbe(probe)
			}
		default:
			c.log.Debug().Str("type", string(env.Type)).Msg("cloud: ignoring unexpected frame type")
		}
	}
}

func (c *wsConn) writePump(ctx context.Context) {
	ticker := time.NewTicker(proto.HeartbeatInterval * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.writeClose()
			return
		case <-c.closed:
			return
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.log.Debug().Err(err).Msg("cloud: write failed")
				c.close()
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				c.log.Debug().Err(err).Msg("cloud: ping failed")
				c.close()
				return
			}
		}
	}
}

// sendFrame queues a pre-encoded envelope. Non-blocking: drops if the queue is
// full (metadata is best-effort). Returns false if the connection is closed.
func (c *wsConn) sendFrame(env []byte) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.send <- env:
		return true
	case <-c.closed:
		return false
	default:
		c.log.Warn().Msg("cloud: send queue full, dropping frame")
		return false
	}
}

func (c *wsConn) uptime() int64 { return int64(time.Since(c.startedAt).Seconds()) }

func (c *wsConn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.Close()
	})
}

func (c *wsConn) writeClose() {
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	_ = c.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(writeWait),
	)
}

// backoff produces jittered exponential reconnect delays.
type backoff struct {
	cur time.Duration
	rng *rand.Rand
}

func newBackoff() *backoff {
	return &backoff{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (b *backoff) next() time.Duration {
	if b.cur < minBackoff {
		b.cur = minBackoff
	} else {
		b.cur *= 2
		if b.cur > maxBackoff {
			b.cur = maxBackoff
		}
	}
	half := b.cur / 2
	jitter := time.Duration(b.rng.Int63n(int64(b.cur-half) + 1))
	return half + jitter
}

// slow moves the next delay into the 30-60s range. It is used for retryable
// operator-actionable states where rapid reconnects cannot help.
func (b *backoff) slow() { b.cur = maxBackoff }

func (b *backoff) reset() { b.cur = 0 }
