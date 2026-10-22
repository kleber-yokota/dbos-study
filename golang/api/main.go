// API (EC2 node) — mirror of app.py in Go.
//
// GET|POST /workflows?name=<workflow>&value=<optional arg>
//   - enqueues the workflow onto main_queue (DBOS system DB)
//   - wakes the worker (Lambda) with a payload-less invoke
// POST /workflows/{workflow_id}/cancel  — kill the execution (CANCELLED)
// POST /workflows/{workflow_id}/resume  — continue it from the last completed
//   step: same workflow_id, no new workflow is created
// On the first run, the worker is what creates the schema and the queue; the
// API does the initial wake-up and waits for the schema to be ready.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

const (
	workerAppName = "dbos-go-worker"
	queueName     = "main_queue"
	workerLambda  = "dbos-worker-go"
)

var (
	clientMu sync.Mutex
	client   dbos.Client
)

// getOrMakeClient: lazy creation. On the first run the schema may not exist
// yet — the error is returned without caching, so the retry happens after the wake-up.
func getOrMakeClient() (dbos.Client, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}
	c, err := dbos.NewClient(context.Background(), dbos.ClientConfig{
		DatabaseURL: os.Getenv("DBOS_SYSTEM_DATABASE_URL"),
		AppName:     workerAppName,
	})
	if err != nil {
		return nil, err
	}
	client = c
	return c, nil
}

func queueReady(c dbos.Client) bool {
	_, err := dbos.RetrieveQueue(c, queueName)
	return err == nil
}

// wake: payload-less Lambda invoke — just wakes the worker.
func wake() error {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("floci", "floci", "")),
	)
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	lc := lambda.NewFromConfig(cfg, func(o *lambda.Options) {
		if ep := os.Getenv("FLOCI_ENDPOINT"); ep != "" {
			o.BaseEndpoint = aws.String(ep)
		}
	})
	_, err = lc.Invoke(context.Background(), &lambda.InvokeInput{
		FunctionName:   aws.String(workerLambda),
		Payload:        []byte("{}"),
		InvocationType: types.InvocationTypeEvent, // async: the handler does not block in the listen window
	})
	if err != nil {
		return fmt.Errorf("invoke %s: %w", workerLambda, err)
	}
	return nil
}

func dispatch(w http.ResponseWriter, req *http.Request) {
	name := req.URL.Query().Get("name")
	if name == "" {
		http.Error(w, `{"error":"name parameter is required"}`, http.StatusBadRequest)
		return
	}
	value := req.URL.Query().Get("value")

	c, err := getOrMakeClient()
	if err != nil || !queueReady(c) {
		// First run: the worker (Lambda) creates the schema and the queue.
		if err := wake(); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"initial wake-up: %v"}`, err), http.StatusBadGateway)
			return
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			c, err = getOrMakeClient()
			if err == nil && queueReady(c) {
				break
			}
			if time.Now().After(deadline) {
				http.Error(w, `{"error":"worker did not create the schema in 90s"}`, http.StatusServiceUnavailable)
				return
			}
			time.Sleep(2 * time.Second)
		}
	}

	arg := value
	if arg == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		arg = "api-" + hex.EncodeToString(b)
	}

	handle, err := dbos.Enqueue[string](c, queueName, name, arg)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"enqueue: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if err := wake(); err != nil {
		log.Printf("wake-up after enqueue: %v", err)
	}

	if err := json.NewEncoder(w).Encode(map[string]string{"workflow_id": handle.GetWorkflowID()}); err != nil {
		log.Printf("writing response: %v", err)
	}
}

// workflowIDFromPath extracts the {workflow_id} from /workflows/{id}/<action>.
func workflowIDFromPath(path, action string) string {
	id := strings.TrimSuffix(path, action)
	return strings.TrimPrefix(id, "/workflows/")
}

// workflowExists: the SDK's cancel/resume are silent no-ops for unknown ids.
func workflowExists(c dbos.Client, id string) error {
	h, err := dbos.RetrieveWorkflow[string](c, id)
	if err != nil {
		return err
	}
	_, err = h.GetStatus()
	return err
}

func cancelWorkflow(w http.ResponseWriter, req *http.Request) {
	id := workflowIDFromPath(req.URL.Path, "/cancel")
	if id == "" {
		http.Error(w, `{"error":"workflow_id is required"}`, http.StatusBadRequest)
		return
	}
	c, err := getOrMakeClient()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"client: %v"}`, err), http.StatusInternalServerError)
		return
	}
	if err := workflowExists(c, id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"workflow not found: %v"}`, err), http.StatusNotFound)
		return
	}
	if err := dbos.CancelWorkflows(c, []string{id}); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"cancel: %v"}`, err), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]string{"workflow_id": id, "status": "CANCELLED"}); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func resumeWorkflow(w http.ResponseWriter, req *http.Request) {
	id := workflowIDFromPath(req.URL.Path, "/resume")
	if id == "" {
		http.Error(w, `{"error":"workflow_id is required"}`, http.StatusBadRequest)
		return
	}
	c, err := getOrMakeClient()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"client: %v"}`, err), http.StatusInternalServerError)
		return
	}
	if err := workflowExists(c, id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"workflow not found: %v"}`, err), http.StatusNotFound)
		return
	}
	// Same workflow_id: the execution is re-enqueued from its last completed
	// step — no new workflow is created.
	if _, err := dbos.ResumeWorkflows[any](c, []string{id}); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"resume: %v"}`, err), http.StatusInternalServerError)
		return
	}
	if err := wake(); err != nil {
		log.Printf("wake-up after resume: %v", err)
	}
	if err := json.NewEncoder(w).Encode(map[string]string{"workflow_id": id, "resumed": "true"}); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func main() {
	log.Printf("[api-go] listening on :8000 (app=%s queue=%s lambda=%s)", workerAppName, queueName, workerLambda)
	http.HandleFunc("/workflows", dispatch)
	http.HandleFunc("/workflows/{workflow_id}/cancel", cancelWorkflow) // Go 1.22 pattern: {id} in the pattern is a wildcard
	http.HandleFunc("/workflows/{workflow_id}/resume", resumeWorkflow)
	if err := http.ListenAndServe(":8000", http.DefaultServeMux); err != nil {
		log.Fatalf("http: %v", err)
	}
}
