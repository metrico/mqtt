package mqtt

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

// Multiple goroutines may invoke methods on a ClientPool simultaneously.
type ClientPool struct {
	clients chan *Client
}

// NewClientPool retuns a new pool with one Client for each configuration.
func NewClientPool(configs ...*ClientConfig) *ClientPool {
	pool := ClientPool{make(chan *Client, len(configs))}
	for _, config := range configs {
		pool.clients <- NewClient(config)
	}
	return &pool
}

// NewNClientPool returns a new pool with n Clients.
func NewNClientPool(n int, config *ClientConfig) *ClientPool {
	pool := ClientPool{make(chan *Client, n)}
	for i := 0; i < n; i++ {
		pool.clients <- NewClient(config)
	}
	return &pool
}

// PublishAtLeastOnce invokes PublishAtLeastOnce on a Client.
func (pool *ClientPool) PublishAtLeastOnce(topic string, message []byte) error {
	client := <-pool.clients
	err := client.PublishAtLeastOnce(topic, message)
	pool.clients <- client
	return err
}

// PublishExactlyOnce invokes PublishExactlyOnce on a Client.
func (pool *ClientPool) PublishExactlyOnce(topic string, message []byte) error {
	client := <-pool.clients
	err := client.PublishExactlyOnce(topic, message)
	pool.clients <- client
	return err
}

// Receive gets invoked for inbound messages. AtMostOnce ignores the return.
// ExactlyOnce repeates Receive until the return is true and AtLeastOnce may
// repeat Receive even after the return is true.
type Receive func(topic string, message []byte) bool

// Connecter abstracts the transport layer establishment.
type Connecter func() (net.Conn, error)

// UnsecuredConnecter creates plain network connections.
// See net.Dial for details on the network & address syntax.
// With or without a timeout, the operating system may still
// impose its own earlier timeout. For example, TCP timeouts
// are often around 3 minutes.
func UnsecuredConnecter(network, address string, timeout time.Duration) Connecter {
	dialer := &net.Dialer{Timeout: timeout}
	return func() (net.Conn, error) {
		return dialer.Dial(network, address)
	}
}

// SecuredConnecter creates TLS network connections.
// See net.Dial for details on the network & address syntax.
// With or without a timeout, the operating system may still
// impose its own earlier timeout. For example, TCP timeouts
// are often around 3 minutes.
func SecuredConnecter(network, address string, conf *tls.Config, timeout time.Duration) Connecter {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config:    conf,
	}
	return func() (net.Conn, error) {
		return dialer.Dial(network, address)
	}
}

// ClientConfig defines Client settings.
type ClientConfig struct {
	Receive     // inbound destination
	Connecter   // remote link
	Persistence // session safeguard

	SessionConfig

	// Messages larger than InSizeLimit are not presented to Receive.
	// The protocol limit is 256 MiB.
	InSizeLimit int

	// Backoff on transport errors.
	RetryDelay time.Duration
}

// Client manages a single network connection.
//
// Multiple goroutines may invoke methods on a Client, but NOT simultaneously.
// See ClientPool for a safe alternative.
type Client struct {
	ClientConfig // read-only

	// Semaphore singleton for writes.
	connSem chan net.Conn

	closed chan struct{}

	// Semaphore allows for one ping request at a time.
	pingAck chan chan<- struct{}

	// Semaphore allows for one subscribe or unsubscribe request at a time.
	subscriptionAck chan chan<- byte

	atLeastOnceLine
	exactlyOnceLine
}

func NewClient(config *ClientConfig) *Client {
	c := &Client{
		ClientConfig:    *config, // copy
		connSem:         make(chan net.Conn, 1),
		pingAck:         make(chan chan<- struct{}, 1),
		subscriptionAck: make(chan chan<- byte, 1),
		closed:          make(chan struct{}),
	}

	if c.Will != nil {
		willCopy := *c.Will
		c.Will = &willCopy
	}

	go c.readRoutine()

	return c
}

