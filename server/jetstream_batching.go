// Copyright 2025 The NATS Authors
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

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// Tracks the total inflight batches, across all streams and accounts that enable batching.
	globalInflightBatches atomic.Int32
)

type batching struct {
	mu    sync.Mutex
	group map[string]*batchGroup
}

type batchGroup struct {
	lseq  uint64
	store StreamStore
	timer *time.Timer
}

func newBatchGroup(s *Server, batchId string, batches *batching, store StreamStore) *batchGroup {
	b := &batchGroup{store: store}

	// Create a timer to clean up after timeout.
	timeout := streamMaxBatchTimeout
	if maxBatchTimeout := s.getOpts().JetStreamLimits.MaxBatchTimeout; maxBatchTimeout > 0 {
		timeout = maxBatchTimeout
	}
	b.timer = time.AfterFunc(timeout, func() {
		b.cleanup(batchId, batches)
	})
	return b
}

// stopCleanupTimerLocked indicates the batch is ready to be committed.
// If the timer has already cleaned up the batch, we can't commit.
// Otherwise, we ensure the timer does not clean up the batch in the meantime.
// Lock should be held.
func (b *batchGroup) stopCleanupTimerLocked() bool {
	return b.timer.Stop()
}

// cleanup deletes underlying resources associated with the batch and unregisters it from the stream's batches.
func (b *batchGroup) cleanup(batchId string, batches *batching) {
	batches.mu.Lock()
	defer batches.mu.Unlock()
	b.cleanupLocked(batchId, batches)
}

// Lock should be held.
func (b *batchGroup) cleanupLocked(batchId string, batches *batching) {
	globalInflightBatches.Add(-1)
	b.timer.Stop()
	b.store.Delete()
	delete(batches.group, batchId)
}

// batchStagedDiff stages all changes for consistency checks until commit.
type batchStagedDiff struct {
	msgIds             map[string]struct{}
	counter            map[string]*msgCounterRunningTotal
	inflight           map[uint64]uint64
	subjectsInBatch    map[string]struct{}
	expectedPerSubject map[string]*batchExpectedPerSubject
}

type batchExpectedPerSubject struct {
	sseq  uint64 // Stream sequence.
	clseq uint64 // Clustered proposal sequence.
}

func (diff *batchStagedDiff) commit(mset *stream) {
	if len(diff.msgIds) > 0 {
		ts := time.Now().UnixNano()
		mset.ddMu.Lock()
		for msgId := range diff.msgIds {
			// We stage with zero, and will update in processJetStreamMsg once we know the sequence.
			mset.storeMsgIdLocked(&ddentry{msgId, 0, ts})
		}
		mset.ddMu.Unlock()
	}

	// Store running totals for counters, we could have multiple counter increments proposed, but not applied yet.
	if len(diff.counter) > 0 {
		if mset.clusteredCounterTotal == nil {
			mset.clusteredCounterTotal = make(map[string]*msgCounterRunningTotal, len(diff.counter))
		}
		for k, c := range diff.counter {
			mset.clusteredCounterTotal[k] = c
		}
	}

	// Track inflight.
	if len(diff.inflight) > 0 {
		if mset.inflight == nil {
			mset.inflight = make(map[uint64]uint64, len(diff.inflight))
		}
		for clseq, size := range diff.inflight {
			mset.inflight[clseq] = size
		}
	}

	// Track sequence and subject.
	if len(diff.expectedPerSubject) > 0 {
		if mset.expectedPerSubjectSequence == nil {
			mset.expectedPerSubjectSequence = make(map[uint64]string, len(diff.expectedPerSubject))
		}
		if mset.expectedPerSubjectInProcess == nil {
			mset.expectedPerSubjectInProcess = make(map[string]struct{}, len(diff.expectedPerSubject))
		}
		for subj, e := range diff.expectedPerSubject {
			mset.expectedPerSubjectSequence[e.clseq] = subj
			mset.expectedPerSubjectInProcess[subj] = struct{}{}
		}
	}
}

