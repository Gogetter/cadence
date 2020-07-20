// Copyright (c) 2017-2020 Uber Technologies Inc.

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package queue

import (
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"

	"github.com/uber/cadence/common/collection"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/loggerimpl"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/shard"
	"github.com/uber/cadence/service/history/task"
)

type (
	processorBaseSuite struct {
		suite.Suite
		*require.Assertions

		controller        *gomock.Controller
		mockShard         *shard.TestContext
		mockTaskProcessor *task.MockProcessor

		redispatchQueue collection.Queue
		logger          log.Logger
		metricsClient   metrics.Client
		metricsScope    metrics.Scope
	}
)

func TestProcessorBaseSuite(t *testing.T) {
	s := new(processorBaseSuite)
	suite.Run(t, s)
}

func (s *processorBaseSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.controller = gomock.NewController(s.T())
	s.mockShard = shard.NewTestContext(
		s.controller,
		&persistence.ShardInfo{
			ShardID:          10,
			RangeID:          1,
			TransferAckLevel: 0,
		},
		config.NewForTest(),
	)
	s.mockTaskProcessor = task.NewMockProcessor(s.controller)

	s.redispatchQueue = collection.NewConcurrentQueue()
	s.logger = loggerimpl.NewDevelopmentForTest(s.Suite)
	s.metricsClient = metrics.NewClient(tally.NoopScope, metrics.History)
	s.metricsScope = s.metricsClient.Scope(metrics.TransferQueueProcessorScope)
}

func (s *processorBaseSuite) TearDownTest() {
	s.controller.Finish()
	s.mockShard.Finish(s.T())
}

func (s *processorBaseSuite) TestRedispatchTask_ProcessorShutDown() {
	redispatchQueue := collection.NewConcurrentQueue()

	numTasks := 5
	for i := 0; i != numTasks; i++ {
		mockTask := task.NewMockTask(s.controller)
		redispatchQueue.Add(mockTask)
	}

	shutDownCh := make(chan struct{})

	successfullyRedispatched := 3
	var calls []*gomock.Call
	for i := 0; i != successfullyRedispatched-1; i++ {
		calls = append(calls, s.mockTaskProcessor.EXPECT().TrySubmit(gomock.Any()).Return(true, nil))
	}
	calls = append(calls, s.mockTaskProcessor.EXPECT().TrySubmit(gomock.Any()).DoAndReturn(func(_ interface{}) (bool, error) {
		close(shutDownCh)
		return true, nil
	}))
	calls = append(calls, s.mockTaskProcessor.EXPECT().TrySubmit(gomock.Any()).Return(false, errors.New("processor shutdown")))
	gomock.InOrder(calls...)

	RedispatchTasks(
		redispatchQueue,
		s.mockTaskProcessor,
		s.logger,
		s.metricsScope,
		shutDownCh,
	)

	s.Equal(numTasks-successfullyRedispatched-1, redispatchQueue.Len())
}

func (s *processorBaseSuite) TestRedispatchTask_Random() {
	redispatchQueue := collection.NewConcurrentQueue()

	numTasks := 10
	dispatched := 0

	for i := 0; i != numTasks; i++ {
		mockTask := task.NewMockTask(s.controller)
		redispatchQueue.Add(mockTask)
		submitted := false
		if rand.Intn(2) == 0 {
			submitted = true
			dispatched++
		}
		s.mockTaskProcessor.EXPECT().TrySubmit(task.NewMockTaskMatcher(mockTask)).Return(submitted, nil)
	}

	shutDownCh := make(chan struct{})
	RedispatchTasks(
		redispatchQueue,
		s.mockTaskProcessor,
		s.logger,
		s.metricsScope,
		shutDownCh,
	)

	s.Equal(numTasks-dispatched, redispatchQueue.Len())
}