func (c *Client) write(p []byte) error {
	conn := <-c.connSem // lock
	defer func() {
		c.connSem <- conn // release
	}()

	n, err := conn.Write(p)
	for err != nil {
		select {
		case <-c.closed:
			return ErrClosed
		default:
			break
		}

		var ne net.Error
		if errors.As(err, &ne) && ne.Temporary() {
			conn.Close()
			return err
		}

		delay := c.RetryDelay
		log.Print("mqtt: write retry in ", delay, " on ", err)
		time.Sleep(delay)

		p = p[n:]
		n, err = conn.Write(p)
		// handle error in current loop
	}

	return nil
}

func (c *Client) writeBuffers(buffers net.Buffers) error {
	conn := <-c.connSem // lock
	defer func() {
		c.connSem <- conn // release
	}()

	n, err := buffers.WriteTo(conn)
	for err != nil {
		select {
		case <-c.closed:
			return ErrClosed
		default:
			break
		}

		var ne net.Error
		if errors.As(err, &ne) && ne.Temporary() {
			conn.Close()
			return err
		}

		var todo net.Buffers
		// don't modify original buffers; it may be used by Store
		for i, bytes := range buffers {
			if n >= int64(len(bytes)) {
				n -= int64(len(bytes))
				continue
			}
			todo = append(todo, bytes[n:])
			todo = append(todo, buffers[i+1:]...)
			break
		}
		buffers = todo

		delay := c.RetryDelay
		log.Print("mqtt: write retry in ", delay, " on ", err)
		time.Sleep(delay)

		n, err = buffers.WriteTo(conn)
		// handle error in current loop
	}

	return nil
}

func (c *Client) readRoutine() {
	// determine only here whether closed
	defer close(c.closed)

	for {
		r, err := c.connect()
		switch err {
		case nil:
			for {
				err := c.nextPacket(r)
				if err == nil {
					continue
				}
				if errors.Is(err, ErrClosed) {
					return
				}
				log.Print(err)
				break
			}

		case ErrClosed:
			return

		default:
			log.Print(err)
			time.Sleep(c.RetryDelay)
		}
	}
}

// ErrProtoReset signals illegal reception from the server.
var errProtoReset = errors.New("mqtt: connection reset on protocol violation")

var errPacketID = fmt.Errorf("%w: wrong packet identifier", errProtoReset)

func (c *Client) nextPacket(r *bufio.Reader) error {
	firstByte, err := c.readByte(r)
	if err != nil {
		return err
	}
	l, err := c.readRemainingLength(r)
	if err != nil {
		return err
	}

	if l > c.InSizeLimit {
		// BUG(pascaldekloe):
		// “The Client MUST acknowledge any Publish Packet it receives
		// according to the applicable QoS rules regardless of whether
		// it elects to process the Application Message that it
		// contains [MQTT-4.5.0-2].”
		log.Printf("mqtt: skipping %d B inbound packet content; limit is %d B", l, c.InSizeLimit)
		_, err := r.Discard(l)
		return unexpectEOF(err)
	}

	p, didPeek, err := c.peekOrReadN(r, l)

	err = c.inbound(firstByte, p)
	if err != nil {
		return err
	}

	if didPeek {
		r.Discard(len(p)) // no errors guaranteed
	}
	return nil
}

func (c *Client) readRemainingLength(r *bufio.Reader) (int, error) {
	var l, shift uint
	for i := 0; i < 4; i++ {
		b, err := c.readByte(r)
		if err != nil {
			return 0, unexpectEOF(err)
		}
		l |= (b & 127) << shift
		if b < 0x80 {
			return int(l), nil
		}
		shift += 7
	}
	return 0, fmt.Errorf("%w: remaining length declaration exceeds 4 B", errProtoReset)
}

func (c *Client) readByte(r *bufio.Reader) (uint, error) {
	for {
		b, err := r.ReadByte()

		var ne net.Error
		switch {
		case err == nil:
			return uint(b), nil
		case errors.As(err, &ne) && ne.Temporary():
			delay := c.RetryDelay
			log.Print("mqtt: read retry in ", delay, " on ", err)
			time.Sleep(delay)
		default:
			return 0, err
		}
	}
}

