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
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// Helper function to check that a cluster is formed
func checkClusterFormed(t testing.TB, servers ...*server.Server) {
	t.Helper()
	expectedNumRoutes := len(servers) - 1
	checkFor(t, 10*time.Second, 100*time.Millisecond, func() error {
		for _, s := range servers {
			if numRoutes := s.NumRoutes(); numRoutes != expectedNumRoutes {
				return fmt.Errorf("Expected %d routes for server %q, got %d", expectedNumRoutes, s.ID(), numRoutes)
			}
		}
		return nil
	})
}

func checkNumRoutes(t *testing.T, s *server.Server, expected int) {
	t.Helper()
	checkFor(t, 5*time.Second, 15*time.Millisecond, func() error {
		if nr := s.NumRoutes(); nr != expected {
			return fmt.Errorf("Expected %v routes, got %v", expected, nr)
		}
		return nil
	})
}

// Helper function to check that a server (or list of servers) have the
// expected number of subscriptions.
func checkExpectedSubs(expected int, servers ...*server.Server) error {
	var err string
	maxTime := time.Now().Add(10 * time.Second)
	for time.Now().Before(maxTime) {
		err = ""
		for _, s := range servers {
			if numSubs := int(s.NumSubscriptions()); numSubs != expected {
				err = fmt.Sprintf("Expected %d subscriptions for server %q, got %d", expected, s.ID(), numSubs)
				break
			}
		}
		if err != "" {
			time.Sleep(10 * time.Millisecond)
		} else {
			break
		}
	}
	if err != "" {
		return errors.New(err)
	}
	return nil
}

func checkSubInterest(t testing.TB, s *server.Server, accName, subject string, timeout time.Duration) {
	t.Helper()
	checkFor(t, timeout, 15*time.Millisecond, func() error {
		acc, err := s.LookupAccount(accName)
		if err != nil {
			return fmt.Errorf("error looking up account %q: %v", accName, err)
		}
		if acc.SubscriptionInterest(subject) {
			return nil
		}
		return fmt.Errorf("no subscription interest for account %q on %q", accName, subject)
	})
}