// checkMsgHeadersPreClusteredProposal checks the message for expected/consistency headers.
// mset.mu lock must NOT be held or used.
// mset.clMu lock must be held.
func checkMsgHeadersPreClusteredProposal(
	diff *batchStagedDiff, mset *stream, subject string, hdr []byte, msg []byte, sourced bool, name string,
	jsa *jsAccount, allowTTL bool, allowMsgCounter bool, stype StorageType, store StreamStore,
	interestPolicy bool, discard DiscardPolicy, maxMsgSize int, maxMsgs int64, maxBytes int64,
) ([]byte, []byte, uint64, *ApiError, error) {
	var incr *big.Int

	// Some header checks must be checked pre proposal.
	if len(hdr) > 0 {
		// Since we encode header len as u16 make sure we do not exceed.
		// Again this works if it goes through but better to be pre-emptive.
		if len(hdr) > math.MaxUint16 {
			err := fmt.Errorf("JetStream header size exceeds limits for '%s > %s'", jsa.acc().Name, mset.cfg.Name)
			return hdr, msg, 0, NewJSStreamHeaderExceedsMaximumError(), err
		}
		// Counter increments.
		// Only supported on counter streams, and payload must be empty (if not coming from a source).
		var ok bool
		if incr, ok = getMessageIncr(hdr); !ok {
			apiErr := NewJSMessageIncrInvalidError()
			return hdr, msg, 0, apiErr, apiErr
		} else if incr != nil && !sourced {
			// Only do checks if the message isn't sourced. Otherwise, we need to store verbatim.
			if !allowMsgCounter {
				apiErr := NewJSMessageIncrDisabledError()
				return hdr, msg, 0, apiErr, apiErr
			} else if len(msg) > 0 {
				apiErr := NewJSMessageIncrPayloadError()
				return hdr, msg, 0, apiErr, apiErr
			} else {
				// Check for incompatible headers.
				var doErr bool
				if getRollup(hdr) != _EMPTY_ ||
					getExpectedStream(hdr) != _EMPTY_ ||
					getExpectedLastMsgId(hdr) != _EMPTY_ ||
					getExpectedLastSeqPerSubjectForSubject(hdr) != _EMPTY_ {
					doErr = true
				} else if _, ok = getExpectedLastSeq(hdr); ok {
					doErr = true
				} else if _, ok = getExpectedLastSeqPerSubject(hdr); ok {
					doErr = true
				}

				if doErr {
					apiErr := NewJSMessageIncrInvalidError()
					return hdr, msg, 0, apiErr, apiErr
				}
			}
		}
		// Expected stream name can also be pre-checked.
		if sname := getExpectedStream(hdr); sname != _EMPTY_ && sname != name {
			return hdr, msg, 0, NewJSStreamNotMatchError(), errStreamMismatch
		}
		// TTL'd messages are rejected entirely if TTLs are not enabled on the stream, or if the TTL is invalid.
		if ttl, err := getMessageTTL(hdr); !sourced && (ttl != 0 || err != nil) {
			if !allowTTL {
				return hdr, msg, 0, NewJSMessageTTLDisabledError(), errMsgTTLDisabled
			} else if err != nil {
				return hdr, msg, 0, NewJSMessageTTLInvalidError(), err
			}
		}
		// Check for MsgIds here at the cluster level to avoid excessive CLFS accounting.
		// Will help during restarts.
		if msgId := getMsgId(hdr); msgId != _EMPTY_ {
			// Dedupe if staged.
			if _, ok = diff.msgIds[msgId]; ok {
				return hdr, msg, 0, nil, errMsgIdDuplicate
			}
			mset.ddMu.Lock()
			if dde := mset.checkMsgId(msgId); dde != nil {
				seq := dde.seq
				mset.ddMu.Unlock()
				// Should not return an invalid sequence, in that case error.
				if seq > 0 {
					return hdr, msg, seq, nil, errMsgIdDuplicate
				} else {
					return hdr, msg, 0, NewJSStreamDuplicateMessageConflictError(), errMsgIdDuplicate
				}
			}
			if diff.msgIds == nil {
				diff.msgIds = map[string]struct{}{msgId: {}}
			} else {
				diff.msgIds[msgId] = struct{}{}
			}
			mset.ddMu.Unlock()
		}
	}

	// Apply increment for counter.
	// But only if it's allowed for this stream. This can happen when we store verbatim for a sourced stream.
	if incr == nil && allowMsgCounter {
		apiErr := NewJSMessageIncrMissingError()
		return hdr, msg, 0, apiErr, apiErr
	}
	if incr != nil && allowMsgCounter && store != nil {
		var initial big.Int
		var sources CounterSources

		// If we've got a running total, update that, since we have inflight proposals updating the same counter.
		var ok bool
		var counter *msgCounterRunningTotal
		if counter, ok = diff.counter[subject]; ok {
			initial = *counter.total
			sources = counter.sources
		} else if counter, ok = mset.clusteredCounterTotal[subject]; ok {
			initial = *counter.total
			sources = counter.sources
			// Make an explicit copy to separate the staged data from what's committed.
			// Don't need to initialize all values, they'll be overwritten later.
			counter = &msgCounterRunningTotal{ops: counter.ops}
		} else {
			// Load last message, and store as inflight running total.
			var smv StoreMsg
			sm, err := store.LoadLastMsg(subject, &smv)
			if err == nil && sm != nil {
				var val CounterValue
				// Return an error if the counter is broken somehow.
				if json.Unmarshal(sm.msg, &val) != nil {
					apiErr := NewJSMessageCounterBrokenError()
					return hdr, msg, 0, apiErr, apiErr
				}
				if ncs := sliceHeader(JSMessageCounterSources, sm.hdr); len(ncs) > 0 {
					if err := json.Unmarshal(ncs, &sources); err != nil {
						apiErr := NewJSMessageCounterBrokenError()
						return hdr, msg, 0, apiErr, apiErr
					}
				}
				initial.SetString(val.Value, 10)
			}
		}
		srchdr := sliceHeader(JSStreamSource, hdr)
		if len(srchdr) > 0 {
			// This is a sourced message, so we can't apply Nats-Incr but
			// instead should just update the source count header.
			fields := strings.Split(string(srchdr), " ")
			origStream := fields[0]
			origSubj := subject
			if len(fields) >= 5 {
				origSubj = fields[4]
			}
			var val CounterValue
			if json.Unmarshal(msg, &val) != nil {
				apiErr := NewJSMessageCounterBrokenError()
				return hdr, msg, 0, apiErr, apiErr
			}
			var sourced big.Int
			sourced.SetString(val.Value, 10)
			if sources == nil {
				sources = map[string]map[string]string{}
			}
			if _, ok = sources[origStream]; !ok {
				sources[origStream] = map[string]string{}
			}
			prevVal := sources[origStream][origSubj]
			sources[origStream][origSubj] = sourced.String()
			// We will also replace the Nats-Incr header with the diff
			// between our last value from this source and this one, so
			// that the arithmetic is always correct.
			var previous big.Int
			previous.SetString(prevVal, 10)
			incr.Sub(&sourced, &previous)
			hdr = setHeader(JSMessageIncr, incr.String(), hdr)
		}
		// Now make the change.
		initial.Add(&initial, incr)
		// Generate the new payload.
		var _msg [128]byte
		msg = fmt.Appendf(_msg[:0], "{%q:%q}", "val", initial.String())
		// Write the updated source count headers.
		if len(sources) > 0 {
			nhdr, err := json.Marshal(sources)
			if err != nil {
				return hdr, msg, 0, NewJSMessageCounterBrokenError(), err
			}
			hdr = setHeader(JSMessageCounterSources, string(nhdr), hdr)
		}

		// Check to see if we are over the max msg size.
		maxSize := int64(mset.srv.getOpts().MaxPayload)
		if maxMsgSize >= 0 && int64(maxMsgSize) < maxSize {
			maxSize = int64(maxMsgSize)
		}
		hdrLen, msgLen := int64(len(hdr)), int64(len(msg))
		// Subtract to prevent against overflows.
		if hdrLen > maxSize || msgLen > maxSize-hdrLen {
			return hdr, msg, 0, NewJSStreamMessageExceedsMaximumError(), ErrMaxPayload
		}

		// Keep the in-memory counters up-to-date.
		if counter == nil {
			counter = &msgCounterRunningTotal{}
		}
		counter.total = &initial
		counter.sources = sources
		counter.ops++
		if diff.counter == nil {
			diff.counter = map[string]*msgCounterRunningTotal{subject: counter}
		} else {
			diff.counter[subject] = counter
		}
	}

	// Check if we have an interest policy and discard new with max msgs or bytes.
	// We need to deny here otherwise it could succeed on some peers and not others
	// depending on consumer ack state. So we deny here, if we allow that means we know
	// it would succeed on every peer.
	if interestPolicy && discard == DiscardNew && (maxMsgs > 0 || maxBytes > 0) {
		// Track inflight.
		if diff.inflight == nil {
			diff.inflight = make(map[uint64]uint64)
		}
		if stype == FileStorage {
			diff.inflight[mset.clseq] = fileStoreMsgSizeRaw(len(subject), len(hdr), len(msg))
		} else {
			diff.inflight[mset.clseq] = memStoreMsgSizeRaw(len(subject), len(hdr), len(msg))
		}

		var state StreamState
		mset.store.FastState(&state)

		var err error
		if maxMsgs > 0 && state.Msgs+uint64(len(mset.inflight)+len(diff.inflight)) > uint64(maxMsgs) {
			err = ErrMaxMsgs
		} else if maxBytes > 0 {
			// TODO(dlc) - Could track this rollup independently.
			var bytesPending uint64
			for _, nb := range mset.inflight {
				bytesPending += nb
			}
			for _, nb := range diff.inflight {
				bytesPending += nb
			}
			if state.Bytes+bytesPending > uint64(maxBytes) {
				err = ErrMaxBytes
			}
		}
		if err != nil {
			return hdr, msg, 0, NewJSStreamStoreFailedError(err, Unless(err)), err
		}
	}

	if len(hdr) > 0 {
		// Expected last sequence per subject.
		if seq, exists := getExpectedLastSeqPerSubject(hdr); exists && store != nil {
			// Allow override of the subject used for the check.
			seqSubj := subject
			if optSubj := getExpectedLastSeqPerSubjectForSubject(hdr); optSubj != _EMPTY_ {
				seqSubj = optSubj
			}

			// The subject is already written to in this batch, we can't allow
			// expected checks since they would be incorrect.
			if _, ok := diff.subjectsInBatch[seqSubj]; ok {
				err := errors.New("last sequence by subject mismatch")
				return hdr, msg, 0, NewJSStreamWrongLastSequenceConstantError(), err
			}

			// If the subject is already in process, block as otherwise we could have
			// multiple messages inflight with the same subject.
			if _, found := mset.expectedPerSubjectInProcess[seqSubj]; found {
				err := errors.New("last sequence by subject mismatch")
				return hdr, msg, 0, NewJSStreamWrongLastSequenceConstantError(), err
			}

			// If we've already done an expected-check on this subject, use the cached result.
			if e, ok := diff.expectedPerSubject[seqSubj]; ok {
				if e.sseq != seq {
					err := fmt.Errorf("last sequence by subject mismatch: %d vs %d", seq, e.sseq)
					return hdr, msg, 0, NewJSStreamWrongLastSequenceError(e.sseq), err
				}
				e.clseq = mset.clseq
			} else {
				var smv StoreMsg
				var fseq uint64
				sm, err := store.LoadLastMsg(seqSubj, &smv)
				if sm != nil {
					fseq = sm.seq
				}
				if err == ErrStoreMsgNotFound && seq == 0 {
					fseq, err = 0, nil
				}
				if err != nil || fseq != seq {
					err = fmt.Errorf("last sequence by subject mismatch: %d vs %d", seq, fseq)
					return hdr, msg, 0, NewJSStreamWrongLastSequenceError(fseq), err
				}

				e = &batchExpectedPerSubject{sseq: fseq, clseq: mset.clseq}
				if diff.expectedPerSubject == nil {
					diff.expectedPerSubject = map[string]*batchExpectedPerSubject{seqSubj: e}
				} else {
					diff.expectedPerSubject[seqSubj] = e
				}
			}
		}
	}

	// Store the subject to ensure other messages in this batch using
	// an expected check on the same subject fail.
	if diff.subjectsInBatch == nil {
		diff.subjectsInBatch = map[string]struct{}{subject: {}}
	} else {
		diff.subjectsInBatch[subject] = struct{}{}
	}

	return hdr, msg, 0, nil, nil
}
