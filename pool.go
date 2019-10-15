package mail

import (
	"errors"
	"io"
	"net/textproto"
	"sync"
	"syscall"
	"time"

)

// Original Code: github.com/jordan-wright/email

type Pool struct {
	newDialerFunc NewDialerFunc
	max           int
	created       int
	clients       chan *client
	rebuild       chan struct{}
	mut           *sync.Mutex
	lastBuildErr  *timestampedErr
	closing       chan struct{}
}

type client struct {
	SendCloser
	failCount int
}

type timestampedErr struct {
	err error
	ts  time.Time
}

const maxFails = 4

var (
	ErrClosed  = errors.New("pool closed")
	ErrTimeout = errors.New("timed out")
)

type NewDialerFunc func() *Dialer

func NewPool(newDialerFn NewDialerFunc, count int) (pool *Pool, err error) {
	pool = &Pool{
		newDialerFunc: newDialerFn,
		max:           count,
		clients:       make(chan *client, count),
		rebuild:       make(chan struct{}),
		closing:       make(chan struct{}),
		mut:           &sync.Mutex{},
	}
	return
}

func (p *Pool) get(timeout time.Duration) *client {
	select {
	case c := <-p.clients:
		return c
	default:
	}

	if p.created < p.max {
		p.makeOne()
	}

	var deadline <-chan time.Time
	if timeout >= 0 {
		deadline = time.After(timeout)
	}

	for {
		select {
		case c := <-p.clients:
			return c
		case <-p.rebuild:
			p.makeOne()
		case <-deadline:
			return nil
		case <-p.closing:
			return nil
		}
	}
}

func shouldReuse(err error) bool {
	// certainly not perfect, but might be close:
	//  - EOF: clearly, the connection went down
	//  - textproto.Errors were valid SMTP over a valid connection,
	//    but resulted from an SMTP error response
	//  - textproto.ProtocolErrors result from connections going down,
	//    invalid SMTP, that sort of thing
	//  - syscall.Errno is probably down connection/bad pipe, but
	//    passed straight through by textproto instead of becoming a
	//    ProtocolError
	//  - if we don't recognize the error, don't reuse the connection
	// A false positive will probably fail on the Reset(), and even if
	// not will eventually hit maxFails.
	// A false negative will knock over (and trigger replacement of) a
	// conn that might have still worked.
	if err == io.EOF {
		return false
	}
	switch err.(type) {
	case *textproto.Error:
		return true
	case *textproto.ProtocolError, textproto.ProtocolError:
		return false
	case syscall.Errno:
		return false
	default:
		return false
	}
}

func (p *Pool) replace(c *client) {
	p.clients <- c
}

func (p *Pool) inc() bool {
	if p.created >= p.max {
		return false
	}

	p.mut.Lock()
	defer p.mut.Unlock()

	if p.created >= p.max {
		return false
	}
	p.created++
	return true
}

func (p *Pool) dec() {
	p.mut.Lock()
	p.created--
	p.mut.Unlock()

	select {
	case p.rebuild <- struct{}{}:
	default:
	}
}

func (p *Pool) makeOne() {
	go func() {
		if p.inc() {
			if c, err := p.build(); err == nil {
				p.clients <- c
			} else {
				p.lastBuildErr = &timestampedErr{err, time.Now()}
				p.dec()
			}
		}
	}()
}

func (p *Pool) build() (*client, error) {
	cl, err := p.newDialerFunc().Dial()
	if err != nil {
		return nil, err
	}
	c := &client{cl, 0}
	return c, nil
}

func (p *Pool) maybeReplace(err error, c *client) {
	if err == nil {
		c.failCount = 0
		p.replace(c)
		return
	}

	c.failCount++
	if c.failCount >= maxFails {
		goto shutdown
	}

	if !shouldReuse(err) {
		goto shutdown
	}
	p.replace(c)
	return

shutdown:
	p.dec()
	c.Close()
}

func (p *Pool) failedToGet(startTime time.Time) error {
	select {
	case <-p.closing:
		return ErrClosed
	default:
	}

	if p.lastBuildErr != nil && startTime.Before(p.lastBuildErr.ts) {
		return p.lastBuildErr.err
	}

	return ErrTimeout
}

// Send sends an email via a connection pulled from the Pool. The timeout may
// be <0 to indicate no timeout. Otherwise reaching the timeout will produce
// and error building a connection that occurred while we were waiting, or
// otherwise ErrTimeout.
func (p *Pool) Send(from string, timeout time.Duration, msg ...*Message) (err error) {
	start := time.Now()
	c := p.get(timeout)
	if c == nil {
		return p.failedToGet(start)
	}

	defer func() {
		p.maybeReplace(err, c)
	}()

	err = Send(c, msg...)
	return
}

// Close immediately changes the pool's state so no new connections will be
// created, then gets and closes the existing ones as they become available.
func (p *Pool) Close() {
	close(p.closing)

	for p.created > 0 {
		c := <-p.clients
		c.Close()
		p.dec()
	}
}
