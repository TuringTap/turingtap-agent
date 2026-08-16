package tunnel

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn adapts a gorilla/websocket.Conn to net.Conn so yamux can run on it.
// All payload is carried in binary frames.
type wsConn struct {
	ws  *websocket.Conn
	r   io.Reader
	rmu sync.Mutex
	wmu sync.Mutex
}

func newWSConn(ws *websocket.Conn) *wsConn { return &wsConn{ws: ws} }

func (c *wsConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r != nil {
			n, err := c.r.Read(p)
			if n > 0 {
				return n, nil
			}
			if err == io.EOF {
				c.r = nil
				continue
			}
			if err != nil {
				return 0, err
			}
		}
		_, r, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		c.r = r
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error                       { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error      { _ = c.ws.SetReadDeadline(t); return c.ws.SetWriteDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
