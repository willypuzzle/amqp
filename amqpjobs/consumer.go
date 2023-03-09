package amqpjobs

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/sdk/v3/plugins/jobs"
	"github.com/roadrunner-server/sdk/v3/plugins/jobs/pipeline"
	"github.com/roadrunner-server/sdk/v3/plugins/status"
	priorityqueue "github.com/roadrunner-server/sdk/v3/priority_queue"
	"github.com/roadrunner-server/sdk/v3/utils"
	"go.uber.org/zap"
)

type Configurer interface {
	// UnmarshalKey takes a single key and unmarshals it into a Struct.
	UnmarshalKey(name string, out any) error

	// Has checks if config section exists.
	Has(name string) bool
}

const (
	pluginName string = "amqp"
)

type Consumer struct {
	mu           sync.Mutex
	log          *zap.Logger
	pq           priorityqueue.Queue
	pipeline     atomic.Pointer[pipeline.Pipeline]
	consumeAll   bool
	publishPlain bool

	// amqp connection notifiers
	notifyCloseConnCh    chan *amqp.Error
	notifyClosePubCh     chan *amqp.Error
	notifyCloseConsumeCh chan *amqp.Error
	notifyCloseStatCh    chan *amqp.Error
	redialCh             chan *redialMsg

	conn        *amqp.Connection
	consumeChan *amqp.Channel
	stateChan   chan *amqp.Channel
	publishChan chan *amqp.Channel
	consumeID   string
	connStr     string

	retryTimeout      time.Duration
	prefetch          int
	priority          int64
	exchangeName      string
	queue             string
	exclusive         bool
	exchangeType      string
	routingKey        string
	multipleAck       bool
	requeueOnFail     bool
	durable           bool
	deleteQueueOnStop bool

	// new in 2.12
	exchangeDurable    bool
	exchangeAutoDelete bool
	queueAutoDelete    bool

	// new in 2.12.2
	queueHeaders map[string]any

	listeners uint32
	delayed   *int64
	stopCh    chan struct{}
	stopped   uint32
}

// NewAMQPConsumer initializes rabbitmq pipeline
func NewAMQPConsumer(configKey string, log *zap.Logger, cfg Configurer, pq priorityqueue.Queue) (*Consumer, error) {
	const op = errors.Op("new_amqp_consumer")
	// we need to obtain two parts of the amqp information here.
	// firs part - address to connect, it is located in the global section under the amqp pluginName
	// second part - queues and other pipeline information
	// if no such key - error
	if !cfg.Has(configKey) {
		return nil, errors.E(op, errors.Errorf("no configuration by provided key: %s", configKey))
	}

	// if no global section
	if !cfg.Has(pluginName) {
		return nil, errors.E(op, errors.Str("no global amqp configuration, global configuration should contain amqp addrs"))
	}

	// PARSE CONFIGURATION START -------
	var conf config
	err := cfg.UnmarshalKey(configKey, &conf)
	if err != nil {
		return nil, errors.E(op, err)
	}

	err = cfg.UnmarshalKey(pluginName, &conf)
	if err != nil {
		return nil, errors.E(op, err)
	}

	conf.InitDefault()
	// PARSE CONFIGURATION END -------

	jb := &Consumer{
		log:          log,
		pq:           pq,
		consumeID:    uuid.NewString(),
		stopCh:       make(chan struct{}, 1),
		consumeAll:   conf.ConsumeAll,
		publishPlain: conf.PublishPlain,

		priority: conf.Priority,
		delayed:  utils.Int64(0),

		publishChan: make(chan *amqp.Channel, 1),
		stateChan:   make(chan *amqp.Channel, 1),
		redialCh:    make(chan *redialMsg, 5),

		notifyCloseConsumeCh: make(chan *amqp.Error, 1),
		notifyCloseConnCh:    make(chan *amqp.Error, 1),
		notifyCloseStatCh:    make(chan *amqp.Error, 1),
		notifyClosePubCh:     make(chan *amqp.Error, 1),

		routingKey:        conf.RoutingKey,
		queue:             conf.Queue,
		durable:           conf.Durable,
		exchangeType:      conf.ExchangeType,
		deleteQueueOnStop: conf.DeleteQueueOnStop,
		exchangeName:      conf.Exchange,
		prefetch:          conf.Prefetch,
		exclusive:         conf.Exclusive,
		multipleAck:       conf.MultipleAck,
		requeueOnFail:     conf.RequeueOnFail,

		// 2.12
		retryTimeout:       time.Duration(conf.RedialTimeout) * time.Second,
		exchangeAutoDelete: conf.ExchangeAutoDelete,
		exchangeDurable:    conf.ExchangeDurable,
		queueAutoDelete:    conf.QueueAutoDelete,
		// 2.12.2
		queueHeaders: conf.QueueHeaders,
	}

	jb.conn, err = amqp.Dial(conf.Addr)
	if err != nil {
		return nil, errors.E(op, err)
	}

	// save address
	jb.connStr = conf.Addr
	err = jb.initRabbitMQ()
	if err != nil {
		return nil, errors.E(op, err)
	}

	pch, err := jb.conn.Channel()
	if err != nil {
		return nil, errors.E(op, err)
	}

	stch, err := jb.conn.Channel()
	if err != nil {
		return nil, errors.E(op, err)
	}

	jb.conn.NotifyClose(jb.notifyCloseConnCh)
	pch.NotifyClose(jb.notifyClosePubCh)
	stch.NotifyClose(jb.notifyCloseStatCh)

	jb.publishChan <- pch
	jb.stateChan <- stch

	// run redialer and requeue listener for the connection
	jb.redialer()
	jb.redialMergeCh()

	return jb, nil
}

