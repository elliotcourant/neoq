package redis

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/acaloiaro/neoq"
	"github.com/acaloiaro/neoq/handler"
	"github.com/acaloiaro/neoq/internal"
	"github.com/acaloiaro/neoq/jobs"
	"github.com/acaloiaro/neoq/testutils"
	"github.com/hibiken/asynq"
)

const (
	queue  = "testing"
	queue2 = "testing2"
)

func init() {
	var err error
	connString := os.Getenv("TEST_REDIS_URL")
	if connString == "" {
		return
	}

	password := os.Getenv("REDIS_PASSWORD")
	clientOpt := asynq.RedisClientOpt{Addr: connString}
	if password != "" {
		clientOpt.Password = password
	}
	inspector := asynq.NewInspector(clientOpt)
	queues, err := inspector.Queues()
	if err != nil {
		panic(err)
	}

	for _, queue := range queues {
		_, err = inspector.DeleteAllPendingTasks(queue)
		if err != nil {
			panic(err)
		}

		_, err = inspector.DeleteAllCompletedTasks(queue)
		if err != nil {
			panic(err)
		}

		_, err = inspector.DeleteAllRetryTasks(queue)
		if err != nil {
			panic(err)
		}

		_, err = inspector.DeleteAllArchivedTasks(queue)
		if err != nil {
			panic(err)
		}
	}
}

