// Copyright IBM Corp. All Rights Reserved.
//
// SPDX-License-Identifier: Apache-2.0
//

package bft_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartBFT-Go/consensus/internal/bft"
	"github.com/SmartBFT-Go/consensus/internal/bft/mocks"
	"github.com/SmartBFT-Go/consensus/pkg/consensus"
	"github.com/SmartBFT-Go/consensus/smartbftprotos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap"
)

const (
	heartbeatTimeout = 60 * time.Second
	heartbeatCount   = 10
)

var (
	heartbeat = &smartbftprotos.Message{
		Content: &smartbftprotos.Message_HeartBeat{
			HeartBeat: &smartbftprotos.HeartBeat{
				View: 10,
				Seq:  10,
			},
		},
	}

	heartbeatFromFarAheadLeader = &smartbftprotos.Message{
		Content: &smartbftprotos.Message_HeartBeat{
			HeartBeat: &smartbftprotos.HeartBeat{
				View: 10,
				Seq:  15,
			},
		},
	}
)

func TestHeartbeatMonitor_New(t *testing.T) {
	basicLog, err := zap.NewDevelopment()
	assert.NoError(t, err)
	log := basicLog.Sugar()

	comm := &mocks.CommMock{}
	handler := &mocks.HeartbeatTimeoutHandler{}

	scheduler := make(chan time.Time)
	hm := bft.NewHeartbeatMonitor(scheduler, log, consensus.DefaultConfig.LeaderHeartbeatTimeout, consensus.DefaultConfig.LeaderHeartbeatCount, comm, handler, &atomic.Value{})
	assert.NotNil(t, hm)
	hm.Close()
}

func TestHeartbeatMonitorLeader(t *testing.T) {
	basicLog, err := zap.NewDevelopment()
	assert.NoError(t, err)
	log := basicLog.Sugar()

	comm := &mocks.CommMock{}
	handler := &mocks.HeartbeatTimeoutHandler{}
	scheduler := make(chan time.Time)

	vs := &atomic.Value{}
	vs.Store(bft.ViewSequence{ViewActive: true})
	hm := bft.NewHeartbeatMonitor(scheduler, log, consensus.DefaultConfig.LeaderHeartbeatTimeout, consensus.DefaultConfig.LeaderHeartbeatCount, comm, handler, vs)

	var heartBeatsSent uint32
	var heartBeatsSentUntilViewBecomesInactive uint32

	var toWG1 sync.WaitGroup
	toWG1.Add(10)
	var toWG2 sync.WaitGroup
	toWG2.Add(10)
	comm.On("BroadcastConsensus", mock.AnythingOfType("*smartbftprotos.Message")).Run(func(args mock.Arguments) {
		msg := args[0].(*smartbftprotos.Message)
		view := msg.GetHeartBeat().View
		atomic.AddUint32(&heartBeatsSent, 1)
		if uint64(10) == view {
			toWG1.Done()
		} else if uint64(20) == view {
			toWG2.Done()
			totalHBsSent := atomic.LoadUint32(&heartBeatsSent)
			if totalHBsSent == 20 {
				// View is stopped
				vs.Store(bft.ViewSequence{ViewActive: false, ProposalSeq: msg.GetHeartBeat().Seq})
				// Record HB number we sent so far
				atomic.StoreUint32(&heartBeatsSentUntilViewBecomesInactive, totalHBsSent)
			}
		}
	}).Return()

	clock := fakeTime{}
	hm.ChangeRole(bft.Leader, 10, 12)
	clock.advanceTime(11, scheduler)
	toWG1.Wait()

	hm.ChangeRole(bft.Leader, 20, 12)
	clock.advanceTime(10, scheduler)
	toWG2.Wait()

	clock.advanceTime(10, scheduler)
	// Ensure we don't advance heartbeats any longer when view is inactive
	assert.Equal(t, atomic.LoadUint32(&heartBeatsSentUntilViewBecomesInactive), atomic.LoadUint32(&heartBeatsSent))

	hm.Close()
}