func FromPipeline(pipeline *pipeline.Pipeline, log *zap.Logger, cfg Configurer, pq priorityqueue.Queue) (*Consumer, error) {
	const op = errors.Op("new_amqp_consumer_from_pipeline")
	// we need to obtain two parts of the amqp information here.
	// firs part - address to connect, it is located in the global section under the amqp pluginName
	// second part - queues and other pipeline information

	// only global section
	if !cfg.Has(pluginName) {
		return nil, errors.E(op, errors.Str("no global amqp configuration, global configuration should contain amqp addrs"))
	}

	// PARSE CONFIGURATION -------
	var conf config
	err := cfg.UnmarshalKey(pluginName, &conf)
	if err != nil {
		return nil, errors.E(op, err)
	}
	conf.InitDefault()
	// PARSE CONFIGURATION -------

	// parse prefetch
	prf, err := strconv.Atoi(pipeline.String(prefetch, "10"))
	if err != nil {
		log.Error("prefetch parse, driver will use default (10) prefetch", zap.String("prefetch", pipeline.String(prefetch, "10")))
	}

	jb := &Consumer{
		log:       log,
		pq:        pq,
		consumeID: uuid.NewString(),
		stopCh:    make(chan struct{}, 1),
		delayed:   utils.Int64(0),

		publishChan: make(chan *amqp.Channel, 1),
		stateChan:   make(chan *amqp.Channel, 1),

		redialCh:             make(chan *redialMsg, 5),
		notifyCloseConsumeCh: make(chan *amqp.Error, 1),
		notifyCloseConnCh:    make(chan *amqp.Error, 1),
		notifyCloseStatCh:    make(chan *amqp.Error, 1),
		notifyClosePubCh:     make(chan *amqp.Error, 1),

		consumeAll:        pipeline.Bool(consumeAll, false),
		publishPlain:      pipeline.Bool(publishPlain, false),
		routingKey:        pipeline.String(routingKey, ""),
		queue:             pipeline.String(queue, "default"),
		exchangeType:      pipeline.String(exchangeType, "direct"),
		exchangeName:      pipeline.String(exchangeKey, "amqp.default"),
		prefetch:          prf,
		priority:          int64(pipeline.Int(priority, 10)),
		durable:           pipeline.Bool(durable, false),
		deleteQueueOnStop: pipeline.Bool(deleteOnStop, false),
		exclusive:         pipeline.Bool(exclusive, false),
		multipleAck:       pipeline.Bool(multipleAsk, false),
		requeueOnFail:     pipeline.Bool(requeueOnFail, false),

		// new in 2.12
		retryTimeout:       time.Duration(pipeline.Int(redialTimeout, 60)) * time.Second,
		exchangeAutoDelete: pipeline.Bool(exchangeAutoDelete, false),
		exchangeDurable:    pipeline.Bool(exchangeDurable, false),
		queueAutoDelete:    pipeline.Bool(queueAutoDelete, false),

		// 2.12.2
		queueHeaders: nil,
	}

	v := pipeline.String(queueHeaders, "")
	if v != "" {
		var tp map[string]any
		err = json.Unmarshal([]byte(v), &tp)
		if err != nil {
			log.Warn("failed to unmarshal headers", zap.String("value", v))
			return nil, err
		}

		jb.queueHeaders = tp
	}

	jb.conn, err = amqp.Dial(conf.Addr)
	if err != nil {
		return nil, errors.E(op, err)
	}

	// save address
	jb.connStr = conf.Addr

	err = jb.initRabbitMQ()
	if err != nil {
		return nil, errors.E(op, err)
	}

	pch, err := jb.conn.Channel()
	if err != nil {
		return nil, errors.E(op, err)
	}

	// channel to report amqp states
	stch, err := jb.conn.Channel()
	if err != nil {
		return nil, errors.E(op, err)
	}

	jb.conn.NotifyClose(jb.notifyCloseConnCh)
	pch.NotifyClose(jb.notifyClosePubCh)
	stch.NotifyClose(jb.notifyCloseStatCh)

	jb.publishChan <- pch
	jb.stateChan <- stch

	// register the pipeline
	// error here is always nil
	_ = jb.Register(context.Background(), pipeline)

	// run redialer for the connection
	jb.redialer()
	jb.redialMergeCh()

	return jb, nil
}