func (c *Client) readN(r *bufio.Reader, n int) ([]byte, error) {
	p := make([]byte, n)
	i, err := r.Read(p)
	for i < n {
		var ne net.Error
		switch {
		case err == nil:
			break // incomplete read
		case errors.As(err, &ne) && ne.Temporary():
			delay := c.RetryDelay
			log.Print("mqtt: read retry in ", delay, " on ", err)
			time.Sleep(delay)
		default:
			return nil, err
		}

		var more int
		more, err = r.Read(p[i:])
		if err != nil && i != 0 {
			err = unexpectEOF(err)
		}
		i += more
	}
	return p, err
}

func (c *Client) peekOrReadN(r *bufio.Reader, n int) (p []byte, didPeek bool, err error) {
	for {
		p, err = r.Peek(n)

		var ne net.Error
		switch {
		case err == nil:
			didPeek = true
			return

		case errors.Is(err, bufio.ErrBufferFull):
			// n does not fit in read buffer
			p, err = c.readN(r, n)
			return

		case errors.As(err, &ne) && ne.Temporary():
			delay := c.RetryDelay
			log.Print("mqtt: read retry in ", delay, " on ", err)
			time.Sleep(delay)

		default:
			return
		}
	}
}

func unexpectEOF(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (c *Client) inbound(firstByte uint, p []byte) error {
	switch packetType := firstByte >> 4; packetType {
	case pubMsg:
		// parse packet
		i := uint(p[0])<<8 | uint(p[1])
		topic := string(p[2:i])
		packetID := uint(p[i])<<8 | uint(p[i+1])
		message := p[i+2:]

		switch firstByte & 0b110 {
		case AtMostOnce << 1:
			c.Receive(topic, message)

		case AtLeastOnce << 1:
			if c.Receive(topic, message) {
				p := packetPool.Get().(*packet)
				defer packetPool.Put(p)
				p.buf = append(p.buf[:0], pubAck<<4, 2, byte(packetID>>8), byte(packetID))
				if err := c.write(p.buf); err != nil {
					return err
				}
			}

		case ExactlyOnce << 1:
			bytes := make([]byte, len(topic)+1+len(message))
			copy(bytes, topic)
			copy(bytes[len(topic)+1:], message)
			err := c.Persistence.Store(packetID, net.Buffers{bytes})
			if err != nil {
				log.Print("mqtt: persistence malfuncion: ", err)
				return nil // don't confirm
			}

			p := packetPool.Get().(*packet)
			defer packetPool.Put(p)
			p.buf = append(p.buf[:0], pubReceived<<4, 2, byte(packetID>>8), byte(packetID))
			if err := c.write(p.buf); err != nil {
				return err
			}

		default:
			return fmt.Errorf("%w: received publish with reserved QoS", errProtoReset)
		}

	case pubRelease: // second round trip for ExactlyOnce reception
		if len(p) != 2 {
			return fmt.Errorf("%w: received publish release with remaining length %d", errProtoReset, len(p))
		}
		packetID := uint(binary.BigEndian.Uint16(p))
		bytes, err := c.Persistence.Load(packetID)
		if err != nil {
			log.Print("mqtt: persistence malfuncion: ", err)
			return nil
		}
		if bytes != nil {
			for i, b := range bytes {
				if b == 0 {
					topic := string(bytes[:i])
					message := bytes[i+1:]
					if !c.Receive(topic, message) {
						return nil // don't confirm; keep in storage
					}
					break
				}
			}
			c.Persistence.Delete(packetID)
		}

		p := packetPool.Get().(*packet)
		defer packetPool.Put(p)
		p.buf = append(p.buf[:0], pubComplete<<4, 2, byte(packetID>>8), byte(packetID))
		if err := c.write(p.buf); err != nil {
			return err
		}

	case pubAck: // confirm of Publish with AtLeastOnce
		if len(p) != 2 {
			return fmt.Errorf("%w: received publish ␆ with remaining length %d", errProtoReset, len(p))
		}
		packetID := uint(binary.BigEndian.Uint16(p))

		err := c.atLeastOnceLine.freeID(packetID)
		if err != nil {
			return err
		}

		err = c.Persistence.Delete(packetID)
		if err != nil {
			return err
		}

	case pubReceived: // first confirm of Publish with ExactlyOnce
		if len(p) != 2 {
			return fmt.Errorf("%w: received publish received with remaining length %d", errProtoReset, len(p))
		}
		packetID := uint(binary.BigEndian.Uint16(p))

		err := c.exactlyOnceLine.releaseID(packetID)
		if err != nil {
			return err
		}

		p := packetPool.Get().(*packet)
		p.buf = append(p.buf[:0], pubRelease<<4, 2, byte(packetID>>8), byte(packetID))
		err = c.Persistence.Store(packetID, net.Buffers{p.buf})
		if err != nil {
			return err
		}
		if err := c.write(p.buf); err != nil {
			return err
		}

	case pubComplete: // second confirm of Publish with ExactlyOnce
		if len(p) != 2 {
			return fmt.Errorf("%w: received publish complete with remaining length %d", errProtoReset, len(p))
		}
		packetID := uint(binary.BigEndian.Uint16(p))

		err := c.exactlyOnceLine.freeID(packetID)
		if err != nil {
			return err
		}
		err = c.Persistence.Delete(packetID)
		if err != nil {
			return err
		}

	case subAck:
		if len(p) < 3 {
			return fmt.Errorf("%w: subscribe ␆ with %d B remaining length", errProtoReset, len(p))
		}
		if packetID := binary.BigEndian.Uint16(p); packetID != subscriptionPacketID {
			return fmt.Errorf("%w: received subscribe ␆ %#04x, while one %#04x in use", errPacketID, packetID, subscriptionPacketID)
		}

		err := c.Persistence.Delete(subscriptionPacketID)
		if err != nil {
			return err
		}

		var ack chan<- byte
		select {
		case ack = <-c.subscriptionAck:
			defer close(ack)
		default:
			return nil // tolerate redundant ␆
		}
		for _, returnCode := range p[2:] {
			select {
			case ack <- returnCode:
				break // OK
			default:
				// This could happen on persistence failure when
				// a redundant ␆ is received before the unlock.
				return nil
			}
		}

	case unsubAck:
		if len(p) != 2 {
			return fmt.Errorf("%w: unsubscribe ␆ with %d B remaining length", errProtoReset, len(p))
		}
		if packetID := binary.BigEndian.Uint16(p); packetID != subscriptionPacketID {
			return fmt.Errorf("%w: received unsubscribe ␆ %#04x, while one %#04x in use", errPacketID, packetID, subscriptionPacketID)
		}

		err := c.Persistence.Delete(subscriptionPacketID)
		if err != nil {
			return err
		}

		var ack chan<- byte
		select {
		case ack = <-c.subscriptionAck:
			close(ack)
		default:
			break // tolerate redundant ␆
		}

	case pong:
		if len(p) != 0 {
			return fmt.Errorf("%w: got ping response with %d byte remaining length", errProtoReset, len(p))
		}
		select {
		case ack := <-c.pingAck:
			close(ack)
		default:
			break // tolerate unsolicited ping response
		}

	case connReq, subReq, unsubReq, ping, disconn:
		return fmt.Errorf("%w: received packet type %d", errProtoReset, packetType)

	case connAck:
		return fmt.Errorf("%w: received redundant connection ␆", errProtoReset)

	default:
		return fmt.Errorf("%w: received reserved packet type %d", errProtoReset, packetType)
	}

	return nil
}

// Connect initiates the transport layer and populates c.connSem.
func (c *Client) connect() (*bufio.Reader, error) {
	conn, err := c.Connecter()
	if err != nil {
		return nil, err
	}

	// launch handshake
	size := 6 // variable header

	var flags uint
	if c.UserName != "" {
		size += 2 + len(c.UserName)
		flags |= 1 << 7
	}
	if c.Password != nil {
		size += 2 + len(c.Password)
		flags |= 1 << 6
	}
	if w := c.Will; w != nil {
		size += 2 + len(w.Topic)
		size += 2 + len(w.Message)
		if w.Retain {
			flags |= 1 << 5
		}
		flags |= uint(w.Deliver) << 3
		flags |= 1 << 2
	}
	if c.CleanSession {
		flags |= 1 << 1
	}
	size += 2 + len(c.ClientID)

	p := packetPool.Get().(*packet)

	// compose header
	p.buf = append(p.buf[:0], connReq<<4)
	for size > 127 {
		p.buf = append(p.buf, byte(size|128))
		size >>= 7
	}
	p.buf = append(p.buf[:0], byte(size))

	p.buf = append(p.buf, 0, 4, 'M', 'Q', 'T', 'T', 4, byte(flags))

	// append payload
	if err := stringCheck(c.ClientID); err != nil {
		conn.Close()
		return nil, err
	}
	p.buf = append(p.buf, byte(len(c.ClientID)>>8), byte(len(c.ClientID)))
	p.buf = append(p.buf, c.ClientID...)

	if w := c.Will; w != nil {
		if err := stringCheck(w.Topic); err != nil {
			conn.Close()
			return nil, err
		}
		p.buf = append(p.buf, byte(len(w.Topic)>>8), byte(len(w.Topic)))
		p.buf = append(p.buf, w.Topic...)
		p.buf = append(p.buf, byte(len(w.Message)>>8), byte(len(w.Message)))
		p.buf = append(p.buf, w.Message...)
	}
	if c.UserName != "" {
		if err := stringCheck(c.UserName); err != nil {
			conn.Close()
			return nil, err
		}
		p.buf = append(p.buf, byte(len(c.UserName)>>8), byte(len(c.UserName)))
		p.buf = append(p.buf, c.UserName...)
	}
	if c.Password != nil {
		p.buf = append(p.buf, byte(len(c.Password)>>8), byte(len(c.Password)))
		p.buf = append(p.buf, c.Password...)
	}

	if _, err := conn.Write(p.buf); err != nil {
		conn.Close()
		return nil, err
	}

	r := bufio.NewReaderSize(conn, 16)

	if head, err := r.ReadByte(); err != nil {
		conn.Close()
		return nil, err
	} else if head != connAck<<4 {
		return nil, fmt.Errorf("mqtt: received head %#x, want connect ␆—connection closed", head)
	}

	if remainingLen, err := r.ReadByte(); err != nil {
		conn.Close()
		return nil, err
	} else if remainingLen != 2 {
		return nil, fmt.Errorf("mqtt: connect ␆ remaining length is %d instead of 2—connection closed", remainingLen)
	}

	if flags, err := r.ReadByte(); err != nil {
		conn.Close()
		return nil, err
	} else if flags > 1 {
		return nil, fmt.Errorf("mqtt: received reserved connect ␆ flags %#x—connection closed", flags)
	}

	if code, err := r.ReadByte(); err != nil {
		conn.Close()
		return nil, err
	} else if r := connectReturn(code); r != accepted {
		return nil, r
	}

	return bufio.NewReaderSize(conn, 64), nil
}

// Publish wires the message with QoS level 0—an “at most once” guarantee.
// Subscribers may or may not receive the message when subject to error.
// This fire-and-forget delivery is the most efficient option.
// Multiple goroutines may invoke Publish similtaneously.
func (c *Client) Publish(topic string, message []byte) error {
	return c.publish(topic, message, pubMsg<<4)
}

// PublishRetained acts like Publish, but causes the message to be stored on the
// server, so that they can be delivered to future subscribers.
func (c *Client) PublishRetained(topic string, message []byte, deliver QoS) error {
	return c.publish(topic, message, pubMsg<<4|retainFlag)
}

// PublishAtLeastOnce persists the message for delivery with QoS level 1—an “at
// least once” guarantee. This acknowledged delivery is more reliable than a
// plain Publish, at the expense of persistence overhead on both the client side
// and the broker side, plus an response message over the network.
func (c *Client) PublishAtLeastOnce(topic string, message []byte) error {
	packet, err := pubmsg(topic, message, pubMsg<<4|AtLeastOnce<<1)
	if err != nil {
		return err
	}
	return c.persistAndTrySend(c.atLeastOnceLine.assignID(), packet, message)
}

// PublishAtLeastOnceRetained acts like PublishAtLeastOnce, but causes the
// message to be stored on the server, so that they can be delivered to future
// subscribers.
func (c *Client) PublishAtLeastOnceRetained(topic string, message []byte) error {
	packet, err := pubmsg(topic, message, pubMsg<<4|AtLeastOnce<<1|retainFlag)
	if err != nil {
		return err
	}
	return c.persistAndTrySend(c.atLeastOnceLine.assignID(), packet, message)
}

// PublishExactlyOnce persists the message for delivery with QoS level 2—an
// “exactly once” guarantee. This double acknowledged delivery prevents the
// duplicate reception chance with PublishAtLeastOnce, at the expense of an
// extra network roundtrip.
func (c *Client) PublishExactlyOnce(topic string, message []byte) error {
	packet, err := pubmsg(topic, message, pubMsg<<4|ExactlyOnce<<1)
	if err != nil {
		return err
	}
	return c.persistAndTrySend(c.exactlyOnceLine.assignID(), packet, message)
}

// PublishExactlyOnceRetained acts like PublishExactlyOnce, but causes the
// message to be stored on the server, so that they can be delivered to future
// subscribers.
func (c *Client) PublishExactlyOnceRetained(topic string, message []byte) error {
	packet, err := pubmsg(topic, message, pubMsg<<4|ExactlyOnce<<1|retainFlag)
	if err != nil {
		return err
	}
	return c.persistAndTrySend(c.exactlyOnceLine.assignID(), packet, message)
}

func (c *Client) publish(topic string, message []byte, head byte) error {
	if err := stringCheck(topic); err != nil {
		return err
	}
	size := 2 + len(topic) + len(message)
	if size < 0 || size > packetMax {
		return errPacketMax
	}

	p := packetPool.Get().(*packet)
	defer packetPool.Put(p)
	p.buf = append(p.buf[:0], head)
	for ; size > 127; size >>= 7 {
		p.buf = append(p.buf, byte(size|128))
	}
	p.buf = append(p.buf, byte(size))
	p.buf = append(p.buf, byte(len(topic)>>8), byte(len(topic)))
	p.buf = append(p.buf, topic...)

	return c.writeBuffers(net.Buffers{p.buf, message})
}

// Pubmsg returns a publish message start, without the packet identifier,
// and without the payload.
func pubmsg(topic string, message []byte, head byte) (*packet, error) {
	if err := stringCheck(topic); err != nil {
		return nil, err
	}
	size := 4 + len(topic) + len(message)
	if size < 0 || size > packetMax {
		return nil, errPacketMax
	}

	p := packetPool.Get().(*packet)
	p.buf = append(p.buf[:0], head)
	for ; size > 127; size >>= 7 {
		p.buf = append(p.buf, byte(size|128))
	}
	p.buf = append(p.buf, byte(size))
	p.buf = append(p.buf, byte(len(topic)>>8), byte(len(topic)))
	p.buf = append(p.buf, topic...)
	return p, nil
}

func (c *Client) persistAndTrySend(packetID uint, packet *packet, message []byte) error {
	defer packetPool.Put(packet)
	packet.buf = append(packet.buf, byte(packetID>>8), byte(packetID))
	buffers := net.Buffers{packet.buf, message}

	key := packetID | localPacketIDFlag
	err := c.Persistence.Store(key, buffers)
	if err != nil {
		return err
	}

	if err := c.writeBuffers(buffers); err != nil {
		log.Print("mqtt: will retry on ", err)
	}
	return nil
}

// Subscribe requests a subscription for all topics that match any of the filters.
func (c *Client) Subscribe(topicFilters ...string) error {
	if len(topicFilters) == 0 {
		return nil
	}

	// measure & validate
	size := 2 + len(topicFilters)*3
	for _, s := range topicFilters {
		if err := stringCheck(s); err != nil {
			return err
		}
		size += len(s)
	}
	if size > packetMax {
		return errPacketMax
	}

	p := packetPool.Get().(*packet)
	defer packetPool.Put(p)
	if cap(p.buf) < size+5 {
		p.buf = make([]byte, 0, size)
	}

	p.buf = append(p.buf[:0], subReq<<4)
	for size > 127 {
		p.buf = append(p.buf, byte(size|128))
		size >>= 7
	}
	p.buf = append(p.buf, byte(size), 0, 1) // including subscriptionPacketID
	for _, s := range topicFilters {
		p.buf = append(p.buf, byte(len(s)>>8), byte(len(s)))
		p.buf = append(p.buf, s...)
		p.buf = append(p.buf, ExactlyOnce)
	}

	returnCodes := make(chan byte, len(topicFilters))
	c.subscriptionAck <- returnCodes // lock
	err := c.Persistence.Store(uint(subscriptionPacketID|localPacketIDFlag), net.Buffers{p.buf})
	if err != nil {
		<-c.subscriptionAck // unlock
		return err
	}
	if err := c.write(p.buf); err != nil {
		panic("TODO(pascaldekloe): Trigger reset")
	}

	var failures []int
	var i int
	for code := range returnCodes {
		switch code {
		case AtMostOnce, AtLeastOnce, ExactlyOnce:
			break // OK
		case 0x80:
			failures = append(failures, i)
		default:
			for range returnCodes {
			} // flush
			panic("TODO(pascaldekloe): Trigger reset")
		}
		i++
	}
	if i > len(topicFilters) {
		return fmt.Errorf("mqtt: subscription ␆ got %d return codes for %d topic filters", i, len(topicFilters))
	}
	if len(failures) != 0 {
		return fmt.Errorf("mqtt: subscription ␆ got return code failure for topic filters %d", failures)
	}
	return nil
}

// Unsubscribe requests a Subscribe cancelation.
func (c *Client) Unsubscribe(topicFilters ...string) error {
	if len(topicFilters) == 0 {
		return nil
	}

	// measure & validate
	size := 2 + len(topicFilters)*2
	for _, s := range topicFilters {
		if err := stringCheck(s); err != nil {
			return err
		}
		size += len(s)
	}
	if size > packetMax {
		return errPacketMax
	}

	p := packetPool.Get().(*packet)
	defer packetPool.Put(p)
	if cap(p.buf) < size+5 {
		p.buf = make([]byte, 0, size)
	}

	p.buf = append(p.buf[:0], unsubReq<<4)
	for size > 127 {
		p.buf = append(p.buf, byte(size|128))
		size >>= 7
	}
	p.buf = append(p.buf, byte(size), 0, 1) // including subscriptionPacketID
	for _, s := range topicFilters {
		p.buf = append(p.buf, byte(len(s)>>8), byte(len(s)))
		p.buf = append(p.buf, s...)
	}

	returnCodes := make(chan byte)
	c.subscriptionAck <- returnCodes // lock
	err := c.Persistence.Store(uint(subscriptionPacketID|localPacketIDFlag), net.Buffers{p.buf})
	if err != nil {
		<-c.subscriptionAck // unlock
		return err
	}
	if err := c.write(p.buf); err != nil {
		panic("TODO(pascaldekloe): Trigger reset")
	}

	for range returnCodes {
		panic("TODO(pascaldekloe): Trigger reset")
	}
	return nil
}

// Ping makes a roundtrip to validate the connection.
func (c *Client) Ping() error {
	ch := make(chan struct{})
	c.pingAck <- ch // lock

	if err := c.write(pingPacket.buf); err != nil {
		<-c.pingAck // unlock
		// TODO(pascaldekloe): Trigger reset
		return err
	}

	<-ch
	return nil
}

// Disconnect is a graceful termination, which also discards the Will.
// The underlying connection is closed.
func (c *Client) Disconnect() error {
	return c.write(disconnPacket.buf)
}
