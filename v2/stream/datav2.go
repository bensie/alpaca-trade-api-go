package stream

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/common"
	"github.com/mitchellh/mapstructure"
	"github.com/vmihailenco/msgpack/v5"
	"nhooyr.io/websocket"
)

const (
	// MaxConnectionAttempts is the maximum number of retries for connecting to the websocket
	MaxConnectionAttempts = 3
)

var (
	// DataStreamURL is the URL for the data websocket stream.
	// The DATA_PROXY_WS environment variable overrides it.
	DataStreamURL = "https://data.alpaca.markets" // TODO: Probably this URL will change.

	// DataFeed is the selected data feed. Default: SIP.
	DataFeed = "SIP" // TODO: Figure this out based on the account
)

var (
	stream *datav2stream
)

type datav2stream struct {
	conn          *websocket.Conn
	authenticated atomic.Value
	closed        atomic.Value
	tradeHandlers map[string]func(trade Trade)
	quoteHandlers map[string]func(quote Quote)
	// TODO: add barsHandler
	// concurrency
	readerOnce    sync.Once
	wsWriteMutex  sync.Mutex
	wsReadMutex   sync.Mutex
	handlersMutex sync.Mutex
}

func newDatav2Stream() *datav2stream {
	if s := os.Getenv("DATA_PROXY_WS"); s != "" {
		DataStreamURL = s
	}
	stream = &datav2stream{
		authenticated: atomic.Value{},
		tradeHandlers: make(map[string]func(trade Trade)),
		quoteHandlers: make(map[string]func(quote Quote)),
	}

	stream.authenticated.Store(false)
	stream.closed.Store(false)

	return stream
}

func (s *datav2stream) subscribeTrades(handler func(trade Trade), symbols ...string) error {
	if err := s.ensureRunning(); err != nil {
		return err
	}

	if err := s.sub(symbols, nil, nil); err != nil {
		return err
	}

	s.handlersMutex.Lock()
	defer s.handlersMutex.Unlock()

	for _, symbol := range symbols {
		s.tradeHandlers[symbol] = handler
	}

	return nil
}

func (s *datav2stream) subscribeQuotes(handler func(quote Quote), symbols ...string) error {
	if err := s.ensureRunning(); err != nil {
		return err
	}

	if err := s.sub(nil, symbols, nil); err != nil {
		return err
	}

	s.handlersMutex.Lock()
	defer s.handlersMutex.Unlock()

	for _, symbol := range symbols {
		s.quoteHandlers[symbol] = handler
	}

	return nil
}

// TODO: add subscribeBars

func (s *datav2stream) unsubscribe(trades []string, quotes []string, bars []string) error {
	if err := s.ensureRunning(); err != nil {
		return err
	}

	s.handlersMutex.Lock()
	defer s.handlersMutex.Unlock()

	for _, trade := range trades {
		delete(s.tradeHandlers, trade)
	}
	for _, quote := range quotes {
		delete(s.quoteHandlers, quote)
	}
	// TODO: bars

	if err := s.unsub(trades, quotes, bars); err != nil {
		return err
	}

	return nil
}

func (s *datav2stream) close() error {
	if s.conn == nil {
		return nil
	}

	s.wsWriteMutex.Lock()
	defer s.wsWriteMutex.Unlock()

	if err := s.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		return err
	}

	// so we know it was gracefully closed
	s.closed.Store(true)

	return nil
}

func (s *datav2stream) ensureRunning() error {
	var err error
	if s.conn == nil {
		s.conn, err = openSocket()
		if err != nil {
			return err
		}
	}

	if err = s.auth(); err != nil {
		return err
	}
	s.readerOnce.Do(func() {
		go s.readForever()
	})
	return nil
}

func (s *datav2stream) reconnect() error {
	s.authenticated.Store(false)
	conn, err := openSocket()
	if err != nil {
		return err
	}
	s.conn = conn
	if err := s.auth(); err != nil {
		return err
	}
	trades := make([]string, 0, len(s.tradeHandlers))
	for trade := range s.tradeHandlers {
		trades = append(trades, trade)
	}
	quotes := make([]string, 0, len(s.quoteHandlers))
	for quote := range s.quoteHandlers {
		quotes = append(quotes, quote)
	}
	bars := make([]string, 0)
	// TODO: bars

	return s.sub(trades, quotes, bars)
}

func (s *datav2stream) readForever() {
	for {
		s.wsReadMutex.Lock()
		msgType, b, err := s.conn.Read(context.TODO())
		s.wsReadMutex.Unlock()

		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				// if this was a graceful closure, don't reconnect
				if s.closed.Load().(bool) {
					return
				}
			} else {
				log.Printf("alpaca stream read error (%v)", err)
			}

			err := s.reconnect()
			if err != nil {
				panic(err)
			}
		}
		if msgType != websocket.MessageBinary {
			continue
		}

		var messages []map[string]interface{}
		if err = msgpack.Unmarshal(b, &messages); err != nil {
			log.Printf("failed to incoming unmarshal message: %v", err)
			continue
		}

		for _, msg := range messages {
			if err := s.handleMsg(msg); err != nil {
				log.Printf("error handling incoming message: %v", err)
				continue
			}
		}
	}
}