func TestHeartbeatMonitorFollower(t *testing.T) {
	noop := func(_ *bft.HeartbeatMonitor) {}

	for _, testCase := range []struct {
		description                 string
		onHeartbeatTimeoutCallCount int
		heartbeatMessage            *smartbftprotos.Message
		event                       func(*bft.HeartbeatMonitor)
		sender                      uint64
		viewActive                  bool
		proposalSeqInView           uint64
	}{
		{
			description:                 "timeout expires",
			sender:                      12,
			heartbeatMessage:            &smartbftprotos.Message{},
			onHeartbeatTimeoutCallCount: 1,
			event:                       noop,
		},
		{
			description:      "heartbeats prevent timeout",
			sender:           12,
			heartbeatMessage: heartbeat,
			event:            noop,
		},
		{
			description:      "heartbeats from leader with inactive view don't prevent timeout",
			sender:           12,
			heartbeatMessage: heartbeat,
			event:            noop,
		},
		{
			description:                 "bad heartbeats do not prevent timeout",
			sender:                      12,
			heartbeatMessage:            prePrepare,
			onHeartbeatTimeoutCallCount: 1,
			event:                       noop,
		},
		{
			description:                 "heartbeats not from the leader do not prevent timeout",
			sender:                      13,
			heartbeatMessage:            heartbeat,
			onHeartbeatTimeoutCallCount: 1,
			event:                       noop,
		},
		{
			description:                 "heartbeats from a leader too far ahead lead to timeout",
			sender:                      12,
			heartbeatMessage:            heartbeatFromFarAheadLeader,
			onHeartbeatTimeoutCallCount: 1,
			event:                       noop,
			proposalSeqInView:           10,
			viewActive:                  true,
		},
		{
			description:       "heartbeats from a leader only 1 seq ahead do not lead to timeout",
			sender:            12,
			heartbeatMessage:  heartbeatFromFarAheadLeader,
			event:             noop,
			proposalSeqInView: 14,
			viewActive:        true,
		},
		{
			description:                 "heartbeats from a leader too far ahead when view is disabled do not cause timeouts",
			sender:                      12,
			heartbeatMessage:            heartbeatFromFarAheadLeader,
			onHeartbeatTimeoutCallCount: 0,
			event:                       noop,
			proposalSeqInView:           10,
		},
		{
			description:                 "view change to dead leader",
			sender:                      12,
			onHeartbeatTimeoutCallCount: 1,
			event: func(hm *bft.HeartbeatMonitor) {
				hm.ChangeRole(bft.Follower, 11, 12)
			},
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			basicLog, err := zap.NewDevelopment()
			assert.NoError(t, err)
			log := basicLog.Sugar()

			scheduler := make(chan time.Time)
			incrementUnit := heartbeatTimeout / heartbeatCount

			comm := &mocks.CommMock{}
			handler := &mocks.HeartbeatTimeoutHandler{}
			handler.On("OnHeartbeatTimeout", uint64(10), uint64(12))
			handler.On("OnHeartbeatTimeout", uint64(11), uint64(12))

			viewSequence := &atomic.Value{}
			viewSequence.Store(bft.ViewSequence{
				ViewActive:  testCase.viewActive,
				ProposalSeq: testCase.proposalSeqInView,
			})
			hm := bft.NewHeartbeatMonitor(scheduler, log, consensus.DefaultConfig.LeaderHeartbeatTimeout, consensus.DefaultConfig.LeaderHeartbeatCount, comm, handler, viewSequence)

			hm.ChangeRole(bft.Follower, 10, 12)

			start := time.Now()
			scheduler <- start
			hm.ProcessMsg(12, heartbeat)
			testCase.event(hm)

			start = start.Add(incrementUnit).Add(time.Second)

			for i := time.Duration(1); i <= heartbeatCount*2; i++ {
				elapsed := start.Add(incrementUnit*i + time.Millisecond)
				scheduler <- elapsed
				hm.ProcessMsg(testCase.sender, testCase.heartbeatMessage)
			}
			hm.Close()

			handler.AssertNumberOfCalls(t, "OnHeartbeatTimeout", testCase.onHeartbeatTimeoutCallCount)
		})
	}
}

func TestHeartbeatMonitorLeaderAndFollower(t *testing.T) {
	basicLog, err := zap.NewDevelopment()
	assert.NoError(t, err)
	log := basicLog.Sugar()

	scheduler1 := make(chan time.Time)
	scheduler2 := make(chan time.Time)

	comm1 := &mocks.CommMock{}
	handler1 := &mocks.HeartbeatTimeoutHandler{}
	vs1 := &atomic.Value{}
	vs1.Store(bft.ViewSequence{ViewActive: true})
	hm1 := bft.NewHeartbeatMonitor(scheduler1, log, consensus.DefaultConfig.LeaderHeartbeatTimeout, consensus.DefaultConfig.LeaderHeartbeatCount, comm1, handler1, vs1)

	comm2 := &mocks.CommMock{}
	handler2 := &mocks.HeartbeatTimeoutHandler{}
	vs2 := &atomic.Value{}
	vs2.Store(bft.ViewSequence{ViewActive: true})
	hm2 := bft.NewHeartbeatMonitor(scheduler2, log, consensus.DefaultConfig.LeaderHeartbeatTimeout, consensus.DefaultConfig.LeaderHeartbeatCount, comm2, handler2, vs2)

	comm1.On("BroadcastConsensus", mock.AnythingOfType("*smartbftprotos.Message")).Run(func(args mock.Arguments) {
		msg := args[0].(*smartbftprotos.Message)
		hm2.ProcessMsg(1, msg)
	})

	comm2.On("BroadcastConsensus", mock.AnythingOfType("*smartbftprotos.Message")).Run(func(args mock.Arguments) {
		msg := args[0].(*smartbftprotos.Message)
		hm1.ProcessMsg(2, msg)
	})

	toWG := &sync.WaitGroup{}
	toWG.Add(1)
	handler1.On("OnHeartbeatTimeout", uint64(12), uint64(2)).Run(func(args mock.Arguments) {
		view := args[0].(uint64)
		if view != 12 {
			t.Fail()
		} else {
			toWG.Done()
		}
	}).Return()

	clock := fakeTime{}

	hm1.ChangeRole(bft.Leader, 10, 1)
	hm2.ChangeRole(bft.Follower, 10, 1)
	clock.advanceTime(heartbeatCount*2, scheduler1, scheduler2)

	hm1.ChangeRole(bft.Follower, 11, 2)
	hm2.ChangeRole(bft.Leader, 11, 2)
	clock.advanceTime(heartbeatCount*2, scheduler1, scheduler2)

	hm1.ChangeRole(bft.Follower, 12, 2)
	hm2.ChangeRole(bft.Leader, 12, 2)
	hm2.Close()
	clock.advanceTime(heartbeatCount*2, scheduler1)
	hm1.Close()

	handler1.AssertCalled(t, "OnHeartbeatTimeout", uint64(12), uint64(2))
	handler1.AssertNumberOfCalls(t, "OnHeartbeatTimeout", 1)
}

type fakeTime struct {
	time time.Time
}

func (t *fakeTime) advanceTime(ticks time.Duration, schedulers ...chan time.Time) {
	for i := time.Duration(1); i <= ticks; i++ {
		incrementUnit := heartbeatTimeout / heartbeatCount
		newTime := t.time.Add(incrementUnit)
		for _, scheduler := range schedulers {
			scheduler <- newTime
		}
		t.time = newTime
	}
}
