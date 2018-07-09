// Package nsq provides an NSQ broker
package nsq

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/micro/go-micro/broker"
	"github.com/micro/go-micro/broker/codec/json"
	"github.com/micro/go-micro/cmd"
	"github.com/nsqio/go-nsq"
	"github.com/pborman/uuid"
)

type nsqBroker struct {
	nsqdTCPAddrs     []string
	lookupdHTTPAddrs []string
	opts             broker.Options
	config           *nsq.Config

	sync.Mutex
	running bool
	p       []*nsq.Producer
	c       []*subscriber
}

type publication struct {
	topic string
	m     *broker.Message
	nm    *nsq.Message
	opts  broker.PublishOptions
}

type subscriber struct {
	topic string
	opts  broker.SubscribeOptions

	c *nsq.Consumer

	// handler so we can resubcribe
	h nsq.HandlerFunc
	// concurrency
	n int
}

var (
	DefaultConcurrentHandlers = 1
)

func init() {
	rand.Seed(time.Now().UnixNano())
	cmd.DefaultBrokers["nsq"] = NewBroker
}

func (n *nsqBroker) Init(opts ...broker.Option) error {
	for _, o := range opts {
		o(&n.opts)
	}

	n.initByContext(n.opts.Context)

	return nil
}

func (n *nsqBroker) initByContext(ctx context.Context) {
	if v, ok := ctx.Value(lookupdHTTPAddrsKey{}).([]string); ok {
		n.lookupdHTTPAddrs = v
	}

	if v, ok := ctx.Value(consumerOptsKey{}).([]string); ok {
		cfgFlag := &nsq.ConfigFlag{Config: n.config}
		for _, opt := range v {
			cfgFlag.Set(opt)
		}
	}
}

func (n *nsqBroker) Options() broker.Options {
	return n.opts
}

func (n *nsqBroker) Address() string {
	return n.nsqdTCPAddrs[rand.Intn(len(n.nsqdTCPAddrs))]
}

func (n *nsqBroker) Connect() error {
	n.Lock()
	defer n.Unlock()

	if n.running {
		return nil
	}

	var producers []*nsq.Producer

	// create producers
	for _, addr := range n.nsqdTCPAddrs {
		p, err := nsq.NewProducer(addr, n.config)
		if err != nil {
			return err
		}
		if err = p.Ping(); err != nil {
			return err
		}
		producers = append(producers, p)
	}

	// create consumers
	for _, c := range n.c {
		channel := c.opts.Queue
		if len(channel) == 0 {
			channel = uuid.NewUUID().String() + "#ephemeral"
		}

		cm, err := nsq.NewConsumer(c.topic, channel, n.config)
		if err != nil {
			return err
		}

		cm.AddConcurrentHandlers(c.h, c.n)

		c.c = cm

		if len(n.lookupdHTTPAddrs) > 0 {
			c.c.ConnectToNSQLookupds(n.lookupdHTTPAddrs)
		} else {
			err = c.c.ConnectToNSQDs(n.nsqdTCPAddrs)
			if err != nil {
				return err
			}
		}
	}

	n.p = producers
	n.running = true
	return nil
}

func (n *nsqBroker) Disconnect() error {
	n.Lock()
	defer n.Unlock()

	if !n.running {
		return nil
	}

	// stop the producers
	for _, p := range n.p {
		p.Stop()
	}

	// stop the consumers
	for _, c := range n.c {
		c.c.Stop()

		if len(n.lookupdHTTPAddrs) > 0 {
			// disconnect from all lookupd
			for _, addr := range n.lookupdHTTPAddrs {
				c.c.DisconnectFromNSQLookupd(addr)
			}
		} else {
			// disconnect from all nsq brokers
			for _, addr := range n.nsqdTCPAddrs {
				c.c.DisconnectFromNSQD(addr)
			}
		}
	}

	n.p = nil
	n.running = false
	return nil
}

