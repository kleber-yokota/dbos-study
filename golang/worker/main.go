// Lambda worker (Floci) — mirrors worker/worker.py in Go.
//
// Floci runs this binary as /var/task/bootstrap (provided.al2023 runtime).
// Each invocation: creates the DBOS context, registers the workflows and the
// queue, and listens to main_queue for 30s — the window in which it processes
// the tasks enqueued by the API (EC2). The invoke is only a wake-up; the data
// comes from the system DB.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

const (
	appName      = "dbos-go-worker"
	queueName    = "main_queue"
	listenWindow = 30 * time.Second
)

// workerError: exported field — the type must be encodable via gob (DBOS
// persists workflow errors with gob; *errors.errorString is not).
type workerError struct{ Msg string }

func (e *workerError) Error() string { return e.Msg }

// --- steps (mirror of worker/worker.py) ---

func stepFetch(ctx context.Context, task string) (string, error) {
	time.Sleep(500 * time.Millisecond)
	return "payload:" + task, nil
}

func stepValidate(ctx context.Context, payload string) (bool, error) {
	time.Sleep(500 * time.Millisecond)
	return payload != "", nil
}

func stepFailIfException(ctx context.Context, task string) (bool, error) {
	time.Sleep(500 * time.Millisecond)
	if task == "exception" {
		return false, &workerError{Msg: fmt.Sprintf("planned failure: %s", task)}
	}
	return true, nil
}

func stepConfirm(ctx context.Context, task string, approved bool) (string, error) {
	time.Sleep(500 * time.Millisecond)
	if approved {
		return task + ":confirmed", nil
	}
	return task + ":rejected", nil
}

// ProcessTask: normal flow (fetch -> validate -> confirm).
func ProcessTask(ctx dbos.Context, task string) (string, error) {
	payload, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		return stepFetch(c, task)
	})
	if err != nil {
		return "", err
	}
	approved, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
		return stepValidate(c, payload)
	})
	if err != nil {
		return "", err
	}
	return confirm(ctx, task, approved)
}

// TestWorkflow: planned failure when task == "exception".
func TestWorkflow(ctx dbos.Context, task string) (string, error) {
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		return stepFetch(c, task)
	}); err != nil {
		return "", err
	}
	approved, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
		return stepFailIfException(c, task)
	})
	if err != nil {
		return "", err
	}
	return confirm(ctx, task, approved)
}

// confirm: the last step, shared by both workflows.
func confirm(ctx dbos.Context, task string, approved bool) (string, error) {
	return dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		return stepConfirm(c, task, approved)
	})
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) (map[string]any, error) {
	dbCtx, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName:     appName,
		DatabaseURL: os.Getenv("DBOS_SYSTEM_DATABASE_URL"),
	})
	if err != nil {
		log.Printf("[worker-go] DBOS init failed: %v", err)
		return nil, fmt.Errorf("dbos init: %w", err)
	}

	dbos.RegisterWorkflow(dbCtx, ProcessTask, dbos.WithWorkflowName("process_task"))
	dbos.RegisterWorkflow(dbCtx, TestWorkflow, dbos.WithWorkflowName("test_workflow"))

	if _, err := dbos.RegisterQueue(dbCtx, queueName,
		dbos.WithWorkerConcurrency(2),
		dbos.WithQueueOnConflict(dbos.QueueConflictNeverUpdate),
	); err != nil {
		log.Printf("[worker-go] registering queue: %v", err)
	}

	if err := dbos.Launch(dbCtx); err != nil {
		log.Printf("[worker-go] DBOS launch failed: %v", err)
		return nil, fmt.Errorf("launch: %w", err)
	}

	log.Printf("[worker-go] listening on '%s' for %v (concurrency 2)", queueName, listenWindow)
	time.Sleep(listenWindow)

	if err := dbos.Shutdown(dbCtx, 30*time.Second); err != nil {
		log.Printf("[worker-go] shutdown: %v", err)
	}
	log.Printf("[worker-go] DBOS shut down")
	return map[string]any{"window": listenWindow.String()}, nil
}
