package throughputlb

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

var (
	errUnavailable         = grpc.Errorf(codes.Unavailable, "there is no address available")
	errMaxRequestsExceeded = errors.New("max requests exceeded")
)

type addrState int64

const (
	stateDown addrState = iota
	stateUp
)

type address struct {
	grpc.Address

	mu             sync.RWMutex
	state          addrState
	activeRequests int
	maxRequests    int
}

func (a *address) claim() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.activeRequests >= a.maxRequests {
		return errMaxRequestsExceeded
	}

	a.activeRequests++

	return nil
}

func (a *address) release() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.activeRequests--
}

func (a *address) goUp() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.state = stateUp
}

func (a *address) goDown(_ error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// TODO: Handle error

	a.state = stateDown
}

func (a *address) isUp() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.state == stateUp
}

func (a *address) isDown() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.state == stateDown
}

func (a *address) capacity() int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.activeRequests
}

type ThroughputLoadBalancerOption func(*ThroughputLoadBalancer)

func WithCleanupInterval(d time.Duration) ThroughputLoadBalancerOption {
	return func(lb *ThroughputLoadBalancer) {
		lb.cleanupInterval = d
	}
}

type ThroughputLoadBalancer struct {
	mu    sync.RWMutex
	addrs []*address

	target          string
	notify          chan []grpc.Address
	maxRequests     int
	numAddrs        int
	cleanupInterval time.Duration
}

func NewThroughputLoadBalancer(
	maxRequests int,
	numAddrs int,
	opts ...ThroughputLoadBalancerOption,
) *ThroughputLoadBalancer {
	lb := &ThroughputLoadBalancer{
		notify:          make(chan []grpc.Address, numAddrs),
		addrs:           make([]*address, numAddrs, numAddrs),
		maxRequests:     maxRequests,
		numAddrs:        numAddrs,
		cleanupInterval: time.Minute,
	}

	for _, o := range opts {
		o(lb)
	}

	return lb
}

func (lb *ThroughputLoadBalancer) Start(target string, cfg grpc.BalancerConfig) error {
	// TODO: Validate target and return error if invalid

	lb.mu.Lock()
	lb.target = target
	for i := 0; i < lb.numAddrs; i++ {
		lb.addrs[i] = &address{
			Address: grpc.Address{
				Addr:     lb.target,
				Metadata: i,
			},
			maxRequests: lb.maxRequests,
		}
	}
	lb.mu.Unlock()

	lb.sendNotify()

	return nil
}

func (lb *ThroughputLoadBalancer) Up(addr grpc.Address) func(error) {
	lb.mu.RLock()
	addrs := lb.addrs
	lb.mu.RUnlock()

	for _, a := range addrs {
		if a.Address == addr {
			a.goUp()

			return a.goDown
		}
	}

	return func(_ error) {}
}

func (lb *ThroughputLoadBalancer) Get(ctx context.Context, opts grpc.BalancerGetOptions) (grpc.Address, func(), error) {
	addr, err := lb.next(opts.BlockingWait)
	if err != nil {
		return grpc.Address{}, func() {}, err
	}

	return addr.Address, addr.release, nil
}

func (lb *ThroughputLoadBalancer) Notify() <-chan []grpc.Address {
	return lb.notify
}

func (*ThroughputLoadBalancer) Close() error {
	// TODO: Should this remove all addresses and notify or just stop opperation?

	return nil
}

func (lb *ThroughputLoadBalancer) sendNotify() {
	lb.mu.RLock()
	addrs := lb.addrs
	lb.mu.RUnlock()

	grpcAddrs := make([]grpc.Address, len(addrs))
	for i, a := range addrs {
		grpcAddrs[i] = a.Address
	}

	lb.notify <- grpcAddrs
}

func (lb *ThroughputLoadBalancer) next(wait bool) (*address, error) {
	for {
		var addr *address
		lowestCapacity := lb.maxRequests * 2

		lb.mu.RLock()
		for _, a := range lb.addrs {
			if a.isDown() {
				continue
			}

			if a.capacity() < lowestCapacity {
				addr = a
				lowestCapacity = a.capacity()
			}
		}
		lb.mu.RUnlock()

		if addr != nil {
			addr.claim()
			return addr, nil
		}

		if !wait {
			return nil, errUnavailable
		}

		time.Sleep(50 * time.Millisecond)
	}
}