func (s *datav2stream) handleMsg(msg map[string]interface{}) error {
	T, ok := msg["T"].(string)
	if !ok {
		return errors.New("unexpected message: T missing")
	}

	switch T {
	case "t":
		symbol, ok := msg["S"].(string)
		if !ok {
			return errors.New("unexpected message: S missing")
		}

		s.handlersMutex.Lock()
		defer s.handlersMutex.Unlock()

		handler, ok := s.tradeHandlers[symbol]
		if !ok {
			handler, ok = s.tradeHandlers["*"]
			if !ok {
				return errors.New("trade handler missing for symbol: " + symbol)
			}
		}

		var trade Trade
		if err := mapstructure.Decode(msg, &trade); err != nil {
			return err
		}

		handler(trade)
	case "q":
		symbol, ok := msg["S"].(string)
		if !ok {
			return errors.New("unexpected message: S missing")
		}

		s.handlersMutex.Lock()
		defer s.handlersMutex.Unlock()

		handler, ok := s.quoteHandlers[symbol]
		if !ok {
			handler, ok = s.quoteHandlers["*"]
			if !ok {
				return errors.New("quote handler missing for symbol: " + symbol)
			}
		}

		var quote Quote
		if err := mapstructure.Decode(msg, &quote); err != nil {
			return err
		}
		handler(quote)
	}
	// TODO: bars
	return nil
}

func (s *datav2stream) sub(trades []string, quotes []string, bars []string) error {
	return s.handleSubscription(true, trades, quotes, bars)
}

func (s *datav2stream) unsub(trades []string, quotes []string, bars []string) error {
	return s.handleSubscription(false, trades, quotes, bars)
}

func (s *datav2stream) handleSubscription(subscribe bool, trades []string, quotes []string, bars []string) error {
	action := "subscribe"
	if !subscribe {
		action = "unsubscribe"
	}

	msg, err := msgpack.Marshal(map[string]interface{}{
		"action": action,
		"trades": trades,
		"quotes": quotes,
		"bars":   bars,
	})
	if err != nil {
		return err
	}

	s.wsWriteMutex.Lock()
	defer s.wsWriteMutex.Unlock()

	if err := s.conn.Write(context.TODO(), websocket.MessageBinary, msg); err != nil {
		return err
	}

	return nil
}

func (s *datav2stream) isAuthenticated() bool {
	return s.authenticated.Load().(bool)
}

func (s *datav2stream) auth() (err error) {
	if s.isAuthenticated() {
		return
	}

	msg, err := msgpack.Marshal(map[string]string{
		"action": "auth",
		"key":    common.Credentials().ID,
		"secret": common.Credentials().Secret,
	})
	if err != nil {
		return err
	}

	s.wsWriteMutex.Lock()
	defer s.wsWriteMutex.Unlock()

	if err := s.conn.Write(context.TODO(), websocket.MessageBinary, msg); err != nil {
		return err
	}

	var resps []map[string]interface{}

	// ensure the auth response comes in a timely manner
	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	s.wsReadMutex.Lock()
	defer s.wsReadMutex.Unlock()

	_, b, err := s.conn.Read(ctx)
	if err != nil {
		return err
	}
	if err := msgpack.Unmarshal(b, &resps); err != nil {
		return err
	}
	if len(resps) < 1 {
		return errors.New("received empty array")
	}
	if resps[0]["T"] != "success" || resps[0]["msg"] != "authenticated" {
		return errors.New("failed to authorize alpaca stream")
	}

	s.authenticated.Store(true)

	return
}

func openSocket() (*websocket.Conn, error) {
	scheme := "wss"
	ub, _ := url.Parse(DataStreamURL)
	switch ub.Scheme {
	case "http", "ws":
		scheme = "ws"
	}
	u := url.URL{Scheme: scheme, Host: ub.Host, Path: "/v2/stream/" + strings.ToLower(DataFeed)}
	for attempts := 1; attempts <= MaxConnectionAttempts; attempts++ {
		c, _, err := websocket.Dial(context.TODO(), u.String(), &websocket.DialOptions{
			CompressionMode: websocket.CompressionContextTakeover,
			HTTPHeader: http.Header{
				"Content-Type": []string{"application/msgpack"},
			},
		})
		if err == nil {
			return c, readConnected(c)
		}
		log.Printf("failed to open Alpaca data stream: %v", err)
		if attempts == MaxConnectionAttempts {
			return nil, err
		}
		time.Sleep(time.Second)
	}
	return nil, errors.New("could not open Alpaca data stream (max retries exceeded)")
}

func readConnected(conn *websocket.Conn) error {
	_, b, err := conn.Read(context.TODO())
	if err != nil {
		return err
	}
	var resps []map[string]interface{}
	if err := msgpack.Unmarshal(b, &resps); err != nil {
		return err
	}
	if len(resps) < 1 {
		return errors.New("received empty array")
	}
	if resps[0]["T"] != "success" || resps[0]["msg"] != "connected" {
		return errors.New("missing connected message")
	}
	return nil
}