func (s *processorBaseSuite) TestRedispatchTask_Concurrent() {
	redispatchQueue := collection.NewConcurrentQueue()

	numTasks := 10
	concurrency := 3
	dispatched := 0

	for i := 0; i != numTasks; i++ {
		mockTask := task.NewMockTask(s.controller)
		redispatchQueue.Add(mockTask)
		submitted := false
		if rand.Intn(2) == 0 {
			submitted = true
			dispatched++
		}
		s.mockTaskProcessor.EXPECT().TrySubmit(task.NewMockTaskMatcher(mockTask)).Return(submitted, nil).AnyTimes()
	}

	shutDownCh := make(chan struct{})
	wg := sync.WaitGroup{}
	wg.Add(concurrency)
	for i := 0; i != concurrency; i++ {
		go func() {
			RedispatchTasks(
				redispatchQueue,
				s.mockTaskProcessor,
				s.logger,
				s.metricsScope,
				shutDownCh,
			)
			wg.Done()
		}()
	}
	wg.Wait()

	s.Equal(numTasks-dispatched, redispatchQueue.Len())
}

func (s *processorBaseSuite) TestSplitQueue() {
	mockQueueSplitPolicy := NewMockProcessingQueueSplitPolicy(s.controller)

	processingQueueStates := []ProcessingQueueState{
		NewProcessingQueueState(
			0,
			newTransferTaskKey(0),
			newTransferTaskKey(100),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, true),
		),
		NewProcessingQueueState(
			1,
			newTransferTaskKey(0),
			newTransferTaskKey(100),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, false),
		),
		NewProcessingQueueState(
			0,
			newTransferTaskKey(100),
			newTransferTaskKey(1000),
			NewDomainFilter(map[string]struct{}{}, true),
		),
	}
	mockQueueSplitPolicy.EXPECT().Evaluate(NewProcessingQueue(processingQueueStates[0], s.logger, s.metricsClient)).Return(nil).Times(1)
	mockQueueSplitPolicy.EXPECT().Evaluate(NewProcessingQueue(processingQueueStates[1], s.logger, s.metricsClient)).Return([]ProcessingQueueState{
		NewProcessingQueueState(
			2,
			newTransferTaskKey(0),
			newTransferTaskKey(100),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, false),
		),
	}).Times(1)
	mockQueueSplitPolicy.EXPECT().Evaluate(NewProcessingQueue(processingQueueStates[2], s.logger, s.metricsClient)).Return([]ProcessingQueueState{
		NewProcessingQueueState(
			0,
			newTransferTaskKey(100),
			newTransferTaskKey(1000),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}, "testDomain2": {}, "testDomain3": {}}, false),
		),
		NewProcessingQueueState(
			1,
			newTransferTaskKey(100),
			newTransferTaskKey(1000),
			NewDomainFilter(map[string]struct{}{"testDomain2": {}}, false),
		),
		NewProcessingQueueState(
			2,
			newTransferTaskKey(100),
			newTransferTaskKey(1000),
			NewDomainFilter(map[string]struct{}{"testDomain3": {}}, false),
		),
	}).Times(1)

	processorBase := s.newTestProcessorBase(
		processingQueueStates,
		nil,
		nil,
		nil,
	)

	nextPollTime := make(map[int]time.Time)
	processorBase.splitProcessingQueueCollection(
		mockQueueSplitPolicy,
		func(level int, pollTime time.Time) {
			nextPollTime[level] = pollTime
		},
	)

	processingQueueCollections := processorBase.processingQueueCollections
	s.Len(processingQueueCollections, 3)
	s.Len(processingQueueCollections[0].Queues(), 2)
	s.Len(processingQueueCollections[1].Queues(), 1)
	s.Len(processingQueueCollections[2].Queues(), 2)
	for idx := 1; idx != len(processingQueueCollections)-1; idx++ {
		s.Less(
			processingQueueCollections[idx-1].Level(),
			processingQueueCollections[idx].Level(),
		)
	}
	s.Len(nextPollTime, 3)
	for _, nextPollTime := range nextPollTime {
		s.Zero(nextPollTime)
	}
}

