package chrome

import (
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/unixpickle/essentials"
)

// Conn is a connection to a protocol endpoint.
type Conn struct {
	ws *websocket.Conn

	sendLock sync.Mutex
	curID    int

	// Closed when the read loop is complete.
	doneChan <-chan struct{}

	// Keep track of calls that are awaiting responses.
	waitingLock sync.Mutex
	waiting     map[int]*waitingCall

	// Callers which are polling for events.
	pollingLock sync.Mutex
	polling     map[string][]*waitingCall

	firstErrLock sync.RWMutex
	firstErr     error
}

// NewConn connects to an endpoint's WebSocket URL.
func NewConn(websocketURL string) (conn *Conn, err error) {
	defer essentials.AddCtxTo("connect to DevTools endpoint", &err)
	dialer := websocket.Dialer{}
	ws, _, err := dialer.Dial(websocketURL, nil)
	if err != nil {
		return
	}
	doneChan := make(chan struct{})
	conn = &Conn{
		ws:       ws,
		doneChan: doneChan,
		waiting:  map[int]*waitingCall{},
		polling:  map[string][]*waitingCall{},
	}
	go func() {
		defer close(doneChan)
		conn.readLoop()
	}()
	return
}

// Close closes the connection.
func (c *Conn) Close() error {
	err := c.ws.Close()
	<-c.doneChan
	return err
}

// Error returns the first connection error encountered.
// If no errors have been encountered, nil is returned.
func (c *Conn) Error() error {
	c.firstErrLock.RLock()
	defer c.firstErrLock.RUnlock()
	return c.firstErr
}

// call makes an API call.
//
// The return value of the API call is unmarshalled into
// resObj following the rules of encoding/json.
//
// If a connection or JSON error occurs, it is returned.
// Just because a call fails with an error doesn't mean
// the connection is dead; it could simply mean that the
// result didn't unmarshal.
func (c *Conn) call(name string, params interface{},
	resObj interface{}) (err error) {
	defer essentials.AddCtxTo("DevTools "+name+" call", &err)

	payload := map[string]interface{}{"method": name}
	if params != nil {
		payload["params"] = params
	}
	doneChan := make(chan error, 1)

	c.sendLock.Lock()
	c.curID++
	payload["id"] = c.curID

	c.waitingLock.Lock()
	c.waiting[c.curID] = &waitingCall{
		OutPtr:   resObj,
		DoneChan: doneChan,
	}
	c.waitingLock.Unlock()

	err = c.ws.WriteJSON(payload)
	c.sendLock.Unlock()

	if err != nil {
		c.gotError("write DevTools message", err)
		return
	}

	select {
	case unmarshalErr := <-doneChan:
		return unmarshalErr
	case <-c.doneChan:
		return errors.New("closed before reading return value")
	}
}

// poll asynchronously waits for an event.
//
// This works very similarly to call, except that poll is
// non-blocking and the error is sent over a channel.
//
// The event handler is guaranteed to be registered by the
// time poll returns.
// The returned channel is closed after the event arrives.
// If no error occurs, the channel will be closed without
// having any value sent to it.
//
// The resObj argument may be written after poll returns,
// since poll is non-blocking.
// However, it will never be written after the error
// channel has been closed.
func (c *Conn) poll(name string, resObj interface{}) <-chan error {
	doneChan := make(chan error, 1)

	c.pollingLock.Lock()
	c.polling[name] = append(c.polling[name], &waitingCall{
		OutPtr:   resObj,
		DoneChan: doneChan,
	})
	c.pollingLock.Unlock()

	res := make(chan error, 1)
	go func() {
		var err error
		select {
		case err = <-doneChan:
		case <-c.doneChan:
			err = errors.New("closed before receiving event")
		}
		if err != nil {
			res <- essentials.AddCtx("poll DevTools "+name+" event", err)
		}
		close(res)
	}()

	return res
}

func (c *Conn) readLoop() {
	for {
		const errContext = "read DevTools message"

		_, reader, err := c.ws.NextReader()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			c.gotError(errContext, err)
			return
		}
		data, err := ioutil.ReadAll(reader)
		if err != nil {
			c.gotError(errContext, err)
			return
		}

		var obj struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(data, &obj); err != nil {
			c.gotError(errContext, err)
			return
		}

		if obj.ID != 0 {
			// We don't run this asynchronously because of a race
			// condition where the remote disconnects after sending
			// the return value.
			c.handleReturnValue(obj.ID, data)
		} else {
			c.handleEvent(obj.Method, data)
		}
	}
}

func (c *Conn) handleReturnValue(id int, data []byte) {
	c.waitingLock.Lock()
	waiter := c.waiting[id]
	delete(c.waiting, id)
	c.waitingLock.Unlock()

	if waiter == nil {
		// Server replied to a message we didn't send.
		return
	}

	var obj struct {
		Result interface{} `json:"result"`
	}
	obj.Result = waiter.OutPtr
	waiter.DoneChan <- json.Unmarshal(data, &obj)
}

func (c *Conn) handleEvent(method string, data []byte) {
	c.pollingLock.Lock()
	pollers := c.polling[method]
	delete(c.polling, method)
	c.pollingLock.Unlock()

	for _, poller := range pollers {
		var obj struct {
			Params interface{} `json:"params"`
		}
		obj.Params = poller.OutPtr
		poller.DoneChan <- json.Unmarshal(data, &obj)
	}
}

func (c *Conn) gotError(ctx string, e error) {
	c.firstErrLock.Lock()
	defer c.firstErrLock.Unlock()
	if c.firstErr == nil {
		c.firstErr = essentials.AddCtx(ctx, e)
	}
}

type waitingCall struct {
	OutPtr   interface{}
	DoneChan chan<- error
}