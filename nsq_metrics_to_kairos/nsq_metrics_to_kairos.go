package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"net/http"
	_ "net/http/pprof"

	"github.com/bitly/go-hostpool"
	"github.com/bitly/go-nsq"
	"github.com/bitly/nsq/internal/app"
	met "github.com/grafana/grafana/pkg/metric"
	"github.com/grafana/grafana/pkg/metric/helper"
	"github.com/raintank/raintank-metric/instrumented_nsq"
)

var (
	showVersion = flag.Bool("version", false, "print version string")
	dryRun      = flag.Bool("dry", false, "dry run (disable actually storing into kairosdb")

	concurrency  = flag.Int("concurrency", 10, "number of workers parsing messages and writing into kairosdb. also number of nsq consumers for both high and low prio topic")
	topic        = flag.String("topic", "metrics", "NSQ topic")
	topicLowPrio = flag.String("topic-lowprio", "metrics-lowprio", "NSQ topic")
	channel      = flag.String("channel", "kairos", "NSQ channel")
	maxInFlight  = flag.Int("max-in-flight", 200, "max number of messages to allow in flight")

	statsdAddr = flag.String("statsd-addr", "localhost:8125", "statsd address (default: localhost:8125)")
	statsdType = flag.String("statsd-type", "standard", "statsd type: standard or datadog (default: standard)")

	consumerOpts     = app.StringArray{}
	producerOpts     = app.StringArray{}
	nsqdTCPAddrs     = app.StringArray{}
	lookupdHTTPAddrs = app.StringArray{}
)

func init() {
	flag.Var(&consumerOpts, "consumer-opt", "option to passthrough to nsq.Consumer (may be given multiple times, http://godoc.org/github.com/bitly/go-nsq#Config)")
	flag.Var(&producerOpts, "producer-opt", "option to passthrough to nsq.Producer (may be given multiple times, see http://godoc.org/github.com/bitly/go-nsq#Config)")

	flag.Var(&nsqdTCPAddrs, "nsqd-tcp-address", "nsqd TCP address (may be given multiple times)")
	flag.Var(&lookupdHTTPAddrs, "lookupd-http-address", "lookupd HTTP address (may be given multiple times)")
}

var metricsToKairosOK met.Count
var metricsToKairosFail met.Count
var messagesSize met.Meter
var metricsPerMessage met.Meter
var msgsLowPrioAge met.Meter  // in ms
var msgsHighPrioAge met.Meter // in ms
var kairosPutDuration met.Timer
var inHighPrioItems met.Meter
var inLowPrioItems met.Meter
var msgsToLowPrioOK met.Count
var msgsToLowPrioFail met.Count
var msgsHandleHighPrioOK met.Count
var msgsHandleHighPrioFail met.Count
var msgsHandleLowPrioOK met.Count
var msgsHandleLowPrioFail met.Count

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Println("nsq_metrics_to_kairos")
		return
	}
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	metrics, err := helper.New(true, *statsdAddr, *statsdType, "nsq_metrics_to_kairos", strings.Replace(hostname, ".", "_", -1))
	if err != nil {
		log.Fatal(err)
	}

	if *channel == "" {
		rand.Seed(time.Now().UnixNano())
		*channel = fmt.Sprintf("tail%06d#ephemeral", rand.Int()%999999)
	}

	if *topic == "" {
		log.Fatal("--topic is required")
	}

	if len(nsqdTCPAddrs) == 0 && len(lookupdHTTPAddrs) == 0 {
		log.Fatal("--nsqd-tcp-address or --lookupd-http-address required")
	}
	if len(nsqdTCPAddrs) > 0 && len(lookupdHTTPAddrs) > 0 {
		log.Fatal("use --nsqd-tcp-address or --lookupd-http-address not both")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	cfg := nsq.NewConfig()
	cfg.UserAgent = "nsq_metrics_to_kairos"
	err = app.ParseOpts(cfg, consumerOpts)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxInFlight = *maxInFlight

	consumer, err := insq.NewConsumer(*topic, *channel, cfg, "high_prio.%s", metrics)
	if err != nil {
		log.Fatal(err)
	}

	consumerLowPrio, err := insq.NewConsumer(*topicLowPrio, *channel, cfg, "low_prio.%s", metrics)
	if err != nil {
		log.Fatal(err)
	}

	pCfg := nsq.NewConfig()
	pCfg.UserAgent = "nsq_metrics_to_kairos"
	err = app.ParseOpts(pCfg, producerOpts)
	if err != nil {
		log.Fatal(err)
	}

	metricsToKairosOK = metrics.NewCount("metrics_to_kairos.ok")
	metricsToKairosFail = metrics.NewCount("metrics_to_kairos.fail")
	messagesSize = metrics.NewMeter("message_size", 0)
	metricsPerMessage = metrics.NewMeter("metrics_per_message", 0)
	msgsLowPrioAge = metrics.NewMeter("low_prio.message_age", 0)
	msgsHighPrioAge = metrics.NewMeter("high_prio.message_age", 0)
	kairosPutDuration = metrics.NewTimer("kairos_put_duration", 0)
	inHighPrioItems = metrics.NewMeter("in_high_prio.items", 0)
	inLowPrioItems = metrics.NewMeter("in_low_prio.items", 0)
	msgsToLowPrioOK = metrics.NewCount("to_kairos.to_low_prio.ok")
	msgsToLowPrioFail = metrics.NewCount("to_low_prio.fail")
	msgsHandleHighPrioOK = metrics.NewCount("handle_high_prio.ok")
	msgsHandleHighPrioFail = metrics.NewCount("handle_high_prio.fail")
	msgsHandleLowPrioOK = metrics.NewCount("handle_low_prio.ok")
	msgsHandleLowPrioFail = metrics.NewCount("handle_low_prio.fail")

	hostPool := hostpool.NewEpsilonGreedy(nsqdTCPAddrs, 0, &hostpool.LinearEpsilonValueCalculator{})
	producers := make(map[string]*nsq.Producer)
	for _, addr := range nsqdTCPAddrs {
		producer, err := nsq.NewProducer(addr, pCfg)
		if err != nil {
			log.Fatalf("failed creating producer %s", err)
		}
		producers[addr] = producer
	}

	gateway, err := NewKairosGateway(*dryRun, *concurrency)
	if err != nil {
		log.Fatal(err)
	}

	handler := NewKairosHandler(gateway, hostPool, producers)
	consumer.AddConcurrentHandlers(handler, *concurrency)

	handlerLowPrio := NewKairosLowPrioHandler(gateway)
	consumerLowPrio.AddConcurrentHandlers(handlerLowPrio, *concurrency)

	err = consumer.ConnectToNSQDs(nsqdTCPAddrs)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("INFO : connected to nsqd")

	err = consumer.ConnectToNSQLookupds(lookupdHTTPAddrs)
	if err != nil {
		log.Fatal(err)
	}

	err = consumerLowPrio.ConnectToNSQDs(nsqdTCPAddrs)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("INFO : connected to nsqd")

	err = consumerLowPrio.ConnectToNSQLookupds(lookupdHTTPAddrs)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		log.Println("INFO starting listener for http/debug on :6060")
		log.Println(http.ListenAndServe(":6060", nil))
	}()

	for {
		select {
		case <-consumer.StopChan:
			consumerLowPrio.Stop()
			return
		case <-consumerLowPrio.StopChan:
			consumer.Stop()
			return
		case <-sigChan:
			consumer.Stop()
			consumerLowPrio.Stop()
		}
	}
}