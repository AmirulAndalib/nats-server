// Copyright 2013-2025 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestServerRestartReSliceIssue(t *testing.T) {
	srvA, srvB, optsA, optsB := runServers(t)
	defer srvA.Shutdown()

	urlA := fmt.Sprintf("nats://%s:%d/", optsA.Host, optsA.Port)
	urlB := fmt.Sprintf("nats://%s:%d/", optsB.Host, optsB.Port)

	// msg to send..
	msg := []byte("Hello World")

	servers := []string{urlA, urlB}

	opts := nats.GetDefaultOptions()
	opts.Timeout = (5 * time.Second)
	opts.ReconnectWait = (50 * time.Millisecond)
	opts.MaxReconnect = 1000

	numClients := 20

	reconnects := int32(0)
	reconnectsDone := make(chan bool, numClients)
	opts.ReconnectedCB = func(nc *nats.Conn) {
		atomic.AddInt32(&reconnects, 1)
		reconnectsDone <- true
	}

	clients := make([]*nats.Conn, numClients)

	// Create 20 random clients.
	// Half connected to A and half to B..
	for i := 0; i < numClients; i++ {
		opts.Url = servers[i%2]
		nc, err := opts.Connect()
		if err != nil {
			t.Fatalf("Failed to create connection: %v\n", err)
		}
		clients[i] = nc
		defer nc.Close()

		// Create 10 subscriptions each..
		for x := 0; x < 10; x++ {
			subject := fmt.Sprintf("foo.%d", (rand.Int()%50)+1)
			nc.Subscribe(subject, func(m *nats.Msg) {
				// Just eat it..
			})
		}
		// Pick one subject to send to..
		subject := fmt.Sprintf("foo.%d", (rand.Int()%50)+1)
		go func() {
			time.Sleep(10 * time.Millisecond)
			for i := 1; i <= 100; i++ {
				if err := nc.Publish(subject, msg); err != nil {
					return
				}
				if i%10 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	// Wait for a short bit..
	time.Sleep(20 * time.Millisecond)

	// Restart SrvB
	srvB.Shutdown()
	srvB = RunServer(optsB)
	defer srvB.Shutdown()

	// Check that all expected clients have reconnected
	done := false
	for i := 0; i < numClients/2 && !done; i++ {
		select {
		case <-reconnectsDone:
			done = true
		case <-time.After(3 * time.Second):
			t.Fatalf("Expected %d reconnects, got %d\n", numClients/2, reconnects)
		}
	}

	// Since srvB was restarted, its defer Shutdown() was last, so will
	// exectue first, which would cause clients that have reconnected to
	// it to try to reconnect (causing delays on Windows). So let's
	// explicitly close them here.
	// NOTE: With fix of NATS GO client (reconnect loop yields to Close()),
	//       this change would not be required, however, it still speeeds up
	//       the test, from more than 7s to less than one.
	for i := 0; i < numClients; i++ {
		nc := clients[i]
		nc.Close()
	}
}

// This will test queue subscriber semantics across a cluster in the presence
// of server restarts.
func TestServerRestartAndQueueSubs(t *testing.T) {
	srvA, srvB, optsA, optsB := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	urlA := fmt.Sprintf("nats://%s:%d/", optsA.Host, optsA.Port)
	urlB := fmt.Sprintf("nats://%s:%d/", optsB.Host, optsB.Port)

	// Client options
	opts := nats.GetDefaultOptions()
	opts.Timeout = 5 * time.Second
	opts.ReconnectWait = 20 * time.Millisecond
	opts.MaxReconnect = 1000
	opts.NoRandomize = true

	// Allow us to block on a reconnect completion.
	reconnectsDone := make(chan bool)
	opts.ReconnectedCB = func(nc *nats.Conn) {
		reconnectsDone <- true
	}

	// Helper to wait on a reconnect.
	waitOnReconnect := func() {
		t.Helper()
		select {
		case <-reconnectsDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("Expected a reconnect, timedout!\n")
		}
	}

	// Create two clients..
	opts.Servers = []string{urlA, urlB}
	c1, err := opts.Connect()
	if err != nil {
		t.Fatalf("Failed to create connection for c1: %v\n", err)
	}
	defer c1.Close()

	opts.Servers = []string{urlB, urlA}
	c2, err := opts.Connect()
	if err != nil {
		t.Fatalf("Failed to create connection for c2: %v\n", err)
	}
	defer c2.Close()

	// Flusher helper function.
	flush := func() {
		// Wait for processing.
		c1.Flush()
		c2.Flush()
		// Wait for a short bit for cluster propagation.
		time.Sleep(50 * time.Millisecond)
	}

	// To hold queue results.
	results := make(map[int]int)
	var mu sync.Mutex

	// This corresponds to the subsriptions below.
	const ExpectedMsgCount = 3

	// Make sure we got what we needed, 1 msg only and all seqnos accounted for..
	checkResults := func(numSent int) {
		mu.Lock()
		defer mu.Unlock()

		for i := 0; i < numSent; i++ {
			if results[i] != ExpectedMsgCount {
				t.Fatalf("Received incorrect number of messages, [%d] vs [%d] for seq: %d\n", results[i], ExpectedMsgCount, i)
			}
		}

		// Auto reset results map
		results = make(map[int]int)
	}

	subj := "foo.bar"
	qgroup := "workers"

	cb := func(msg *nats.Msg) {
		mu.Lock()
		defer mu.Unlock()
		seqno, _ := strconv.Atoi(string(msg.Data))
		results[seqno] = results[seqno] + 1
	}

	// Create queue subscribers
	c1.QueueSubscribe(subj, qgroup, cb)
	c2.QueueSubscribe(subj, qgroup, cb)

	// Do a wildcard subscription.
	c1.Subscribe("foo.*", cb)
	c2.Subscribe("foo.*", cb)

	// Wait for processing.
	flush()

	sendAndCheckMsgs := func(numToSend int) {
		for i := 0; i < numToSend; i++ {
			if i%2 == 0 {
				c1.Publish(subj, []byte(strconv.Itoa(i)))
			} else {
				c2.Publish(subj, []byte(strconv.Itoa(i)))
			}
		}
		// Wait for processing.
		flush()
		// Check Results
		checkResults(numToSend)
	}

	////////////////////////////////////////////////////////////////////////////
	// Base Test
	////////////////////////////////////////////////////////////////////////////

	// Make sure subscriptions are propagated in the cluster
	if err := checkExpectedSubs(4, srvA, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	// Now send 10 messages, from each client..
	sendAndCheckMsgs(10)

	////////////////////////////////////////////////////////////////////////////
	// Now restart SrvA and srvB, re-run test
	////////////////////////////////////////////////////////////////////////////

	srvA.Shutdown()
	// Wait for client on A to reconnect to B.
	waitOnReconnect()

	srvA = RunServer(optsA)
	defer srvA.Shutdown()

	srvB.Shutdown()
	// Now both clients should reconnect to A.
	waitOnReconnect()
	waitOnReconnect()

	srvB = RunServer(optsB)
	defer srvB.Shutdown()

	// Make sure the cluster is reformed
	checkClusterFormed(t, srvA, srvB)

	// Make sure subscriptions are propagated in the cluster
	// Clients will be connected to srvA, so that will be 4,
	// but srvB will only have 2 now since we coaelsce.
	if err := checkExpectedSubs(4, srvA); err != nil {
		t.Fatalf("%v", err)
	}
	if err := checkExpectedSubs(2, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	// Now send another 10 messages, from each client..
	sendAndCheckMsgs(10)

	// Since servers are restarted after all client's close defer calls,
	// their defer Shutdown() are last, and so will be executed first,
	// which would cause clients to try to reconnect on exit, causing
	// delays on Windows. So let's explicitly close them here.
	c1.Close()
	c2.Close()
}

// This will test request semantics across a route
func TestRequestsAcrossRoutes(t *testing.T) {
	srvA, srvB, optsA, optsB := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	urlA := fmt.Sprintf("nats://%s:%d/", optsA.Host, optsA.Port)
	urlB := fmt.Sprintf("nats://%s:%d/", optsB.Host, optsB.Port)

	nc1, err := nats.Connect(urlA)
	if err != nil {
		t.Fatalf("Failed to create connection for nc1: %v\n", err)
	}
	defer nc1.Close()

	nc2, err := nats.Connect(urlB)
	if err != nil {
		t.Fatalf("Failed to create connection for nc2: %v\n", err)
	}
	defer nc2.Close()

	response := []byte("I will help you")

	// Connect responder to srvA
	nc1.Subscribe("foo-req", func(m *nats.Msg) {
		nc1.Publish(m.Reply, response)
	})
	// Make sure the route and the subscription are propagated.
	nc1.Flush()

	if err = checkExpectedSubs(1, srvA, srvB); err != nil {
		t.Fatal(err.Error())
	}

	for i := 0; i < 100; i++ {
		if _, err = nc2.Request("foo-req", []byte(strconv.Itoa(i)), 250*time.Millisecond); err != nil {
			t.Fatalf("Received an error on Request test [%d]: %s", i, err)
		}
	}
}

// This will test request semantics across a route to queues
func TestRequestsAcrossRoutesToQueues(t *testing.T) {
	srvA, srvB, optsA, optsB := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	urlA := fmt.Sprintf("nats://%s:%d/", optsA.Host, optsA.Port)
	urlB := fmt.Sprintf("nats://%s:%d/", optsB.Host, optsB.Port)

	nc1, err := nats.Connect(urlA)
	if err != nil {
		t.Fatalf("Failed to create connection for nc1: %v\n", err)
	}
	defer nc1.Close()

	nc2, err := nats.Connect(urlB)
	if err != nil {
		t.Fatalf("Failed to create connection for nc2: %v\n", err)
	}
	defer nc2.Close()

	response := []byte("I will help you")

	// Connect one responder to srvA
	nc1.QueueSubscribe("foo-req", "booboo", func(m *nats.Msg) {
		nc1.Publish(m.Reply, response)
	})
	// Make sure the route and the subscription are propagated.
	nc1.Flush()

	// Connect the other responder to srvB
	nc2.QueueSubscribe("foo-req", "booboo", func(m *nats.Msg) {
		nc2.Publish(m.Reply, response)
	})

	if err = checkExpectedSubs(2, srvA, srvB); err != nil {
		t.Fatal(err.Error())
	}

	for i := 0; i < 100; i++ {
		if _, err = nc2.Request("foo-req", []byte(strconv.Itoa(i)), 500*time.Millisecond); err != nil {
			t.Fatalf("Received an error on Request test [%d]: %s", i, err)
		}
	}

	for i := 0; i < 100; i++ {
		if _, err = nc1.Request("foo-req", []byte(strconv.Itoa(i)), 500*time.Millisecond); err != nil {
			t.Fatalf("Received an error on Request test [%d]: %s", i, err)
		}
	}
}

// This is in response to Issue #1144
// https://github.com/nats-io/nats-server/issues/1144
func TestQueueDistributionAcrossRoutes(t *testing.T) {
	srvA, srvB, _, _ := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	checkClusterFormed(t, srvA, srvB)

	urlA := srvA.ClientURL()
	urlB := srvB.ClientURL()

	nc1, err := nats.Connect(urlA)
	if err != nil {
		t.Fatalf("Failed to create connection for nc1: %v\n", err)
	}
	defer nc1.Close()

	nc2, err := nats.Connect(urlB)
	if err != nil {
		t.Fatalf("Failed to create connection for nc2: %v\n", err)
	}
	defer nc2.Close()

	var qsubs []*nats.Subscription

	// Connect queue subscriptions as mentioned in the issue. 2(A) - 6(B) - 4(A)
	for i := 0; i < 2; i++ {
		sub, _ := nc1.QueueSubscribeSync("foo", "bar")
		qsubs = append(qsubs, sub)
	}
	nc1.Flush()
	for i := 0; i < 6; i++ {
		sub, _ := nc2.QueueSubscribeSync("foo", "bar")
		qsubs = append(qsubs, sub)
	}
	nc2.Flush()
	for i := 0; i < 4; i++ {
		sub, _ := nc1.QueueSubscribeSync("foo", "bar")
		qsubs = append(qsubs, sub)
	}
	nc1.Flush()

	if err := checkExpectedSubs(7, srvA, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	send := 10000
	for i := 0; i < send; i++ {
		nc2.Publish("foo", nil)
	}
	nc2.Flush()

	tp := func() int {
		var total int
		for i := 0; i < len(qsubs); i++ {
			pending, _, _ := qsubs[i].Pending()
			total += pending
		}
		return total
	}

	checkFor(t, time.Second, 10*time.Millisecond, func() error {
		if total := tp(); total != send {
			return fmt.Errorf("Number of total received %d", total)
		}
		return nil
	})

	// The bug is essentially that when we deliver across a route, we
	// prefer locals, but if we randomize to a block of bounce backs, then
	// we walk to the end and find the same local for all the remote options.
	// So what you will see in this case is a large value at #9 (2+6, next one local).

	avg := send / len(qsubs)
	for i := 0; i < len(qsubs); i++ {
		total, _, _ := qsubs[i].Pending()
		if total > avg+(avg*3/10) {
			if i == 8 {
				t.Fatalf("Qsub in 8th position gets majority of the messages (prior 6 spots) in this test")
			}
			t.Fatalf("Received too high, %d vs %d", total, avg)
		}
	}
}