func (n *nsqBroker) Publish(topic string, message *broker.Message, opts ...broker.PublishOption) error {
	p := n.p[rand.Intn(len(n.p))]

	options := broker.PublishOptions{}
	for _, o := range opts {
		o(&options)
	}

	var (
		doneChan chan *nsq.ProducerTransaction
		delay    time.Duration
	)
	if options.Context != nil {
		if v, ok := options.Context.Value(asyncPublishKey{}).(chan *nsq.ProducerTransaction); ok {
			doneChan = v
		}
		if v, ok := options.Context.Value(deferredPublishKey{}).(time.Duration); ok {
			delay = v
		}
	}

	b, err := n.opts.Codec.Marshal(message)
	if err != nil {
		return err
	}

	if doneChan != nil {
		if delay > 0 {
			return p.DeferredPublishAsync(topic, delay, b, doneChan)
		}
		return p.PublishAsync(topic, b, doneChan)
	} else {
		if delay > 0 {
			return p.DeferredPublish(topic, delay, b)
		}
		return p.Publish(topic, b)
	}
}

func (n *nsqBroker) Subscribe(topic string, handler broker.Handler, opts ...broker.SubscribeOption) (broker.Subscriber, error) {
	options := broker.SubscribeOptions{
		AutoAck: true,
	}

	for _, o := range opts {
		o(&options)
	}

	concurrency, maxInFlight := DefaultConcurrentHandlers, DefaultConcurrentHandlers
	if options.Context != nil {
		if v, ok := options.Context.Value(concurrentHandlerKey{}).(int); ok {
			maxInFlight, concurrency = v, v
		}
		if v, ok := options.Context.Value(maxInFlightKey{}).(int); ok {
			maxInFlight = v
		}
	}
	channel := options.Queue
	if len(channel) == 0 {
		channel = uuid.NewUUID().String() + "#ephemeral"
	}
	config := *n.config
	config.MaxInFlight = maxInFlight

	c, err := nsq.NewConsumer(topic, channel, &config)
	if err != nil {
		return nil, err
	}

	h := nsq.HandlerFunc(func(nm *nsq.Message) error {
		if !options.AutoAck {
			nm.DisableAutoResponse()
		}

		var m broker.Message

		if err := n.opts.Codec.Unmarshal(nm.Body, &m); err != nil {
			return err
		}

		return handler(&publication{
			topic: topic,
			m:     &m,
			nm:    nm,
		})
	})

	c.AddConcurrentHandlers(h, concurrency)

	if len(n.lookupdHTTPAddrs) > 0 {
		err = c.ConnectToNSQLookupds(n.lookupdHTTPAddrs)
	} else {
		err = c.ConnectToNSQDs(n.nsqdTCPAddrs)
	}
	if err != nil {
		return nil, err
	}

	sub := &subscriber{
		c:     c,
		opts:  options,
		topic: topic,
		h:     h,
		n:     concurrency,
	}

	n.c = append(n.c, sub)

	return sub, nil
}

func (n *nsqBroker) String() string {
	return "nsq"
}

func (p *publication) Topic() string {
	return p.topic
}

func (p *publication) Message() *broker.Message {
	return p.m
}

func (p *publication) Ack() error {
	p.nm.Finish()
	return nil
}

func (s *subscriber) Options() broker.SubscribeOptions {
	return s.opts
}

func (s *subscriber) Topic() string {
	return s.topic
}

func (s *subscriber) Unsubscribe() error {
	s.c.Stop()
	return nil
}

func NewBroker(opts ...broker.Option) broker.Broker {
	options := broker.Options{
		// Default codec
		Codec: json.NewCodec(),
		// Default context
		Context: context.Background(),
	}

	for _, o := range opts {
		o(&options)
	}

	var nsqdTCPAddrs []string

	for _, addr := range options.Addrs {
		if len(addr) > 0 {
			nsqdTCPAddrs = append(nsqdTCPAddrs, addr)
		}
	}

	if len(nsqdTCPAddrs) == 0 {
		nsqdTCPAddrs = []string{"127.0.0.1:4150"}
	}

	return &nsqBroker{
		nsqdTCPAddrs: nsqdTCPAddrs,
		opts:         options,
		config:       nsq.NewConfig(),
	}
}
