package amqpjobs

import (
	"fmt"

	"github.com/google/uuid"
)

// pipeline rabbitmq info
const (
	exchangeKey   string = "exchange"
	exchangeType  string = "exchange_type"
	queue         string = "queue"
	routingKey    string = "routing_key"
	consumeAll    string = "consume_all"
	publishPlain  string = "publish_plain"
	prefetch      string = "prefetch"
	exclusive     string = "exclusive"
	durable       string = "durable"
	deleteOnStop  string = "delete_queue_on_stop"
	priority      string = "priority"
	multipleAsk   string = "multiple_ask"
	requeueOnFail string = "requeue_on_fail"

	// new in 2.12
	redialTimeout      string = "redial_timeout"
	exchangeDurable    string = "exchange_durable"
	exchangeAutoDelete string = "exchange_auto_delete"
	queueAutoDelete    string = "queue_auto_delete"

	dlx           string = "x-dead-letter-exchange"
	dlxRoutingKey string = "x-dead-letter-routing-key"
	dlxTTL        string = "x-message-ttl"
	dlxExpires    string = "x-expires"

	// new in 2.12.2
	queueHeaders string = "queue_headers"

	// new in 2023.1.0
	consumerIDKey string = "consumer_id"

	contentType string = "application/octet-stream"
)

// config is used to parse pipeline configuration
type config struct {
	// global
	Addr string `mapstructure:"addr"`

	// local
	Prefetch     int    `mapstructure:"prefetch"`
	Queue        string `mapstructure:"queue"`
	Priority     int64  `mapstructure:"priority"`
	Exchange     string `mapstructure:"exchange"`
	ExchangeType string `mapstructure:"exchange_type"`

	RoutingKey        string `mapstructure:"routing_key"`
	ConsumeAll        bool   `mapstructure:"consume_all"`
	PublishPlain      bool   `mapstructure:"publish_plain"`
	Exclusive         bool   `mapstructure:"exclusive"`
	Durable           bool   `mapstructure:"durable"`
	DeleteQueueOnStop bool   `mapstructure:"delete_queue_on_stop"`
	MultipleAck       bool   `mapstructure:"multiple_ask"`
	RequeueOnFail     bool   `mapstructure:"requeue_on_fail"`

	// new in 2.12.1
	ExchangeDurable    bool `mapstructure:"exchange_durable"`
	ExchangeAutoDelete bool `mapstructure:"exchange_auto_delete"`
	QueueAutoDelete    bool `mapstructure:"queue_auto_delete"`
	RedialTimeout      int  `mapstructure:"redial_timeout"`

	// new in 2.12.2
	QueueHeaders map[string]any `mapstructure:"queue_headers"`
	// new in 2023.1.0
	ConsumerID string `mapstructure:"consumer_id"`
}

func (c *config) InitDefault() {
	// all options should be in sync with the pipeline defaults in the ConsumerFromPipeline method
	if c.ExchangeType == "" {
		c.ExchangeType = "direct"
	}

	if c.Exchange == "" {
		c.Exchange = "amqp.default"
	}

	if c.RedialTimeout == 0 {
		c.RedialTimeout = 60
	}

	if c.Prefetch == 0 {
		c.Prefetch = 10
	}

	if c.Priority == 0 {
		c.Priority = 10
	}

	if c.Addr == "" {
		c.Addr = "amqp://guest:guest@127.0.0.1:5672/"
	}

	if c.ConsumerID == "" {
		c.ConsumerID = fmt.Sprintf("roadrunner-%s", uuid.NewString())
	}
}
