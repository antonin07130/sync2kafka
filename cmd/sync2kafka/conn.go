package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"sync"

	kafkasync "github.com/mcluseau/kafka-sync"

	"github.com/mcluseau/sync2kafka/client"
)

const kvBufferSize = 1000

var (
	token             = flag.String("token", "", "Require a token to operate")
	allowAllTopics    = flag.Bool("allow-all-topics", false, "Allow any topic to be synchronized")
	allowedTopicsFile = flag.String("allowed-topics-file", "", "File containing allowed topics (1 per line; # is comment)")
)

type KeyValue = kafkasync.KeyValue
type SyncStats = kafkasync.Stats
type SyncInitInfo = client.SyncInitInfo
type SyncResult = client.SyncResult
type JsonKV = client.JsonKV
type BinaryKV = client.BinaryKV

func handleConn(conn net.Conn) {
	logPrefix := fmt.Sprintf("from %v: ", conn.RemoteAddr().String())

	log.Print(logPrefix, "new connection")
	status := newConnStatus(conn)

	defer func() {
		log.Print(logPrefix, "closing connection")
		conn.Close()
		status.Finished()

		if err := recover(); err != nil {
			buf := make([]byte, 64*1024)
			runtime.Stack(buf, false)
			log.Print(logPrefix, "panic: ", err, "\n", string(buf))
		}
	}()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	init := &SyncInitInfo{}
	if err := dec.Decode(init); err != nil {
		log.Print(logPrefix, "failed to read init object: ", err)
		return
	}

	if init.Token != *token {
		log.Print(logPrefix, "authentication failed: wrong token")
		return
	}

	topic := *targetTopic
	if len(init.Topic) != 0 {
		topic = init.Topic
	}

	if len(topic) == 0 {
		log.Printf("%srejecting: no topic specified and no default topic", logPrefix)
		return
	}

	if !isTopicAllowed(topic) {
		log.Printf("%srejecting topic %q", logPrefix, init.Topic)
		return
	}

	if !lockTopic(topic) {
		log.Printf("%srejecting, topic %q already locked.", logPrefix, topic)
		return
	}
	defer unlockTopic(topic)

	log.Printf("%saccepting topic %q", logPrefix, init.Topic)
	status.TargetTopic = topic
	logPrefix += fmt.Sprintf("to topic %q: ", init.Topic)

	wg := sync.WaitGroup{}
	wg.Add(1)

	var syncErr error

	kvSource := make(chan KeyValue, kvBufferSize)

	cancel := make(chan bool, 1)
	defer close(cancel)

	go func() {
		defer wg.Done()
		status.SyncStats, syncErr = (&syncSpec{
			Source:      kvSource,
			TargetTopic: topic,
			DoDelete:    init.DoDelete,
			Cancel:      cancel,
		}).sync()
	}()

	status.Status = "reading data"

	var err error
	switch init.Format {
	case "json":
		err = readJsonKVs(dec, kvSource, status)

	case "binary":
		log.Println("read binary")
		err = readBinaryKVs(dec, kvSource, status)

	default:
		log.Printf("%sunknown mode %q, closing connection", logPrefix, init.Format)
		return
	}

	if err != nil {
		log.Printf("%sfailed to read values from %v: %v", logPrefix, conn.RemoteAddr(), err)
		return
	}

	log.Print(logPrefix, "finished reading values")
	close(kvSource)

	status.Status = "finializing"
	wg.Wait()

	if status.SyncStats != nil {
		log.Print(logPrefix, "sync stats:\n", status.SyncStats.LogString())
	}

	if syncErr != nil {
		enc.Encode(SyncResult{false})

		log.Print(logPrefix, "sync failed: ", syncErr)
		return
	}

	enc.Encode(SyncResult{true})
}

func readJsonKVs(dec *json.Decoder, out chan KeyValue, status *ConnStatus) error {
	for {
		obj := JsonKV{}
		if err := dec.Decode(&obj); err != nil {
			return err
		}

		if obj.EndOfTransfer {
			return nil
		}

		status.ItemsRead++

		out <- KeyValue{
			Key:   *obj.Key,
			Value: *obj.Value,
		}
	}
}

func readBinaryKVs(dec *json.Decoder, out chan KeyValue, status *ConnStatus) error {
	for {
		obj := BinaryKV{}
		if err := dec.Decode(&obj); err != nil {
			return err
		}

		if obj.EndOfTransfer {
			return nil
		}

		status.ItemsRead++

		out <- KeyValue{
			Key:   obj.Key,
			Value: obj.Value,
		}
	}
}

func isTopicAllowed(topic string) bool {
	if *allowAllTopics {
		return true
	}

	if len(*allowedTopicsFile) == 0 {
		return topic == *targetTopic
	}

	// check allowed topics file
	file, err := os.Open(*allowedTopicsFile)
	if err != nil {
		log.Print("failed to open allowed topics file, not allowing: ", err)
		return false
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if len(topic) == 0 {
			continue
		}

		if line[0] == '#' {
			continue
		}

		if line == topic {
			return true
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("failed to read allowed topics, not allowing: %v", err)
		return false
	}

	// nothing more to allow
	return false
}
