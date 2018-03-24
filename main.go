// Copyright (c) OpenFaaS Project 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/bsm/sarama-cluster"
	"github.com/openfaas-incubator/kafka-connector/types"
)

// Sarama currently cannot support latest kafka protocol version 0_11_
var (
	SARAMA_KAFKA_PROTO_VER = sarama.V0_10_2_0
)

type connectorConfig struct {
	gatewayURL      string
	upstreamTimeout time.Duration
	topics          []string
	printResponse   bool
	rebuildInterval time.Duration
	broker          string
}

func main() {
	var client sarama.Client
	var err error

	config := buildConnectorConfig()

	brokers := []string{config.broker + ":9092"}
	for {

		client, err = sarama.NewClient(brokers, nil)
		if client != nil && err == nil {
			break
		}
		if client != nil {
			client.Close()
		}
		fmt.Println("Wait for brokers ("+config.broker+") to come up.. ", brokers)

		time.Sleep(1 * time.Second)
	}

	makeConsumer(client, brokers, config)
}

func makeConsumer(client sarama.Client, brokers []string, config connectorConfig) {
	//setup consumer
	cConfig := cluster.NewConfig()
	cConfig.Version = SARAMA_KAFKA_PROTO_VER
	cConfig.Consumer.Return.Errors = true
	cConfig.Consumer.Offsets.Initial = sarama.OffsetNewest //OffsetOldest
	cConfig.Group.Return.Notifications = true
	cConfig.Group.Session.Timeout = 6 * time.Second
	cConfig.Group.Heartbeat.Interval = 2 * time.Second

	group := "faas-kafka-queue-workers"

	topics := config.topics
	log.Printf("Binding to topics: %v", config.topics)

	consumer, err := cluster.NewConsumer(brokers, group, topics, cConfig)
	if err != nil {
		log.Fatalln("Fail to create Kafka consumer: ", err)
	}

	defer consumer.Close()

	c := makeClient(config.upstreamTimeout)

	topicMap := types.NewTopicMap()

	lookupBuilder := types.FunctionLookupBuilder{
		GatewayURL: config.gatewayURL,
		Client:     makeClient(config.rebuildInterval),
	}

	ticker := time.NewTicker(time.Second * 3)

	go func() {
		for {
			<-ticker.C
			lookups, err := lookupBuilder.Build()
			if err != nil {
				log.Fatalln(err)
			}
			log.Println("Syncing topic map")
			topicMap.Sync(&lookups)
		}
	}()

	mcb := func(msg *sarama.ConsumerMessage) {
		if len(msg.Value) > 0 {

			matchedFunctions := topicMap.Match(msg.Topic)
			for _, matchedFunction := range matchedFunctions {

				log.Printf("Invoke function: %s", matchedFunction)

				reader := bytes.NewReader([]byte(msg.Value))
				httpReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/function/%s", config.gatewayURL, matchedFunction), reader)
				defer httpReq.Body.Close()

				res, doErr := c.Do(httpReq)
				if doErr != nil {
					log.Println("Invalid response:", doErr)
					return
				}
				if res.Body != nil {
					defer res.Body.Close()

					bytesOut, readErr := ioutil.ReadAll(res.Body)
					if readErr != nil {
						log.Printf("Error reading body")
					}

					stringOutput := string(bytesOut)
					if config.printResponse {
						log.Printf("Response [%d] from %s %s", res.StatusCode, matchedFunction, stringOutput)
					} else {
						log.Printf("Response [%d] from %s", res.StatusCode, matchedFunction)
					}
				}

			}
		}
	}

	num := 0

	for {
		select {
		case msg, ok := <-consumer.Messages():
			if ok {
				num = (num + 1) % math.MaxInt32
				fmt.Printf("[#%d] Received on [%v,%v]: '%s'\n", num, msg.Topic, msg.Partition, string(msg.Value))

				mcb(msg)
				consumer.MarkOffset(msg, "") // mark message as processed
			}
		case err = <-consumer.Errors():
			fmt.Println("consumer error: ", err)
		case ntf := <-consumer.Notifications():
			fmt.Printf("Rebalanced: %+v\n", ntf)
		}
	}
}

func makeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 10 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     120 * time.Millisecond,
		},
	}
}

func buildConnectorConfig() connectorConfig {

	broker := "kafka"
	if val, exists := os.LookupEnv("broker_host"); exists {
		broker = val
	}

	topics := []string{}
	if val, exists := os.LookupEnv("topics"); exists {
		for _, topic := range strings.Split(val, ",") {
			if len(topic) > 0 {
				topics = append(topics, topic)
			}
		}
	}
	if len(topics) == 0 {
		log.Fatal(`Provide a list of topics i.e. topics="payment_published,slack_joined"`)
	}

	gatewayURL := "http://gateway:8080"
	if val, exists := os.LookupEnv("gateway_url"); exists {
		gatewayURL = val
	}

	upstreamTimeout := time.Second * 30
	rebuildInterval := time.Second * 3

	if val, exists := os.LookupEnv("upstream_timeout"); exists {
		parsedVal, err := time.ParseDuration(val)
		if err == nil {
			upstreamTimeout = parsedVal
		}
	}

	if val, exists := os.LookupEnv("rebuild_interval"); exists {
		parsedVal, err := time.ParseDuration(val)
		if err == nil {
			rebuildInterval = parsedVal
		}
	}

	return connectorConfig{
		gatewayURL:      gatewayURL,
		upstreamTimeout: upstreamTimeout,
		topics:          topics,
		rebuildInterval: rebuildInterval,
		broker:          broker,
	}
}