func (c *Consumer) Push(ctx context.Context, job *jobs.Job) error {
	const op = errors.Op("rabbitmq_push")
	// check if the pipeline registered

	// load atomic value
	pipe := c.pipeline.Load()
	if pipe.Name() != job.Options.Pipeline {
		return errors.E(op, errors.Errorf("no such pipeline: %s, actual: %s", job.Options.Pipeline, pipe.Name()))
	}

	err := c.handleItem(ctx, fromJob(job))
	if err != nil {
		return errors.E(op, err)
	}

	return nil
}

func (c *Consumer) Register(_ context.Context, p *pipeline.Pipeline) error {
	c.pipeline.Store(p)
	return nil
}

func (c *Consumer) Run(_ context.Context, p *pipeline.Pipeline) error {
	start := time.Now()
	const op = errors.Op("rabbit_run")

	pipe := c.pipeline.Load()
	if pipe.Name() != p.Name() {
		return errors.E(op, errors.Errorf("no such pipeline registered: %s", pipe.Name()))
	}

	// protect connection (redial)
	c.mu.Lock()
	defer c.mu.Unlock()

	// declare/bind/check the queue
	var err error
	err = c.declareQueue()
	if err != nil {
		return err
	}

	c.consumeChan, err = c.conn.Channel()
	if err != nil {
		return errors.E(op, err)
	}

	err = c.consumeChan.Qos(c.prefetch, 0, false)
	if err != nil {
		return errors.E(op, err)
	}

	// start reading messages from the channel
	deliv, err := c.consumeChan.Consume(
		c.queue,
		c.consumeID,
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return errors.E(op, err)
	}

	c.consumeChan.NotifyClose(c.notifyCloseConsumeCh)
	// run listener
	c.listener(deliv)

	atomic.StoreUint32(&c.listeners, 1)
	c.log.Debug("pipeline was started", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))
	return nil
}

func (c *Consumer) State(ctx context.Context) (*jobs.State, error) {
	const op = errors.Op("amqp_driver_state")
	select {
	case stateCh := <-c.stateChan:
		defer func() {
			c.stateChan <- stateCh
		}()

		q, err := stateCh.QueueInspect(c.queue)
		if err != nil {
			return nil, errors.E(op, err)
		}

		pipe := c.pipeline.Load()

		return &jobs.State{
			Priority: uint64(pipe.Priority()),
			Pipeline: pipe.Name(),
			Driver:   pipe.Driver(),
			Queue:    q.Name,
			Active:   int64(q.Messages),
			Delayed:  atomic.LoadInt64(c.delayed),
			Ready:    ready(atomic.LoadUint32(&c.listeners)),
		}, nil

	case <-ctx.Done():
		return nil, errors.E(op, errors.TimeOut, ctx.Err())
	}
}

func (c *Consumer) Pause(_ context.Context, p string) {
	start := time.Now()
	pipe := c.pipeline.Load()
	if pipe.Name() != p {
		c.log.Error("no such pipeline", zap.String("requested", p))
	}

	l := atomic.LoadUint32(&c.listeners)
	// no active listeners
	if l == 0 {
		c.log.Warn("no active listeners, nothing to pause")
		return
	}

	atomic.AddUint32(&c.listeners, ^uint32(0))

	// protect connection (redial)
	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.consumeChan.Cancel(c.consumeID, true)
	if err != nil {
		c.log.Error("cancel publish channel, forcing close", zap.Error(err))
		errCl := c.consumeChan.Close()
		if errCl != nil {
			c.log.Error("force close was failed", zap.Error(err))
			return
		}
		return
	}

	c.log.Debug("pipeline was paused", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))
}