func (s *processorBaseSuite) TestUpdateAckLevel_Transfer_ProcessedFinished() {
	processingQueueStates := []ProcessingQueueState{
		NewProcessingQueueState(
			2,
			newTransferTaskKey(100),
			newTransferTaskKey(100),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, false),
		),
		NewProcessingQueueState(
			0,
			newTransferTaskKey(1000),
			newTransferTaskKey(1000),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}, "testDomain2": {}}, true),
		),
	}
	queueShutdown := false
	queueShutdownFn := func() error {
		queueShutdown = true
		return nil
	}

	processorBase := s.newTestProcessorBase(
		processingQueueStates,
		nil,
		nil,
		queueShutdownFn,
	)

	processFinished, err := processorBase.updateAckLevel()
	s.NoError(err)
	s.True(processFinished)
	s.True(queueShutdown)
}

func (s *processorBaseSuite) TestUpdateAckLevel_Tranfer_ProcessNotFinished() {
	processingQueueStates := []ProcessingQueueState{
		NewProcessingQueueState(
			2,
			newTransferTaskKey(5),
			newTransferTaskKey(100),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, false),
		),
		NewProcessingQueueState(
			1,
			newTransferTaskKey(2),
			newTransferTaskKey(100),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, false),
		),
		NewProcessingQueueState(
			0,
			newTransferTaskKey(100),
			newTransferTaskKey(1000),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}, "testDomain2": {}}, true),
		),
	}
	updateAckLevel := int64(0)
	updateTransferAckLevelFn := func(ackLevel task.Key) error {
		updateAckLevel = ackLevel.(transferTaskKey).taskID
		return nil
	}

	processorBase := s.newTestProcessorBase(
		processingQueueStates,
		nil,
		updateTransferAckLevelFn,
		nil,
	)

	processFinished, err := processorBase.updateAckLevel()
	s.NoError(err)
	s.False(processFinished)
	s.Equal(int64(2), updateAckLevel)
}

func (s *processorBaseSuite) TestUpdateAckLevel_Timer() {
	now := time.Now()
	processingQueueStates := []ProcessingQueueState{
		NewProcessingQueueState(
			2,
			newTimerTaskKey(now.Add(-5*time.Second), 0),
			newTimerTaskKey(now, 0),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, false),
		),
		NewProcessingQueueState(
			1,
			newTimerTaskKey(now.Add(-3*time.Second), 0),
			newTimerTaskKey(now.Add(5*time.Second), 0),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}}, false),
		),
		NewProcessingQueueState(
			0,
			newTimerTaskKey(now.Add(-1*time.Second), 0),
			newTimerTaskKey(now.Add(100*time.Second), 0),
			NewDomainFilter(map[string]struct{}{"testDomain1": {}, "testDomain2": {}}, true),
		),
	}
	updateAckLevel := time.Time{}
	updateTransferAckLevelFn := func(ackLevel task.Key) error {
		updateAckLevel = ackLevel.(timerTaskKey).visibilityTimestamp
		return nil
	}

	timerQueueProcessBase := s.newTestProcessorBase(processingQueueStates, nil, updateTransferAckLevelFn, nil)
	processFinished, err := timerQueueProcessBase.updateAckLevel()
	s.NoError(err)
	s.False(processFinished)
	s.Equal(now.Add(-5*time.Second), updateAckLevel)
}

func (s *processorBaseSuite) newTestProcessorBase(
	processingQueueStates []ProcessingQueueState,
	maxReadLevel updateMaxReadLevelFn,
	updateTransferAckLevel updateClusterAckLevelFn,
	transferQueueShutdown queueShutdownFn,
) *processorBase {
	return newProcessorBase(
		s.mockShard,
		processingQueueStates,
		s.mockTaskProcessor,
		newTransferQueueProcessorOptions(s.mockShard.GetConfig(), true, false),
		maxReadLevel,
		updateTransferAckLevel,
		transferQueueShutdown,
		s.logger,
		s.metricsClient,
	)
}