func TestBasicJobProcessing(t *testing.T) {
	timeoutTimer := time.After(5 * time.Second)
	done := make(chan bool)

	connString := os.Getenv("TEST_REDIS_URL")
	if connString == "" {
		t.Skip("Skipping: TEST_REDIS_URL not set")
		return
	}

	password := os.Getenv("REDIS_PASSWORD")
	ctx := context.TODO()
	nq, err := neoq.New(
		ctx,
		neoq.WithBackend(Backend),
		WithAddr(connString),
		WithPassword(password),
		WithShutdownTimeout(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nq.Shutdown(ctx)

	h := handler.New(queue, func(_ context.Context) (err error) {
		done <- true
		return
	})

	err = nq.Start(ctx, h)
	if err != nil {
		t.Error(err)
	}

	jid, e := nq.Enqueue(ctx, &jobs.Job{
		Queue: queue,
		Payload: map[string]interface{}{
			"message": fmt.Sprintf("hello world: %d", internal.RandInt(10000000000)),
		},
	})
	if e != nil || jid == jobs.DuplicateJobID {
		t.Error(e)
	}

	select {
	case <-timeoutTimer:
		err = jobs.ErrJobTimeout
	case <-done:
		log.Println("DONE")
	}

	if err != nil {
		t.Error(err)
	}
}

// TestBasicJobMultipleQueue tests that the redis backend is able to process jobs on multiple queues
func TestBasicJobMultipleQueue(t *testing.T) {
	done := make(chan bool)
	doneCnt := 0

	timeoutTimer := time.After(30 * time.Second)

	connString := os.Getenv("TEST_REDIS_URL")
	if connString == "" {
		t.Skip("Skipping: TEST_REDIS_URL not set")
		return
	}

	password := os.Getenv("REDIS_PASSWORD")
	ctx := context.TODO()
	nq, err := neoq.New(ctx,
		neoq.WithBackend(Backend),
		WithAddr(connString),
		WithPassword(password),
		WithShutdownTimeout(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nq.Shutdown(ctx)

	h := handler.New(queue, func(_ context.Context) (err error) {
		done <- true
		return
	})

	h2 := handler.New(queue2, func(_ context.Context) (err error) {
		done <- true
		return
	})

	err = nq.Start(ctx, h)
	if err != nil {
		t.Error(err)
	}

	err = nq.Start(ctx, h2)
	if err != nil {
		t.Error(err)
	}

	jid, e := nq.Enqueue(ctx, &jobs.Job{
		Queue: queue,
		Payload: map[string]interface{}{
			"message": fmt.Sprintf("hello world: %d", internal.RandInt(10000000000)),
		},
	})
	if e != nil || jid == jobs.DuplicateJobID {
		t.Error(e)
	}

	jid2, e := nq.Enqueue(ctx, &jobs.Job{
		Queue: queue2,
		Payload: map[string]interface{}{
			"message": fmt.Sprintf("hello world: %d", internal.RandInt(10000000000)),
		},
	})
	if e != nil || jid2 == jobs.DuplicateJobID {
		t.Error(e)
	}

results_loop:
	for {
		select {
		case <-timeoutTimer:
			err = jobs.ErrJobTimeout
			break results_loop
		case <-done:
			doneCnt++
			if doneCnt == 2 {
				break results_loop
			}
		}
	}

	if err != nil {
		t.Error(err)
	}
}

func TestStartCron(t *testing.T) {
	done := make(chan bool)

	timeoutTimer := time.After(5 * time.Second)

	connString := os.Getenv("TEST_REDIS_URL")
	if connString == "" {
		t.Skip("Skipping: TEST_REDIS_URL not set")
		return
	}

	password := os.Getenv("REDIS_PASSWORD")
	ctx := context.TODO()
	nq, err := neoq.New(ctx, neoq.WithBackend(Backend), WithAddr(connString), WithPassword(password))
	if err != nil {
		t.Fatal(err)
	}
	defer nq.Shutdown(ctx)

	h := handler.NewPeriodic(func(_ context.Context) (err error) {
		done <- true
		return
	})

	err = nq.StartCron(ctx, "* * * * * *", h)
	if err != nil {
		t.Error(err)
	}

	select {
	case <-timeoutTimer:
		err = jobs.ErrJobTimeout
	case <-done:
	}

	if err != nil {
		t.Error(err)
	}
}

func TestJobProcessingWithOptions(t *testing.T) {
	const queue = "testing"
	timeoutTimer := time.After(5 * time.Second)
	logsChan := make(chan string, 1)

	connString := os.Getenv("TEST_REDIS_URL")
	if connString == "" {
		t.Skip("Skipping: TEST_REDIS_URL not set")
		return
	}

	password := os.Getenv("REDIS_PASSWORD")
	ctx := context.Background()
	nq, err := neoq.New(
		ctx,
		neoq.WithBackend(Backend),
		WithAddr(connString),
		WithPassword(password),
		WithShutdownTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nq.Shutdown(ctx)

	nq.SetLogger(testutils.NewTestLogger(logsChan))

	h := handler.New(queue, func(_ context.Context) (err error) {
		time.Sleep(50 * time.Millisecond)
		return
	})
	h.WithOptions(
		handler.JobTimeout(1*time.Millisecond),
		handler.Concurrency(1),
	)

	err = nq.Start(ctx, h)
	if err != nil {
		t.Error(err)
	}

	jid, e := nq.Enqueue(ctx, &jobs.Job{
		Queue: queue,
		Payload: map[string]interface{}{
			"message": fmt.Sprintf("hello world: %d", internal.RandInt(10000000000)),
		},
	})
	if e != nil || jid == jobs.DuplicateJobID {
		t.Error(e)
	}

	expectedLogMsg := "error handling job [error job exceeded its 1ms timeout: context deadline exceeded]" //nolint: dupword
	select {
	case <-timeoutTimer:
		err = jobs.ErrJobTimeout
	case actualLogMsg := <-logsChan:
		if actualLogMsg == expectedLogMsg {
			err = nil
			break
		}

		err = fmt.Errorf("%s != %s", actualLogMsg, expectedLogMsg) //nolint:all
	}

	if err != nil {
		t.Error(err)
	}
}

func TestJobProcessingWithJobDeadline(t *testing.T) {
	const queue = "testing"
	timeoutTimer := time.After(100 * time.Millisecond)
	done := make(chan bool)

	connString := os.Getenv("TEST_REDIS_URL")
	if connString == "" {
		t.Skip("Skipping: TEST_REDIS_URL not set")
		return
	}

	password := os.Getenv("REDIS_PASSWORD")
	ctx := context.Background()
	nq, err := neoq.New(
		ctx,
		neoq.WithBackend(Backend),
		WithAddr(connString),
		WithPassword(password),
		WithShutdownTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nq.Shutdown(ctx)

	h := handler.New(queue, func(_ context.Context) (err error) {
		time.Sleep(50 * time.Millisecond)
		done <- true
		return
	})

	err = nq.Start(ctx, h)
	if err != nil {
		t.Error(err)
	}

	dl := time.Now().UTC()
	jid, e := nq.Enqueue(ctx, &jobs.Job{
		Queue: queue,
		Payload: map[string]interface{}{
			"message": fmt.Sprintf("hello world: %d", internal.RandInt(10000000000)),
		},
		Deadline: &dl,
	})
	if e != nil || jid == jobs.DuplicateJobID {
		t.Error(e)
	}

	select {
	case <-timeoutTimer:
		err = nil
	case <-done:
		err = errors.New("job should not have completed, but did") //nolint:all
	}

	if err != nil {
		t.Error(err)
	}
}
