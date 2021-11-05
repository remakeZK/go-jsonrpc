package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	rpcParseError     = -32700
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcMaxLimit       = -32603
)

// RPCServer provides a jsonrpc 2.0 http server handler
type RPCServer struct {
	methods map[string]rpcHandler

	// aliasedMethods contains a map of alias:original method names.
	// These are used as fallbacks if a method is not found by the given method name.
	aliasedMethods map[string]string

	paramDecoders map[reflect.Type]ParamDecoder

	maxRequestSize int64
}

// NewServer creates new RPCServer instance
func NewServer(opts ...ServerOption) *RPCServer {
	config := defaultServerConfig()
	for _, o := range opts {
		o(&config)
	}

	return &RPCServer{
		methods:        map[string]rpcHandler{},
		aliasedMethods: map[string]string{},
		paramDecoders:  config.paramDecoders,
		maxRequestSize: config.maxRequestSize,
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (s *RPCServer) handleWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// TODO: allow setting
	// (note that we still are mostly covered by jwt tokens)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Header.Get("Sec-WebSocket-Protocol") != "" {
		w.Header().Set("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error(err)
		w.WriteHeader(500)
		return
	}

	(&wsConn{
		conn:    c,
		handler: s,
		exiting: make(chan struct{}),
	}).handleWsConn(ctx)

	if err := c.Close(); err != nil {
		log.Error(err)
		return
	}
}

type SimpleLimiter struct {
	lm map[string]int
	mu *sync.Mutex
}

var maxRequestTimes int
var l SimpleLimiter

func init() {
	maxRequestTimes = 60
	if s := os.Getenv("FORCE_API_LIMITER"); s != "" {
		tmp, err := strconv.Atoi(s)
		if err != nil {
			panic(fmt.Errorf("force api limiter should be valid: %w", err))
		}
		maxRequestTimes = tmp
	}

	l.mu = &sync.Mutex{}
	l.lm = map[string]int{}

	go func() {
		for {
			log.Info("refreshing limit map")
			time.Sleep(time.Minute)
			l.mu.Lock()
			l.lm = map[string]int{}
			l.mu.Unlock()
		}
	}()

}

// TODO: return errors to clients per spec
func (s *RPCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	h := strings.ToLower(r.Header.Get("Connection"))
	if strings.Contains(h, "upgrade") {
		s.handleWS(ctx, w, r)
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		//return nil, fmt.Errorf("userip: %q is not IP:port", req.RemoteAddr)
		log.Errorf("error remote addr: %s", err)
	}
	l.mu.Lock()
	l.lm[ip]++
	count := l.lm[ip]
	l.mu.Unlock()

	if count >= maxRequestTimes {
		wf := func(cb func(io.Writer)) {
			cb(w)
		}

		bufferedRequest := new(bytes.Buffer)
		var req request
		// We use LimitReader to enforce maxRequestSize. Since it won't return an
		// EOF we can't actually know if the client sent more than the maximum or
		// not, so we read one byte more over the limit to explicitly query that.
		// FIXME: Maybe there's a cleaner way to do this.
		reqSize, err := bufferedRequest.ReadFrom(io.LimitReader(r.Body, s.maxRequestSize+1))
		if err != nil {
			// ReadFrom will discard EOF so any error here is unexpected and should
			// be reported.
			rpcError(wf, &req, rpcParseError, fmt.Errorf("reading request: %w", err))
			return
		}
		if reqSize > s.maxRequestSize {
			rpcError(wf, &req, rpcParseError,
				// rpcParseError is the closest we have from the standard errors defined
				// in [jsonrpc spec](https://www.jsonrpc.org/specification#error_object)
				// to report the maximum limit.
				fmt.Errorf("request bigger than maximum %d allowed",
					s.maxRequestSize))
			return
		}

		if err := json.NewDecoder(bufferedRequest).Decode(&req); err != nil {
			rpcError(wf, &req, rpcParseError, fmt.Errorf("unmarshaling request: %w", err))
			return
		}

		log.Warnf("ip %s reach to limit method %s count %d\n", ip, req.Method, count)
		rpcError(wf, &req, rpcMaxLimit, fmt.Errorf("request meet limit %s %d", ip, count))
		return
	}

	s.handleReader(ctx, r.Body, w, rpcError)
}

func rpcError(wf func(func(io.Writer)), req *request, code int, err error) {
	log.Errorf("RPC Error: %s", err)
	wf(func(w io.Writer) {
		if hw, ok := w.(http.ResponseWriter); ok {
			hw.WriteHeader(500)
		}

		log.Warnf("rpc error: %s", err)

		if req.ID == nil { // notification
			return
		}

		resp := response{
			Jsonrpc: "2.0",
			ID:      *req.ID,
			Error: &respError{
				Code:    code,
				Message: err.Error(),
			},
		}

		err = json.NewEncoder(w).Encode(resp)
		if err != nil {
			log.Warnf("failed to write rpc error: %s", err)
			return
		}
	})
}

// Register registers new RPC handler
//
// Handler is any value with methods defined
func (s *RPCServer) Register(namespace string, handler interface{}) {
	s.register(namespace, handler)
}

func (s *RPCServer) AliasMethod(alias, original string) {
	s.aliasedMethods[alias] = original
}

var _ error = &respError{}