func checkNoSubInterest(t *testing.T, s *server.Server, accName, subject string, timeout time.Duration) {
	t.Helper()
	acc, err := s.LookupAccount(accName)
	if err != nil {
		t.Fatalf("error looking up account %q: %v", accName, err)
	}

	start := time.Now()
	for time.Now().Before(start.Add(timeout)) {
		if acc.SubscriptionInterest(subject) {
			t.Fatalf("Did not expect interest for %q", subject)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runThreeServers(t *testing.T) (srvA, srvB, srvC *server.Server, optsA, optsB, optsC *server.Options) {
	srvA, optsA = RunServerWithConfig("./configs/srv_a.conf")
	srvB, optsB = RunServerWithConfig("./configs/srv_b.conf")
	srvC, optsC = RunServerWithConfig("./configs/srv_c.conf")

	checkClusterFormed(t, srvA, srvB, srvC)
	return
}

func runServers(t *testing.T) (srvA, srvB *server.Server, optsA, optsB *server.Options) {
	srvA, optsA = RunServerWithConfig("./configs/srv_a.conf")
	srvB, optsB = RunServerWithConfig("./configs/srv_b.conf")

	checkClusterFormed(t, srvA, srvB)
	return
}

func TestProperServerWithRoutesShutdown(t *testing.T) {
	before := runtime.NumGoroutine()
	srvA, srvB, _, _ := runServers(t)
	srvA.Shutdown()
	srvB.Shutdown()
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	delta := after - before
	// There may be some finalizers or IO, but in general more than
	// 2 as a delta represents a problem.
	if delta > 2 {
		t.Fatalf("Expected same number of goroutines, %d vs %d\n", before, after)
	}
}

func TestDoubleRouteConfig(t *testing.T) {
	srvA, srvB, _, _ := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()
}

func TestBasicClusterPubSub(t *testing.T) {
	srvA, srvB, optsA, optsB := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	clientA := createClientConn(t, optsA.Host, optsA.Port)
	defer clientA.Close()

	clientB := createClientConn(t, optsB.Host, optsB.Port)
	defer clientB.Close()

	sendA, expectA := setupConn(t, clientA)
	sendA("SUB foo 22\r\n")
	sendA("PING\r\n")
	expectA(pongRe)

	if err := checkExpectedSubs(1, srvA, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	sendB, expectB := setupConn(t, clientB)
	sendB("PUB foo 2\r\nok\r\n")
	sendB("PING\r\n")
	expectB(pongRe)

	expectMsgs := expectMsgsCommand(t, expectA)

	matches := expectMsgs(1)
	checkMsg(t, matches[0], "foo", "22", "", "2", "ok")
}

func TestClusterQueueSubs(t *testing.T) {
	srvA, srvB, optsA, optsB := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	clientA := createClientConn(t, optsA.Host, optsA.Port)
	defer clientA.Close()

	clientB := createClientConn(t, optsB.Host, optsB.Port)
	defer clientB.Close()

	sendA, expectA := setupConn(t, clientA)
	sendB, expectB := setupConn(t, clientB)

	expectMsgsA := expectMsgsCommand(t, expectA)
	expectMsgsB := expectMsgsCommand(t, expectB)

	// Capture sids for checking later.
	qg1SidsA := []string{"1", "2", "3"}

	// Three queue subscribers
	for _, sid := range qg1SidsA {
		sendA(fmt.Sprintf("SUB foo qg1 %s\r\n", sid))
	}
	sendA("PING\r\n")
	expectA(pongRe)

	// Make sure the subs have propagated to srvB before continuing
	// New cluster proto this will only be 1.
	if err := checkExpectedSubs(1, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	sendB("PUB foo 2\r\nok\r\n")
	sendB("PING\r\n")
	expectB(pongRe)

	// Make sure we get only 1.
	matches := expectMsgsA(1)
	checkMsg(t, matches[0], "foo", "", "", "2", "ok")

	// Capture sids for checking later.
	pSids := []string{"4", "5", "6"}

	// Create 3 normal subscribers
	for _, sid := range pSids {
		sendA(fmt.Sprintf("SUB foo %s\r\n", sid))
	}

	// Create a FWC Subscriber
	pSids = append(pSids, "7")
	sendA("SUB > 7\r\n")
	sendA("PING\r\n")
	expectA(pongRe)

	// Make sure the subs have propagated to srvB before continuing
	// Normal foo and the queue group will be one a piece, so 2 + wc == 3
	if err := checkExpectedSubs(3, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	// Send to B
	sendB("PUB foo 2\r\nok\r\n")
	sendB("PING\r\n")
	expectB(pongRe)

	// Give plenty of time for the messages to flush, so that we don't
	// accidentally only read some of them.
	time.Sleep(time.Millisecond * 250)

	// Should receive 5.
	matches = expectMsgsA(5)
	checkForQueueSid(t, matches, qg1SidsA)
	checkForPubSids(t, matches, pSids)

	// Send to A
	sendA("PUB foo 2\r\nok\r\n")

	// Give plenty of time for the messages to flush, so that we don't
	// accidentally only read some of them.
	time.Sleep(time.Millisecond * 250)

	// Should receive 5.
	matches = expectMsgsA(5)
	checkForQueueSid(t, matches, qg1SidsA)
	checkForPubSids(t, matches, pSids)

	// Now add queue subscribers to B
	qg2SidsB := []string{"1", "2", "3"}
	for _, sid := range qg2SidsB {
		sendB(fmt.Sprintf("SUB foo qg2 %s\r\n", sid))
	}
	sendB("PING\r\n")
	expectB(pongRe)

	// Make sure the subs have propagated to srvA before continuing
	// This will be all the subs on A and just 1 from B that gets coalesced.
	if err := checkExpectedSubs(len(qg1SidsA)+len(pSids)+1, srvA); err != nil {
		t.Fatalf("%v", err)
	}

	// Send to B
	sendB("PUB foo 2\r\nok\r\n")

	// Give plenty of time for the messages to flush, so that we don't
	// accidentally only read some of them.
	time.Sleep(time.Millisecond * 250)

	// Should receive 1 from B.
	matches = expectMsgsB(1)
	checkForQueueSid(t, matches, qg2SidsB)

	// Should receive 5 still from A.
	matches = expectMsgsA(5)
	checkForQueueSid(t, matches, qg1SidsA)
	checkForPubSids(t, matches, pSids)

	// Now drop queue subscribers from A
	for _, sid := range qg1SidsA {
		sendA(fmt.Sprintf("UNSUB %s\r\n", sid))
	}
	sendA("PING\r\n")
	expectA(pongRe)

	// Make sure the subs have propagated to srvB before continuing
	if err := checkExpectedSubs(1+1+len(qg2SidsB), srvB); err != nil {
		t.Fatalf("%v", err)
	}

	// Send to B
	sendB("PUB foo 2\r\nok\r\n")

	// Should receive 1 from B.
	matches = expectMsgsB(1)
	checkForQueueSid(t, matches, qg2SidsB)

	sendB("PING\r\n")
	expectB(pongRe)

	// Should receive 4 now.
	matches = expectMsgsA(4)
	checkForPubSids(t, matches, pSids)

	// Send to A
	sendA("PUB foo 2\r\nok\r\n")

	// Give plenty of time for the messages to flush, so that we don't
	// accidentally only read some of them.
	time.Sleep(time.Millisecond * 250)

	// Should receive 4 now.
	matches = expectMsgsA(4)
	checkForPubSids(t, matches, pSids)
}

// Issue #22
func TestClusterDoubleMsgs(t *testing.T) {
	srvA, srvB, optsA, optsB := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	clientA1 := createClientConn(t, optsA.Host, optsA.Port)
	defer clientA1.Close()

	clientA2 := createClientConn(t, optsA.Host, optsA.Port)
	defer clientA2.Close()

	clientB := createClientConn(t, optsB.Host, optsB.Port)
	defer clientB.Close()

	sendA1, expectA1 := setupConn(t, clientA1)
	sendA2, expectA2 := setupConn(t, clientA2)
	sendB, expectB := setupConn(t, clientB)

	expectMsgsA1 := expectMsgsCommand(t, expectA1)
	expectMsgsA2 := expectMsgsCommand(t, expectA2)

	// Capture sids for checking later.
	qg1SidsA := []string{"1", "2", "3"}

	// Three queue subscribers
	for _, sid := range qg1SidsA {
		sendA1(fmt.Sprintf("SUB foo qg1 %s\r\n", sid))
	}
	sendA1("PING\r\n")
	expectA1(pongRe)

	// Make sure the subs have propagated to srvB before continuing
	if err := checkExpectedSubs(1, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	sendB("PUB foo 2\r\nok\r\n")
	sendB("PING\r\n")
	expectB(pongRe)

	// Make sure we get only 1.
	matches := expectMsgsA1(1)
	checkMsg(t, matches[0], "foo", "", "", "2", "ok")
	checkForQueueSid(t, matches, qg1SidsA)

	// Add a FWC subscriber on A2
	sendA2("SUB > 1\r\n")
	sendA2("SUB foo 2\r\n")
	sendA2("PING\r\n")
	expectA2(pongRe)
	pSids := []string{"1", "2"}

	// Make sure the subs have propagated to srvB before continuing
	if err := checkExpectedSubs(1+2, srvB); err != nil {
		t.Fatalf("%v", err)
	}

	sendB("PUB foo 2\r\nok\r\n")
	sendB("PING\r\n")
	expectB(pongRe)

	matches = expectMsgsA1(1)
	checkMsg(t, matches[0], "foo", "", "", "2", "ok")
	checkForQueueSid(t, matches, qg1SidsA)

	matches = expectMsgsA2(2)
	checkMsg(t, matches[0], "foo", "", "", "2", "ok")
	checkForPubSids(t, matches, pSids)

	// Close ClientA1
	clientA1.Close()

	sendB("PUB foo 2\r\nok\r\n")
	sendB("PING\r\n")
	expectB(pongRe)

	time.Sleep(10 * time.Millisecond)

	matches = expectMsgsA2(2)
	checkMsg(t, matches[0], "foo", "", "", "2", "ok")
	checkForPubSids(t, matches, pSids)
}

// This will test that we drop remote sids correctly.
func TestClusterDropsRemoteSids(t *testing.T) {
	srvA, srvB, optsA, _ := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	clientA := createClientConn(t, optsA.Host, optsA.Port)
	defer clientA.Close()

	sendA, expectA := setupConn(t, clientA)

	// Add a subscription
	sendA("SUB foo 1\r\n")
	sendA("PING\r\n")
	expectA(pongRe)

	// Wait for propagation.
	time.Sleep(100 * time.Millisecond)

	if sc := srvA.NumSubscriptions(); sc != 1 {
		t.Fatalf("Expected one subscription for srvA, got %d\n", sc)
	}
	if sc := srvB.NumSubscriptions(); sc != 1 {
		t.Fatalf("Expected one subscription for srvB, got %d\n", sc)
	}

	// Add another subscription
	sendA("SUB bar 2\r\n")
	sendA("PING\r\n")
	expectA(pongRe)

	// Wait for propagation.
	time.Sleep(100 * time.Millisecond)

	if sc := srvA.NumSubscriptions(); sc != 2 {
		t.Fatalf("Expected two subscriptions for srvA, got %d\n", sc)
	}
	if sc := srvB.NumSubscriptions(); sc != 2 {
		t.Fatalf("Expected two subscriptions for srvB, got %d\n", sc)
	}

	// unsubscription
	sendA("UNSUB 1\r\n")
	sendA("PING\r\n")
	expectA(pongRe)

	// Wait for propagation.
	time.Sleep(100 * time.Millisecond)

	if sc := srvA.NumSubscriptions(); sc != 1 {
		t.Fatalf("Expected one subscription for srvA, got %d\n", sc)
	}
	if sc := srvB.NumSubscriptions(); sc != 1 {
		t.Fatalf("Expected one subscription for srvB, got %d\n", sc)
	}

	// Close the client and make sure we remove subscription state.
	clientA.Close()

	// Wait for propagation.
	time.Sleep(100 * time.Millisecond)
	if sc := srvA.NumSubscriptions(); sc != 0 {
		t.Fatalf("Expected no subscriptions for srvA, got %d\n", sc)
	}
	if sc := srvB.NumSubscriptions(); sc != 0 {
		t.Fatalf("Expected no subscriptions for srvB, got %d\n", sc)
	}
}

// This will test that we drop remote sids correctly.
func TestAutoUnsubscribePropagation(t *testing.T) {
	srvA, srvB, optsA, _ := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	clientA := createClientConn(t, optsA.Host, optsA.Port)
	defer clientA.Close()

	sendA, expectA := setupConn(t, clientA)
	expectMsgs := expectMsgsCommand(t, expectA)

	// We will create subscriptions that will auto-unsubscribe and make sure
	// we are not accumulating orphan subscriptions on the other side.
	for i := 1; i <= 100; i++ {
		sub := fmt.Sprintf("SUB foo %d\r\n", i)
		auto := fmt.Sprintf("UNSUB %d 1\r\n", i)
		sendA(sub)
		sendA(auto)
		// This will trip the auto-unsubscribe
		sendA("PUB foo 2\r\nok\r\n")
		expectMsgs(1)
	}

	sendA("PING\r\n")
	expectA(pongRe)

	time.Sleep(50 * time.Millisecond)

	// Make sure number of subscriptions on B is correct
	if subs := srvB.NumSubscriptions(); subs != 0 {
		t.Fatalf("Expected no subscriptions on remote server, got %d\n", subs)
	}
}

func TestAutoUnsubscribePropagationOnClientDisconnect(t *testing.T) {
	srvA, srvB, optsA, _ := runServers(t)
	defer srvA.Shutdown()
	defer srvB.Shutdown()

	cluster := []*server.Server{srvA, srvB}

	clientA := createClientConn(t, optsA.Host, optsA.Port)
	defer clientA.Close()

	sendA, expectA := setupConn(t, clientA)

	// No subscriptions. Ready to test.
	if err := checkExpectedSubs(0, cluster...); err != nil {
		t.Fatalf("%v", err)
	}

	sendA("SUB foo 1\r\n")
	sendA("UNSUB 1 1\r\n")
	sendA("PING\r\n")
	expectA(pongRe)

	// Waiting cluster subs propagation
	if err := checkExpectedSubs(1, cluster...); err != nil {
		t.Fatalf("%v", err)
	}

	clientA.Close()

	// No subs should be on the cluster when all clients is disconnected
	if err := checkExpectedSubs(0, cluster...); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestClusterNameOption(t *testing.T) {
	conf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		cluster {
			name: MyCluster
			listen: 127.0.0.1:-1
		}
	`))

	s, opts := RunServerWithConfig(conf)
	defer s.Shutdown()

	c := createClientConn(t, opts.Host, opts.Port)
	defer c.Close()

	si := checkInfoMsg(t, c)
	if si.Cluster != "MyCluster" {
		t.Fatalf("Expected a cluster name of %q, got %q", "MyCluster", si.Cluster)
	}
}

func TestEphemeralClusterName(t *testing.T) {
	conf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		cluster {
			listen: 127.0.0.1:-1
		}
	`))

	s, opts := RunServerWithConfig(conf)
	defer s.Shutdown()

	c := createClientConn(t, opts.Host, opts.Port)
	defer c.Close()

	si := checkInfoMsg(t, c)
	if si.Cluster == "" {
		t.Fatalf("Expected an ephemeral cluster name to be set")
	}
}

type captureErrLogger struct {
	dummyLogger
	ch chan string
}

func (c *captureErrLogger) Errorf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	select {
	case c.ch <- msg:
	default:
	}
}

func TestClusterNameConflictsDropRoutes(t *testing.T) {
	ll := &captureErrLogger{ch: make(chan string, 4)}

	conf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		cluster {
			name: MyCluster33
			listen: 127.0.0.1:5244
		}
	`))

	s1, _ := RunServerWithConfig(conf)
	defer s1.Shutdown()
	s1.SetLogger(ll, false, false)

	conf2 := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		cluster {
			name: MyCluster22
			listen: 127.0.0.1:-1
			routes = [nats-route://127.0.0.1:5244]
		}
	`))

	s2, _ := RunServerWithConfig(conf2)
	defer s2.Shutdown()
	s2.SetLogger(ll, false, false)

	select {
	case msg := <-ll.ch:
		if !strings.Contains(msg, "Rejecting connection") || !strings.Contains(msg, "does not match") {
			t.Fatalf("Got bad error about cluster name mismatch")
		}
	case <-time.After(time.Second):
		t.Fatalf("Expected an error, timed out")
	}
}

func TestClusterNameDynamicNegotiation(t *testing.T) {
	conf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		cluster {listen: 127.0.0.1:5244}
	`))

	seed, _ := RunServerWithConfig(conf)
	defer seed.Shutdown()

	oconf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		cluster {
			listen: 127.0.0.1:-1
			routes = [nats-route://127.0.0.1:5244]
		}
	`))

	// Create a random number of additional servers, up to 20.
	numServers := rand.Intn(20) + 1
	servers := make([]*server.Server, 0, numServers+1)
	servers = append(servers, seed)

	for i := 0; i < numServers; i++ {
		s, _ := RunServerWithConfig(oconf)
		defer s.Shutdown()
		servers = append(servers, s)
	}

	// If this passes we should have all the same name.
	checkClusterFormed(t, servers...)

	clusterName := seed.ClusterName()
	for _, s := range servers {
		if s.ClusterName() != clusterName {
			t.Fatalf("Expected the cluster names to all be the same as %q, got %q", clusterName, s.ClusterName())
		}
	}
}