func (c *Consumer) Resume(_ context.Context, p string) {
	start := time.Now()
	pipe := c.pipeline.Load()
	if pipe.Name() != p {
		c.log.Error("no such pipeline", zap.String("requested", p))
	}

	// protect connection (redial)
	c.mu.Lock()
	defer c.mu.Unlock()

	l := atomic.LoadUint32(&c.listeners)
	// no active listeners
	if l == 1 {
		c.log.Warn("amqp listener is already in the active state")
		return
	}

	var err error
	err = c.declareQueue()
	if err != nil {
		c.log.Error("unable to start listener", zap.Error(err))
		return
	}

	c.consumeChan, err = c.conn.Channel()
	if err != nil {
		c.log.Error("create channel", zap.Error(err))
		return
	}

	err = c.consumeChan.Qos(c.prefetch, 0, false)
	if err != nil {
		c.log.Error("QOS", zap.Error(err))
		return
	}

	// start reading messages from the channel
	deliv, err := c.consumeChan.Consume(
		c.queue,
		c.consumeID,
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		c.log.Error("consume operation was failed", zap.Error(err))
		return
	}

	// run listener
	c.listener(deliv)

	// increase number of listeners
	atomic.AddUint32(&c.listeners, 1)
	c.log.Debug("pipeline was resumed", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))
}

func (c *Consumer) Stop(context.Context) error {
	start := time.Now()
	atomic.StoreUint32(&c.stopped, 1)
	c.stopCh <- struct{}{}

	pipe := c.pipeline.Load()
	c.log.Debug("pipeline was stopped", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))
	close(c.redialCh)
	return nil
}

func (c *Consumer) Status() (*status.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch, err := c.conn.Channel()
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = ch.Close()
	}()

	_, err = ch.QueueInspect(c.queue)
	if err != nil {
		c.log.Error("queue inspect", zap.Error(err))
		return &status.Status{
			Code: 500,
		}, nil
	}

	return &status.Status{
		Code: 200,
	}, nil
}

func (c *Consumer) packMsg(msg *Item) (amqp.Table, error) {
	var table amqp.Table
	var err error

	if c.publishPlain {
		table, err = packPlain(msg)
	} else {
		table, err = pack(msg.ID(), msg)
	}

	return table, err
}

// handleItem
func (c *Consumer) handleItem(ctx context.Context, msg *Item) error {
	const op = errors.Op("rabbitmq_handle_item")
	select {
	case pch := <-c.publishChan:
		// return the channel back
		defer func() {
			c.publishChan <- pch
		}()

		// convert
		table, err := c.packMsg(msg)
		if err != nil {
			return errors.E(op, err)
		}

		// handle timeouts
		if msg.Options.DelayDuration() > 0 {
			atomic.AddInt64(c.delayed, 1)
			// TODO declare separate method for this if condition
			// TODO dlx cache channel??
			delayMs := int64(msg.Options.DelayDuration().Seconds() * 1000)
			tmpQ := fmt.Sprintf("delayed-%d.%s.%s", delayMs, c.exchangeName, c.queue)
			_, err = pch.QueueDeclare(tmpQ, true, false, false, false, amqp.Table{
				dlx:           c.exchangeName,
				dlxRoutingKey: c.routingKey,
				dlxTTL:        delayMs,
				dlxExpires:    delayMs * 2,
			})
			if err != nil {
				atomic.AddInt64(c.delayed, ^int64(0))
				return errors.E(op, err)
			}

			err = pch.QueueBind(tmpQ, tmpQ, c.exchangeName, false, nil)
			if err != nil {
				atomic.AddInt64(c.delayed, ^int64(0))
				return errors.E(op, err)
			}

			// insert to the local, limited pipeline
			err = pch.PublishWithContext(ctx, c.exchangeName, tmpQ, false, false, amqp.Publishing{
				Headers:      table,
				ContentType:  contentType,
				Timestamp:    time.Now(),
				DeliveryMode: amqp.Persistent,
				Body:         msg.Body(),
			})

			if err != nil {
				atomic.AddInt64(c.delayed, ^int64(0))
				return errors.E(op, err)
			}

			return nil
		}

		err = pch.PublishWithContext(ctx, c.exchangeName, c.routingKey, false, false, amqp.Publishing{
			Headers:      table,
			ContentType:  contentType,
			Timestamp:    time.Now(),
			DeliveryMode: amqp.Persistent,
			Body:         msg.Body(),
		})

		if err != nil {
			return errors.E(op, err)
		}

		return nil
	case <-ctx.Done():
		return errors.E(op, errors.TimeOut, ctx.Err())
	}
}

func ready(r uint32) bool {
	return r > 0
}
