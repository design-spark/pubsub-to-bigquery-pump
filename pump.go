package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/mchmarny/gcputil/metric"
)

const (
	// custom metrics dimensions
	invocationMetric = "invocation"
	messagesMetric   = "message"
	durationMetric   = "duration"
)

func pump() (count int, err error) {
	ctx := context.Background()
	start := time.Now()

	logger.Printf("creating pubsub client[%s]", projectID)
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("pubsub client[%s]: %v",
			projectID, err)
	}

	logger.Printf("creating importer[%s.%s.%s]",
		projectID, dsName, tblName)
	imp, err := NewImportClient(ctx, dsName, tblName)
	if err != nil {
		return 0, fmt.Errorf("bigquery client[%s.%s]: %v",
			dsName, tblName, err)
	}
	defer imp.Clear()

	logger.Printf("creating pubsub subscription[%s]", subName)
	s := client.Subscription(subName)
	inCtx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	messageCounter := 0
	totalCounter := 0
	var innerError error
	lastMessage := time.Now()

	// this will cancel the sub receive loop if max stall time has reached
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for c := range ticker.C {
			elapsed := int(time.Since(lastMessage).Seconds())
			if elapsed > maxStall {
				logger.Printf("max stall time reached: %v", c)
				cancel()
				ticker.Stop()
				return
			}
		}
	}()

	// start pulling messages from subscription
	receiveErr := s.Receive(inCtx, func(ctx context.Context, msg *pubsub.Message) {

		lastMessage = time.Now()

		mu.Lock()
		defer mu.Unlock()

		messageCounter++
		totalCounter++

		// append message to the importer
		appendErr := imp.Append(msg.Data)
		if appendErr != nil {
			logger.Printf("error on data append: %v", appendErr)
			innerError = appendErr
			return
		}

		msg.Ack() //TODO: Ack after inserts?

		// check whether time to exec the batch
		if messageCounter == batchSize {
			logger.Println("batch size reached")
			messageCounter = 0
			if insertErr := imp.Insert(ctx); insertErr != nil {
				innerError = insertErr
				return
			}
		}

		// check if max job time has been reached
		elapsed := int(time.Since(start).Seconds())
		if elapsed > maxDuration {
			logger.Println("max job exec time reached")
			cancel()
		}

	}) // end revive

	// ticker times no longer needed
	ticker.Stop()

	// receive error
	if receiveErr != nil {
		return 0, fmt.Errorf("pubsub subscription[%s] receive: %v",
			subName, receiveErr)
	}

	// error inside of receive handler
	if innerError != nil {
		return 0, fmt.Errorf("pubsub receive[%s] process error: %v",
			subName, innerError)
	}

	// insert leftovers
	if insertErr := imp.Insert(ctx); insertErr != nil {
		return 0, fmt.Errorf("bigquery insert[%s] error: %v",
			subName, insertErr)
	}

	// metrics
	totalDuration := time.Since(start).Seconds()
	if metricErr := submitMetrics(ctx, subName, totalCounter, totalDuration); metricErr != nil {
		return 0, fmt.Errorf("metrics[%s] error: %v",
			subName, metricErr)
	}

	return totalCounter, nil
}

func submitMetrics(ctx context.Context, id string, c int, d float64) error {
	m, err := metric.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("metric client[%s]: %v", projectID, err)
	}

	l := map[string]string{"subscription": id}

	if err = m.Publish(ctx, invocationMetric, int64(1), l); err != nil {
		return fmt.Errorf("metric record[%s][%s]: %v", id, invocationMetric, err)
	}

	if err = m.Publish(ctx, messagesMetric, int64(c), l); err != nil {
		return fmt.Errorf("metric record[%s][%s]: %v", id, messagesMetric, err)
	}

	if err = m.Publish(ctx, durationMetric, d, l); err != nil {
		return fmt.Errorf("metric record[%s][%s]: %v", id, durationMetric, err)
	}

	return nil
}
